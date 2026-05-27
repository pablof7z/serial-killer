package main_test

import (
	"context"
	"fmt"
	"iter"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/policies"

	"serial-killer/internal/chain"
)

func setupTestRelay(t *testing.T) (string, *chain.State, func()) {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "chain-state-*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpFile.Close()

	store := &slicestore.SliceStore{}
	if err := store.Init(); err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	relay := khatru.NewRelay()
	relay.Info.Name = "test-relay"
	relay.Info.Description = "test relay"

	relay.UseEventstore(store, 500)

	chainState := chain.NewState(tmpFile.Name())

	// Save original store functions so we can wrap them
	originalQueryStored := relay.QueryStored
	originalStoreEvent := relay.StoreEvent
	originalReplaceEvent := relay.ReplaceEvent
	originalDeleteEvent := relay.DeleteEvent

	// Wrap QueryStored to filter out tombstoned chain events
	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) {
			for evt := range originalQueryStored(ctx, filter) {
				if chain.IsChainEvent(evt) {
					chainID, _ := chain.GetChainID(evt)
					if chainState.IsChainEventDeleted(chainID, evt.ID.Hex()) {
						continue // skip tombstoned chain events
					}
				}
				if !yield(evt) {
					return
				}
			}
		}
	}

	// Wrap StoreEvent: for chain events, atomically validate and update head
	relay.StoreEvent = func(ctx context.Context, evt nostr.Event) error {
		if chain.IsChainEvent(evt) {
			_, accepted, reason := chainState.AcceptIfValid(evt)
			if !accepted {
				return fmt.Errorf("%s", reason)
			}
		}
		return originalStoreEvent(ctx, evt)
	}

	// Wrap ReplaceEvent: for chain events, atomically validate and update head
	relay.ReplaceEvent = func(ctx context.Context, evt nostr.Event) error {
		if chain.IsChainEvent(evt) {
			_, accepted, reason := chainState.AcceptIfValid(evt)
			if !accepted {
				return fmt.Errorf("%s", reason)
			}
		}
		return originalReplaceEvent(ctx, evt)
	}

	// Wrap DeleteEvent: for chain events, don't delete from storage, just update state
	relay.DeleteEvent = func(ctx context.Context, id nostr.ID) error {
		var target *nostr.Event
		for evt := range originalQueryStored(ctx, nostr.Filter{IDs: []nostr.ID{id}}) {
			target = &evt
			break
		}
		if target != nil && chain.IsChainEvent(*target) {
			chainID, _ := chain.GetChainID(*target)
			_ = chainState.HandleDeletion(chainID, id.Hex())
			return nil
		}
		return originalDeleteEvent(ctx, id)
	}

	// OnEvent: basic validation for non-chain events
	relay.OnEvent = policies.SeqEvent(
		policies.ValidateKind,
		policies.PreventLargeContent(100000),
	)

	// OnEventSaved: no-op for chain events (head already updated atomically in StoreEvent)
	relay.OnEventSaved = func(ctx context.Context, evt nostr.Event) {
		// Chain events are handled atomically in StoreEvent wrapper
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	srv := &http.Server{Handler: relay}
	go func() {
		_ = srv.Serve(listener)
	}()

	url := fmt.Sprintf("ws://%s", listener.Addr().String())

	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		store.Close()
		_ = os.Remove(tmpFile.Name())
	}

	// Wait for server to be ready
	time.Sleep(100 * time.Millisecond)

	return url, chainState, cleanup
}

func publishEvent(t *testing.T, pool *nostr.Pool, urls []string, evt nostr.Event) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for res := range pool.PublishMany(ctx, urls, evt) {
		if res.Error != nil {
			return fmt.Errorf("[%s] %w", res.RelayURL, res.Error)
		}
	}
	return nil
}

func queryChainEvents(t *testing.T, pool *nostr.Pool, urls []string, pubkey nostr.PubKey, chainName string) []nostr.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := nostr.Filter{
		Authors: []nostr.PubKey{pubkey},
		Tags:    nostr.TagMap{"C": {chainName}},
	}

	var events []nostr.Event
	for ievt := range pool.FetchMany(ctx, urls, filter, nostr.SubscriptionOptions{}) {
		events = append(events, ievt.Event)
	}
	return events
}

