# fixgw

A FIX 4.4 order-entry gateway and matching engine for a crypto venue, written in Go.

Institutional clients speak FIX; crypto matching engines speak JSON over
WebSocket. `fixgw` is the acceptor that sits in front: it terminates FIX
sessions, authenticates counterparties, applies pre-trade risk controls, matches
orders, and writes a tamper-evident audit trail.

> Status: work in progress, built in public in six phases. Not production software.

| Phase | Scope | State |
|---|---|---|
| 1 | Repo, CI, domain types (fixed-point price/qty, order state machine, ClOrdID registry) | done |
| 2 | FIX session layer: signed logon, sequence recovery, durable store, audit log, chaos proxy | done |
| 3 | Matching engine, pre-trade risk, kill switch, cancel/replace | done |
| 4 | Venue translation, WebSocket bridge, dashboard | done |
| 5 | Conformance suite, benchmarks, fuzzing, backtest | planned |
| 6 | Deployment and live demo | planned |

## What works today

**Session layer**

- FIX 4.4 acceptor on [quickfixgo](https://github.com/quickfixgo/quickfix), with data-dictionary validation.
- **Signed logon.** `Username(553)`, `Password(554)` and a hex HMAC-SHA256 in
  `RawData(96)` over `SendingTime|MsgType|MsgSeqNum|SenderCompID|TargetCompID|Password`.
  Constant-time comparison, `SendingTime` skew bound, replayed signatures rejected.
  A failed logon gets a generic `Logout` text; the reason goes only to the gateway log.
- **Durable sequence numbers** in SQLite ([ADR 0003](docs/adr/0003-durable-sequence-store.md)).
- **Audit log:** every raw message in and out, JSON lines, nanosecond timestamps,
  hash-chained so deleted or edited lines are detectable. Passwords and signatures redacted.
- **Chaos proxy:** swallows whole client messages or severs the connection.

**Order handling and matching**

- Price-time-priority limit order book per symbol. Trades execute at the resting
  order's price. Limit and market orders; `GTC`, `IOC`, `FOK` (`Day` is treated as
  `GTC` for now). Partial fills with `ExecType=F` and `OrdStatus` 1 or 2 kept
  distinct, correct `LeavesQty`, `CumQty` and `AvgPx`.
- **Self-trade prevention:** an order that would hit its own side's resting order
  is cancelled instead (cancel-newest).
- **Cancel and replace.** `OrderCancelRequest(35=F)` and
  `OrderCancelReplaceRequest(35=G)` with `OrigClOrdID` chaining: after a replace
  only the newest `ClOrdID` resolves. A quantity decrease keeps queue priority; a
  price change or increase loses it and may trade at once. Failures return
  `OrderCancelReject(35=9)` with `CxlRejReason` and `CxlRejResponseTo` set correctly.
- Details and trade-offs: [ADR 0004](docs/adr/0004-single-threaded-matching.md).

**Pre-trade risk** (limits per counterparty, see [deploy/risk.json](deploy/risk.json))

Checked in order; every rejection is an `ExecutionReport` with `ExecType=8` and a specific `Text`.

1. Kill switch, global or per counterparty
2. Rate limit (token bucket). Rejected orders still spend a token.
3. Instrument conformance: minimum, maximum, lot size, tick size
4. Per-order notional
5. Price collar around the last trade (or midpoint)
6. Open-order count and gross exposure

Cancels are always allowed, including during a halt. Optional cancel-on-disconnect.

**Kill switch.** `POST /api/killswitch?scope=CLIENT1&on=true&mass_cancel=true` with
`Authorization: Bearer $FIXGW_ADMIN_TOKEN`. Omit `scope` for everyone. With
`mass_cancel` every matching open order is cancelled and its owner notified. The
endpoint is disabled unless the token is set.

**Venue translation** ([ADR 0005](docs/adr/0005-venue-routing.md))

A symbol can be routed to an external exchange instead of the internal book, with
`-venues deploy/venues.example.json`. The client still speaks FIX to the gateway;
the gateway speaks each exchange's own API.

- Adapters for **Binance Spot testnet** (signed REST) and **Deribit testnet**
  (client-credentials REST). API keys come from environment variables, never from
  the file.
- Orders go through the same pre-trade risk as internal ones, then are checked
  against the venue's own tick and step. A mismatch is rejected, not rounded.
- Placement and cancel are asynchronous so the engine never waits on the network.
  A reconciliation loop polls each live order and turns venue state changes into
  `ExecutionReport`s: fills priced per increment, cancels, expiries.
- If the venue shows a fill on an order the gateway already closed (kill switch
  racing a fill), it is logged and shown on the dashboard as a **break**; it is
  never silently reported to the client.
- Replace is refused on routed symbols (`35=9`); cancel and re-enter.

