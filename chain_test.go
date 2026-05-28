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

	// Wrap StoreEvent: for chain events validate; for non-chain events check opt-out
	relay.StoreEvent = func(ctx context.Context, evt nostr.Event) error {
		if chain.IsChainEvent(evt) {
			return chainState.StoreChainEvent(evt, func(evt nostr.Event) error {
				return originalStoreEvent(ctx, evt)
			})
		}
		_, err := chainState.StoreOptOutEvent(evt, func(evt nostr.Event) error {
			return originalStoreEvent(ctx, evt)
		})
		return err
	}

	// Wrap ReplaceEvent: for chain events validate; for non-chain events check opt-out
	relay.ReplaceEvent = func(ctx context.Context, evt nostr.Event) error {
		if chain.IsChainEvent(evt) {
			return chainState.StoreChainEvent(evt, func(evt nostr.Event) error {
				return originalStoreEvent(ctx, evt)
			})
		}
		_, err := chainState.StoreOptOutEvent(evt, func(evt nostr.Event) error {
			return originalReplaceEvent(ctx, evt)
		})
		return err
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
			if err := chainState.HandleDeletion(chainID, id.Hex()); err != nil {
				return err
			}
			return nil
		}
		return originalDeleteEvent(ctx, id)
	}

	// chainDeletionValidator rejects kind-5 events targeting non-head chain events.
	// This runs in OnEvent before storage, guaranteeing the negative OK is delivered.
	chainDeletionValidator := func(ctx context.Context, evt nostr.Event) (bool, string) {
		if evt.Kind != 5 {
			return false, ""
		}
		for _, tag := range evt.Tags {
			if len(tag) < 2 || tag[0] != "e" {
				continue
			}
			id, err := nostr.IDFromHex(tag[1])
			if err != nil {
				continue
			}
			var target *nostr.Event
			for e := range originalQueryStored(ctx, nostr.Filter{IDs: []nostr.ID{id}}) {
				target = &e
				break
			}
			if target != nil && chain.IsChainEvent(*target) {
				chainID, _ := chain.GetChainID(*target)
				if !chainState.IsHead(chainID, tag[1]) {
					return true, "invalid: chain:not-head"
				}
			}
		}
		return false, ""
	}

	relay.OnEvent = policies.SeqEvent(
		policies.ValidateKind,
		policies.PreventLargeContent(100000),
		chainDeletionValidator,
	)

	relay.OnEventSaved = func(ctx context.Context, evt nostr.Event) {}

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
		Tags:    nostr.TagMap{"d": {chainName}},
	}

	var events []nostr.Event
	for ievt := range pool.FetchMany(ctx, urls, filter, nostr.SubscriptionOptions{}) {
		events = append(events, ievt.Event)
	}
	return events
}

// --- Part 1: Explicit chain tests (non-replaceable kind 9001) ---

func TestGenesisEventSucceeds(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	evt := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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

	appendEvt := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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

	// A second genesis (no prev) should fail because head already exists
	genesis2 := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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

	// Delete append2 (current head)
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

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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

	// Delete append1 (current head)
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

	// Try to append to the deleted event (head has rewound to genesis)
	append2 := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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

	// Delete tip-first: append2 (head), then append1 (new head after first deletion)
	del2 := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", append2.ID.Hex()},
			{"k", fmt.Sprintf("%d", append2.Kind)},
		},
	}
	del2.PubKey = sk.Public()
	if err := del2.Sign(sk); err != nil {
		t.Fatalf("failed to sign del2: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, del2); err != nil {
		t.Fatalf("failed to delete append2: %v", err)
	}

	del1 := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", append1.ID.Hex()},
			{"k", fmt.Sprintf("%d", append1.Kind)},
		},
	}
	del1.PubKey = sk.Public()
	if err := del1.Sign(sk); err != nil {
		t.Fatalf("failed to sign del1: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, del1); err != nil {
		t.Fatalf("failed to delete append1: %v", err)
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

func TestDeleteNonHeadIsRejected(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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

	// Try to delete genesis (not the head — append2 is)
	delGenesis := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", genesis.ID.Hex()},
			{"k", fmt.Sprintf("%d", genesis.Kind)},
		},
	}
	delGenesis.PubKey = sk.Public()
	if err := delGenesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign deletion: %v", err)
	}

	err := publishEvent(t, pool, []string{url}, delGenesis)
	if err == nil {
		t.Fatal("expected rejection when deleting non-head event, got success")
	}
	if !strings.Contains(err.Error(), "not-head") {
		t.Fatalf("expected not-head error, got: %v", err)
	}
}