func TestGenesisEventSucceeds(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	evt := nostr.Event{
		Kind:      30078,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
		},
		Content: "genesis content",
	}
	evt.PubKey = sk.Public()
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("failed to sign: %v", err)
	}

	if err := publishEvent(t, pool, []string{url}, evt); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	events := queryChainEvents(t, pool, []string{url}, sk.Public(), "test-chain")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "genesis content" {
		t.Errorf("expected content %q, got %q", "genesis content", events[0].Content)
	}
}

func TestAppendWithCorrectPrevSucceeds(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	// Genesis
	genesis := nostr.Event{
		Kind:      30078,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
		},
		Content: "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, genesis); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	// Append
	appendEvt := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", genesis.ID.Hex()},
		},
		Content: "append content",
	}
	appendEvt.PubKey = sk.Public()
	if err := appendEvt.Sign(sk); err != nil {
		t.Fatalf("failed to sign append: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, appendEvt); err != nil {
		t.Fatalf("failed to publish append: %v", err)
	}

	events := queryChainEvents(t, pool, []string{url}, sk.Public(), "test-chain")
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

func TestAppendWithStalePrevIsRejected(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	// Genesis
	genesis := nostr.Event{
		Kind:      30078,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
		},
		Content: "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, genesis); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	// First append succeeds
	append1 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", genesis.ID.Hex()},
		},
		Content: "append1",
	}
	append1.PubKey = sk.Public()
	if err := append1.Sign(sk); err != nil {
		t.Fatalf("failed to sign append1: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, append1); err != nil {
		t.Fatalf("failed to publish append1: %v", err)
	}

	// Second append with stale prev (genesis instead of append1) should fail
	append2 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", genesis.ID.Hex()},
		},
		Content: "append2",
	}
	append2.PubKey = sk.Public()
	if err := append2.Sign(sk); err != nil {
		t.Fatalf("failed to sign append2: %v", err)
	}

	err := publishEvent(t, pool, []string{url}, append2)
	if err == nil {
		t.Fatal("expected stale-prev rejection, got success")
	}
	if !strings.Contains(err.Error(), "stale-prev") {
		t.Fatalf("expected stale-prev error, got: %v", err)
	}
}

func TestDuplicateGenesisIsRejected(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	genesis := nostr.Event{
		Kind:      30078,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
		},
		Content: "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, genesis); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	// Duplicate genesis should fail
	genesis2 := nostr.Event{
		Kind:      30078,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
		},
		Content: "duplicate genesis",
	}
	genesis2.PubKey = sk.Public()
	if err := genesis2.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis2: %v", err)
	}

	err := publishEvent(t, pool, []string{url}, genesis2)
	if err == nil {
		t.Fatal("expected duplicate genesis rejection, got success")
	}
	if !strings.Contains(err.Error(), "stale-prev") {
		t.Fatalf("expected stale-prev error, got: %v", err)
	}
}

func TestDeletionRewindsHead(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	// Genesis
	genesis := nostr.Event{
		Kind:      30078,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
		},
		Content: "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, genesis); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	// Append1
	append1 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", genesis.ID.Hex()},
		},
		Content: "append1",
	}
	append1.PubKey = sk.Public()
	if err := append1.Sign(sk); err != nil {
		t.Fatalf("failed to sign append1: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, append1); err != nil {
		t.Fatalf("failed to publish append1: %v", err)
	}

	// Append2
	append2 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", append1.ID.Hex()},
		},
		Content: "append2",
	}
	append2.PubKey = sk.Public()
	if err := append2.Sign(sk); err != nil {
		t.Fatalf("failed to sign append2: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, append2); err != nil {
		t.Fatalf("failed to publish append2: %v", err)
	}

	// Delete append2
	delEvt := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", append2.ID.Hex()},
			{"k", fmt.Sprintf("%d", append2.Kind)},
		},
		Content: "rollback chain event",
	}
	delEvt.PubKey = sk.Public()
	if err := delEvt.Sign(sk); err != nil {
		t.Fatalf("failed to sign deletion: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, delEvt); err != nil {
		t.Fatalf("failed to publish deletion: %v", err)
	}

	// Query should return only genesis and append1
	events := queryChainEvents(t, pool, []string{url}, sk.Public(), "test-chain")
	if len(events) != 2 {
		t.Fatalf("expected 2 events after deletion, got %d", len(events))
	}
	ids := make(map[string]bool)
	for _, e := range events {
		ids[e.ID.Hex()] = true
	}
	if !ids[genesis.ID.Hex()] {
		t.Error("genesis should still be active")
	}
	if !ids[append1.ID.Hex()] {
		t.Error("append1 should still be active")
	}
	if ids[append2.ID.Hex()] {
		t.Error("append2 should be tombstoned")
	}
}

