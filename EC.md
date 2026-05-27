# NIP-EC

## Relay-enforced event chains

`draft` `optional` `relay`

This NIP defines an opt-in mechanism for relay-enforced linear event chains.

```
accept event iff prev == current known state
```

---

## Tags

A chain event MUST contain:

```json
["chain"]
```

and MAY contain a `d` tag to name the chain:

```json
["d", "<chain-name>"]
```

A non-genesis chain event MUST also contain exactly one:

```json
["prev", "<event-id>"]
```

A chain event MAY be any kind except `5`. More than one `prev` tag MUST be rejected.

## Chain identity

The chain is identified by `(pubkey, d)` where `d` is the value of the `["d"]` tag. If no `["d"]` tag is present, the chain name is implicitly the event kind as a string.

## Head rule

The relay maintains one active head per `(pubkey, d)`.

**No active head:**
- No `prev` → accept (genesis)
- Has `prev` → reject: `invalid: chain:missing-prev <prev>`

**Active head exists:**
- `prev == head` → accept
- `prev != head` → reject: `invalid: chain:stale-prev current=<head>`
- No `prev` → reject: `invalid: chain:missing-prev <head>`

Malformed tag structure: `invalid: chain:bad-tags`

## Opting out

To dissolve an active chain, publish any event for that `(pubkey, d)` key that includes `["prev", "<current-head>"]` but does NOT include `["chain"]`. The relay accepts the event (prev is valid) and removes chain enforcement for that key — subsequent events no longer require `prev`.

## Deletion and rollback

The relay MUST honor NIP-09 deletions targeting the current head. Deletions targeting non-head events MUST be rejected.

Deleting the head rewinds it to the deleted event's `prev`. If the deleted event is the genesis, the chain becomes empty and a new genesis MAY be accepted.

The relay MUST remember deletions and MUST NOT reactivate a deleted event if it is received again.

---

# Relay advertisement

Relays implementing this NIP SHOULD include `"FF"` in their NIP-11 supported NIPs list. A relay MUST NOT advertise support unless it also honors NIP-09 deletions for chain events.

---

# Appendix: Reconciling diverged relays

Two relays can diverge when clients write to them independently:

```
Relay A: G -> H -> X
Relay B: G -> H -> Y
```

Both are valid. The application chooses which branch wins.

To reconcile (choosing A as canonical):

1. Find the last common event (`H`)
2. Delete the losing suffix on relay B tip-first, one event at a time, until the head rewinds to `H`
3. Replay the winning events to relay B in order (`X`, then any subsequent events)
