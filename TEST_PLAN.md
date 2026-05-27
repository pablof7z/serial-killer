# Test Plan for serial-killer

## Overview

This document covers test scenarios for the Khatru relay with chain-enforced event ordering (`cmd/relay`) and the REPL client (`cmd/client`). Tests are organized into unit tests (`internal/chain`), relay integration tests, client+relay end-to-end tests, edge cases, and stress tests.

---

## 1. Unit Tests: `internal/chain/state.go`

These tests exercise chain logic in isolation without a running relay.

### 1.1 Chain Event Validation

| ID | Test | Steps | Expected Result |
|---|---|---|---|
| CHAIN-001 | Non-chain event | Call `ValidateChainEvent` on event with no `chain` or `C` tags | Returns `(ChainID{}, "", false, "")` (not a chain event) |
| CHAIN-002 | Valid genesis event | Event with `["chain"]`, `["C", "test"]`, kind 30078, no `prev` | Returns chainID, `prev=""`, `reject=false` |
| CHAIN-003 | Valid append event | Event with `["chain"]`, `["C", "test"]`, `["prev", "<id>"]`, kind 7375 | Returns chainID, `prev="<id>"`, `reject=false` |
| CHAIN-004 | Missing `chain` tag | Event with only `["C", "test"]` | `reject=true`, msg contains `bad-tags` |
| CHAIN-005 | Missing `C` tag | Event with only `["chain"]` | `reject=true`, msg contains `bad-tags` |
| CHAIN-006 | Multiple `C` tags | Event with two `["C", "..."]` tags | `reject=true`, msg contains `bad-tags` |
| CHAIN-007 | Multiple `prev` tags | Event with two `["prev", "..."]` tags | `reject=true`, msg contains `bad-tags` |
| CHAIN-008 | Kind 5 as chain event | Deletion request with `chain` and `C` tags | `reject=true`, msg contains `kind 5 cannot be a chain event` |

### 1.2 Acceptance Logic (`CanAccept`)

| ID | Test | Preconditions | Steps | Expected Result |
|---|---|---|---|---|
| CHAIN-009 | Genesis on empty chain | Fresh `State`, no entries | Call `CanAccept(genesisEvent)` | `accept=true`, returns correct `chainID` |
| CHAIN-010 | Duplicate genesis | Chain exists with head set | Call `CanAccept(secondGenesis)` | `accept=false`, msg contains `stale-prev` |
| CHAIN-011 | Append with correct prev | Chain exists, head = `idA` | Call `CanAccept(appendEvent(prev=idA))` | `accept=true` |
| CHAIN-012 | Append with stale prev | Chain exists, head = `idB` | Call `CanAccept(appendEvent(prev=idA))` where `idA != idB` | `accept=false`, msg contains `stale-prev` |
| CHAIN-013 | Append with missing prev | Empty chain | Call `CanAccept(appendEvent(prev=idX))` | `accept=false`, msg contains `missing-prev` |
| CHAIN-014 | Append to tombstoned prev | Chain where `idA` is tombstoned, head rewound to `idB` | Call `CanAccept(appendEvent(prev=idA))` | `accept=false` (missing-prev) |
| CHAIN-015 | Append prev unknown to state | Chain exists, head = `idA`, but event references `idZ` not in `cs.Events` | Call `CanAccept(appendEvent(prev=idZ))` | `accept=false`, msg contains `missing-prev` |

### 1.3 State Mutation (`AcceptEvent`)

| ID | Test | Steps | Expected Result |
|---|---|---|---|
| CHAIN-016 | Accept genesis | `AcceptEvent(genesis, chainID)` | Chain created, `Head = event.ID`, event in `Events` map |
| CHAIN-017 | Accept append | `AcceptEvent(append, chainID)` on existing chain | `Head` updated to new event ID, prev recorded |

### 1.4 Deletion and Rollback (`HandleDeletion`)

