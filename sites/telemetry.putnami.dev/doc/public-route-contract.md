# Public route contract — telemetry.putnami.dev

This workload serves the public hostname `telemetry.putnami.dev`. A hosting
platform can enforce default-deny routing on a public host: only the paths and
methods a workload declares reach the workload, and everything else terminates
at a shared reject backend.

This document is the workload's half of that contract: what it declares, and
what enforcing it changes. The platform owns the gateway, its route inventory
and the rollout.

## Ownership

| | |
|---|---|
| Public host | `telemetry.putnami.dev` (`infra/runtime.json` → `ingress.domains`) |
| Serving workload | `sites/telemetry.putnami.dev` |
| Canonical artifact | `schema/http-routes.json`, protocol `putnami.http-routes.v1` |
| Artifact producer | `go.putnami.dev/http` `ServerPlugin.Describe`; committed from the same describe the build runs |
| Provenance | every route is stamped `project: telemetry-receiver`, `package: go.putnami.dev/http`, `sourceKind: manual` |
| Release opt-in | `putnami.json` sets `@putnami/cloud.deploy.enforceHttpRoutes: true`; the platform decides when enforcement reaches the gateway |
| Platform auth | `security.platformAuth: "disabled"` — the host is genuinely public; the workload does its own authentication |

## Declared contract

| Path | Methods | Auth | Purpose |
|---|---|---|---|
| `/v1/logs` | `POST` | none — anonymous by design | OTLP/JSON CLI-usage ingest from the `putnami` CLI |
| `/v1/cli-usage/aggregate` | `GET`, `HEAD` | OIDC bearer: pinned issuer + audience + caller allowlist, fail-closed | private aggregate read for allowlisted internal readers |

Both routes are `match: exact` and `publicEdge: true`. Three `(path, method)`
rules total, so the shared URL map's rule, predicate, and configuration-size
limits are not a factor for this host. There is no prefix, template, catch-all,
SPA fallback, static mount, or OAuth callback surface — nothing on this workload
needs a wildcard rule.

`HEAD /v1/cli-usage/aggregate` is declared because the Go server answers HEAD
from the matching GET handler; the emitter records that pairing, and the
authenticated HEAD path is probed in the tests.

## Recorded behavior

- **Authentication.** Ingest is anonymous: no session, API key, or cookie, and
  the identity chain is route-scoped so a bearer token on `/v1/logs` is ignored
  and cannot drive JWKS fetches. The aggregate route denies every request unless
  audience, issuer, and caller allowlist are all configured.
- **CORS / preflight.** No CORS policy is installed. The framework synthesizes an
  `OPTIONS` answer for a known path with no `OPTIONS` route — `204` with an
  `Allow` header on `/v1/logs`, and `401` on the aggregate route because the
  scoped auth chain runs first. Neither answer carries an `Access-Control-*`
  header, so no browser flow exists to regress. `OPTIONS` is not declared and
  will be rejected at the edge.
- **Batching.** A drained CLI buffer is one request carrying many log records on
  the single declared path. Batching needs no additional route or method.
- **Content type.** The receiver does not branch on `Content-Type`, and an
  exact-match edge rule cannot inspect it. Requests are accepted with the OTLP
  content type, a foreign one, or none at all. No content-type predicate is
  available to tighten this contract.
- **Rejection.** Undeclared paths and methods already return `404` from the
  workload itself (wrong OTLP signals, wrong methods, the framework health path,
  the host root), so the edge reject is indistinguishable from today's answer.
- **Status contract.** `202` accepted (byte-identical whether the payload had
  content problems or the collector is down), `400` malformed, `413` oversize,
  `429` rate-limited. The receiver never redirects, which matters because the
  production sender refuses to follow redirects.

## What enforcement actually changes

Only four requests answer anything but `404` today and would start being
rejected at the edge. None is reachable by a production client:

| Request | Today | Why it is safe to reject |
|---|---|---|
| `OPTIONS /v1/logs` | `204` + `Allow` | synthesized preflight; no CORS policy, no browser client |
| `OPTIONS /v1/cli-usage/aggregate` | `401` | synthesized preflight, already refused |
| `POST /v1/logs/` | `202` | trailing-slash variant; the CLI sender builds `endpoint + "/v1/logs"` |
| `GET /v1/cli-usage/aggregate/` | `200` | trailing-slash variant; internal readers use the declared path |

The trailing-slash variants exist because `putnami.http-routes.v1` `exact`
matching is byte-for-byte (`/docs` and `/docs/` are distinct paths) while the Go
router splits a trailing slash into an empty final segment and still matches the
registered pattern. The variants are *not* an authentication hole: they resolve
to the registered pattern, and the route-scoped identity resolver and guard key
off that pattern, so an anonymous `GET /v1/cli-usage/aggregate/` is refused
exactly like the declared path. Both facts are pinned by tests.

The workload composes no health plugin, so no `/_/health` route is mounted and
none is declared. Adding `http.NewHealthPlugin()` to the application mounts the
route and describes it; the artifact and this contract must then declare it.

## Blockers

None. No callback, collector-protocol, wildcard, or provider-limit blocker
applies to this host: two exact routes, three rules, no browser client, no
static or SPA surface.

## Evidence and tests

`route_contract_test.go` pins every claim above and is the workload's evidence
for an enforcement rollout:

| Test | Claim |
|---|---|
| `TestCommittedRouteArtifactMatchesServedRoutes` | the committed artifact is byte-identical to what the server describes — completeness |
| `TestRouteArtifactIsIndependentOfDeployTimeConfig` | limits, collector endpoint, and aggregate-auth configuration cannot change the described contract |
| `TestCommittedRouteArtifactIsCanonical` | canonical ordering, digest, and round-trip idempotence — determinism |
| `TestCommittedRouteArtifactIsWorkloadOwned` | exact-match only, public-edge, workload provenance, rule budget |
| `TestDeclaredRoutesCarryRealTraffic` | every declared `(path, method)` carries real traffic — the golden CLI-usage payload and a signed caller token — through an edge that admits only the artifact |
| `TestUndeclaredRequestsNeverReachTheWorkload` | eleven undeclared probes return `404` and reach the workload zero times |
| `TestUndeclaredRequestsMeasureWhatEnforcementChanges` | the unenforced answer for each of those probes, so the table above cannot go stale |
| `TestTrailingSlashFormKeepsTheAuthenticationBoundary` | the variant form is authenticated like the declared path |
| `TestTelemetryProtocolBehaviorSurvivesEnforcement` | status matrix, batching, content-type independence, no redirects, no CORS headers |

The client side is pinned in `tooling/cli/internal/telemetry`:
`TestDrainHandsOffEncodedBufferBefore2xx` asserts the production sender uses
`POST` on `/v1/logs` with the OTLP content type, and
`TestDrainDropsHandedOffBufferOnCollectorFailure` asserts a non-2xx answer — the
shape a reject backend returns — stays fail-silent and never changes a CLI
command's result.

## Ownership split

This repository owns the artifact, its completeness and determinism, the
explicit `enforceHttpRoutes` release intent, and the tests above. The platform
owns the enforcement rollout: the URL-map preview and change, production probes,
the observation window, and rollback. Re-run the tests in this project before
that window. Any change to this workload's public surface must regenerate
`schema/http-routes.json` and update this document before the platform
reconciles its route matcher.
