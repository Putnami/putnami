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
- **Resource attributes** every runtime populates: `service.name`, `service.version`, `putnami.framework`, and `service.instance.id`, which names the process that pushes.
- **Exporter behaviour** (`ExportContract`): log records every 10s, metrics every 60s and spans in batches of at most 512 at most 30s apart, mandatory final flush on shutdown, drop-on-collector-error (telemetry never affects the workload), and a bounded in-memory queue.
- **HTTP server metrics** every runtime records for each request it serves: `http.server.request_count`, a counter, and `http.server.duration`, a histogram in `ms` with fixed bucket bounds, both labeled `http.method`, `http.route` (the route pattern) and `http.status_code`. `conventions.go` defines them and [`fixtures/conventions/http-server-metrics.json`](fixtures/conventions/http-server-metrics.json) commits them for the TypeScript tests.
- **Trace sampling** every runtime applies to its root spans: keep the first `telemetry.traces.budget` roots (default 100), then a root when `telemetry.traces.ratio` (default 0.01) picks its trace ID, with at least `telemetry.traces.floorPerMinute` kept roots each UTC minute (default 1). The ratio picks a trace ID whose last 8 bytes, read big-endian and shifted right one bit, are below the ratio times 2^63. A span with a parent follows the parent's sampled flag, unless `telemetry.traces.parent` is `decide` (default `follow`) and the parent comes from outside: the request carries a valid `traceparent` and no valid `x-putnami-traceparent` on the same trace, as behind a load balancer's edge. Such a server span then takes a root decision and keeps the parent's trace ID, so the app's own sampling applies behind the edge; a parent a Putnami client sent is still followed. The counter `trace.decisions`, labeled `outcome` `kept` or `dropped`, counts each root decision. `sampling.go` defines them and [`fixtures/conventions/trace-sampling.json`](fixtures/conventions/trace-sampling.json) commits the rule's cases for both runtimes.
- **Proxy-safe trace context.** A client writes its `traceparent` value twice, the second time in `x-putnami-traceparent`, a header proxies leave alone. A server without a valid `traceparent` has no remote parent; when `x-putnami-traceparent` is a valid version 00 value with the same trace ID and another parent ID, a proxy rewrote `traceparent`, and the server span parents to the second header's span; otherwise it parents to `traceparent`. `traceparent.go` defines the header and `ResolveRemoteParent`, and [`fixtures/conventions/proxy-safe-traceparent.json`](fixtures/conventions/proxy-safe-traceparent.json) commits the rule's cases for both runtimes.
- **Event trace context** every runtime carries across an event: a publish inside a span writes the event attributes `traceparent`, `00-<trace id>-<span id>-<flags>` in lowercase hex, and `tracestate` when the span has one, replacing caller values; a publish outside a span writes neither. A delivery runs its handler in a consumer span named `process <topic>`, kind consumer, with `messaging.operation.name` `process`, `messaging.destination.name` and `messaging.message.id`. That span is the child of a valid `traceparent` (version `00`, non-zero ids) and takes its flags and its tracestate, unless the tracestate is longer than 512 characters or holds a control character; any other event starts a new root. A failed handler ends the span with status error and the message `events.handler_failed`. `event_trace.go` defines them and [`fixtures/conventions/event-trace-context.json`](fixtures/conventions/event-trace-context.json) commits the publish and consume cases for both runtimes.
- **Export directives** an endpoint returns: any export response may carry `Putnami-Export-Directive: trace-budget=<n>, trace-ratio=<0..1>, trace-floor=<n>, trace-parent=<follow|decide>, log-level=<debug|info|warn|error>, ttl=<seconds>`. For `ttl` seconds, at most 3600, each trace setting the directive carries replaces the service's own, higher or lower, since Putnami Cloud renders settings a person stored ([ADR 0002](doc/adr/0002-a-directive-sets-the-trace-settings.md)); the service applies the higher log level of the directive and its own, and still emits every error record; a new valid directive replaces it, and a malformed one is ignored. A `429` with a `Retry-After` in seconds or as an IMF-fixdate delays the next export of that signal to that endpoint, process-wide, by at most 3600 seconds; log records wait in the queue, and a shutdown during the delay drops them with a warning. `directive.go` defines them and [`fixtures/conventions/export-directives.json`](fixtures/conventions/export-directives.json) commits the cases for both runtimes.

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

[`cliusage/`](cliusage/) is a sub-package of this contract, not a second protocol. It pins the closed vocabulary of Putnami CLI usage telemetry — event names, the per-event attribute allow/require sets, the expected OTLP value kind per attribute, the error-category enum, and the command names — as one importable source that the producer (the CLI's data-minimization guard) and the consumer (the receiver's sanitizer) both enforce, so the two cannot drift.

The command names are the eleven job commands of the CLI catalog when the vocabulary first shipped and every public command that an extension of the `@putnami` scope declares. A name that a released CLI sends never leaves the list, and a name that no released CLI sends may leave it. The receiver drops a record that names an unknown command. Keeping the vocabulary closed is a bounded-cardinality guarantee as much as a privacy one: a syntactically valid but attacker-chosen command name cannot consume the receiver's aggregate-key budget. `cliusage/otlp-logs.golden.json` pins the exact record shape a session emits. Whether telemetry is sent at all, and the notice and opt-out that gate it, are the CLI's user-facing behaviour and are documented with the CLI.

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

## Feature and spec

The module owns the feature of its contract, `telemetry/otlp-push-contract`,
specified in [`specs/otlp-push-contract.json`](specs/otlp-push-contract.json).
The tests here prove its requirements, `cliusage` included. So do the Go
telemetry framework (`go/framework/telemetry`), whose log sink, OTLP mappings,
final flush and dropped collector errors are checked against this package, the
TypeScript renderer of `@putnami/application`, which reproduces the golden
digests, the CLI (`tooling/cli`), whose usage encoder
produces the `cliusage` golden and whose guard records only vocabulary events,
and the usage receiver
(`sites/telemetry.putnami.dev`), which keeps a conforming record. Those projects
keep the features of what they deliver: whether the CLI sends usage telemetry,
its notice and its opt-out belong to the CLI. The final flush and the dropped
collector error moved here from the Go framework's feature,
`go/service-telemetry`. The design intent lives in
[`doc/adr/0001-otlp-json-is-the-only-transport.md`](doc/adr/0001-otlp-json-is-the-only-transport.md).
