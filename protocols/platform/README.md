# Platform Endpoints Protocol

## Why

The platform protocol exists so every Putnami runtime presents the same operational HTTP surface to operators, dashboards, and orchestrators.

Without a shared contract, Go, TypeScript, and future runtimes can drift on:

- which paths are exposed (`/healthz` vs `/_/health` vs `/admin/health`),
- the JSON envelope shape for liveness and readiness,
- whether 200 / 503 means "alive but degraded" or "draining",
- how probes are registered and discovered,
- whether pprof leaks heap details on a public port by default.

Operators write alerts and dashboards against these contracts. Drift across languages forces them to special-case every runtime.

## What

This package defines the versioned contract for:

- the canonical operational endpoint set (`/healthz`, `/livez`, `/readyz`, `/version`, opt-in `/debug/pprof/*`),
- the JSON response envelope shape and status values (`ok`, `unavailable`, `degraded`),
- the HTTP status mapping (200 / 503),
- the capability contracts plugins implement to contribute probes (`HealthChecker` for liveness, `ReadinessChecker` for readiness),
- the discovery semantics (auto-discovery from the module tree, explicit registrations win, keyed by plugin name),
- the per-probe timeout default,
- the pprof exposure posture (opt-in only).

## How

The protocol standardises the **wire contract** — paths, response shapes, status codes, discovery rules — not the authoring model. Each runtime maps its language-native plugin system onto these canonical shapes.

- Go's `go.putnami.dev/platform` exposes capability interfaces in `go.putnami.dev/app`
  (`HealthChecker`, `ReadinessChecker`) and walks the module tree to discover probes.
- TypeScript's `@putnami/platform` (or equivalent) implements the same envelope and
  discovers probes from the DI container.

Use this package when implementing a Putnami platform plugin, building tooling that polls operational endpoints (uptime checks, k8s probes, dashboard scrapers), or writing cross-runtime compliance tests.

### Canonical Endpoints

| Path | Method | Aggregates probes | Gated by running flag | Opt-in |
| ---- | ------ | ----------------- | --------------------- | ------ |
| `/livez` | GET | no | no | no |
| `/healthz` | GET | yes (`HealthChecker`) | yes | no |
| `/readyz` | GET | yes (`ReadinessChecker`) | yes | no |
| `/version` | GET | no | no | no |
| `/debug/pprof/*` | GET (POST for `/symbol`) | no | no | yes |

All endpoints mount under a configurable prefix (default empty). `NormalizePrefix` and `JoinPrefix` define the canonical normalisation: ASCII whitespace and slashes at either end are dropped together, then one leading slash is added. Empty, `/`, `//` and `/ /` become `""`; `_` becomes `/_`; `/admin/`, `//admin`, ` admin` and `/ admin` become `/admin`. Interior characters, a doubled slash included, are kept.

### Canonical Envelope

```json
{
  "status": "ok|unavailable|degraded",
  "checks": {
    "probe-name": "ok|<error message>"
  }
}
```

Rules:

- `unavailable` is emitted before `Start` / after `Stop` on `/healthz` and `/readyz`. The `checks` map MUST be empty — the runtime short-circuited before running probes.
- `ok` means every probe returned nil. Each `checks` entry MUST be exactly `"ok"`.
- `degraded` means at least one probe failed. The failing probe's `checks` entry is the failure's `error.Error()` string. The runtime MUST NOT wrap, redact, or rewrite the message — operators read it directly in alert pages.
- `/livez` uses the same envelope without the `checks` field. It returns `{"status":"ok"}` whenever the handler can run; it MUST NOT be gated by the running flag (k8s liveness probes that fail during normal startup or shutdown cause restart loops).
- `/version` returns a `VersionInfo` object with `omitempty` fields. Empty info is acceptable when no build metadata is available.

### HTTP Status Mapping

| Envelope status | HTTP code |
| --------------- | --------- |
| `ok` | 200 |
| `unavailable` | 503 |
| `degraded` | 503 |

Anything other than 200 or 503 on these endpoints is a contract violation.

### Required Guarantees

- Probes are discovered automatically by walking the runtime's module / DI tree; every plugin satisfying the capability interface is registered under its name.
- Explicit registration via the workload's API (e.g. `AddHealthChecker(name, fn)`) overrides any auto-discovered probe of the same name. Workloads use this to override built-in probes per environment without monkey-patching plugins.
- Probes are concurrency-safe and respect context cancellation. A per-probe timeout (default 5 seconds) caps how long a single probe can stall the endpoint.
- pprof is opt-in only. Runtimes MUST disable `/debug/pprof/*` unless the workload explicitly enables it.

### Producers and consumers

| Role | Implementation |
| --- | --- |
| Producer (Go) | `go.putnami.dev/platform` — mounts the canonical endpoints, walks the module tree for probes, and serves the envelope. |
| Producer (TypeScript) | `@putnami/application` (`platform()` plugin) — same endpoints and envelope, probes discovered from the DI container. |
| Consumer (operational) | Kubernetes probes, uptime checks, dashboards, and orchestrators poll the endpoints and read the status mapping. Nothing about them is Putnami-specific by design. |
| Consumer (in repo) | `protocols/capabilities` reuses this package's probe vocabulary for its health-contributor entries, so a capability manifest and a running probe name the same thing. |

### Current Conformance

