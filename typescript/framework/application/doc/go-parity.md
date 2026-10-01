# TypeScript ↔ Go Parity

How the TypeScript `@putnami/application` platform, security, and endpoint capabilities map to their Go framework counterparts.

## Overview

Putnami keeps the two runtimes behaviourally aligned: an app written against the TypeScript framework and one written against the Go framework should present the same operational surface (platform endpoints, auth decisions, OpenAPI security, client resilience) so a single control plane can treat them interchangeably.

This page is the acceptance-criterion map for that alignment. Each row lists a TypeScript capability, its Go behavioural counterpart, and any **intentional** divergence. Only parity that has actually shipped is claimed here.

## Capability Map

| Capability | TypeScript (`@putnami/application`) | Go counterpart | Notes / intentional divergence |
| --- | --- | --- | --- |
| Platform endpoints, health & readiness (incl. `required`) | `src/platform/` (`platform.plugin.ts`, `protocol.ts`, `checker.ts`) | `go/framework/platform` (`plugin.go`), `protocols/platform` | Both derive the mandatory operational path set and probe behaviour from the shared `protocols/platform` contract; readiness aggregates registered checks and honours the `required` flag on each. |
| Composable auth strategies (bearer-JWT / API-key / introspection, `anyOf` / `allOf`) | `src/security/` strategies + `.secure()` guard composition | `go/framework/security/{jwt,apikey,introspect}.go` | Same strategy set and the same `anyOf` (OR) / `allOf` (AND) composition semantics; validated tokens surface a common principal shape. |
| Route policy + decision telemetry + `AuthDecision` | `src/security/` route policy + auth-decision telemetry | `go/framework/security/observe.go`, `protocols/identity` `AuthDecision` | Both emit an `AuthDecision` (allow/deny + reason) as structured telemetry, keyed to the shared `protocols/identity` vocabulary so the Identity dogfood reads one schema across runtimes. |
| OpenAPI security schemes | `src/openapi/openapi.ts` (`buildSecurityRequirements` / `requiredSchemes`) | Go OpenAPI security derivation | Both derive `components.securitySchemes` and per-operation `security` requirements from route security metadata; the default api-key scheme uses the same `X-Api-Key` header as `go/framework/security/apikey.go`. |
| Client 32 MiB response cap + retry telemetry | sibling **`@putnami/client`** `src/runtime/` (`http-transport.ts`, `connect-transport.ts`, `response-cap.ts`, `retry.ts`) | `go/framework/client` transports (`http_transport.go`, `connect_transport.go`, `circuit_breaker.go`) | Ships in the `@putnami/client` package, not `@putnami/application`. Both cap buffered responses at 32 MiB and emit retry/circuit-breaker telemetry; the Go client is the reference implementation the TS client mirrors. |
| Rate-limit trusted-proxy CIDR | `src/http/rate-limit.middleware.ts` | `go/framework/http/middleware_ratelimit.go` | Both resolve the client IP from `X-Forwarded-For` only when the immediate peer is a trusted proxy (CIDR allow-list), so the forwarded chain cannot be spoofed to evade limits. |
| Typed request headers | `.headers(schema)` → `ctx.headerParams()` (validated, coerced) + OpenAPI `in: "header"` | Go endpoint header binding/validation | Same validate-and-coerce contract as `.params()`/`.query()`. **Divergence:** TS exposes the validated record on `ctx.headerParams()` and leaves the raw `ctx.headers` `Headers` object untouched (Go binds into a typed struct). |
| Per-endpoint CSRF exemption | `.csrfExempt()` sets the route's existing `csrfExempt` flag | Go endpoint / CSRF middleware exemption | Both are opt-in per endpoint and scoped to a single route — never widening exemption to siblings. Sugar over the existing route flag, not a new CSRF code path. |

## Intentional Divergences

- **No TypeScript `pprof` handler.** The Go platform plugin serves `/debug/pprof` (`go/framework/platform/pprof.go`). The TypeScript platform only references the `pprof` path prefix in the shared protocol and keeps it excluded/opt-in — there is no TS profiling endpoint. This divergence is deliberate: runtime profiling is a Go-runtime concern.
- **Typed-header surface.** TypeScript keeps the native `Headers` object on `ctx.headers` and publishes the validated view on `ctx.headerParams()`, rather than replacing `ctx.headers`, so content negotiation and header-reading middleware keep the standard `Headers` API.

## See Also

- [Endpoint Builder](endpoint-builder.md) — `.headers()`, `.csrfExempt()`, and the rest of the fluent builder
- [Security](security.md) — auth strategies, route policy, and OpenAPI security schemes
- [Health & Readiness](health.md) — platform probe endpoints
