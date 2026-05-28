package relayapp

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

func Main() {
	relay := khatru.NewRelay()

	relay.Info.Name = "serial-killer"
	relay.Info.Description = "A khatru relay with chain-enforced event ordering"
	relay.Info.Software = "https://github.com/fiatjaf/khatru"
	relay.Info.Version = "0.1.0"
	relay.Info.AddSupportedNIP(9)                                     // Deletions
	relay.Info.SupportedNIPs = append(relay.Info.SupportedNIPs, "EC") // Chain enforcement

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

	db := boltdb.BoltBackend{Path: dbPath}
	if err := db.Init(); err != nil {
		fmt.Printf("failed to init boltdb: %v\n", err)
		return
	}
	defer db.Close()

	relay.UseEventstore(&db, 500)

	chainState := chain.NewState(statePath)
	rebuildChainState(relay, chainState)

	originalQueryStored := relay.QueryStored
	originalStoreEvent := relay.StoreEvent
	originalReplaceEvent := relay.ReplaceEvent
	originalDeleteEvent := relay.DeleteEvent

	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) {
			for evt := range originalQueryStored(ctx, filter) {
				if chain.IsChainEvent(evt) {
					chainID, _ := chain.GetChainID(evt)
					if chainState.IsChainEventDeleted(chainID, evt.ID.Hex()) {
						continue
					}
				}
				if !yield(evt) {
					return
				}
			}
		}
	}

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

	fmt.Printf("Chain-enforced relay running on :%s\n", port)
	fmt.Println("Supported NIPs: 1, 9, 11, 42, 70, 86, EC")
	if err := http.ListenAndServe(":"+port, relay); err != nil {
		fmt.Printf("server error: %v\n", err)
	}
}

func rebuildChainState(relay *khatru.Relay, cs *chain.State) {
	if relay.QueryStored == nil {
		return
	}

	var storedEvents []nostr.Event
	for evt := range relay.QueryStored(context.Background(), nostr.Filter{}) {
		storedEvents = append(storedEvents, evt)
	}

	slices.SortFunc(storedEvents, func(a, b nostr.Event) int {
		if a.CreatedAt < b.CreatedAt {
			return -1
		}
		if a.CreatedAt > b.CreatedAt {
			return 1
		}
		return 0
	})

	cs.RebuildFromEvents(storedEvents)
	fmt.Printf("Rebuilt chain state from %d stored events\n", len(storedEvents))
}
