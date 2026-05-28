package chain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
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

	if ok, msg := s.acceptChainIfValid(chainID, prev, evt.ID.Hex()); !ok {
		return ChainID{}, false, msg
	}
	if err := s.recordChainAccepted(chainID, prev, evt.ID.Hex()); err != nil {
		return ChainID{}, false, err.Error()
	}
	return chainID, true, ""
}

// StoreChainEvent atomically validates, stores, and records a chain event.
func (s *State) StoreChainEvent(evt nostr.Event, store func(nostr.Event) error) error {
	chainID, prev, reject, msg := ValidateChainEvent(evt)
	if reject {
		return errors.New(msg)
	}
	if chainID.PubKey == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if ok, msg := s.acceptChainIfValid(chainID, prev, evt.ID.Hex()); !ok {
		return errors.New(msg)
	}
	if store != nil {
		if err := store(evt); err != nil {
			return err
		}
	}
	return s.recordChainAccepted(chainID, prev, evt.ID.Hex())
}

// acceptChainIfValid checks the chain acceptance rule:
// the incoming prev must match the current active head.
// Caller must hold s.mu.Lock().
func (s *State) acceptChainIfValid(chainID ChainID, prev, evtID string) (bool, string) {
	cs := s.chains[chainID.String()]

	if cs != nil {
		if existing, ok := cs.Events[evtID]; ok {
			if existing.Tombstoned {
				return false, fmt.Sprintf("invalid: chain:tombstoned %s", evtID)
			}
			return true, ""
		}
	}

	if cs == nil || cs.Head == "" {
		if prev != "" {
			return false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
		}
		return true, ""
	}

	if prev == "" {
		return false, fmt.Sprintf("invalid: chain:stale-prev current=%s", cs.Head)
	}
	if prev != cs.Head {
		return false, fmt.Sprintf("invalid: chain:stale-prev current=%s", cs.Head)
	}
	if h, ok := cs.Events[prev]; !ok || h.Tombstoned {
		return false, fmt.Sprintf("invalid: chain:missing-prev %s", prev)
	}

	return true, ""
}

// recordChainAccepted records an already accepted chain event.
// Caller must hold s.mu.Lock().
func (s *State) recordChainAccepted(chainID ChainID, prev, evtID string) error {
	cs := s.chains[chainID.String()]
	if cs == nil {
		cs = &ChainState{Events: make(map[string]*ChainEvent)}
		s.chains[chainID.String()] = cs
	}

	cs.Events[evtID] = &ChainEvent{Prev: prev}
	cs.Head = evtID
	return s.save()
}

// AcceptOptOut handles opt-out events: events that have prev but no ["chain"].
// It derives the chain key from (pubkey, kind, d-tag-value-or-kind-as-string).
//
//   - If no active chain exists for the key: returns dissolved=false, err="" (nothing to do)
//   - If active chain and prev matches head: dissolves chain, returns dissolved=true, err=""
//   - If active chain and prev doesn't match: returns dissolved=false, err="invalid: chain:stale-prev current=<head>"
//   - If active chain and no prev: returns dissolved=false, err="invalid: chain:missing-prev <head>"
func (s *State) AcceptOptOut(evt nostr.Event) (dissolved bool, err string) {
	dissolved, storeErr := s.StoreOptOutEvent(evt, nil)
	if storeErr != nil {
		return false, storeErr.Error()
	}
	return dissolved, ""
}

// StoreOptOutEvent atomically validates, stores, and applies a non-chain event
// that may dissolve an active chain.
func (s *State) StoreOptOutEvent(evt nostr.Event, store func(nostr.Event) error) (dissolved bool, err error) {
	chainID, ok := getChainIDForOptOut(evt)
	if !ok {
		// Has ["chain"] tag — not an opt-out event
		if store != nil {
			return false, store(evt)
		}
		return false, nil
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
	if cs == nil || cs.Head == "" {
		// No active chain for this key — nothing to do
		if store != nil {
			return false, store(evt)
		}
		return false, nil
	}

	head := cs.Head
	if prevVal == "" {
		return false, fmt.Errorf("invalid: chain:missing-prev %s", head)
	}
	if prevVal != head {
		return false, fmt.Errorf("invalid: chain:stale-prev current=%s", head)
	}
	if store != nil {
		if err := store(evt); err != nil {
			return false, err
		}
	}

	// prev matches head — dissolve the chain
	delete(s.chains, chainID.String())
	return true, s.save()
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

	if err := s.handleDeletionLocked(chainID, eventID); err != nil {
		return err
	}
	return s.save()
}

// Caller must hold s.mu.Lock().
func (s *State) handleDeletionLocked(chainID ChainID, eventID string) error {
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

	return nil
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

// RebuildFromEvents rebuilds chain state from a slice of events.
func (s *State) RebuildFromEvents(events []nostr.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.chains = make(map[string]*ChainState)

	slices.SortFunc(events, func(a, b nostr.Event) int {
		if a.CreatedAt < b.CreatedAt {
			return -1
		}
		if a.CreatedAt > b.CreatedAt {
			return 1
		}
		return strings.Compare(a.ID.Hex(), b.ID.Hex())
	})

	eventsByID := make(map[string]nostr.Event, len(events))
	for _, evt := range events {
		eventsByID[evt.ID.Hex()] = evt
	}

	for _, evt := range events {
		if evt.Kind == 5 {
			s.rebuildDeletion(evt, eventsByID)
			continue
		}

		chainID, prev, reject, _ := ValidateChainEvent(evt)
		if reject {
			continue
		}
		if chainID.PubKey == "" {
			s.rebuildOptOut(evt)
			continue
		}
		if ok, _ := s.acceptChainIfValid(chainID, prev, evt.ID.Hex()); !ok {
			continue
		}
		_ = s.recordChainAcceptedNoSave(chainID, prev, evt.ID.Hex())
	}

	_ = s.save()
}

func (s *State) rebuildOptOut(evt nostr.Event) {
	chainID, ok := getChainIDForOptOut(evt)
	if !ok {
		return
	}
	prevVal := ""
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && tag[0] == "prev" {
			prevVal = tag[1]
			break
		}
	}
	cs := s.chains[chainID.String()]
	if cs == nil || prevVal == "" || prevVal != cs.Head {
		return
	}
	delete(s.chains, chainID.String())
}

func (s *State) rebuildDeletion(evt nostr.Event, eventsByID map[string]nostr.Event) {
	for _, tag := range evt.Tags {
		if len(tag) < 2 || tag[0] != "e" {
			continue
		}
		target, ok := eventsByID[tag[1]]
		if !ok || target.PubKey != evt.PubKey || !IsChainEvent(target) {
			continue
		}
		chainID, _ := GetChainID(target)
		_ = s.handleDeletionLocked(chainID, tag[1])
	}
}

// Caller must hold s.mu.Lock().
func (s *State) recordChainAcceptedNoSave(chainID ChainID, prev, evtID string) error {
	cs := s.chains[chainID.String()]
	if cs == nil {
		cs = &ChainState{Events: make(map[string]*ChainEvent)}
		s.chains[chainID.String()] = cs
	}
	cs.Events[evtID] = &ChainEvent{Prev: prev}
	cs.Head = evtID
	return nil
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
