# ADR 0004: A single-threaded matching engine around a pure order book

## Status
Accepted

## Context
Order flow arrives concurrently, one goroutine per FIX session. Matching must be
deterministic: the same inputs in the same order must give the same fills, or the
book cannot be audited or replayed (Phase 5 backtesting depends on this).

## Decision
- **One goroutine owns all books.** `engine.Engine` hands every public call to a
  single goroutine through an unbuffered channel and waits for the result.
  Concurrency stays at the session layer; the engine is a sequential machine.
  Locks around the book would give mutual exclusion but not a defined order of
  events, and the order is what makes the system replayable.
- **The book is a pure data structure.** `engine.Book` knows prices, quantities
  and queue positions only. It does not know about FIX, risk, sessions or order
  state, so it can be tested exhaustively on its own, including a randomized test
  that checks quantity conservation after every operation.
- **The registry is the system of record.** The engine decides fills; the order
  registry records them and produces the execution reports. Both counterparties of
  a trade are updated by the same engine step, so a trade is never half-recorded.
- **Book structure:** a hash map of price to FIFO queue (`container/list`) for O(1)
  lookup, plus an ascending sorted slice of active prices for best-price and depth
  traversal. Inserting a new price level is O(n) in the number of levels, which is
  fine at demo scale. Production venues use a preallocated array indexed by tick,
  since the tick range is bounded; that is the known upgrade path.
- **Trades execute at the resting order's price.** An aggressive order gets price
  improvement and the maker is never filled worse than quoted.
- **Self-trade prevention is cancel-newest.** Matching stops when the incoming
  order meets a resting order with the same key (account if set, else session), and
  the incoming order's remainder is cancelled. The resting order is untouched.
- **Replace semantics:** a pure quantity decrease keeps queue position; a price
  change or quantity increase loses it, and may trade immediately if it crosses.
  The order is re-indexed under the new `ClOrdID`, and the old one stays burned.
- **Invariant faults halt trading.** If recording a fill fails, the engine engages
  the global kill switch rather than continuing on inconsistent state.

## Consequences
- Throughput is bounded by one core and the registry mutex. That is the right
  trade for correctness here; sharding by symbol is possible later because books
  are independent.
- Every engine call is a goroutine hand-off, which adds latency. Phase 5 measures
  it.
- State is in memory. A restart keeps sequence numbers (ADR 0003) but loses
  working orders and the book. Persisting them is future work.