func TestDeletedEventCannotBeReactivated(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	// Genesis
	genesis := nostr.Event{
		Kind:      30078,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
		},
		Content: "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, genesis); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	// Append1
	append1 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", genesis.ID.Hex()},
		},
		Content: "append1",
	}
	append1.PubKey = sk.Public()
	if err := append1.Sign(sk); err != nil {
		t.Fatalf("failed to sign append1: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, append1); err != nil {
		t.Fatalf("failed to publish append1: %v", err)
	}

	// Delete append1
	delEvt := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", append1.ID.Hex()},
			{"k", fmt.Sprintf("%d", append1.Kind)},
		},
		Content: "rollback chain event",
	}
	delEvt.PubKey = sk.Public()
	if err := delEvt.Sign(sk); err != nil {
		t.Fatalf("failed to sign deletion: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, delEvt); err != nil {
		t.Fatalf("failed to publish deletion: %v", err)
	}

	// Try to append to deleted append1
	append2 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", append1.ID.Hex()},
		},
		Content: "append2",
	}
	append2.PubKey = sk.Public()
	if err := append2.Sign(sk); err != nil {
		t.Fatalf("failed to sign append2: %v", err)
	}

	err := publishEvent(t, pool, []string{url}, append2)
	if err == nil {
		t.Fatal("expected rejection when appending to tombstoned event")
	}
	// After deletion, head rewinds to genesis. Appending to the deleted event
	// may fail with stale-prev (because prev != current head) or missing-prev
	// (because the prev event is tombstoned). Both are valid rejections.
	if !strings.Contains(err.Error(), "stale-prev") && !strings.Contains(err.Error(), "missing-prev") {
		t.Fatalf("expected stale-prev or missing-prev error, got: %v", err)
	}
}

func TestQueryReturnsOnlyActiveEvents(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	// Create 3 events: genesis, append1, append2
	genesis := nostr.Event{
		Kind:      30078,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
		},
		Content: "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, genesis); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	append1 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", genesis.ID.Hex()},
		},
		Content: "append1",
	}
	append1.PubKey = sk.Public()
	if err := append1.Sign(sk); err != nil {
		t.Fatalf("failed to sign append1: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, append1); err != nil {
		t.Fatalf("failed to publish append1: %v", err)
	}

	append2 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"C", "test-chain"},
			{"prev", append1.ID.Hex()},
		},
		Content: "append2",
	}
	append2.PubKey = sk.Public()
	if err := append2.Sign(sk); err != nil {
		t.Fatalf("failed to sign append2: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, append2); err != nil {
		t.Fatalf("failed to publish append2: %v", err)
	}

	// Delete append1 (which should also tombstone append2)
	delEvt := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", append1.ID.Hex()},
			{"k", fmt.Sprintf("%d", append1.Kind)},
		},
		Content: "rollback chain event",
	}
	delEvt.PubKey = sk.Public()
	if err := delEvt.Sign(sk); err != nil {
		t.Fatalf("failed to sign deletion: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, delEvt); err != nil {
		t.Fatalf("failed to publish deletion: %v", err)
	}

	// Query should return only genesis
	events := queryChainEvents(t, pool, []string{url}, sk.Public(), "test-chain")
	if len(events) != 1 {
		t.Fatalf("expected 1 active event, got %d", len(events))
	}
	if events[0].ID != genesis.ID {
		t.Errorf("expected genesis to be the only active event, got %s", events[0].ID.Hex())
	}
}
