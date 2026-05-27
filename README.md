# serial-killer

A Nostr relay and client implementing **NIP-FF** — relay-enforced linear event chains.

## What is this?

Nostr applications that need ordered, linear state (e.g. a wallet's token history) face a fundamental problem: two clients can publish conflicting next states, and different relays may see different branches. There is no protocol-level truth.

NIP-FF solves this with one primitive:

```
accept event iff prev == current active head
```

A supporting relay maintains exactly one active head per `(pubkey, C)` pair and atomically validates each new event against it. If two clients race to append, one wins and the other gets a `stale-prev` rejection.

This repo contains:

- **`cmd/relay`** — a Nostr relay that enforces chain ordering, persists events to disk, and filters tombstoned events from query results
- **`cmd/client`** — an interactive REPL client for creating, querying, and reconciling chains across relays

## Chain format

A chain event carries two required tags:

```json
["chain"]
["C", "<chain-name>"]
```

The chain is identified by `(pubkey, C)`. Event kind is **not** part of the identity — different kinds can coexist in the same chain.

**Genesis event** (no `prev`):

```json
{
  "kind": 17375,
  "pubkey": "<pubkey>",
  "tags": [
    ["chain"],
    ["C", "nip60:wallet:<wallet-id>"]
  ],
  "content": "..."
}
```

**Append event** (must reference current head):

```json
{
  "kind": 7375,
  "pubkey": "<pubkey>",
  "tags": [
    ["chain"],
    ["C", "nip60:wallet:<wallet-id>"],
    ["prev", "<previous-event-id>"]
  ],
  "content": "..."
}
```

The relay rejects any event whose `prev` does not match the current active head.

### Chain name namespacing

Use descriptive, collision-resistant names:

| Good | Bad |
|------|-----|
| `nip60:wallet:<wallet-id>` | `state` |
| `myapp:v1:user-state` | `default` |
| `cashu:<mint-pubkey>:wallet:<wallet-id>` | `main` |

## NIP-09 deletion and rollback

A supporting relay **must** honor NIP-09 deletion requests for chain events.

- Deleting the head rewinds to the previous active event
- Deleting an ancestor tombstones it and all descendants; head rewinds to the nearest active ancestor
- Tombstoned events are never reactivated (prevents resurrection of rolled-back branches)

```
A -> B -> C -> D   (delete B)   =>   A
```

## Multi-relay divergence

Two relays can diverge if clients write to them independently:

```
Relay A: G -> H -> X
Relay B: G -> H -> Y
```

Both are internally valid. The application decides which branch wins.

To reconcile (choosing relay A as canonical):

1. **Delete the losing suffix** on relay B — publish a kind-5 event deleting `Y`
2. **Replay the winning suffix** — publish the original signed `X` to relay B

```
Relay A: G -> H -> X
Relay B: G -> H -> X   ✓
```

The client's `diverge` and `fix` commands automate this workflow.

## Getting started

**Requirements:** Go 1.25+, [just](https://github.com/casey/just)

```sh
# Build both binaries
just build-all

# Start two relays on ports 3334 and 3335 (for testing divergence)
just run-relays

# Start the interactive client
just run-client
```

### Client commands

```
key [hex]                           show or set private key
connect <url>                       connect to a relay (checks for NIP-FF support)
disconnect <url>                    disconnect from a relay
relays                              list connected relays
genesis <chain> [kind] <content>    create the first event in a chain
append <chain> [kind] <content>     append to the chain head
query <relay-url> <chain>           fetch and display a chain from one relay
query-all <chain>                   fetch from all connected relays
diverge <chain>                     detect divergence across relays
fix <relay-url> <chain> <canonical> reconcile a diverged relay against canonical
delete <relay-url> <chain> <event>  tombstone an event (NIP-09)
```

### Example session

```
> connect ws://localhost:3334
> connect ws://localhost:3335
> genesis wallet:test "initial state"
> append wallet:test "second state"
> query-all wallet:test
> diverge wallet:test
> fix ws://localhost:3335 wallet:test ws://localhost:3334
```

## Running tests

```sh
just test          # unit tests
./test_repl.sh     # end-to-end: genesis -> divergence -> fix across two relays
```

## Architecture

```
cmd/relay/main.go          relay entry point
cmd/client/main.go         readline REPL; chain reconstruction and reconciliation
internal/chain/state.go    chain state machine (head tracking, tombstoning, atomic accept)
chain_test.go              integration tests against a live in-process relay
```

The relay intercepts three operations:

- **On publish** — validates chain tags and atomically checks `prev == head` before accepting; rejects stale or malformed events
- **On query** — filters tombstoned events so deleted chain history is never surfaced to clients
- **On deletion** — tombstones chain events rather than removing them, then rewinds the head to the nearest active ancestor

On startup the relay reconstructs chain state from stored events, so heads survive restarts.

## NIP-FF relay advertisement

A relay supporting this NIP advertises `"FF"` in its NIP-11 supported NIPs list. The client warns if a relay does not advertise NIP-FF support.
