# Analytics protocol

**One closed, Putnami-owned wire for a web application to measure its own audience — no third party, no consent banner, no open vocabulary.**

## Why

A Putnami web application has no way to count its own page views. Teams reach for Google Analytics or Amplitude, which ships visitor data to a third party and needs a consent banner; the alternative — a home-grown beacon — puts a visitor-controlled JSON blob straight into an application's database.

Both problems are the same problem: nobody owns the wire. This package owns it. It fixes the shape a browser tracker may POST to `/_putnami/analytics/events` and the exact vocabulary a server may accept, so the data at rest is bounded by construction rather than by whoever last edited the ingest route.

The vocabulary is closed for a reason that outlives this feature. Every accepted value becomes a key in a daily counter table on the receiving side, so an open vocabulary is an unbounded metric series that an anonymous visitor writes into. Cardinality is a security property here, not a tuning knob.

## What

- **`Batch`** — the request body: `protocolVersion` (exactly `1`), `sentAt`, and 1..50 `events`, with a 65 536-byte ceiling on the body.
- **`Event`** — one `page_view` or one `action`, keyed by a UUID v7 `eventId` so a retried beacon dedups. `form_submit` is exported so consumers share the name, and is **rejected on the wire**: it is recorded server-side, and accepting it from a browser would let a visitor forge a submission outcome.
- **`Page`** — the viewed `path`, the matched `route` pattern, the `referrer`, and the five `utm` campaign keys.
- **Closed enums** — viewport classes, referrer types, device types, browsers, operating systems, record sources, visitor kinds, form outcomes, campaign keys, and the daily-counter dimensions. Each has an `Is…` predicate; a consumer tests membership with the predicate, never with its own copy of the list.
- **18 error codes** under the `analytics.` prefix, pinned by `TestConformance_ErrorCodes`. The TypeScript sanitizer counts drop reasons by these strings minus the prefix.
- **The fixture corpora** in [`fixtures/`](fixtures/): four valid batches, one invalid batch per error code, the embedded golden batch, the bot-token list, 25 user-agent classification cases, and 12 referrer cases.

Every bound is a constant (`MaxEvents`, `MaxPropKeys`, `MaxPropStringLen`, …) rather than a literal, because the TypeScript sanitizer mirrors each one and a bound that only exists inside an `if` cannot be mirrored on purpose.

**Every length bound counts UTF-8 bytes, not characters.** `MaxPathLen`, `MaxRouteLen`, `MaxReferrerLen`, `MaxUTMLen`, and `MaxPropStringLen` are all applied with Go's `len()` on a string, which is its encoded byte length; `MaxBodyBytes` is the encoded request body. The unit is load-bearing across languages: a 300-character Cyrillic path is 600 bytes, so a sanitizer that measures characters would accept a path this package rejects, and no ASCII fixture can reveal the difference. `TestValidatePageBoundsAreBytes` pins it here.

## How

`strict.go` decodes with `DisallowUnknownFields` and **classifies** the decode failure instead of flattening it: a JSON type mismatch becomes `analytics.attribute_kind` on the offending field, an undefined field becomes `analytics.unknown_attribute` naming it, and only a genuinely undecodable body becomes `analytics.parse_error`. `validate.go` then applies the contract's rules and returns every violation, in document order, as `diagnostic.Diagnostic` values addressed by field path (`events[1].page.utm.source`).

The Go code here is the **contract**, not a runtime. There is no Go web layer, so nothing in this package serves traffic. Its job is to be the executable twin of the TypeScript sanitizer in `@putnami/analytics`, which runs the same fixture corpus: for every file in `fixtures/batch/valid/` the sanitizer must accept every event, and for every file in `fixtures/batch/invalid/` it must drop the batch or the offending event with a reason equal to the file-name stem. That naming rule is pinned here by `TestConformance_InvalidFixtureNamesMatchCodes`, and the completeness of the corpus by `TestConformance_InvalidFixturesCoverEveryCode` — a reject branch no fixture exercises is one the other runtime can silently omit.

