package main

import (
	"context"
	"flag"
	"fmt"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/policies"

	"serial-killer/internal/chain"
)

func main() {
	relay := khatru.NewRelay()

	relay.Info.Name = "serial-killer"
	relay.Info.Description = "A khatru relay with chain-enforced event ordering"
	relay.Info.Software = "https://github.com/fiatjaf/khatru"
	relay.Info.Version = "0.1.0"
	relay.Info.AddSupportedNIP(9) // Deletions
	relay.Info.SupportedNIPs = append(relay.Info.SupportedNIPs, "FF") // Chain enforcement

	defaultPort := os.Getenv("PORT")
	if defaultPort == "" {
		defaultPort = "3334"
	}

	defaultDataPath := os.Getenv("DATA_PATH")
	if defaultDataPath == "" {
		defaultDataPath = "./data"
	}

	var port string
	var dataPath string
	flag.StringVar(&port, "port", defaultPort, "Port to run the relay on")
	flag.StringVar(&dataPath, "data", defaultDataPath, "Data directory for the relay (BoltDB + chain state)")
	flag.Parse()

	if err := os.MkdirAll(dataPath, 0755); err != nil {
		fmt.Printf("failed to create data directory: %v\n", err)
		return
	}

	dbPath := filepath.Join(dataPath, "relay.db")
	statePath := filepath.Join(dataPath, "chain-state.json")

	// Initialize BoltDB backend
	db := boltdb.BoltBackend{Path: dbPath}
	if err := db.Init(); err != nil {
		fmt.Printf("failed to init boltdb: %v\n", err)
		return
	}
	defer db.Close()

	relay.UseEventstore(&db, 500)

	// Chain state manager
	chainState := chain.NewState(statePath)

	// Rebuild chain state from existing events on startup
	rebuildChainState(relay, chainState)

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
			_, accepted, reason := chainState.AcceptIfValid(evt)
			if !accepted {
				return fmt.Errorf("%s", reason)
			}
		} else {
			_, errMsg := chainState.AcceptOptOut(evt)
			if errMsg != "" {
				return fmt.Errorf("%s", errMsg)
			}
		}
		return originalStoreEvent(ctx, evt)
	}

	// Wrap ReplaceEvent: for chain events validate; for non-chain events check opt-out
	relay.ReplaceEvent = func(ctx context.Context, evt nostr.Event) error {
		if chain.IsChainEvent(evt) {
			_, accepted, reason := chainState.AcceptIfValid(evt)
			if !accepted {
				return fmt.Errorf("%s", reason)
			}
		} else {
			_, errMsg := chainState.AcceptOptOut(evt)
			if errMsg != "" {
				return fmt.Errorf("%s", errMsg)
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

	// OnEvent: validate kinds, size, and chain deletion targets
	relay.OnEvent = policies.SeqEvent(
		policies.ValidateKind,
		policies.PreventLargeContent(100000),
		chainDeletionValidator,
	)

	// OnEventSaved: no-op for chain events (head already updated atomically in StoreEvent)
	relay.OnEventSaved = func(ctx context.Context, evt nostr.Event) {
		// Chain events are handled atomically in StoreEvent wrapper
	}

	// Start HTTP server
	fmt.Printf("Chain-enforced relay running on :%s\n", port)
	fmt.Println("Supported NIPs: 1, 9, 11, 42, 70, 86, FF")
	if err := http.ListenAndServe(":"+port, relay); err != nil {
		fmt.Printf("server error: %v\n", err)
	}
}

// rebuildChainState scans all events from the store and rebuilds chain state.
func rebuildChainState(relay *khatru.Relay, cs *chain.State) {
	if relay.QueryStored == nil {
		return
	}

	// Collect all events that have chain tags
	var chainEvents []nostr.Event
	for evt := range relay.QueryStored(context.Background(), nostr.Filter{}) {
		if chain.IsChainEvent(evt) {
			chainEvents = append(chainEvents, evt)
		}
	}

	// Sort by created_at ascending
	slices.SortFunc(chainEvents, func(a, b nostr.Event) int {
		if a.CreatedAt < b.CreatedAt {
			return -1
		}
		if a.CreatedAt > b.CreatedAt {
			return 1
		}
		return 0
	})

	cs.RebuildFromEvents(chainEvents)
	fmt.Printf("Rebuilt chain state from %d chain events\n", len(chainEvents))
}
