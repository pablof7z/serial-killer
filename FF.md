# NIP-FF

## Relay-enforced event chains

`draft` `optional` `relay`

## Abstract

This NIP defines an opt-in mechanism for relay-enforced linear event chains.

A chain event is any event containing:

```json
["chain"]
```

and exactly one chain-name tag:

```json
["C", "<chain-name>"]
```

The chain is identified by:

```
(pubkey, C)
```

The event kind is not part of the chain identity. This allows multiple kinds to participate in the same logical state chain.

A relay that supports this NIP maintains one active head for each `(pubkey, C)` pair. A new chain event is accepted only if it extends the relay's current active head.

Relays supporting this NIP MUST honor NIP-09 deletion requests for chain events. NIP-09 currently defines kind `5` deletion request events referencing targets with `e` or `a` tags, with `k` tags recommended for target kinds; this NIP tightens deletion handling for relay-enforced chains.

## Motivation

Some applications need ordered user state.

Without relay enforcement, two clients can publish conflicting next states. Later, a client may see either branch, both branches, or neither branch. That is not sufficient for applications where state must be linear.

This NIP gives applications one primitive:

```
accept event iff prev == current active head
```

That is enough to detect stale writes, missing data, and relay-local divergence.

## Chain event format

A chain event MUST contain:

```json
["chain"]
```

This tag is the explicit opt-in flag for this NIP.

A chain event MUST contain exactly one non-empty `C` tag:

```json
["C", "<chain-name>"]
```

The `C` value names the chain.

A chain event MAY be any event kind except kind `5`.

Kind `5` deletion requests are not themselves chain events. They are control events that may delete chain events.

## Chain identity

The chain key is:

```
(pubkey, C)
```

The event kind is ignored.

These two events are in the same chain if they have the same `pubkey` and `C`, even though they have different kinds:

```json
{
  "kind": 17375,
  "pubkey": "<pubkey>",
  "tags": [
    ["chain"],
    ["C", "nip60"]
  ],
  "content": "..."
}
```

```json
{
  "kind": 7375,
  "pubkey": "<pubkey>",
  "tags": [
    ["chain"],
    ["C", "nip60"],
    ["prev", "<previous-event-id>"]
  ],
  "content": "..."
}
```

Applications SHOULD namespace `C` values carefully.

Good:

```
nip60
myapp:v1:user-state
cashu:<mint-pubkey>
```

Bad:

```
state
default
main
```

Since kind is not part of the chain key, sloppy `C` values cause real collisions.

## Previous pointer

A genesis chain event has no `prev` tag.

Every non-genesis chain event MUST contain exactly one `prev` tag:

```json
["prev", "<event-id>"]
```

The `<event-id>` value MUST be a 32-byte lowercase hex event id.

The previous event MUST:

```
exist on the relay
be active
have the same pubkey
have the same C value
contain ["chain"]
```

The previous event MAY have a different kind.

## Genesis event

```json
{
  "kind": 17375,
  "pubkey": "<pubkey>",
  "created_at": 1730000000,
  "tags": [
    ["chain"],
    ["C", "nip60"]
  ],
  "content": "...",
  "sig": "..."
}
```

A relay MUST only accept a genesis event if it has no active head for the same `(pubkey, C)`.

## Append event

```json
{
  "kind": 7375,
  "pubkey": "<pubkey>",
  "created_at": 1730000100,
  "tags": [
    ["chain"],
    ["C", "nip60"],
    ["prev", "<previous-event-id>"]
  ],
  "content": "...",
  "sig": "..."
}
```

A relay MUST only accept this event if:

```
previous-event-id == current active head for (pubkey, C)
```

## Relay behavior

A relay that supports this NIP MUST enforce one active head per:

```
(pubkey, C)
```

The relay MUST perform the head check and insertion atomically.

Without atomicity, two concurrent writes can both pass validation and create a fork on the same relay.

## Validation

A chain event MUST contain exactly one:

```json
["chain"]
```

A chain event MUST contain exactly one non-empty:

```json
["C", "<chain-name>"]
```

A chain event MUST contain zero or one:

```json
["prev", "<event-id>"]
```

A chain event with no `prev` tag is a genesis event.

A chain event with one `prev` tag is an append event.

A chain event with more than one `prev` tag MUST be rejected.

Malformed chain events SHOULD be rejected with:

```json
["OK", "<event-id>", false, "invalid: chain:bad-tags"]
```

## Append rule

Let `head` be the relay's current active head for `(pubkey, C)`.

### No current active head

If `head` does not exist:

```
event has no prev -> accept
event has prev    -> reject
```

Recommended rejection:

```json
["OK", "<event-id>", false, "invalid: chain:missing-prev <prev-event-id>"]
```

### Current active head exists

