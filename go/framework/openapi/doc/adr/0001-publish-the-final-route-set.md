# ADR 0001 — Publish the final route set, and do not let caches keep it

- **Status**: accepted
- **Scope**: `go.putnami.dev/openapi` (`go/framework/openapi`)

## Context

Plugins configure in registration order, and other plugins register endpoints
on the shared `api.Plugin` during their own `Configure`. A document rendered
once at the OpenAPI plugin's `Configure` holds only the routes registered
before it. The document is also security-relevant: it lists internal paths and
required roles and scopes, and `RegisterOn` serves it as an unauthenticated
GET.

## Decision

The plugin re-renders from the current route set at every observation point:
`Configure` (so `Spec()` works), `Start`, `Describe`, `OpenAPISpecJSON`, and the
first runtime request after configure. It never keeps a permanent snapshot.

Each render produces one canonical JSON byte sequence, reused by the in-memory
`SpecSource`, describe artifacts and their gzipped companion, and the runtime
handler. Object keys are stable, generated structural arrays are normalized,
and the document ends with one newline. A formatting-only publication
therefore cannot change the contract hash embedded in a client. Merging
another producer or normalizing provider values is a separate, semantic step.

The client generator reads the document through `SpecSource` in memory, never
from `schema/openapi.json`.

The response carries `Cache-Control: no-store` by default. A workload that
wants the document public and cacheable sets `CacheControl`; one that wants it
private puts the route behind auth middleware.

## Rejected alternatives

- **Render once at `Configure`.** The document depends on plugin order.
- **Require the OpenAPI plugin to be last.** Invisible until wrong, and two
  plugins cannot both be last.
- **Watch the api plugin for changes.** More machinery, and it still needs a
  point where the route set is final.
- **Cacheable by default.** A CDN keeps serving the document after the route
  moves behind auth.
- **Generate from the file on disk.** It depends on describe ordering and a
  file that may be stale or absent.

## Consequences

- Rendering happens several times per process, bounded by route count, never
  per request.
- A route registered after the first served request does not appear until the
  document is invalidated; registration is a wiring-time activity.
- Publishing the document as a public artifact is an explicit `CacheControl`
  line in the workload.