func TestTombstonedEventCannotBeReactivated(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"chain"},
			{"d", "test-chain"},
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
			{"d", "test-chain"},
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

	// Delete append1 (current head)
	delEvt := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", append1.ID.Hex()},
			{"k", fmt.Sprintf("%d", append1.Kind)},
		},
	}
	delEvt.PubKey = sk.Public()
	if err := delEvt.Sign(sk); err != nil {
		t.Fatalf("failed to sign deletion: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, delEvt); err != nil {
		t.Fatalf("failed to publish deletion: %v", err)
	}

	// Re-publish the exact same append1 event — it must be rejected as tombstoned
	err := publishEvent(t, pool, []string{url}, append1)
	if err == nil {
		t.Fatal("expected tombstoned rejection when re-publishing deleted event, got success")
	}
	if !strings.Contains(err.Error(), "tombstoned") {
		t.Fatalf("expected tombstoned error, got: %v", err)
	}
}

func TestDeleteGenesisAllowsFreshGenesis(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "reset-chain"}},
		Content:   "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, genesis); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	delGenesis := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", genesis.ID.Hex()},
			{"k", fmt.Sprintf("%d", genesis.Kind)},
		},
		Content: "delete genesis",
	}
	delGenesis.PubKey = sk.Public()
	if err := delGenesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign deletion: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, delGenesis); err != nil {
		t.Fatalf("failed to delete genesis: %v", err)
	}

	plain := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"d", "reset-chain"}},
		Content:   "plain after delete",
	}
	plain.PubKey = sk.Public()
	if err := plain.Sign(sk); err != nil {
		t.Fatalf("failed to sign plain event: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, plain); err != nil {
		t.Fatalf("plain event should be accepted after deleting genesis: %v", err)
	}

	freshGenesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 2,
		Tags:      nostr.Tags{{"chain"}, {"d", "reset-chain"}},
		Content:   "fresh genesis",
	}
	freshGenesis.PubKey = sk.Public()
	if err := freshGenesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign fresh genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, freshGenesis); err != nil {
		t.Fatalf("fresh genesis should be accepted after deleting genesis: %v", err)
	}
}

func TestReplaceableChainKeepsHistory(t *testing.T) {
	url, _, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()
	pool := nostr.NewPool()
	defer pool.Close("test done")

	genesis := nostr.Event{
		Kind:      3,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "contacts"}},
		Content:   "contacts-v1",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, genesis); err != nil {
		t.Fatalf("failed to publish genesis: %v", err)
	}

	appendEvt := nostr.Event{
		Kind:      3,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"chain"}, {"d", "contacts"}, {"prev", genesis.ID.Hex()}},
		Content:   "contacts-v2",
	}
	appendEvt.PubKey = sk.Public()
	if err := appendEvt.Sign(sk); err != nil {
		t.Fatalf("failed to sign append: %v", err)
	}
	if err := publishEvent(t, pool, []string{url}, appendEvt); err != nil {
		t.Fatalf("failed to publish append: %v", err)
	}

	events := queryChainEvents(t, pool, []string{url}, sk.Public(), "contacts")
	if len(events) != 2 {
		t.Fatalf("replaceable chain should retain both history events, got %d", len(events))
	}
}

