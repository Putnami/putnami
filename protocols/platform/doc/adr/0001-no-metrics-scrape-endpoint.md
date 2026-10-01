# ADR 0001 — The operational surface has no `/metrics` scrape endpoint

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/platform` (`protocols/platform`)

## Context

Most frameworks expose health, ready, version, and a Prometheus `/metrics`.
Putnami targets serverless runtimes first, where scraping fails: instances
scale to zero and lose their metrics, are usually not addressable, and live too
briefly to be discovered. An endpoint correct only on long-lived instances
would produce gaps that look like outages.

## Decision

`/metrics` is not part of the platform protocol.

1. The canonical endpoint set is `/livez`, `/healthz`, `/readyz`, `/version`,
   plus opt-in `/debug/pprof/*`. `CanonicalPaths` pins it; the conformance test
   fails when it changes, and changing it bumps the protocol version.
2. Telemetry leaves the process by pushing OTLP/JSON over HTTP to a collector
   ([`protocols/telemetry`](../../../telemetry/README.md)).
3. A workload that wants a metrics handler for local development mounts its
   own. The platform plugin never mounts one.

## Rejected alternatives

- **Serve `/metrics` with a serverless caveat.** Operators alert on what the
  protocol exposes.
- **Serve it only on long-lived runtimes.** Moves a runtime special case into
  the contract.
- **Push and scrape.** Two aggregation and counter-reset semantics.
- **A push-gateway bridge in the platform plugin.** Re-creates scraping with an
  extra hop; transport belongs to telemetry.

## Consequences

- Every runtime needs a collector to see metrics; there is no curl fallback.
- Teams from scrape-based stacks change their collection topology.
- The telemetry exporter contract (flush cadence, final flush on shutdown,
  drop-on-error) carries the load of being the only path.
