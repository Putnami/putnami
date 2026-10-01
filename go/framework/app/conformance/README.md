# Health-Probe Conformance Pack

This pack certifies the **liveness / readiness / version** health contract every
Putnami workload exposes, so a downstream can run the health contract as a pack.
It pins the current shared envelope and required-readiness contract.

The contract has two halves, split by the module boundary:

- **Probe machinery (this module, `go.putnami.dev/app`)** — probe discovery, the
  liveness/readiness registry split, verbatim probe-error propagation, the
  DI-injected probe fail-closed rule, and the capability projection the emitter
  writes (`HealthChecker` → probe `health` → `/healthz`; `ReadinessChecker` →
  probe `readiness` → `/readyz`, the wiring in
  [`../capabilities.go`](../capabilities.go)). The Go runner certifies this half.
- **HTTP response surface** — the `/livez`, `/healthz`, `/readyz`, `/version`
  envelopes and status codes. This is served by `go.putnami.dev/platform` (a
  dependent of this module, which the Go runner cannot import without a cycle) and
  by `@putnami/application`'s platform plugin. The **TypeScript runner**
  (`@putnami/application/conformance`) certifies it end-to-end over real HTTP —
  including required-readiness — and `go.putnami.dev/protocol/platform`'s
  own conformance pins the shared envelope. Together the two halves certify the
  whole contract.

## Runners

The machinery is **exported** so a downstream opts in with one committed line
instead of copying assertions:

- **Go:** `go.putnami.dev/app/conformance` — call `conformance.Run(t)` from a test
  (`go/framework/app/health_conformance_test.go` is that thin caller). It asserts
  probe discovery, the registry split, verbatim errors, the injected-probe
  fail-closed rule, and the probe-kind projection through the real describe
  emitter.
- **TypeScript:** `@putnami/application/conformance` — call
  `registerHealthConformanceTests()` from a `bun:test` file
  (`typescript/framework/application/test/conformance/health.test.ts` is that thin
  caller). It boots a real app with `platform()` + `http()` and asserts every
  `/livez` / `/healthz` / `/readyz` / `/version` response — status code, envelope
  shape, `checks` map, version fields, and required-readiness — against the
  protocol's own `validateEnvelope`.

Both `conformance` packages deliberately import their test framework (`testing` /
`bun:test`) in non-test source: they exist to be called by a downstream project's
own test binary, matching the database conformance-pack convention. This is a **pure**
pack — no external service, no skip gate — so it runs in the normal unit gate.

## Pack manifest

[`pack.json`](./pack.json) follows the committed **pack-manifest convention**:
a minimal, forward-stable descriptor a project references (by `id`) to declare it
runs this pack. [`pack_test.go`](./pack_test.go) strict-parses it, pins the id and
languages, and checks every `capabilityKinds` value against the capabilities
vocabulary.

| Field | Meaning |
|-------|---------|
| `id` | Stable pack identifier (`putnami.health.conformance`). Projects reference and aggregate a pack by this id. |
| `corpus` | **Optional**. Omitted here: the health pack certifies in-process behavior, not a committed corpus file. |
| `capabilityKinds` | The [`protocols/capabilities`](../../../../protocols/capabilities) probe kinds this pack certifies: `["health", "liveness", "readiness"]` — `/healthz`, `/livez`, and `/readyz` respectively. |
| `languages` | The runtimes with an exported runner for the pack (`["go", "typescript"]`). |

The shape is a strict superset of the transaction pack: `corpus` becomes
optional and forward-compatible so a pure behavioral pack omits it, while
aggregation by `id` is unaffected.