**Dashboard.** `http://127.0.0.1:8080/` once the gateway is running: live FIX tape
(passwords redacted), order-book ladder, blotter, sessions, venue and reconciliation
events, and the kill switch (enter the admin token in the page). It is a static
page with no build step, fed by a WebSocket at `/ws`. The feed never blocks the
gateway: each browser has a bounded queue and the oldest events are dropped for a
slow one.

## Recovery demo

`fixsim -scenario gap` logs on through the chaos proxy, sends two orders, then has
the proxy swallow the next two `NewOrderSingle`s. The client believes they were
sent. The following order arrives with a sequence number higher than the gateway
expects, the gateway sends `ResendRequest(35=2)`, and the client replays the
missing messages with `PossDupFlag=Y`. This runs as an automated test
(`TestSequenceGapRecovery`).

## Quickstart

Requires Go 1.26 or newer.

```bash
FIXGW_ADMIN_TOKEN=change-me go run ./cmd/fixgw
```

In another terminal:

```bash
go run ./cmd/fixsim -scenario all
```

Scenarios: `authfail`, `heartbeat`, `orders`, `risk`, `gap`, `trading` (two
counterparties: partial fills, price improvement, self-trade prevention, FOK,
replace chain) and `reconnect`. `reconnect` resumes sequence numbers without a
reset, so run it against a fresh gateway (`-store memory`, or delete
`store/fixgw.db`).

Open `http://127.0.0.1:8080/` for the dashboard and run `fixsim` to watch it move.
Read-only endpoints on the same port: `/healthz`, `/api/sessions`, `/api/orders`,
`/api/book?symbol=BTC/USD&depth=10`.

To route ETH/USD to the Binance testnet (free keys from testnet.binance.vision):

```bash
export BINANCE_TESTNET_KEY=... BINANCE_TESTNET_SECRET=...
go run ./cmd/fixgw -venues deploy/venues.example.json
```

The credentials in `deploy/credentials.example.json` are public demo values. Do not reuse them anywhere.

## Testing

```bash
go test ./...
go test -race ./...
```

- **Book:** price-time priority, maker-price execution, market and IOC/FOK,
  self-trade prevention, reduce-vs-replace priority, and a randomized test that
  checks the book's structure and quantity conservation after every operation.
- **Engine:** 6,000 random orders, cancels and replaces from four counterparties,
  then: books uncrossed, total bought equals total sold, every order's quantity is
  accounted for, and the registry's open orders match the book exactly.
- **Over FIX:** two clients trading, every risk reject, rate limit, the kill
  switch over HTTP (including refusal without a token), cancel-on-disconnect,
  sequence-gap recovery, reconnect, audit-chain verification.
- **Venues:** the Binance adapter is tested against a fake server that verifies
  the HMAC signature the way Binance does; Deribit against a fake that checks auth
  and token caching. The router is tested with a scriptable venue: partial fills
  priced per increment, placement rejects, cancel refused because the order just
  filled, a fill after local close reported as a break. Over FIX: a routed order's
  full lifecycle, venue-grid rejects, a kill switch cancelling at the venue, and
  unrouted symbols still matching internally.
- **Bridge:** drop-oldest back-pressure, a slow subscriber not blocking others,
  snapshot-then-live ordering over a real WebSocket, no subscriber leak on disconnect.

Not tested: the adapters against the real testnets. They need API keys; the fakes
encode my reading of the documented API, so expect to fix details on first contact.

## Layout

```
cmd/fixgw           gateway binary
cmd/fixsim          FIX initiator: scenarios and chaos driver
internal/oms        order types, state machine, order registry, fixed-point decimals
internal/engine     order book and the single-threaded matching engine
internal/risk       pre-trade checks, rate limiter, kill switch
internal/auth       HMAC logon signing and verification
internal/audit      hash-chained raw-message audit log
internal/fixsession acceptor application, session registry, SQLite store, admin API
internal/fixmsg     FIX tag constants and message builders
internal/chaos      FIX-aware TCP proxy (drop messages, sever connection)
internal/venue      Venue interface, Binance and Deribit adapters, router and reconciliation
internal/bridge     event hub (drop-oldest fan-out) and the WebSocket feed
internal/simclient  initiator used by fixsim and the integration tests
web                 dashboard (static HTML, CSS, JS; embedded in the binary)
docs/adr            architecture decision records
```

## Limitations

Orders and books live in memory: a restart keeps sequence numbers but loses working
orders. `Day` orders do not expire. No TLS on the FIX port. Sessions are configured
statically. `ClOrdID` uniqueness is tracked per process lifetime, not per trading
day. On routed symbols the price collar has no reference price, the venue's
acknowledgement is not awaited before the client sees `New` (a venue reject arrives
as a cancel with the reason), fills are detected by polling (about a second of
latency), and orders left at a venue stay there when the gateway stops. The
dashboard feed is unauthenticated: keep the HTTP port on localhost or behind a
reverse proxy. Matching is single-threaded by design; throughput has not been measured yet
(Phase 5).

## License

MIT