| Runtime | Implementation | Status |
| ------- | -------------- | ------ |
| Go | `go.putnami.dev/platform` | ✅ |
| TypeScript | `@putnami/application` (`platform()` plugin) | ✅ |

Both runtimes validate against the *same* files: Go through
`conformance_test.go` and TypeScript through
`typescript/framework/application/test/platform/envelope-fixtures.test.ts`,
which reads [`fixtures/envelope/`](fixtures/envelope/) directly. Envelope drift
between the two therefore fails both suites rather than one.

### Versioning and Compatibility

`ProtocolVersion` is `1` and is pinned by `conformance_test.go`, so a bump is a
reviewed act. It is bumped by any backwards-incompatible change to the endpoint
paths, the response shapes, the status-code rules, or the capability semantics —
the same list `CanonicalPaths` and the envelope tests pin. Adding a behavioral
taxonomy label that reuses the existing envelope (see `platform.missing_probe`
below) is not a wire change and does not bump anything.

There is no JSON Schema for the envelope. The Go types, `strict.go`, and the
shared fixture corpus are the normative surface; both runtimes consume the
corpus directly, so a schema would be a third description to keep in sync rather
than a second source of truth. Publishing one remains open work if an external
validator ever needs it.

### Error Taxonomy

Tooling and compliance suites key off these codes. Do not invent new **wire-shape** codes — ones a strict validator emits, or that add a new status, path, or envelope field — in framework code without a protocol bump. A purely additive taxonomy label for behavior already expressed through the *existing* envelope (like `platform.missing_probe`, surfaced as an ordinary `degraded` `checks` entry) adds no new wire shape and needs no bump.

| Code | When |
| ---- | ---- |
| `platform.invalid_probe_name` | Probe name violates the canonical regex (`[a-z0-9][a-z0-9_./-]{0,63}`). |
| `platform.duplicate_probe` | Two probes registered under the same name in the same registry. |
| `platform.invalid_status` | Envelope `status` is not `ok`, `unavailable`, or `degraded`. |
| `platform.invalid_endpoint` | Endpoint spec violates the canonical contract (wrong path, wrong method, missing kind). |
| `platform.invalid_envelope` | Envelope payload violates shape rules (e.g. `degraded` with no failing probe). |
| `platform.invalid_capability` | Capability kind is not `health` or `readiness`. |
| `platform.invalid_prefix` | Prefix could not be normalised to the canonical form. |
| `platform.probe_timeout` | Probe exceeded the configured per-probe timeout. |
| `platform.probe_contract_broken` | Probe violated a runtime contract (panic, mutated request, etc.). |
| `platform.missing_probe` | A probe named in `required` was not registered or discovered. Behavioral taxonomy only — the runtime reports it through the existing `degraded` envelope (a synthesized failing `checks` entry keyed by the missing name), so no strict validator emits it, the wire shape is unchanged, and no protocol bump is required. |

### Non-Goals

- **Metrics exposition** — `/metrics` is deliberately not in this protocol and not on the roadmap for it. Putnami targets serverless runtimes first, where scrape-model endpoints don't fit: instances scale to zero, are not externally addressable, and live too briefly to register with a collector. Telemetry must push (OTLP/JSON over HTTP) to a collector that owns aggregation. A workload that wants ad-hoc metrics during local development can mount its own handler — the platform plugin does not. The reasoning and the rejected alternatives are recorded in [`doc/adr/0001-no-metrics-scrape-endpoint.md`](doc/adr/0001-no-metrics-scrape-endpoint.md).
- **Authentication / authorisation** of operational endpoints — workloads typically expose these on an admin port or behind a sidecar. Protecting them is out of scope.
- **Custom dashboarding / alerting** — workload concern.
- **Multi-tenant probe partitioning** — deliberately unmodelled until a workload needs it.

## Ownership, support, and evidence

**Owner** — the `protocols` standardization layer. `protocols/platform`
(`go.putnami.dev/protocol/platform`) is the single owning project: the endpoint
set, the envelope, and the status mapping change here first, and each runtime
follows.

**Support status** — `stable` in the workspace-root
[`putnami.support.json`](../../putnami.support.json) catalog, whose vocabulary
[`protocols/support`](../support/README.md) defines. Support status is a public
commitment, not a maturity stage.

**Evidence for that status**

- Two independent runtimes implement it and both validate against the *same*
  fixture corpus, not a copy: `conformance_test.go` (Go) and
  `typescript/framework/application/test/platform/envelope-fixtures.test.ts`
  (TypeScript).
- The conformance test pins the exact canonical path set, the status values, the
  status-code mapping, and `ProtocolVersion`, so widening any of them is a
  reviewed change rather than a silent one.
- The contract is what operators already automate against: liveness that is
  never gated on the running flag, readiness that is, and `503` for both
  `unavailable` and `degraded`.
- Known gap, recorded rather than hidden: there is no published JSON Schema (see
  *Versioning and Compatibility*). It does not weaken the commitment, because
  both implementations consume the corpus that would generate it.

**Owning user feature** — none, deliberately. Operational endpoints are a
property every workload gets by construction, not a capability a user selects;
declaring a product feature for them would claim an outcome no user asked for.
The user-visible outcomes that depend on this contract — a workload that reports
readiness truthfully during a deploy — belong to the workload's own features.
The durable decisions therefore live in [`doc/adr/`](doc/adr/) rather than in a
spec, which by contract details exactly one authored feature.
