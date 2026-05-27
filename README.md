# serial-killer

A Nostr relay implementing **NIP-EC** — relay-enforced linear event chains.

**The problem:** Nostr clients that maintain ordered state (wallets, document history) have no way to prevent conflicting writes. Two clients can publish different "next states", and relays silently diverge.

**The fix:** Tag your events with `["chain"]` to opt in. The relay accepts a new event only if its `["prev"]` matches the current head. Stale writes are rejected. No silent forks.

---

## Demo: divergence and recovery

### Start two relays

```bash
$ just run-relays
```

```
Rebuilt chain state from 0 chain events
Chain-enforced relay running on :3334
Supported NIPs: 1, 9, 11, 42, 70, 86, EC
Rebuilt chain state from 0 chain events
Chain-enforced relay running on :3335
Supported NIPs: 1, 9, 11, 42, 70, 86, EC
```

### Connect and create a genesis event

Connect to both relays and publish the first event of a chain. Both relays accept it.

```
> connect ws://localhost:3334
Connected to ws://localhost:3334
> connect ws://localhost:3335
Connected to ws://localhost:3335
> genesis chain=wallet First balance state
Publishing event f1274080bce22c3c0838c34f5587ce294bd6fde8dd0273a8b67f2fd38eb2f95f to 2 relay(s)...
  [ws://localhost:3334] OK
  [ws://localhost:3335] OK
```

### Query both relays

Both relays have the same genesis event.

```
> query-all wallet

=== Relay: ws://localhost:3334
G f1274080bce22c3c... (kind=1, content="First balance state")

=== Relay: ws://localhost:3335
G f1274080bce22c3c... (kind=1, content="First balance state")
```

### Simulate divergence

Disconnect from relay-b, then append. Only relay-a advances.

```
> disconnect ws://localhost:3335
Disconnected from ws://localhost:3335 (connection may persist in pool)
> append chain=wallet Second state (relay-a only)
Publishing event eaa6391967095684d8d890dc6f25528da0a9e9e5a9c74404f401caf7282ea195 to 1 relay(s)...
  [ws://localhost:3334] OK
```

Reconnect to relay-b and attempt another append. Relay-a accepts (prev matches its head), but relay-b rejects — it missed an event and its head is behind.

```
> connect ws://localhost:3335
Connected to ws://localhost:3335
> append chain=wallet Third state
Publishing event 91dee37814364a53861425384c4626058804f521d28e0ea19b471dab63cee942 to 2 relay(s)...
  [ws://localhost:3335] FAILED: msg: invalid: chain:stale-prev current=f1274080bce22c3c0838c34f5587ce294bd6fde8dd0273a8b67f2fd38eb2f95f
  [ws://localhost:3334] OK
```

The chains have diverged. Relay-b is stuck at the genesis while relay-a has advanced two events ahead.

```
> query-all wallet

=== Relay: ws://localhost:3335
G f1274080bce22c3c... (kind=1, content="First balance state")

=== Relay: ws://localhost:3334
G f1274080bce22c3c... (kind=1, content="First balance state")
  eaa6391967095684... (kind=1, content="Second state (relay-a only)")
H 91dee37814364a53... (kind=1, content="Third state")
```

### Inspect the divergence

```
> diverge wallet
Checking divergence for chain 'wallet'...

DIVERGENCE between ws://localhost:3334 and ws://localhost:3335
  Last common: f1274080bce22c3c0838c34f5587ce294bd6fde8dd0273a8b67f2fd38eb2f95f
  Branch A: eaa6391967095684d8d890dc6f25528da0a9e9e5a9c74404f401caf7282ea195 -> 91dee37814364a53861425384c4626058804f521d28e0ea19b471dab63cee942
```

Relay-a is ahead by 2 events. Relay-b has no divergent branch — it simply never received the events.

### Reconcile

The `fix` command replays the canonical relay's missing events onto the diverged one.

```
> fix ws://localhost:3335 wallet ws://localhost:3334
Last common event: f1274080bce22c3c0838c34f5587ce294bd6fde8dd0273a8b67f2fd38eb2f95f
Replaying 2 event(s) to target relay...
  Replayed eaa6391967095684d8d890dc6f25528da0a9e9e5a9c74404f401caf7282ea195
  Replayed 91dee37814364a53861425384c4626058804f521d28e0ea19b471dab63cee942
Done.
> query-all wallet

=== Relay: ws://localhost:3335
G f1274080bce22c3c... (kind=1, content="First balance state")
  eaa6391967095684... (kind=1, content="Second state (relay-a only)")
H 91dee37814364a53... (kind=1, content="Third state")

=== Relay: ws://localhost:3334
G f1274080bce22c3c... (kind=1, content="First balance state")
  eaa6391967095684... (kind=1, content="Second state (relay-a only)")
H 91dee37814364a53... (kind=1, content="Third state")
```

Both relays are now in sync.

---

## Chain event format

**Genesis** (starts a chain):

```json
{
  "tags": [["chain"], ["d", "wallet"]]
}
```

**Append** (extends the chain):

```json
{
  "tags": [["chain"], ["d", "wallet"], ["prev", "<current-head-id>"]]
}
```

If no `["d"]` tag is present, the chain name defaults to the event kind as a string.

## Opting out

To dissolve a chain, publish an event for the same key with `["prev", "<current-head>"]` but without `["chain"]`. The relay accepts it and removes chain enforcement — subsequent events no longer need `prev`.

## Deletion and rollback

Send a standard NIP-09 kind-5 event referencing the chain head by `e` tag. The head rewinds to the deleted event's `prev`. Deleted events cannot be reactivated.

## Running

**Requirements:** Go 1.21+, [just](https://github.com/casey/just)

```sh
just build-all    # compile relay and client
just run-relay    # start relay on :3334
just run-relays   # start two relays on :3334 and :3335
just run-client   # start interactive REPL
just test         # run tests
```

### REPL commands

```
connect <url>                         connect to a relay
disconnect <url>                      disconnect
relays                                list connected relays
genesis [chain=NAME] [kind=N] <text>  start a new chain
append  [chain=NAME] [kind=N] <text>  extend the chain
query-all <chain>                     show chain from all connected relays
diverge <chain>                       detect divergence across relays
fix <relay> <chain> <canonical>       reconcile a diverged relay
delete <relay> <chain> <event-id>     delete a chain event
```