func TestRebuildHonorsOptOutEvents(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "chain-state-*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	sk := nostr.Generate()
	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "opt-chain"}},
		Content:   "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}

	appendEvt := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"chain"}, {"d", "opt-chain"}, {"prev", genesis.ID.Hex()}},
		Content:   "append",
	}
	appendEvt.PubKey = sk.Public()
	if err := appendEvt.Sign(sk); err != nil {
		t.Fatalf("failed to sign append: %v", err)
	}

	optOut := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 2,
		Tags:      nostr.Tags{{"d", "opt-chain"}, {"prev", appendEvt.ID.Hex()}},
		Content:   "opt out",
	}
	optOut.PubKey = sk.Public()
	if err := optOut.Sign(sk); err != nil {
		t.Fatalf("failed to sign opt-out: %v", err)
	}

	chainID, ok := chain.GetChainID(genesis)
	if !ok {
		t.Fatal("expected genesis chain id")
	}
	chainState := chain.NewState(tmpFile.Name())
	chainState.RebuildFromEvents([]nostr.Event{genesis, appendEvt, optOut})

	if chainState.HasChain(chainID) {
		t.Fatal("rebuild should leave chain dissolved after persisted opt-out event")
	}
}

func TestStoreChainEventDoesNotAdvanceHeadOnStoreFailure(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "chain-state-*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	chainState := chain.NewState(tmpFile.Name())
	sk := nostr.Generate()
	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "store-fail"}},
		Content:   "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	chainID, ok := chain.GetChainID(genesis)
	if !ok {
		t.Fatal("expected chain id")
	}

	storeErr := fmt.Errorf("store failed")
	if err := chainState.StoreChainEvent(genesis, func(nostr.Event) error {
		return storeErr
	}); err == nil {
		t.Fatal("expected store error")
	}
	if chainState.HasChain(chainID) {
		t.Fatal("chain state advanced even though storage failed")
	}
}

func TestStoreOptOutDoesNotDissolveOnStoreFailure(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "chain-state-*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	chainState := chain.NewState(tmpFile.Name())
	sk := nostr.Generate()
	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "opt-store-fail"}},
		Content:   "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	chainID, accepted, msg := chainState.AcceptIfValid(genesis)
	if !accepted {
		t.Fatalf("genesis rejected: %s", msg)
	}

	optOut := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"d", "opt-store-fail"}, {"prev", genesis.ID.Hex()}},
		Content:   "opt out",
	}
	optOut.PubKey = sk.Public()
	if err := optOut.Sign(sk); err != nil {
		t.Fatalf("failed to sign opt-out: %v", err)
	}

	if _, err := chainState.StoreOptOutEvent(optOut, func(nostr.Event) error {
		return fmt.Errorf("store failed")
	}); err == nil {
		t.Fatal("expected store error")
	}
	if !chainState.HasChain(chainID) {
		t.Fatal("chain was dissolved even though opt-out storage failed")
	}
	if got := chainState.GetHead(chainID); got != genesis.ID.Hex() {
		t.Fatalf("expected head to remain %s, got %s", genesis.ID.Hex(), got)
	}
}

// --- Opt-out tests ---

