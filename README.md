# serial-killer

A Nostr relay implementing **NIP-EC** — relay-enforced linear event chains.

## What problem does this solve?

Nostr applications that maintain ordered state (wallets, document history, ledgers) have no way to prevent conflicting writes. Two clients can publish different "next states" simultaneously, and different relays may end up with different histories. There is no protocol-level way to detect or prevent this.

This relay enforces that every chain has exactly one active head. A new event is only accepted if it correctly references that head. If two clients write concurrently, one wins and the other is told the head has moved. No silent forks.

## How it works for clients

Tag your events with `["chain"]` and `["C", "<chain-name>"]` to opt into enforcement.

The relay will:

- **Accept a genesis event** if no chain exists yet for that `(pubkey, C)`
- **Accept an append event** only if its `["prev", "<id>"]` matches the relay's current head
- **Reject stale writes** with `stale-prev` so the client knows to fetch the latest head and retry
- **Reject forks** — only one branch can ever be active
- **Honor deletions** — deleting a chain event rolls the head back to the nearest surviving ancestor

Deleted events stay deleted. A relay syncing a deleted event later cannot resurrect it.

## Chain event format

**Genesis** (starts a new chain):

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

**Append** (extends the chain):

```json
{
  "kind": 7375,
  "pubkey": "<pubkey>",
  "tags": [
    ["chain"],
    ["C", "nip60"],
    ["prev", "<current-head-id>"]
  ],
  "content": "..."
}
```

Event kind is not part of the chain identity — different kinds can appear in the same chain.

### Querying

```json
["REQ", "<sub-id>", {
  "authors": ["<pubkey>"],
  "#C": ["nip60"]
}]
```

The relay returns only active (non-deleted) events. Reconstruct the chain by following `prev` tags from head to genesis.

### Chain name namespacing

| Good | Bad |
|------|-----|
| `nip60` | `state` |
| `myapp:v1:user-state` | `default` |
| `cashu:<mint-pubkey>` | `main` |

Since kind is not part of the chain key, generic names cause real collisions.

## Deletion and rollback

Send a standard NIP-09 kind-5 event referencing the chain event by `e` tag.

```
A -> B -> C -> D   (delete B)   =>   A
```

Deleting any event in the chain rewinds the head to the nearest surviving ancestor and invalidates everything after the deleted event.

## Diverged relays

Two relays that accepted different writes after the same head are both internally valid — the relay has no way to resolve this on its own. The application decides which branch wins and reconciles manually:

1. Fetch both chains, find the last common event
2. Delete the losing suffix from the diverged relay
3. Replay the winning events to the diverged relay in order

The included client has `diverge` and `fix` commands for this.

## Running

**Requirements:** Go 1.25+, [just](https://github.com/casey/just)

```sh
just build-all   # compile relay and client
just run-relay   # start relay on port 3334
just run-client  # start interactive client
```

To test divergence locally:

```sh
just run-relays  # starts two relays on ports 3334 and 3335
```

### Client commands

```
key [hex]                           show or set private key
connect <url>                       connect to a relay
disconnect <url>                    disconnect
relays                              list connected relays
genesis <chain> [kind] <content>    start a new chain
append <chain> [kind] <content>     extend the chain
query <relay-url> <chain>           show chain from one relay
query-all <chain>                   show chain from all relays
diverge <chain>                     detect divergence across relays
fix <relay-url> <chain> <canonical> reconcile a diverged relay
delete <relay-url> <chain> <event>  delete a chain event
```

## Tests

```sh
just test          # unit tests
./test_repl.sh     # end-to-end: genesis -> divergence -> fix across two relays
```