| ID | Test | Preconditions | Steps | Expected Result |
|---|---|---|---|
| CHAIN-018 | Delete single event | Chain: `G -> A` (head=A) | `HandleDeletion(chainID, A)` | A tombstoned, head rewound to `G` |
| CHAIN-019 | Delete middle event | Chain: `G -> A -> B -> C` (head=C) | `HandleDeletion(chainID, B)` | B, C tombstoned; head rewound to `A` |
| CHAIN-020 | Delete genesis | Chain: `G -> A` (head=A) | `HandleDeletion(chainID, G)` | G, A tombstoned; head = `""` |
| CHAIN-021 | Delete non-existent | Chain exists | `HandleDeletion(chainID, "fake")` | No-op, no error |
| CHAIN-022 | Delete on unknown chain | Fresh state | `HandleDeletion(unknownChainID, "x")` | No-op, no error |
| CHAIN-023 | Tombstone prevents re-append | Chain: `G -> A`, A tombstoned, head=G | `CanAccept(append(prev=A))` | `accept=false` (missing-prev) |

### 1.5 State Rebuild (`RebuildFromEvents`)

| ID | Test | Steps | Expected Result |
|---|---|---|---|
| CHAIN-024 | Rebuild single chain | Provide `[genesis, append1, append2]` sorted by `created_at` | Head = `append2`, all events in `Events` |
| CHAIN-025 | Rebuild multiple chains | Provide events for `chainA` and `chainB` | Both chains independently correct |
| CHAIN-026 | Rebuild unsorted events | Provide `[append2, genesis, append1]` | After sorting internally, head = `append2` |
| CHAIN-027 | Rebuild with divergent branches | Provide two active branches: `G->A` and `G->B` where `B.CreatedAt > A.CreatedAt` | Head = `B` (last wins). **Note:** this documents current behavior; only one head is tracked. |

---

## 2. Integration Tests: Relay (`cmd/relay`)

These tests require a running relay instance (or use `httptest` with a WebSocket connection).

### 2.1 Basic Chain Operations

| ID | Test | Steps | Expected Result |
|---|---|---|---|
| RELAY-001 | Publish non-chain event | Send kind 1 text note with no `chain`/`C` tags | Accepted, stored, queryable normally |
| RELAY-002 | Publish genesis | Send kind 30078 with `["chain"]`, `["C", "notes"]` | `OK` response, event stored |
| RELAY-003 | Query genesis | `REQ` with `{"authors": [pubkey], "#C": ["notes"]}` | Returns genesis event |
| RELAY-004 | Publish append | Query head, send kind 7375 with `["prev", genesisID]` | `OK` response, event stored |
| RELAY-005 | Query chain | `REQ` with author + `C` tag filter | Returns both genesis and append in chronological order |

### 2.2 Chain Validation Rules

| ID | Test | Steps | Expected Result |
|---|---|---|---|
| RELAY-006 | Reject duplicate genesis | Send second genesis for same `(pubkey, C)` | `OK` with error message (or `NOTICE`); event NOT stored |
| RELAY-007 | Reject stale prev | Send append with `prev` pointing to old head after new append accepted | Rejected with `stale-prev` |
| RELAY-008 | Reject missing prev on empty chain | Send append for chain that has no genesis | Rejected with `missing-prev` |
| RELAY-009 | Reject malformed chain tags | Send event with two `C` tags | Rejected with `bad-tags` |
| RELAY-010 | Reject kind 5 chain event | Send kind 5 with `chain` and `C` tags | Rejected with `kind 5 cannot be a chain event` |

### 2.3 Deletion and Query Filtering

| ID | Test | Steps | Expected Result |
|---|---|---|---|
| RELAY-011 | Delete chain event (NIP-09) | Send kind 5 with `e` tag targeting append event | Deletion accepted; target no longer returned in queries |
| RELAY-012 | Query after deletion | `REQ` for chain after deleting append | Returns only genesis; head rewound |
| RELAY-013 | Delete non-chain event | Send kind 5 targeting normal kind 1 note | Standard NIP-09 deletion; note removed from queries |
| RELAY-014 | Delete middle event | Delete event in middle of `G->A->B->C` | `C` and deleted event hidden; `A` becomes visible as head |
| RELAY-015 | Tombstone persistence | Restart relay after deletion | Query still does not return tombstoned event; head remains rewound |

### 2.4 Startup Rebuild

