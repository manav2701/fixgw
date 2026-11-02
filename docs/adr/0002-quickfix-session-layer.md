# ADR 0002: quickfixgo for the session layer, own code above it

## Status
Accepted

## Context
FIX has two layers: the session layer (logon, heartbeats, sequence numbers,
resend/gap-fill) and the application layer (orders, executions). Hand-rolling
the session layer is weeks of conformance work with little portfolio value.

## Decision
Use `github.com/quickfixgo/quickfix` for session mechanics and keep all
application behaviour (auth, order handling, risk, matching, audit) in this
repository, behind the `quickfix.Application` callbacks. A hand-written,
zero-allocation decoder is built separately in Phase 5 as a benchmarked
component, not on the critical path of the gateway.

## Consequences
- Resend, gap fill and `PossDupFlag` handling come from a battle-tested library;
  this repo proves them with a chaos proxy and an integration test.
- Raw-message audit is captured through a `quickfix.LogFactory`, so every byte
  in and out is recorded regardless of message type.
