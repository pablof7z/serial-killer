# serial-killer

A Nostr relay implementing **[NIP-EC](https://github.com/pablof7z/serial-killer/blob/main/EC.md)** — relay-enforced linear event chains.

A relay-side mechanism that prevents concurrent or stale writes by requiring every event to chain off the last accepted one.

**Scope:** don't mistake this for a global enforcement or ordering guarantee. This enforcement is exclusive to each individual relay. There is no global.

---

## Demo

### Connect to two relays
```bash
> connect ws://127.0.0.1:3334
> connect ws://127.0.0.1:3335
```

### Publish the first event in the chain to both
```bash
> genesis kind=1 First balance state
Publishing event 5bd13a90dd314c19... to 2 relay(s)...
  [ws://127.0.0.1:3334] OK
  [ws://127.0.0.1:3335] OK
```

### Disconnect one relay and advance the chain without it
```bash
> disconnect ws://127.0.0.1:3335
> append kind=1 Second state
Publishing event e03059ac16d3250f... to 1 relay(s)...
  [ws://127.0.0.1:3334] OK
```

### Reconnect — the stale relay rejects the next append
```bash
> connect ws://127.0.0.1:3335
> append kind=1 Third state
Publishing event 31796b5c22203926... to 2 relay(s)...
  [ws://127.0.0.1:3335] FAILED: msg: invalid: chain:stale-prev current=5bd13a90dd314c19...
  [ws://127.0.0.1:3334] OK
```

### Detect and fix the divergence
```bash
> diverge 1
DIVERGENCE between ws://127.0.0.1:3334 and ws://127.0.0.1:3335
  Last common: 5bd13a90dd314c19...
  Branch A: e03059ac16d3250f... -> 31796b5c22203926...

> fix ws://127.0.0.1:3335 1 ws://127.0.0.1:3334
Replaying 2 event(s) to target relay...
  Replayed e03059ac16d3250f...
  Replayed 31796b5c22203926...
Done.
```

---

Any kind works. Here's a kind-3 contact list chained via `nak`:

```bash
$ nak event -k 3 -t chain -p fa984bd7... ec1.f7z.io
publishing to ec1.f7z.io... success.

$ nak event -k 3 -p fa984bd7... ec1.f7z.io
publishing to ec1.f7z.io... failed: msg: invalid: chain:missing-prev 444037895ad02e3e...
```

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
