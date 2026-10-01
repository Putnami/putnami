# ADR 0001 — Structured records have fixed precedence and isolated sinks

- **Status**: accepted
- **Scope**: `go.putnami.dev/logger` (`go/framework/logger`)

## Context

A record combines logger fields, request fields, call-site attributes, trace
identity, error data, and framework metadata. Without one precedence rule a
user field can forge severity or trace identity, and sinks disagree. Logging
runs on failure paths, where a panicking `MarshalJSON`, `LogValuer`, formatter,
or sink must not crash the process.

## Decision

The logger builds one sink-neutral `LogEntry`. Fields merge in this order,
later wins: persistent `With` context, request `FieldBag`, call-site
attributes, framework-reserved fields. JSON output protects severity, message,
timestamp, logger name, trace keys, and error data from user collisions; a
colliding user field is not emitted under that key. The default logger writes
newline-delimited JSON and reads its level once from `LOG_LEVEL`.

Every sink write recovers panics; the panicking value or sink is dropped for
that sink. `Flush` and `Close` visit every sink and return the first error.
Buffer sinks flush on capacity, interval, error-level records, and close.

## Rejected alternatives

- **Per-sink field merging.** One call would yield different records per
  format.
- **Attributes replace reserved fields.** Request data could forge severity,
  trace, or error identity.
- **Propagate marshaling panics.** Observability could kill the work it
  observes.
- **Hide flush and close errors.** They are actionable during shutdown.

## Consequences

- Panic isolation favors availability over delivering a malformed record.
- `Append` callers keep their own counter, because accumulated slices are
  capped.
- Cross-runtime equality holds only for cases in the shared logging
  conformance corpus.