| ID | Test | Steps | Expected Result |
|---|---|---|---|
| RELAY-016 | Restart with existing chain | Stop relay, ensure DB and `chain-state.json` exist, start relay | Chain state correctly rebuilt; queries return same events |
| RELAY-017 | Restart after deletion | Stop relay after deleting middle event, restart | Deleted event and descendants remain tombstoned; head correct |

---

## 3. End-to-End Tests: Client + Relay

These tests exercise the REPL client against one or more running relay instances.

### 3.1 Single-Relay Operations

| ID | Test | Client Commands | Expected Result |
|---|---|---|---|
| E2E-001 | Key management | `key` then `key <hex>` | Shows current key; sets new key successfully |
| E2E-002 | Connect and list | `connect ws://localhost:3334`, `relays` | Relay listed as connected |
| E2E-003 | Genesis command | `genesis notes "Hello world"` | Event published; relay returns `OK` |
| E2E-004 | Append command | `append notes "Second entry"` | Client queries for head, builds append event, publishes; relay accepts |
| E2E-005 | Query command | `query ws://localhost:3334 notes` | Prints `G <genesis>` and `H <append>` with content |
| E2E-006 | Delete command | `delete ws://localhost:3334 notes <append-id>` | Kind 5 published; subsequent query shows only genesis |

### 3.2 Multi-Relay Operations

| ID | Test | Setup | Steps | Expected Result |
|---|---|---|---|---|
| E2E-007 | Publish to multiple relays | Connect relay A and relay B | `genesis test "multi"` | Event accepted by both relays |
| E2E-008 | Query-all | Connect A and B, chain exists on both | `query-all test` | Prints chain from both relays; identical |
| E2E-009 | Divergence detection | Relay A: `G->A1->A2`; Relay B: `G->A1->B2` (diverged at A1) | `diverge test` | Reports divergence between A and B; last common = A1; prints branches |
| E2E-010 | Divergence - in sync | Both relays have identical chain | `diverge test` | Reports "in sync" |
| E2E-011 | Divergence - no common | Relay A has `chainX`, Relay B has `chainY` (different genesis) | `diverge test` | Reports "no common events" |
| E2E-012 | Fix divergence | A diverged from B; B is canonical | `fix <A> test <B>` | Deletes `A2` from A, replays `B2` to A; both now in sync |
| E2E-013 | Fix - already in sync | Both relays identical | `fix <A> test <B>` | Reports "already in sync" |
| E2E-014 | Fix - no common ancestor | A and B have completely unrelated chains for same `(pubkey, C)` | `fix <A> test <B>` | Reports "No common ancestor found. Cannot fix automatically." |

---

## 4. Edge Cases and Stress Tests

| ID | Test | Description | Expected Result |
|---|---|---|---|
| EDGE-001 | Empty chain query | Query chain that has never been created | Client prints "No events found"; relay returns empty set |
| EDGE-002 | Single-event chain | Chain with only genesis | Query shows `G` (also `H` since genesis is both); head = genesis ID |
| EDGE-003 | Concurrent appends | Two clients append simultaneously to same chain on same relay | Only one accepted; other rejected with `stale-prev`. **Note:** There is a race window between `CanAccept` and `AcceptEvent`. |
| EDGE-004 | Append during deletion | Client appends while another deletes the head | Depending on timing, append may reference a head that is tombstoned mid-operation; should be rejected |
| EDGE-005 | Long chain performance | Create chain of 1000+ events | Publish and query remain performant; head tracking stays O(1) |
| EDGE-006 | Multiple chains per pubkey | Create `chainA` and `chainB` under same pubkey | States are isolated; no cross-chain interference |
| EDGE-007 | Same chain name, different pubkey | Pubkey 1 creates `notes`, Pubkey 2 creates `notes` | Treated as independent chains; both accepted |
| EDGE-008 | Malformed prev value | Client sends `prev` tag with non-hex or wrong length | Relay may store it but chain validation only checks existence in state, not format. If prev doesn't match head, rejected. |
| EDGE-009 | Fix replay of existing IDs | Target relay already has winning events (tombstoned) | Replay may be rejected by Khatru as duplicate event ID. Client should handle `FAILED` responses gracefully. |
| EDGE-010 | Client findHead with diverged relays | Relays A and B diverged; `append` queries both | Client may build an invalid chain mixing events from A and B, then create append with incorrect `prev`. This is expected client behavior under divergence; user should run `diverge`/`fix` first. |
| EDGE-011 | Chain state file corruption | `chain-state.json` is truncated or invalid JSON | `NewState` should handle error gracefully (current code ignores load errors). Document behavior: starts fresh, may lose tombstone info. |
| EDGE-012 | Deletion of genesis after long chain | Chain `G->A1->...->A100`, delete `G` | All events tombstoned; head = `""`. Query returns nothing. Genesis can now be recreated. |
| EDGE-013 | Rebuild with out-of-order timestamps | Events stored with non-monotonic `created_at` (e.g., clock skew) | `RebuildFromEvents` sorts by `created_at`; last event wins head. Document this behavior. |

