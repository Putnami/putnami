# ADR 0001 — The web analytics wire is Putnami-owned and closed

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/analytics` (`protocols/analytics`)

## Context

A Putnami web application needs audience measurement without a third-party
script (which exports visitor data and forces a consent banner) or a
hand-rolled beacon per application.

The producer is a browser anyone can modify, and the consumer writes accepted
values into the application database as keys of a daily counter table. So:

1. **Cardinality is a security property.** A free-form name or key is an
   unbounded series an anonymous sender writes, paid for by the owner.
2. **The two runtimes must agree exactly.** The browser sanitizes before
   sending and the server before storing; if one bound differs, one side is
   decorative.

[`protocols/telemetry`](../../../telemetry/README.md) does not fit: OTLP
describes a running service, and a browser page view is not one.

## Decision

1. **A Putnami-owned JSON wire, not OTLP.** `POST /_putnami/analytics/events`
   carries a `Batch` of `protocolVersion`, `sentAt`, and 1..50 events.
2. **Closed enums with predicates.** Viewport class, referrer type, device
   type, browser, operating system, source, visitor kind, outcome, campaign
   key, and counter dimension are fixed lists in `analytics.go`, each with an
   `Is…` predicate. A new member is a protocol change.
3. **Every string is bounded and every free field is a grammar.** Action names
   match `^[a-z][a-z0-9_]{0,63}$`, property keys `^[a-z][a-z0-9_]{0,31}$`.
   Paths, routes, referrers, campaign values, and property strings have explicit
   maxima. Property values are a string, a finite number, or a boolean.
4. **Action names are declared server-side.** The grammar is the shape; the
   server's declaration list is the membership test.
5. **`form_submit` is server-only.** It is exported as a shared name and
   rejected as a wire value, so a visitor cannot forge a submission outcome.
6. **The Go code is the contract; JSON Schemas are documentation.**
   `schemas/batch.json` and `schemas/event.json` are published for readers; no
   Go code loads them.
7. **The Go package is the twin of the TypeScript sanitizer and owns the
   corpus.** It serves no traffic. `fixtures/batch/valid` must be accepted
   whole, `fixtures/batch/invalid` rejected, and the file-name stem is the drop
   reason. `TestConformance_InvalidFixturesCoverEveryCode` asserts the invalid
   corpus covers every code except `parse_error`, which `validate_test.go` pins
   directly because a `.json` corpus cannot hold non-JSON.

## Rejected alternatives

- **OTLP logs via `protocols/telemetry`.** Every resource field would be a
  placeholder, and audience data would flow into a pipeline tuned for other
  retention, sampling, and egress.
- **Free-form properties sanitized at query time.** The unbounded series is
  already written by then.
- **A third-party script.** Exports visitor data and forces a consent banner.
- **Per-application beacon shapes.** Moves cardinality and validation into
  every application, with no shared corpus to prove parity.

## Consequences

- A new event name, enum member, or bound is a two-language change plus a
  fixture; the fixture is what makes the other runtime pick it up.
- A new required field is `protocolVersion: 2`. Open pages keep sending `1`,
  and the version separates an old tracker from a hostile sender.
- The wire tracks no external specification.
