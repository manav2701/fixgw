# fixgw

A FIX 4.4 order-entry gateway for a crypto venue, written in Go.

Institutional clients speak FIX; crypto matching engines speak JSON over
WebSocket. `fixgw` is the acceptor that sits in front: it terminates FIX
sessions, authenticates counterparties, manages the order lifecycle and writes
a tamper-evident audit trail.

> Status: work in progress, built in public in six phases. Not production software.

| Phase | Scope | State |
|---|---|---|
| 1 | Repo, CI, domain types (fixed-point price/qty, order state machine, ClOrdID registry) | done |
| 2 | FIX session layer: signed logon, sequence recovery, durable store, audit log, chaos proxy | done |
| 3 | Matching engine, risk checks, kill switch | planned |
| 4 | Venue translation, WebSocket bridge, dashboard | planned |
| 5 | Conformance suite, benchmarks, fuzzing, backtest | planned |
| 6 | Deployment and live demo | planned |

## What works today

- **FIX 4.4 acceptor** on [quickfixgo](https://github.com/quickfixgo/quickfix), with data-dictionary validation.
- **Signed logon.** The client sends `Username(553)`, `Password(554)` and a hex
  HMAC-SHA256 in `RawData(96)` over
  `SendingTime|MsgType|MsgSeqNum|SenderCompID|TargetCompID|Password`. The
  gateway compares in constant time, bounds `SendingTime` skew, and rejects
  replayed signatures. A failed logon gets a generic `Logout` text; the reason
  goes only to the gateway log.
- **Order lifecycle, acknowledge-only for now.** `NewOrderSingle` becomes an
  `ExecutionReport` with `ExecType=0` / `OrdStatus=0`. Unknown symbols, excess
  price/quantity precision and reused `ClOrdID`s become `ExecType=8` rejects.
  `OrderCancelRequest` returns `ExecType=4`, or an `OrderCancelReject` with the
  right `CxlRejReason` (0 too late, 1 unknown order, 6 duplicate id). Matching
  arrives in Phase 3.
- **Durable sequence numbers.** A SQLite `MessageStore` keeps sequence numbers and
  the outbound archive across restarts ([ADR 0003](docs/adr/0003-durable-sequence-store.md)).
- **Audit log.** Every raw inbound and outbound message, as JSON lines with
  nanosecond timestamps. Each entry carries the hash of the previous one, so a
  deleted or edited line is detectable (`audit.Verify`). Passwords and
  signatures are redacted.
- **Chaos proxy.** A TCP proxy that understands FIX framing can swallow whole
  client messages or sever the connection.

## Recovery demo

`fixsim -scenario gap` logs on through the chaos proxy, sends two orders, then
has the proxy swallow the next two `NewOrderSingle`s. The client believes they
were sent. The following order arrives with a sequence number higher than the
gateway expects, the gateway sends `ResendRequest(35=2)`, and the client replays
the missing messages with `PossDupFlag=Y`. Real output against the gateway
binary:

```
== gap: PASS
   ok   logged on through chaos proxy
   ok   2 baseline orders acknowledged
   ok   proxy swallowed 2 orders (G544400-3, G544400-4); gateway has not seen them
   ok   all 4 post-gap orders acknowledged (4/4), including the 2 that were swallowed
   ok   gateway sent ResendRequest (35=2): 1
   ok   client replayed application messages with PossDupFlag=Y: 4
   ok   session still logged on
```

The same flow runs as an automated test (`TestSequenceGapRecovery`).

## Quickstart

Requires Go 1.23 or newer.

```bash
go run ./cmd/fixgw -config deploy/fixgw.cfg -creds deploy/credentials.example.json
```

In another terminal:

```bash
go run ./cmd/fixsim -scenario all
```

Scenarios: `authfail`, `heartbeat`, `orders`, `gap`, and `reconnect`. `reconnect`
resumes sequence numbers without a reset, so run it against a fresh gateway
(`-store memory`, or delete `store/fixgw.db`).

The gateway also serves `GET /healthz`, `GET /api/sessions` and `GET /api/orders`
on `127.0.0.1:8080`.

The credentials in `deploy/credentials.example.json` are public demo values. Do not reuse them anywhere.

## Layout

```
cmd/fixgw           gateway binary
cmd/fixsim          FIX initiator: scenarios and chaos driver
internal/oms        order types, state machine, ClOrdID registry, fixed-point decimals
internal/auth       HMAC logon signing and verification
internal/audit      hash-chained raw-message audit log
internal/fixsession acceptor application, session registry, SQLite store, server wiring
internal/fixmsg     FIX tag constants and message builders
internal/chaos      FIX-aware TCP proxy (drop messages, sever connection)
internal/simclient  initiator used by fixsim and the integration tests
docs/adr            architecture decision records
```

## Design decisions

- [ADR 0001](docs/adr/0001-fixed-point-prices.md): fixed-point integers, never floats
- [ADR 0002](docs/adr/0002-quickfix-session-layer.md): quickfixgo for sessions, own code above it
- [ADR 0003](docs/adr/0003-durable-sequence-store.md): SQLite message store

## Limitations

No matching engine yet, so orders are acknowledged but never fill. No TLS on the
FIX port. Sessions are configured statically. `ClOrdID` uniqueness is tracked per
process lifetime, not per trading day, and the order registry is in memory.

## License

MIT
