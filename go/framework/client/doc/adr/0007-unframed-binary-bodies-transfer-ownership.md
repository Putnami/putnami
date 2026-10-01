# ADR 0007 — Unframed binary bodies transfer ownership

- **Status**: accepted
- **Scope**: `go.putnami.dev/client` (`go/framework/client`)

## Context

[go/framework/api ADR 0011](../../../api/doc/adr/0011-streamed-octets-carry-their-own-media-type.md)
declares streamed binary bodies. A reader cannot be rewound, and cancelling the
unary attempt when headers arrive would cancel the still-active body. This
record states the Go client's ownership rules.

## Decision

- An upload carries a single-use `io.Reader`. Authentication and contract
  validation happen before reading it; retries, forwarded-user remint retries,
  redirects and transport replay never send it again.
- A download returns a `StreamedBinaryPayload` with the status, full
  Content-Type and an `io.ReadCloser` the caller owns. It is never accumulated
  in memory. Errors use the ordinary bounded read and typed error path.
- The bound is enforced incrementally.
- Unary total and attempt budgets continue through body consumption. EOF, a
  read failure, `Close`, caller cancellation or application stop closes the
  body and releases those contexts once.
- Bounded binary methods keep their limits and return byte slices.

## Consequences

- Callers must close downloads, including abandoned ones.
- An upload failure is final; a retry is a new call with a new source.
