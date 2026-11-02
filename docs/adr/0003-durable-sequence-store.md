# ADR 0003: SQLite message store for sequence numbers

## Status
Accepted

## Context
A FIX session survives disconnects only if both sides remember their sequence
numbers and the sender keeps a copy of what it sent (for `ResendRequest`).
quickfix's default file store keeps these in flat files next to the process. On a
container that gets rescheduled, or a host that is rebuilt, those files vanish.
The next logon then arrives at the wrong sequence number and the only fix is a
manual session reset agreed with every client, typically at the worst moment.

## Decision
Implement `quickfix.MessageStore` on SQLite (`modernc.org/sqlite`, pure Go, no
cgo, so cross-compiling and the scratch container image stay simple):

- `sessions(session_id, creation_time, next_sender, next_target)`
- `messages(session_id, seq, body)` for the outbound archive

Design points:

- Sequence numbers are cached in memory but every change is written to the
  database first; memory never runs ahead of what is durable.
- `SaveMessageAndIncrNextSenderMsgSeqNum` is one transaction, so a crash cannot
  leave a sent sequence number without its archived copy.
- WAL mode with `synchronous=NORMAL`: durable across process crashes, and
  bounded loss (the last few commits) on power failure. Switching to `FULL` is a
  one-line change if a venue needs it; the cost is an fsync per sent message. This
  trade-off gets measured in Phase 5.
- The connection pool is capped at one connection. SQLite serialises writers
  anyway, and it keeps `IterateMessages` callbacks from deadlocking.

## Consequences
- Restarting the gateway keeps sessions in sequence. Covered by
  `TestSQLiteStorePersistsAcrossReopen`.
- One writer means the store is a throughput ceiling at very high message rates.
  Moving to Postgres is possible without touching the session code, because
  only `quickfix.MessageStore` is implemented.
- The database file is state: it needs a volume in deployment (Phase 6).
