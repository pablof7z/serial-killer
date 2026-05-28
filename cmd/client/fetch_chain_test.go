package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
)

// setupSimpleRelay spins up an in-memory relay with no chain enforcement.
func setupSimpleRelay(t *testing.T) (url string, cleanup func()) {
	t.Helper()

	store := &slicestore.SliceStore{}
	if err := store.Init(); err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	relay := khatru.NewRelay()
	relay.UseEventstore(store, 500)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	srv := &http.Server{Handler: relay}
	go func() { _ = srv.Serve(listener) }()

	time.Sleep(50 * time.Millisecond)

	url = fmt.Sprintf("ws://%s", listener.Addr().String())
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		store.Close()
	}
	return url, cleanup
}

func newTestClient(t *testing.T, relayURL string) (*ClientState, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cs := &ClientState{
		secretKey: nostr.Generate(),
		pool:      nostr.NewPool(),
		ctx:       ctx,
		cancel:    cancel,
		relays:    map[string]bool{relayURL: true},
	}
	if _, err := cs.pool.EnsureRelay(relayURL); err != nil {
		cancel()
		t.Fatalf("failed to connect pool to relay: %v", err)
	}
	cleanup := func() {
		cancel()
		cs.pool.Close("test done")
	}
	return cs, cleanup
}

func publishDirect(t *testing.T, cs *ClientState, relayURL string, evt nostr.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(cs.ctx, 5*time.Second)
	defer cancel()
	for res := range cs.pool.PublishMany(ctx, []string{relayURL}, evt) {
		if res.Error != nil {
			t.Fatalf("failed to publish event: %v", res.Error)
		}
	}
}

// TestFetchChainNamedDTag verifies that fetchChain returns events identified by
// an explicit ["d", name] tag — the path broken by the ContainsAny bug.
func TestFetchChainNamedDTag(t *testing.T) {
	url, cleanup := setupSimpleRelay(t)
	defer cleanup()
	cs, csCleanup := newTestClient(t, url)
	defer csCleanup()

	evt := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "nip60"}},
		Content:   "state1",
	}
	evt.PubKey = cs.secretKey.Public()
	if err := evt.Sign(cs.secretKey); err != nil {
		t.Fatalf("sign: %v", err)
	}
	publishDirect(t, cs, url, evt)

	got := cs.fetchChain([]string{url}, "nip60", 7375)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].ID != evt.ID {
		t.Fatalf("wrong event returned")
	}
}

// TestFetchChainImplicitByKind verifies that fetchChain finds events with no d
// tag (implicit chain identified by kind number).
func TestFetchChainImplicitByKind(t *testing.T) {
	url, cleanup := setupSimpleRelay(t)
	defer cleanup()
	cs, csCleanup := newTestClient(t, url)
	defer csCleanup()

	evt := nostr.Event{
		Kind:      10000,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}},
		Content:   "genesis",
	}
	evt.PubKey = cs.secretKey.Public()
	if err := evt.Sign(cs.secretKey); err != nil {
		t.Fatalf("sign: %v", err)
	}
	publishDirect(t, cs, url, evt)

	got := cs.fetchChain([]string{url}, "", 10000)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].ID != evt.ID {
		t.Fatalf("wrong event returned")
	}
}

// TestFetchChainSkipsNonChainEvents verifies that regular events (no ["chain"]
// tag) are not returned even when they match the kind/d filter.
func TestFetchChainSkipsNonChainEvents(t *testing.T) {
	url, cleanup := setupSimpleRelay(t)
	defer cleanup()
	cs, csCleanup := newTestClient(t, url)
	defer csCleanup()

	// non-chain event: has d tag but no ["chain"] tag
	nonChain := nostr.Event{
		Kind:      1,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"d", "mychain"}},
		Content:   "not a chain event",
	}
	nonChain.PubKey = cs.secretKey.Public()
	if err := nonChain.Sign(cs.secretKey); err != nil {
		t.Fatalf("sign: %v", err)
	}
	publishDirect(t, cs, url, nonChain)

	got := cs.fetchChain([]string{url}, "mychain", 1)
	if len(got) != 0 {
		t.Fatalf("expected 0 events, got %d", len(got))
	}
}

// TestFindHeadNamedChain verifies that findHead returns the correct head ID
// for a named chain — the exact flow that was broken in the user's session.
func TestFindHeadNamedChain(t *testing.T) {
	url, cleanup := setupSimpleRelay(t)
	defer cleanup()
	cs, csCleanup := newTestClient(t, url)
	defer csCleanup()

	genesis := nostr.Event{
		Kind:      7375,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"chain"}, {"d", "nip60"}},
		Content:   "state1",
	}
	genesis.PubKey = cs.secretKey.Public()
	if err := genesis.Sign(cs.secretKey); err != nil {
		t.Fatalf("sign: %v", err)
	}
	publishDirect(t, cs, url, genesis)

	head := cs.findHead("nip60", 7375)
	if head == "" {
		t.Fatal("findHead returned empty — append would fail")
	}
	if head != genesis.ID.Hex() {
		t.Fatalf("expected head %s, got %s", genesis.ID.Hex(), head)
	}
}

func TestHandleDeleteInvalidEventIDDoesNotPanic(t *testing.T) {
	url, cleanup := setupSimpleRelay(t)
	defer cleanup()
	cs, csCleanup := newTestClient(t, url)
	defer csCleanup()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handleDelete should not panic on invalid event id: %v", r)
		}
	}()

	cs.handleDelete([]string{url, "test-chain", "not-a-hex-id"})
}
