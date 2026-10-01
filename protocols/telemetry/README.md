# Telemetry protocol

**Push observability data as OTLP/JSON over HTTP, identically from every Putnami runtime.**

## Why

Putnami targets serverless first. Instances scale to zero, are not externally addressable, and live too briefly to register with a scraping collector — so `/metrics` scrape is an explicit non-goal of the [platform protocol](../platform/README.md), and every runtime *pushes* telemetry instead.

Before this protocol the two runtimes pushed differently: the Go framework wired an OpenTelemetry `MeterProvider` but shipped nothing out the door, and the TypeScript framework POSTed a bespoke per-second JSON payload no standard collector could ingest. A workload spanning both runtimes could not point them at one collector. This protocol fixes the one push wire we support so it can.

## What

The canonical push transport is **OTLP/JSON over HTTP** — the documented OpenTelemetry wire format ([OTLP/HTTP spec](https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/protocol/otlp.md#otlphttp)) — POSTed to a standard collector. The protocol pins:

- **Signal endpoints.** `/v1/metrics`, `/v1/traces`, `/v1/logs`, joined onto the configured collector base URL.
- **Envelope shape per signal.** The subset of the OTLP types Putnami emits (`MetricsRequest`, `TracesRequest`, `LogsRequest` and the nested resource/scope/data-point types), with the proto3-JSON encodings the spec requires: 64-bit integers (timestamps, counts, int values) as **decimal strings**, trace/span IDs as **lowercase hex**, enums as **integers**, lowerCamelCase field names.
- **Canonical serialization.** `AttrsFromStrings` sorts attributes by key and `MarshalCanonical` emits compact JSON with HTML escaping off and a fixed field order, so two runtimes produce **byte-identical** bytes for equivalent input. The equivalence goldens + pinned SHA-256 digests guard it.
- **Resource attributes** every runtime populates: `service.name`, `service.version`, `putnami.framework`.
- **Exporter behaviour** (`ExportContract`): default 10s flush cadence, mandatory final flush on shutdown, drop-on-collector-error (telemetry never affects the workload), and a bounded in-memory queue.

Why the transport is hand-rolled OTLP/JSON and nothing else is recorded in [`doc/adr/0001-otlp-json-is-the-only-transport.md`](doc/adr/0001-otlp-json-is-the-only-transport.md).

## How

The Go types here are the source of truth. `strict.go` strict-parses and validates each signal request (unknown fields rejected; `telemetry.*` error taxonomy). `conformance_test.go` pins the version, signals, resource keys, contract values, and error codes, and runs the [`fixtures/<signal>/{valid,invalid}`](fixtures/) corpus through the parser. `equivalence_test.go` builds a fixed input, marshals it canonically, and asserts the bytes equal `fixtures/equivalence/<signal>.golden.json` and hash to a pinned digest. There is no JSON Schema here: the wire is the OpenTelemetry OTLP/JSON specification, and a Putnami-owned schema would be a second description of someone else's contract.

Each runtime maps its native telemetry source onto this contract and tests against the same fixtures and digests:

- **Go** (`go.putnami.dev/telemetry`) translates the OTel SDK's `metricdata`/`ReadOnlySpan` types and the framework logger's records into these envelopes — no OTel exporter SDK dependency.
- **TypeScript** (`@putnami/application`) renders its per-second metric buckets and log records into the same envelopes, replacing the previous bespoke payload, and reimplements `MarshalCanonical` so its `cross-language.test.ts` matches the Go digests.

Regenerate goldens after an intentional encoding change with `UPDATE_GOLDEN=1 go test ./...`, copy the printed digests into `equivalence_test.go`, and update the TypeScript test in lockstep.

## Producers and consumers

| Role | Implementation |
| --- | --- |
| Producer (Go framework) | `go.putnami.dev/telemetry` — metrics, traces, and logs exporters. |
| Producer (TypeScript framework) | `@putnami/application` — metrics and logs; traces are defined but not yet produced. |
| Producer (CLI) | `tooling/cli/internal/telemetry` sends CLI usage sessions as OTLP logs, using the `cliusage` vocabulary below as its data-minimization guard. |
| Consumer (in repo) | `sites/telemetry.putnami.dev` strict-parses incoming OTLP/JSON requests with this package, sanitizes CLI usage records against `cliusage`, and aggregates them. |
| Consumer (out of repo) | Any standard OTLP/HTTP collector. Nothing in the envelope is Putnami-specific beyond the resource attributes. |

## `cliusage`: the CLI usage vocabulary

[`cliusage/`](cliusage/) is a sub-package of this contract, not a second protocol. It pins the closed vocabulary of Putnami CLI usage telemetry — event names, the per-event attribute allow/require sets, the expected OTLP value kind per attribute, the error-category enum, and the public job-command names — as one importable source that the producer (the CLI's data-minimization guard) and the consumer (the receiver's sanitizer) both enforce, so the two cannot drift.

Keeping the vocabulary closed is a bounded-cardinality guarantee as much as a privacy one: a syntactically valid but attacker-chosen command name cannot consume the receiver's aggregate-key budget. `cliusage/otlp-logs.golden.json` pins the exact record shape a session emits. Whether telemetry is sent at all, and the notice and opt-out that gate it, are the CLI's user-facing behaviour and are documented with the CLI.

## Versioning and compatibility

`ProtocolVersion` is `1` and is pinned by `conformance_test.go`. It is bumped whenever a wire-visible change would break a producer or a consumer of these envelopes — a changed field name or encoding, a new required field, or a changed canonical serialization. The envelopes themselves are a *subset* of OTLP/JSON: Putnami may start emitting a field the OpenTelemetry specification already defines without breaking a collector, but the strict parser rejects unknown fields, so any such addition lands here first and is a reviewed change with regenerated goldens.

The equivalence goldens and their pinned digests are the compatibility guard that matters in practice: an encoding change that both runtimes make consistently still fails until the goldens are regenerated and the digests updated in both languages.

## Non-goals

- **No `/metrics` scrape endpoint** — already a platform-protocol non-goal, and scrape does not fit serverless.
- **No new external libraries** — the OTLP/JSON envelope is hand-rolled against the wire format; no OTel exporter SDK packages.
- **One transport only** — gRPC OTLP, statsd, and the Prometheus push gateway are explicit non-goals.

## Adoption status

| Signal | Go framework | TypeScript framework |
| --- | --- | --- |
| metrics | ✅ OTLP/JSON exporter (`/v1/metrics`) | ✅ OTLP/JSON renderer (`/v1/metrics`) |
| traces | ✅ OTLP/JSON span exporter (`/v1/traces`) | — *(defined, not yet adopted — no span producer yet)* |
| logs | ✅ OTLP/JSON log sink (`/v1/logs`) | ✅ OTLP/JSON log sink (`/v1/logs`) |

All adopted paths are tested against this package's fixtures and equivalence digests.

## Ownership, support, and evidence

**Owner** — the `protocols` standardization layer. `protocols/telemetry`
(`go.putnami.dev/protocol/telemetry`) is the single owning project for the
envelope subset, the canonical serialization, the resource conventions, the
exporter contract, and the `cliusage` vocabulary.

**Support status** — `stable` in the workspace-root
[`putnami.support.json`](../../putnami.support.json) catalog, whose vocabulary
[`protocols/support`](../support/README.md) defines. Support status is a public
commitment, not a maturity stage.

**Evidence for that status**

- The wire is OTLP/JSON, an externally specified and widely implemented format;
  this package pins the subset Putnami emits rather than inventing one.
- Byte-level cross-language agreement is proven, not asserted: the Go
  `equivalence_test.go` goldens and their SHA-256 digests are re-checked by
  `typescript/framework/application/test/telemetry/cross-language.test.ts` for
  all three signals.
- `conformance_test.go` pins `ProtocolVersion`, the signal paths, the resource
  attribute keys, the exporter contract values, and the error taxonomy, and runs
  the whole valid/invalid corpus; `invalid_fixture_coverage_test.go` proves
  every invalid fixture is genuinely rejected.
- A real consumer runs in production: `sites/telemetry.putnami.dev` parses these
  envelopes with this package.
- The one gap is adoption, not contract: TypeScript does not yet produce traces.
  The envelope is defined, tested, and byte-pinned; nothing about it changes
  when a span producer appears.

**Owning user feature** — none in this repository. The wire itself is
infrastructure: a workload's observability is delivered by the framework and CLI
packages that produce it, and the one genuinely user-facing surface here — CLI
usage telemetry, its notice, and its opt-out — is owned by the CLI's own vertical
scope, which is where a user chooses anything at all. Declaring a product
feature for the envelope would claim a user outcome this package cannot deliver
alone. Its durable decisions live in [`doc/adr/`](doc/adr/) rather than in a
spec, which by contract details exactly one authored feature.