If `head` exists:

```
event.prev == head -> accept
event.prev != head -> reject
event has no prev  -> reject
```

If the referenced previous event is unknown or inactive:

```json
["OK", "<event-id>", false, "invalid: chain:missing-prev <prev-event-id>"]
```

If the referenced previous event exists but is not the active head:

```json
["OK", "<event-id>", false, "invalid: chain:stale-prev current=<head-event-id>"]
```

The relay MUST NOT use `created_at` to decide which event wins.

If two events extend the same head concurrently, the relay accepts whichever event wins the atomic insertion and rejects the other as stale.

## Duplicate events

If the relay already has the exact same event, it SHOULD use its normal duplicate handling and MUST NOT change the chain head.

Example:

```json
["OK", "<event-id>", true, "duplicate: already have this event"]
```

## Reading a chain

Clients SHOULD query by author and `C` tag:

```json
["REQ", "<sub-id>", {
  "authors": ["<pubkey>"],
  "#C": ["nip60"]
}]
```

Clients SHOULD NOT include `kinds` unless they know the chain only uses those kinds.

A client reconstructs the chain by following `prev` tags.

A valid active chain has:

```
one genesis event
no missing prev event
no active fork
one active head
```

Clients MUST ignore events with the same `C` value that do not contain:

```json
["chain"]
```

## NIP-09 deletion support

A relay that supports this NIP MUST honor valid NIP-09 deletion requests for chain events.

A relay MUST NOT advertise support for this NIP unless it honors NIP-09 deletion requests for chain events.

A NIP-09 deletion request that deletes a chain event is a normal kind `5` event.

Example:

```json
{
  "kind": 5,
  "pubkey": "<pubkey>",
  "created_at": 1730000200,
  "tags": [
    ["e", "<chain-event-id>"],
    ["k", "7375"]
  ],
  "content": "rollback chain event",
  "sig": "..."
}
```

The deletion request itself SHOULD NOT contain:

```json
["chain"]
```

and SHOULD NOT contain:

```json
["C", "<chain-name>"]
```

The target event is what opts into this NIP, not the deletion request.

For this NIP, a relay MUST treat a chain event as deleted if:

```
a valid NIP-09 deletion request references the event by e tag
the deletion request has the same pubkey as the target event
the target event contains ["chain"]
the target event contains ["C", "<chain-name>"]
```

## Chain rollback through deletion

Deleted chain events are inactive.

Inactive events MUST NOT be used as active heads.

Inactive events MUST NOT be accepted as `prev` targets.

If the current head is deleted, the relay rewinds the active head to the deleted event's previous active ancestor.

Example:

```
A -> B -> C
```

Delete `C`:

```
A -> B
```

Active head:

```
B
```

If an ancestor is deleted, all of its descendants become inactive for chain-head calculation.

Example:

```
A -> B -> C -> D
```

Delete `B`:

```
A
```

Active head:

```
A
```

The relay MAY still store `B`, `C`, and `D`, but it MUST NOT consider them active.

If the genesis event is deleted:

```
A -> B -> C
```

the active chain becomes empty:

```
<empty>
```

After that, the relay MAY accept a new genesis event for the same `(pubkey, C)`.

## Tombstones

A relay supporting this NIP MUST remember NIP-09 deletions of chain events well enough to prevent deleted chain events from becoming active again.

If a deleted chain event is later received again, the relay MUST NOT reactivate it.

If reactivating deleted events were allowed, delayed relay propagation could resurrect rolled-back branches and break reconciliation.

## Deleting a suffix

To roll back:

```
A -> B -> C -> D
```

to:

```
A -> B
```

the client SHOULD publish a NIP-09 deletion request for every event in the discarded suffix:

```json
{
  "kind": 5,
  "pubkey": "<pubkey>",
  "created_at": 1730000300,
  "tags": [
    ["e", "<C>"],
    ["e", "<D>"],
    ["k", "<kind-of-C>"],
    ["k", "<kind-of-D>"]
  ],
  "content": "rollback chain suffix",
  "sig": "..."
}
```

For chain-head calculation, deleting `C` is enough.

For normal deletion semantics and relay interoperability, deleting every event in the suffix is cleaner.

## Publishing algorithm

For one relay:

```
1. Fetch events for (pubkey, C).
2. Find the relay's active head.
3. Create a new event with ["prev", "<head>"].
4. Publish the event.
5. If accepted, done.
6. If missing-prev, fetch or publish the missing previous event.
7. If stale-prev, fetch current head, rebase or discard local state, then retry.
```

For multiple relays, clients MUST track success separately per relay.

A write succeeding on relay A says nothing about relay B.

## Relay advertisement

Relays that implement this NIP SHOULD advertise support for it.