---

## 5. Known Issues and Areas of Concern

The following behaviors were identified during code review and should be explicitly tested or monitored:

1. **Race Condition in Head Update**: `CanAccept` and `AcceptEvent` are two separate calls in the relay handler. Between checking the head and updating it, another event could be accepted, causing the first append to become stale and fail when `AcceptEvent` runs. This is acceptable for a single-relay deployment but should be documented.

2. **`RebuildFromEvents` and Divergent Branches**: If the event store contains multiple active branches for the same chain (e.g., from a concurrent write race or before a fix), `RebuildFromEvents` processes all events sorted by `created_at` and sets the head to the last one. Earlier branches are silently overwritten in the state head pointer, though their events remain in `cs.Events`. This can lead to a `prev` pointing to an event that is not the head.

3. **Client `findHead` with Diverged Relays**: The client queries ALL connected relays and aggregates events into a single map before building the chain. If relays are diverged, the client may assemble an invalid mixed chain and produce an append with a `prev` that does not match any single relay's head. This is mitigated by the user running `diverge`/`fix`, but the client does not prevent it proactively.

4. **Fix Command Replay of Duplicate IDs**: When `fix` replays winning suffix events to a target relay, those event IDs may already exist in the target's storage (as tombstoned or rejected events). Khatru may reject them as duplicates. The fix command does not have a mechanism to force overwrite.

5. **Chain State Persistence Ignores Load Errors**: `NewState` calls `load()` but ignores the error. If `chain-state.json` is corrupted, the relay starts with empty chain state. Since events are still in BoltDB, `rebuildChainState` will rebuild active events, but tombstone information is lost. Deleted events would become queryable again until a new deletion is issued.

---

## 6. Test Environment Setup

### Prerequisites
- Go 1.25+
- A temporary directory for each relay instance (to avoid DB/state conflicts)
- WebSocket client library or `nostr` CLI for manual testing

### Running a Test Relay
```bash
# Terminal 1
mkdir -p /tmp/relay-a/db
cd /tmp/relay-a
CHAIN_STATE_PATH=/tmp/relay-a/chain-state.json go run ./cmd/relay/main.go
# Listens on :3334
```

### Running a Second Relay
```bash
# Terminal 2
mkdir -p /tmp/relay-b/db
cd /tmp/relay-b
# Modify port if needed, or use environment variable
PORT=3335 go run ./cmd/relay/main.go
```

### Running the Client
```bash
go run ./cmd/client/main.go
> connect ws://localhost:3334
> connect ws://localhost:3335
> genesis test "hello"
```

---

## 7. Success Criteria

- All unit tests in `internal/chain` pass with `go test ./internal/chain/...`
- Relay integration tests confirm that invalid chain events are rejected before storage
- Deletion correctly tombstones events and rewinds heads
- Multi-relay divergence detection identifies mismatched chains accurately
- The `fix` command brings diverged relays back into sync for the tested scenarios
- Edge cases either pass as documented or have their actual behavior captured for future fixes

---

### Critical Files for Implementation

- `/Users/pablofernandez/Work/serial-killer/internal/chain/state.go`
- `/Users/pablofernandez/Work/serial-killer/cmd/relay/main.go`
- `/Users/pablofernandez/Work/serial-killer/cmd/client/main.go`
- `/Users/pablofernandez/Work/serial-killer/go.mod`
