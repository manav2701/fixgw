# ADR 0005: Venue routing is asynchronous and reconciled by polling

## Status
Accepted

## Context
Some symbols should trade at an external exchange. The client keeps speaking FIX;
the gateway must translate to each venue's API and translate the answers back into
execution reports, without breaking the guarantees the internal engine gives:
one ordered stream of state changes per order, no fill reported twice, no fill
reported on a closed order.

Two constraints shape the design. The engine goroutine must never wait on the
network (ADR 0004). And venue APIs differ in how they push state (WebSocket user
streams, polling, webhooks), but all of them can be queried for an order's current
state.

## Decision
- **A `Venue` is four methods:** `Place`, `Cancel`, `Query`, `Name`. Everything
  else (symbol mapping, grid checks, turning state into reports) is in the
  `Router`, so a new venue is a small adapter.
- **Validate, then accept, then hand off.** The engine runs the usual risk checks,
  then the venue's tick/step/minimum check (reject, never round), accepts the order
  and returns the `New` acknowledgement, then calls `Router.Submit`, which returns
  at once. The venue call runs in its own goroutine.
- **State changes come from `Query`.** One reconciliation loop polls every live
  order and applies the difference between the venue's state and ours. It is the
  only producer of fills. The price of an increment is derived from the change in
  total executed value, not from the running average.
- **`apply` is idempotent and serialised.** Placement, the poll loop and cancel
  handling can reach it for the same order; a mutex covers the read-compute-fill
  sequence, and applying the same state twice changes nothing.
- **Local close beats a late venue fill, loudly.** A kill switch closes routed
  orders locally and asks the venue to cancel. If the venue reports a fill the
  gateway already closed, it is logged and published as a `break` event instead of
  being sent to the client.
- **Cancel is confirmed by the venue.** The client's cancel is acknowledged only
  after the venue accepts it. If the venue refuses (usually because the order just
  filled) the router refreshes the order's state first, then sends the reject.
- **Replace is refused** on routed symbols. Amend semantics differ too much to
  emulate faithfully.
- **Credentials live in the environment,** named by the routing file, never in it.

## Consequences
- Fills arrive up to one poll interval late. A user-data WebSocket stream would cut
  that to milliseconds; the router would then use `Query` only to reconcile.
- The client sees `New` before the venue has accepted. A venue reject is reported as
  a cancel carrying the venue's reason. Awaiting the venue first would be more
  faithful but would hold the order path for a network round trip.
- Routed symbols have no collar reference price yet.
- Venues stay in sync only while the gateway runs; orders open at a venue when it
  stops are not recovered on restart, because the registry is in memory.
