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
	Locked     bool   `json:"locked"`
}

// ChainState holds the current head and all events for a chain.
type ChainState struct {
	Head   string                 `json:"head"`
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

// Callers must hold s.mu.Lock().
func (s *State) save() error {
	data, err := json.MarshalIndent(s.chains, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// GetChainID extracts the chain identity from an event.
// The chain name is the value of the ["d"] tag, or the kind number as a string if absent.
// Returns ok=true only if the event has a ["chain"] tag.
func GetChainID(evt nostr.Event) (ChainID, bool) {
	hasChain := false
	chainName := ""
	for _, tag := range evt.Tags {
		if len(tag) == 1 && tag[0] == "chain" {
			hasChain = true
		} else if len(tag) >= 2 && tag[0] == "d" && tag[1] != "" {
			chainName = tag[1]
		}
	}
	if !hasChain {
		return ChainID{}, false
	}
	if chainName == "" {
		chainName = fmt.Sprintf("%d", evt.Kind)
	}
	return ChainID{PubKey: evt.PubKey.Hex(), ChainName: chainName}, true
}

// getChainIDForOptOut derives the chain key from an event that may not have ["chain"].
// This is used for opt-out events: same key derivation as chain events but without
// requiring ["chain"]. Returns ok=false only if the event has ["chain"] (use GetChainID instead).
func getChainIDForOptOut(evt nostr.Event) (ChainID, bool) {
	for _, tag := range evt.Tags {
		if len(tag) == 1 && tag[0] == "chain" {
			// This is a chain event, not an opt-out
			return ChainID{}, false
		}
	}
	chainName := ""
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && tag[0] == "d" && tag[1] != "" {
			chainName = tag[1]
			break
		}
	}
	if chainName == "" {
		chainName = fmt.Sprintf("%d", evt.Kind)
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
	chainCount := 0
	dCount := 0
	prevCount := 0
	prevVal := ""
	hasChainTag := false

	for _, tag := range evt.Tags {
		if len(tag) == 1 && tag[0] == "chain" {
			hasChainTag = true
			chainCount++
		} else if len(tag) >= 2 && tag[0] == "d" && tag[1] != "" {
			dCount++
		} else if len(tag) >= 2 && tag[0] == "prev" {
			prevCount++
			prevVal = tag[1]
		}
	}

	if !hasChainTag {
		return ChainID{}, "", false, ""
	}

	if evt.Kind == 5 {
		return ChainID{}, "", true, "invalid: chain: kind 5 cannot be a chain event"
	}

	if chainCount != 1 || prevCount > 1 || dCount > 1 {
		return ChainID{}, "", true, "invalid: chain:bad-tags"
	}

	chainID, ok := GetChainID(evt)
	if !ok {
		return ChainID{}, "", true, "invalid: chain:bad-tags"
	}

	return chainID, prevVal, false, ""
}

// AcceptIfValid atomically validates and accepts a chain event.
// Every chain event uses the same unified acceptance path.
func (s *State) AcceptIfValid(evt nostr.Event) (chainID ChainID, accepted bool, msg string) {
	chainID, prev, reject, msg := ValidateChainEvent(evt)
	if reject {
		return ChainID{}, false, msg
	}
	if chainID.PubKey == "" {
		return ChainID{}, false, ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.acceptChainIfValid(chainID, prev, evt.ID.Hex())
}

// acceptChainIfValid implements the chain acceptance rule:
// the incoming prev must match the current active head.
// Caller must hold s.mu.Lock().
func (s *State) acceptChainIfValid(chainID ChainID, prev, evtID string) (ChainID, bool, string) {
	cs := s.chains[chainID.String()]

	if cs != nil {
		if existing, ok := cs.Events[evtID]; ok {
			if existing.Tombstoned {
				return ChainID{}, false, fmt.Sprintf("invalid: chain:tombstoned %s", evtID)
			}
			return chainID, true, ""
		}
	}

	if cs == nil {
		if prev != "" {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
		}
		cs = &ChainState{Events: make(map[string]*ChainEvent)}
		s.chains[chainID.String()] = cs
	} else {
		if prev == "" {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:stale-prev current=%s", cs.Head)
		}
		if prev != cs.Head {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:stale-prev current=%s", cs.Head)
		}
		if h, ok := cs.Events[prev]; !ok || h.Tombstoned {
			return ChainID{}, false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
		}
	}

	cs.Events[evtID] = &ChainEvent{Prev: prev}
	cs.Head = evtID
	_ = s.save()
	return chainID, true, ""
}

// AcceptOptOut handles opt-out events: events that have prev but no ["chain"].
// It derives the chain key from (pubkey, kind, d-tag-value-or-kind-as-string).
//
//   - If no active chain exists for the key: returns dissolved=false, err="" (nothing to do)
//   - If active chain and prev matches head: dissolves chain, returns dissolved=true, err=""
//   - If active chain and prev doesn't match: returns dissolved=false, err="invalid: chain:stale-prev current=<head>"
//   - If active chain and no prev: returns dissolved=false, err="invalid: chain:missing-prev <head>"
func (s *State) AcceptOptOut(evt nostr.Event) (dissolved bool, err string) {
	chainID, ok := getChainIDForOptOut(evt)
	if !ok {
		// Has ["chain"] tag — not an opt-out event
		return false, ""
	}

	// Extract prev tag
	prevVal := ""
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && tag[0] == "prev" {
			prevVal = tag[1]
			break
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cs := s.chains[chainID.String()]
	if cs == nil {
		// No active chain for this key — nothing to do
		return false, ""
	}

	head := cs.Head
	if prevVal == "" {
		return false, fmt.Sprintf("invalid: chain:missing-prev %s", head)
	}
	if prevVal != head {
		return false, fmt.Sprintf("invalid: chain:stale-prev current=%s", head)
	}

	// prev matches head — dissolve the chain
	delete(s.chains, chainID.String())
	_ = s.save()
	return true, ""
}

// IsHead returns true if eventID is the current active head of the chain.
func (s *State) IsHead(chainID ChainID, eventID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, exists := s.chains[chainID.String()]
	return exists && cs.Head == eventID
}

// HandleDeletion processes a NIP-09 deletion for a chain event.
// Only the current head may be deleted; the head rewinds to the deleted event's prev.
func (s *State) HandleDeletion(chainID ChainID, eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cs, exists := s.chains[chainID.String()]
	if !exists || cs.Head != eventID {
		return fmt.Errorf("invalid: chain:not-head")
	}

	ev, ok := cs.Events[eventID]
	if !ok {
		return fmt.Errorf("invalid: chain:not-head")
	}

	ev.Tombstoned = true
	cs.Head = ev.Prev

	return s.save()
}

// IsChainEventDeleted checks if a specific event is tombstoned.
func (s *State) IsChainEventDeleted(chainID ChainID, eventID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, exists := s.chains[chainID.String()]
	if !exists {
		return false
	}
	ev, ok := cs.Events[eventID]
	return ok && ev.Tombstoned
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

// GetChainState returns a copy of the current state for a chain.
func (s *State) GetChainState(chainID ChainID) (*ChainState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, exists := s.chains[chainID.String()]
	if !exists {
		return nil, false
	}
	copyCS := &ChainState{
		Head:   cs.Head,
		Events: make(map[string]*ChainEvent, len(cs.Events)),
	}
	for k, v := range cs.Events {
		copyCS.Events[k] = &ChainEvent{Prev: v.Prev, Tombstoned: v.Tombstoned}
	}
	return copyCS, true
}

// RebuildFromEvents rebuilds chain state from a slice of events (sorted by created_at asc).
func (s *State) RebuildFromEvents(events []nostr.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, evt := range events {
		chainID, prev, reject, _ := ValidateChainEvent(evt)
		if reject || chainID.PubKey == "" {
			continue
		}

		cs, exists := s.chains[chainID.String()]
		if !exists {
			cs = &ChainState{Events: make(map[string]*ChainEvent)}
			s.chains[chainID.String()] = cs
		}

		if existing, ok := cs.Events[evt.ID.Hex()]; ok && existing.Tombstoned {
			continue
		}

		cs.Events[evt.ID.Hex()] = &ChainEvent{Prev: prev}
		cs.Head = evt.ID.Hex()
	}

	_ = s.save()
}

// DeleteChain removes all chain state for the given chain, lifting protection.
// After this call the slot is treated as unprotected — a new genesis is accepted.
func (s *State) DeleteChain(chainID ChainID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.chains, chainID.String())
	_ = s.save()
}

// HasChain reports whether the given chain has any stored state.
func (s *State) HasChain(chainID ChainID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.chains[chainID.String()]
	return ok
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