`fixtures/bots.json` is embedded as `BotsJSON` and matched case-insensitively as lowercase substrings by `IsBotUserAgent`. An empty User-Agent counts as a bot: a real browser always sends one, so treating its absence as human would make the cheapest possible forgery the one that inflates the audience.

[`schemas/batch.json`](schemas/batch.json) and [`schemas/event.json`](schemas/event.json) are **published documentation**. No Go code reads them; the Go types and `validate.go` are the source of truth, and the schemas exist so a non-Putnami consumer can read the wire without reading Go.

### Notes for the TypeScript classifier

`fixtures/user-agents.json` and `fixtures/referrers.json` describe behaviour this package does not implement — the classifier and the referrer folder live in `@putnami/analytics`. The Go tests here check only that every expected value is a closed-enum member and that the `bot` column agrees with `IsBotUserAgent`; the TypeScript test checks the full classification.

Two rules the twin must implement deliberately, because JavaScript's defaults diverge from this package's:

- **Measure lengths in UTF-8 bytes** — `Buffer.byteLength(value)`, never `value.length`, which counts UTF-16 code units. See the bound note above.
- **Require whole numbers for `seq` and `engagementMs`** — `Number.isInteger(value)`, not `typeof value === 'number'`. Go's decoder rejects `1.5` into an `int` field and the failure surfaces as `analytics.attribute_kind`; a `typeof` check would accept it and store a fractional sequence number.

One expectation resolves an ambiguity in the classification rules: **tablet wins over mobile**. A user agent carrying `iPad` or `Tablet`, or `Android` without `Mobi`, is a tablet; only then does `Mobi`/`Android` mean mobile. Ordering the rules the other way makes the tablet clause unreachable, and the corpus (`SM-X710`, an Android tablet with neither token) would classify as mobile.

## Producers and consumers

| Role | Implementation |
| --- | --- |
| Producer | `@putnami/analytics` browser tracker — builds and POSTs the batch. |
| Consumer | `@putnami/analytics` server ingest — sanitizes against this vocabulary, then folds into the application's own Postgres. |
| Reference payload | `GoldenBatchJSON` — the one importable batch, proven conforming by `golden_test.go`. |

## Versioning

Additive only. A new event name, a new enum member, or a new optional field is a **minor** change: it lands with new fixtures in the corpus, and both runtimes pick it up together. A new **required** field, a removed field, a narrowed bound, or a removed enum member is a breaking change and requires `protocolVersion: 2` — deployed browser trackers keep sending `1` long after a server ships, so the version is the only thing that lets a server tell an old tracker from a hostile one.

## Non-goals

- **No read API.** Querying and visualizing the collected data is not part of this contract.
- **No OTLP mapping.** This is a browser→own-server beacon, not observability push; [`telemetry`](../telemetry/README.md) owns that wire, and the reasoning is in [`doc/adr/0001`](doc/adr/0001-putnami-owned-wire-with-closed-vocabulary.md).
- **No server-to-cloud transport.** Data stays in the application's own database. Nothing here describes shipping it anywhere.
- **No runtime.** No ingest handler, no storage schema, no identity derivation. Those belong to `@putnami/analytics`.

## Ownership and support status

- **Owner**: `go.putnami.dev/protocol/analytics` (`protocols/analytics`).
- **Status**: `preview` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) under the classification
  contract in [`protocols/support`](../support/README.md). Support status is a
  public commitment, not a maturity stage.
- **Evidence**: the version, the 18 error codes, every bound, and every closed
  enum are pinned by `conformance_test.go`; the invalid corpus carries one file
  per code and is proven exhaustive against `ValidErrorCodes`; the golden batch
  is validated rather than asserted. It is not `stable`: the only consumer,
  `@putnami/analytics`, does not exist yet, so no cross-language parity has been
  demonstrated and the corpus should still be able to change.
- **User-facing feature**: none of its own. The user-visible behavior is the
  analytics plugin that produces and consumes these documents; a wire contract is
  not a product feature. The durable decisions for this module live in
  [`doc/adr/`](doc/adr/).
