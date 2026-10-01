# ADR 0001 — Hand-rolled OTLP/JSON over HTTP is the only telemetry transport

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/telemetry` (`protocols/telemetry`)

## Context

A workload spanning the Go and TypeScript runtimes must point both at one
standard collector and get comparable output. Scraping is unavailable on
serverless runtimes
([platform ADR 0001](../../../platform/doc/adr/0001-no-metrics-scrape-endpoint.md)).
The framework adds no external runtime dependency to solve a wire problem,
because every workload inherits it.

## Decision

1. **OTLP/JSON over HTTP is the only push wire.** Signals go to `/v1/metrics`,
   `/v1/traces`, and `/v1/logs` under the configured collector base URL.
2. **The envelope is a pinned subset of OTLP/JSON** with proto3-JSON encodings:
   64-bit integers as decimal strings, trace and span IDs as lowercase hex,
   enums as integers, lowerCamelCase names. The strict parser rejects unknown
   fields.
3. **The encoder is hand-rolled in each runtime.** No OpenTelemetry exporter
   SDK is a framework dependency.
4. **Canonical serialization is part of the contract.** `AttrsFromStrings`
   sorts attributes by key and `MarshalCanonical` emits a fixed field order.
   Equivalence goldens and pinned SHA-256 digests enforce byte-identical output
   across languages.
5. **Telemetry never affects the workload.** Default 10 s flush cadence,
   mandatory final flush on shutdown, bounded in-memory queue, drop on collector
   error.

## Rejected alternatives

- **OpenTelemetry exporter SDKs.** A large dependency tree with its own
  lifecycle, batching, and retry in every workload, and no byte-identical
  cross-language output.
- **gRPC OTLP.** A protocol stack and codegen for the same collectors; JSON
  over HTTP works unchanged from serverless and through proxies.
- **A bespoke JSON payload plus collector adapters.** Putnami would own an
  ingestion format nobody else implements.
- **statsd or a Prometheus push gateway.** A second aggregation model.
- **Normalize at the collector.** Differences would live in an untested
  component, with no goldens.

## Consequences

- An encoding change is a two-language change: regenerate goldens, copy
  digests, update the TypeScript test in the same pull request.
- Putnami tracks the OTLP/JSON specification by hand; a new upstream field is a
  reviewed change here first.
- Workloads need a collector to see telemetry.
- A transport-shaped configuration option is a second transport and needs its
  own decision.