// TestOptOut: publish a chain (genesis + append), then publish an opt-out event
// (has prev=head, no ["chain"]), then publish a new event with no prev and no ["chain"]
// — this last event should be accepted (chain dissolved).
func TestOptOut(t *testing.T) {
	_, chainState, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()

	// Genesis
	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "opt-chain"}},
		Content:   "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	chainID, accepted, msg := chainState.AcceptIfValid(genesis)
	if !accepted {
		t.Fatalf("genesis rejected: %s", msg)
	}

	// Append
	appendEvt := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"chain"}, {"d", "opt-chain"}, {"prev", genesis.ID.Hex()}},
		Content:   "append",
	}
	appendEvt.PubKey = sk.Public()
	if err := appendEvt.Sign(sk); err != nil {
		t.Fatalf("failed to sign append: %v", err)
	}
	if _, ok, m := chainState.AcceptIfValid(appendEvt); !ok {
		t.Fatalf("append rejected: %s", m)
	}

	// Opt-out: has prev=head, no ["chain"]
	optOut := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 2,
		Tags:      nostr.Tags{{"d", "opt-chain"}, {"prev", appendEvt.ID.Hex()}},
		Content:   "opt-out",
	}
	optOut.PubKey = sk.Public()
	if err := optOut.Sign(sk); err != nil {
		t.Fatalf("failed to sign opt-out: %v", err)
	}
	dissolved, errMsg := chainState.AcceptOptOut(optOut)
	if errMsg != "" {
		t.Fatalf("opt-out rejected: %s", errMsg)
	}
	if !dissolved {
		t.Fatal("expected chain to be dissolved, got dissolved=false")
	}

	// After dissolve, chain state should be gone
	if chainState.HasChain(chainID) {
		t.Fatal("expected chain to be dissolved (HasChain should return false)")
	}

	// Now a new event with no prev and no ["chain"] should pass AcceptOptOut with no error
	// and dissolved=false (no active chain to dissolve)
	plain := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 3,
		Tags:      nostr.Tags{{"d", "opt-chain"}},
		Content:   "plain after dissolve",
	}
	plain.PubKey = sk.Public()
	if err := plain.Sign(sk); err != nil {
		t.Fatalf("failed to sign plain: %v", err)
	}
	dissolved2, errMsg2 := chainState.AcceptOptOut(plain)
	if errMsg2 != "" {
		t.Fatalf("plain event after dissolve rejected: %s", errMsg2)
	}
	if dissolved2 {
		t.Fatal("expected dissolved=false for plain event with no active chain")
	}
}

// TestOptOutInvalidPrev: active chain exists, publish non-chain event with wrong prev
// → rejected with stale-prev.
func TestOptOutInvalidPrev(t *testing.T) {
	_, chainState, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "opt-chain"}},
		Content:   "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if _, ok, m := chainState.AcceptIfValid(genesis); !ok {
		t.Fatalf("genesis rejected: %s", m)
	}

	// Non-chain event with wrong prev
	wrongPrev := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"d", "opt-chain"}, {"prev", strings.Repeat("b", 64)}},
		Content:   "wrong",
	}
	wrongPrev.PubKey = sk.Public()
	if err := wrongPrev.Sign(sk); err != nil {
		t.Fatalf("failed to sign wrongPrev: %v", err)
	}

	dissolved, errMsg := chainState.AcceptOptOut(wrongPrev)
	if dissolved {
		t.Fatal("expected not dissolved, got dissolved=true")
	}
	if !strings.Contains(errMsg, "stale-prev") {
		t.Errorf("expected stale-prev error, got: %s", errMsg)
	}
}

// TestOptOutMissingPrev: active chain exists, publish non-chain event with no prev
// → rejected with missing-prev.
func TestOptOutMissingPrev(t *testing.T) {
	_, chainState, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()

	genesis := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "opt-chain"}},
		Content:   "genesis",
	}
	genesis.PubKey = sk.Public()
	if err := genesis.Sign(sk); err != nil {
		t.Fatalf("failed to sign genesis: %v", err)
	}
	if _, ok, m := chainState.AcceptIfValid(genesis); !ok {
		t.Fatalf("genesis rejected: %s", m)
	}

	// Non-chain event with no prev
	noPrev := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"d", "opt-chain"}},
		Content:   "no prev",
	}
	noPrev.PubKey = sk.Public()
	if err := noPrev.Sign(sk); err != nil {
		t.Fatalf("failed to sign noPrev: %v", err)
	}

	dissolved, errMsg := chainState.AcceptOptOut(noPrev)
	if dissolved {
		t.Fatal("expected not dissolved, got dissolved=true")
	}
	if !strings.Contains(errMsg, "missing-prev") {
		t.Errorf("expected missing-prev error, got: %s", errMsg)
	}
}

