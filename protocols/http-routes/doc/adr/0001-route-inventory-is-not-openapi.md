# ADR 0001 — Route reachability is its own fail-closed inventory, not OpenAPI

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/http-routes` (`protocols/http-routes`)

## Context

A deny-by-default deployment edge needs to know exactly which paths and methods
of a workload are publicly reachable. Too narrow breaks the application; too
wide publishes a private route.

OpenAPI covers typed API endpoints only, not file-system web routes, static
mounts, or public files, and it states no reachability. Router introspection is
per-framework, so the edge would reimplement Go and TypeScript matching. A
hand-written allowlist drifts. Routers also accept `*`, regex groups, or
optional parameters the edge cannot reproduce.

## Decision

The framework emits `putnami.http-routes.v1`, a canonical, digest-addressed
artifact at `schema/http-routes.json`.

1. **Reachability is declared.** `publicEdge` is required on every route, even
   when false, so a consumer can detect a public prefix or template that would
   admit a private route.
2. **The match language is smaller than any router:** exact, segment template
   with at most one `{name...}` catch-all, and non-root prefix. Producers
   convert native syntax (`:id`, `[id]`, `[...path]`) and report
   `http_routes.unsupported_pattern` for anything they cannot represent exactly.
3. **Ambiguity fails closed.** Overlapping routes with intersecting methods and
   different visibility are an error, computed on segment boundaries and
   over-approximating for catch-alls. Consumers never apply part of an invalid
   inventory.
4. **The root prefix `/` is forbidden**, so a static mount cannot imply a
   public `/*`.
5. **OpenAPI is not an input.** It may serve as a parity check for typed API
   endpoints.
6. **Versioning.** Changing matching, canonicalization, digest semantics, a
   required field, or a closed enum needs a new major identifier and schema
   URL. An additive extension keeps every previously valid manifest
   byte-identical.

## Rejected alternatives

- **Allowlist from OpenAPI.** One route source of five, with no reachability.
- **Ship router patterns to the edge.** Exports two languages' matching
  semantics, wildcards and regex included.
- **Visibility by path convention** (`/internal/*`). One renamed prefix
  publishes a private route.
- **Allow a root `/` prefix.** One mount collapses the inventory into "allow
  everything".
- **Apply the valid subset.** A partial allowlist fails silently.

## Consequences

- A pattern the grammar cannot express must be expanded into exact or template
  routes, or the build fails.
- The canonical digest and cross-language golden catch drift in tests.
- A route not marked public breaks reachability rather than leaking. That is
  the intended direction of failure.
