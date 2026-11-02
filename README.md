# fixgw

A FIX 4.4 order-entry gateway for a crypto venue, written in Go.

Status: work in progress, built in public in six phases. Not production software.

Phase 1 delivers the project skeleton, CI and the domain core: fixed-point
price/quantity types, the order state machine and a ClOrdID registry. See
`docs/adr` for design decisions.