Clients MUST NOT assume enforcement unless the relay advertises this NIP or the client has out-of-band knowledge.

A relay that does not support this NIP may accept forks.

A relay that does not honor NIP-09 deletions for chain events MUST NOT advertise support for this NIP.

## Security considerations

This NIP does not make relays honest.

A malicious relay can still:

```
hide events
lie about its head
accept forks
ignore deletions
resurrect deleted events
reject valid writes
```

The useful guarantee is narrower:

```
An honest supporting relay maintains at most one active head for each (pubkey, C).
```

## Non-goals

This NIP does not define:

```
multi-relay consensus
merge events
DAG histories
CRDTs
global ordering
automatic conflict resolution
```

Applications decide which branch wins when relays diverge.

---

# Appendix A: Reconciling two diverged relays

Assume two relays were synced:

```
Relay A: G -> H
Relay B: G -> H
```

Then client A can only reach relay A and writes `X`:

```
Relay A: G -> H -> X
Relay B: G -> H
```

Later client B can only reach relay B and writes `Y`:

```
Relay A: G -> H -> X
Relay B: G -> H -> Y
```

Both relays are internally valid.

There is no protocol-level truth saying whether `X` or `Y` wins.

The app must choose.

## Choose `X` as canonical

Current state:

```
Relay A: G -> H -> X
Relay B: G -> H -> Y
```

Goal:

```
Relay A: G -> H -> X
Relay B: G -> H -> X
```

### Step 1: Fetch both chains

The client queries both relays:

```json
["REQ", "chain", {
  "authors": ["<pubkey>"],
  "#C": ["nip60"]
}]
```

The client reconstructs both chains:

```
Relay A: G -> H -> X
Relay B: G -> H -> Y
```

The last common event is:

```
H
```

Winning suffix:

```
X
```

Losing suffix:

```
Y
```

### Step 2: Delete the losing suffix on relay B

Publish a NIP-09 deletion request to relay B:

```json
{
  "kind": 5,
  "pubkey": "<pubkey>",
  "created_at": 1730000400,
  "tags": [
    ["e", "<Y>"],
    ["k", "<kind-of-Y>"]
  ],
  "content": "rollback losing chain branch",
  "sig": "..."
}
```

Relay B honors the deletion and rewinds:

```
Relay B: G -> H
```

Its active head is now:

```
H
```

### Step 3: Replay `X` to relay B

Publish the exact original signed event `X` to relay B.

Do not recreate it.

The event still points to `H`:

```json
{
  "id": "<X>",
  "kind": 7375,
  "pubkey": "<pubkey>",
  "tags": [
    ["chain"],
    ["C", "nip60"],
    ["prev", "<H>"]
  ],
  "content": "...",
  "sig": "..."
}
```

Relay B checks:

```
prev == current active head
```

Since relay B's active head is now `H`, it accepts `X`.

Final state:

```
Relay A: G -> H -> X
Relay B: G -> H -> X
```

The relays are reconciled.

## Longer divergence

Current state:

```
Relay A: G -> H -> X1 -> X2 -> X3
Relay B: G -> H -> Y1 -> Y2
```

Choose relay A's branch as canonical.

Delete relay B's losing suffix:

```json
{
  "kind": 5,
  "pubkey": "<pubkey>",
  "created_at": 1730000500,
  "tags": [
    ["e", "<Y1>"],
    ["e", "<Y2>"],
    ["k", "<kind-of-Y1>"],
    ["k", "<kind-of-Y2>"]
  ],
  "content": "rollback losing chain suffix",
  "sig": "..."
}
```

Relay B rewinds:

```
Relay B: G -> H
```

Replay the canonical suffix to relay B in order:

```
publish X1
publish X2
publish X3
```

Final state:

```
Relay A: G -> H -> X1 -> X2 -> X3
Relay B: G -> H -> X1 -> X2 -> X3
```

## Preserving useful state from both branches

Sometimes the losing branch contains useful application-level changes.

Do not keep both branches in the chain.

Instead, create a new linear event that contains the merged application state.

Diverged state:

```
Relay A: G -> H -> X
Relay B: G -> H -> Y
```

The app chooses `X` as canonical but wants to preserve some meaning from `Y`.

First publish a new event `M` after `X` on relay A:

```
Relay A: G -> H -> X -> M
Relay B: G -> H -> Y
```

`M` has one previous pointer:

```json
["prev", "<X>"]
```

Its content contains the app-level result of reconciling `X` and `Y`.

Then on relay B:

```
1. Delete Y.
2. Replay X.
3. Replay M.
```

Final state:

```
Relay A: G -> H -> X -> M
Relay B: G -> H -> X -> M
```

This keeps the chain linear while still letting the application preserve useful semantic data from the discarded branch.
