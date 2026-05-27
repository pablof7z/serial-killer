package chain

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"fiatjaf.com/nostr"
)

// ChainID identifies a chain by (pubkey, chainName).
type ChainID struct {
	PubKey    string
	ChainName string
}

func (c ChainID) String() string {
	return c.PubKey + ":" + c.ChainName
}

// ChainEvent holds metadata about an event in a chain.
type ChainEvent struct {
	Prev       string `json:"prev"`
	Tombstoned bool   `json:"tombstoned"`
}

// ChainState holds the current head and all events for a chain.
type ChainState struct {
	Head   string                  `json:"head"`
	Events map[string]*ChainEvent `json:"events"`
}

// State manages all chain heads and tombstones.
type State struct {
	mu     sync.RWMutex
	chains map[string]*ChainState
	path   string
}

// NewState creates a new chain state manager.
func NewState(path string) *State {
	s := &State{
		chains: make(map[string]*ChainState),
		path:   path,
	}
	_ = s.load()
	return s
}

// Load state from disk.
func (s *State) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.chains)
}

// Save state to disk.
// Callers must hold s.mu.Lock().
func (s *State) save() error {
	data, err := json.MarshalIndent(s.chains, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// GetChainID extracts the chain identity from an event.
func GetChainID(evt nostr.Event) (ChainID, bool) {
	chainName := ""
	hasChain := false
	for _, tag := range evt.Tags {
		if len(tag) == 1 && tag[0] == "chain" {
			hasChain = true
		} else if len(tag) >= 2 && tag[0] == "C" {
			chainName = tag[1]
		}
	}
	if !hasChain || chainName == "" {
		return ChainID{}, false
	}
	return ChainID{PubKey: evt.PubKey.Hex(), ChainName: chainName}, true
}

// IsChainEvent returns true if the event opts into chain enforcement.
func IsChainEvent(evt nostr.Event) bool {
	_, ok := GetChainID(evt)
	return ok
}

// ValidateChainEvent checks if the event has well-formed chain tags.
func ValidateChainEvent(evt nostr.Event) (chainID ChainID, prev string, reject bool, msg string) {
	chainID, ok := GetChainID(evt)
	if !ok {
		return ChainID{}, "", false, "" // not a chain event
	}

	// Check that it's not kind 5
	if evt.Kind == 5 {
		return ChainID{}, "", true, "invalid: chain: kind 5 cannot be a chain event"
	}

	chainCount := 0
	cCount := 0
	prevCount := 0
	prevVal := ""

	for _, tag := range evt.Tags {
		if len(tag) == 1 && tag[0] == "chain" {
			chainCount++
		} else if len(tag) >= 2 && tag[0] == "C" && tag[1] != "" {
			cCount++
		} else if len(tag) >= 2 && tag[0] == "prev" {
			prevCount++
			prevVal = tag[1]
		}
	}

	if chainCount != 1 {
		return ChainID{}, "", true, "invalid: chain:bad-tags"
	}
	if cCount != 1 {
		return ChainID{}, "", true, "invalid: chain:bad-tags"
	}
	if prevCount > 1 {
		return ChainID{}, "", true, "invalid: chain:bad-tags"
	}

	return chainID, prevVal, false, ""
}

// CanAccept checks if a chain event can be accepted given current state.
func (s *State) CanAccept(evt nostr.Event) (chainID ChainID, accept bool, msg string) {
	chainID, prev, reject, msg := ValidateChainEvent(evt)
	if reject {
		return ChainID{}, false, msg
	}
	if chainID.PubKey == "" {
		return ChainID{}, false, "" // not a chain event
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	cs, exists := s.chains[chainID.String()]
	if !exists {
		// No active head: genesis only
		if prev == "" {
			return chainID, true, ""
		}
		return ChainID{}, false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
	}

	// Head exists
	if prev == "" {
		return ChainID{}, false, fmt.Sprintf("invalid: chain:stale-prev current=%s", cs.Head)
	}
	if prev != cs.Head {
		return ChainID{}, false, fmt.Sprintf("invalid: chain:stale-prev current=%s", cs.Head)
	}

	// Check that the prev event exists and is active in this chain
	if _, ok := cs.Events[prev]; !ok {
		return ChainID{}, false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
	}
	if cs.Events[prev].Tombstoned {
		return ChainID{}, false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
	}

	return chainID, true, ""
}

// AcceptIfValid atomically validates and accepts a chain event, eliminating the
// race window between CanAccept and AcceptEvent.
func (s *State) AcceptIfValid(evt nostr.Event) (chainID ChainID, accepted bool, msg string) {
	chainID, prev, reject, msg := ValidateChainEvent(evt)
	if reject {
		return ChainID{}, false, msg
	}
	if chainID.PubKey == "" {
		return ChainID{}, false, "" // not a chain event
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cs, exists := s.chains[chainID.String()]
	if !exists {
		if prev == "" {
			cs = &ChainState{Events: make(map[string]*ChainEvent)}
			s.chains[chainID.String()] = cs
		} else {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
		}
	}

	// Duplicate check: if we already have this exact event and it's active, accept without changing head
	if existing, ok := cs.Events[evt.ID.Hex()]; ok && !existing.Tombstoned {
		return chainID, true, ""
	}

	if exists {
		if prev == "" {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:stale-prev current=%s", cs.Head)
		}
		if prev != cs.Head {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:stale-prev current=%s", cs.Head)
		}
		if _, ok := cs.Events[prev]; !ok {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
		}
		if cs.Events[prev].Tombstoned {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
		}
	}

	cs.Events[evt.ID.Hex()] = &ChainEvent{Prev: prev, Tombstoned: false}
	cs.Head = evt.ID.Hex()
	_ = s.save()

	return chainID, true, ""
}

// AcceptEvent updates the chain state for an accepted chain event.
func (s *State) AcceptEvent(evt nostr.Event, chainID ChainID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cs, exists := s.chains[chainID.String()]
	if !exists {
		cs = &ChainState{Events: make(map[string]*ChainEvent)}
		s.chains[chainID.String()] = cs
	}

	prev := ""
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && tag[0] == "prev" {
			prev = tag[1]
			break
		}
	}

	cs.Events[evt.ID.Hex()] = &ChainEvent{Prev: prev, Tombstoned: false}
	cs.Head = evt.ID.Hex()

	return s.save()
}

// IsTombstoned checks if an event is marked as deleted.
func (s *State) IsTombstoned(chainID ChainID, eventID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cs, exists := s.chains[chainID.String()]
	if !exists {
		return false
	}
	ev, ok := cs.Events[eventID]
	if !ok {
		return false
	}
	return ev.Tombstoned
}

// HandleDeletion processes a NIP-09 deletion for a chain event.
// It marks the event and its descendants as tombstoned, and rewinds the head.
func (s *State) HandleDeletion(chainID ChainID, eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cs, exists := s.chains[chainID.String()]
	if !exists {
		return nil
	}

	// Mark the target and all descendants as tombstoned
	s.tombstoneDescendants(cs, eventID)

	// Rewind head to nearest active ancestor
	newHead := s.findNearestActiveAncestor(cs, eventID)
	cs.Head = newHead

	return s.save()
}

// tombstoneDescendants recursively marks an event and all its descendants as tombstoned.
func (s *State) tombstoneDescendants(cs *ChainState, eventID string) {
	if ev, ok := cs.Events[eventID]; ok && !ev.Tombstoned {
		ev.Tombstoned = true
		// Find all descendants and tombstone them too
		for id, e := range cs.Events {
			if e.Prev == eventID && !e.Tombstoned {
				s.tombstoneDescendants(cs, id)
			}
		}
	}
}

// findNearestActiveAncestor walks back from an event to find the nearest active ancestor.
func (s *State) findNearestActiveAncestor(cs *ChainState, eventID string) string {
	ev, ok := cs.Events[eventID]
	if !ok || ev.Prev == "" {
		return "" // genesis or unknown
	}
	parent, ok := cs.Events[ev.Prev]
	if !ok {
		return "" // parent unknown
	}
	if !parent.Tombstoned {
		return ev.Prev // parent is active
	}
	return s.findNearestActiveAncestor(cs, ev.Prev) // keep walking back
}

// IsChainEventDeleted checks if a specific event is deleted.
func (s *State) IsChainEventDeleted(chainID ChainID, eventID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cs, exists := s.chains[chainID.String()]
	if !exists {
		return false
	}
	ev, ok := cs.Events[eventID]
	if !ok {
		return false
	}
	return ev.Tombstoned
}

// GetChainState returns the current state for a chain (for queries).
func (s *State) GetChainState(chainID ChainID) (*ChainState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cs, exists := s.chains[chainID.String()]
	if !exists {
		return nil, false
	}
	// Return a copy
	copyCS := &ChainState{
		Head:   cs.Head,
		Events: make(map[string]*ChainEvent, len(cs.Events)),
	}
	for k, v := range cs.Events {
		copyCS.Events[k] = &ChainEvent{Prev: v.Prev, Tombstoned: v.Tombstoned}
	}
	return copyCS, true
}

// GetHead returns the current active head for a chain.
func (s *State) GetHead(chainID ChainID) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cs, exists := s.chains[chainID.String()]
	if !exists {
		return ""
	}
	return cs.Head
}

// RebuildFromEvents rebuilds chain state from a slice of events.
// Useful for syncing on startup.
func (s *State) RebuildFromEvents(events []nostr.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Process events in order of created_at
	for _, evt := range events {
		chainID, prev, reject, _ := ValidateChainEvent(evt)
		if reject {
			continue
		}
		if chainID.PubKey == "" {
			continue
		}

		cs, exists := s.chains[chainID.String()]
		if !exists {
			cs = &ChainState{Events: make(map[string]*ChainEvent)}
			s.chains[chainID.String()] = cs
		}

		// Don't reactivate tombstoned events on rebuild
		if existing, ok := cs.Events[evt.ID.Hex()]; ok && existing.Tombstoned {
			continue
		}

		cs.Events[evt.ID.Hex()] = &ChainEvent{Prev: prev, Tombstoned: false}
		cs.Head = evt.ID.Hex()
	}

	_ = s.save()
}

// ListChains returns all known chain IDs.
func (s *State) ListChains() []ChainID {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var ids []ChainID
	for k := range s.chains {
		parts := strings.SplitN(k, ":", 2)
		if len(parts) == 2 {
			ids = append(ids, ChainID{PubKey: parts[0], ChainName: parts[1]})
		}
	}
	return ids
}