// TestDTagChainName: two chain events with different ["d"] values → accepted as
// separate chains (no interference).
func TestDTagChainName(t *testing.T) {
	_, chainState, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()

	evtA := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "alpha"}},
		Content:   "alpha genesis",
	}
	evtA.PubKey = sk.Public()
	if err := evtA.Sign(sk); err != nil {
		t.Fatalf("failed to sign evtA: %v", err)
	}
	chainIDA, ok, msg := chainState.AcceptIfValid(evtA)
	if !ok {
		t.Fatalf("evtA rejected: %s", msg)
	}
	if chainIDA.ChainName != "alpha" {
		t.Errorf("expected chain name 'alpha', got %q", chainIDA.ChainName)
	}

	evtB := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"chain"}, {"d", "beta"}},
		Content:   "beta genesis",
	}
	evtB.PubKey = sk.Public()
	if err := evtB.Sign(sk); err != nil {
		t.Fatalf("failed to sign evtB: %v", err)
	}
	chainIDB, ok, msg := chainState.AcceptIfValid(evtB)
	if !ok {
		t.Fatalf("evtB rejected: %s", msg)
	}
	if chainIDB.ChainName != "beta" {
		t.Errorf("expected chain name 'beta', got %q", chainIDB.ChainName)
	}

	// Both chains have their own independent heads
	if chainState.GetHead(chainIDA) != evtA.ID.Hex() {
		t.Error("expected evtA to be head of alpha chain")
	}
	if chainState.GetHead(chainIDB) != evtB.ID.Hex() {
		t.Error("expected evtB to be head of beta chain")
	}

	// Appending to alpha with no prev should be rejected (stale-prev)
	evtA2 := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 2,
		Tags:      nostr.Tags{{"chain"}, {"d", "alpha"}},
		Content:   "alpha second no prev",
	}
	evtA2.PubKey = sk.Public()
	if err := evtA2.Sign(sk); err != nil {
		t.Fatalf("failed to sign evtA2: %v", err)
	}
	if _, ok, m := chainState.AcceptIfValid(evtA2); ok {
		t.Fatal("expected rejection (alpha chain has active head), got accept")
	} else if !strings.Contains(m, "stale-prev") {
		t.Errorf("expected stale-prev, got: %s", m)
	}

	// Appending to beta with correct prev should succeed
	evtB2 := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 3,
		Tags:      nostr.Tags{{"chain"}, {"d", "beta"}, {"prev", evtB.ID.Hex()}},
		Content:   "beta append",
	}
	evtB2.PubKey = sk.Public()
	if err := evtB2.Sign(sk); err != nil {
		t.Fatalf("failed to sign evtB2: %v", err)
	}
	if _, ok, m := chainState.AcceptIfValid(evtB2); !ok {
		t.Fatalf("evtB2 rejected: %s", m)
	}
}

// TestImplicitChainNameIsKind: chain event with no ["d"] tag → chain name defaults
// to kind as string.
func TestImplicitChainNameIsKind(t *testing.T) {
	_, chainState, cleanup := setupTestRelay(t)
	defer cleanup()

	sk := nostr.Generate()

	evt := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}},
		Content:   "implicit chain name",
	}
	evt.PubKey = sk.Public()
	if err := evt.Sign(sk); err != nil {
		t.Fatalf("failed to sign: %v", err)
	}

	chainID, accepted, msg := chainState.AcceptIfValid(evt)
	if !accepted {
		t.Fatalf("expected accept, got: %s", msg)
	}
	if chainID.ChainName != "9001" {
		t.Errorf("expected implicit chain name '9001', got %q", chainID.ChainName)
	}
	if chainState.GetHead(chainID) != evt.ID.Hex() {
		t.Errorf("expected head to be event ID")
	}

	// A second event without prev must see the first event as the head (same chain namespace)
	evt2 := nostr.Event{
		Kind:      9001,
		CreatedAt: nostr.Now() + 1,
		Tags:      nostr.Tags{{"chain"}, {"prev", evt.ID.Hex()}},
		Content:   "second",
	}
	evt2.PubKey = sk.Public()
	if err := evt2.Sign(sk); err != nil {
		t.Fatalf("failed to sign evt2: %v", err)
	}
	if _, ok, msg2 := chainState.AcceptIfValid(evt2); !ok {
		t.Fatalf("evt2 rejected (expected shared chain namespace): %s", msg2)
	}
}
