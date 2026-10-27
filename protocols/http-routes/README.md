# HTTP Routes Protocol

`putnami.http-routes.v1` is the provider-neutral, deterministic inventory of
application routes that may or may not be reachable through a public HTTP edge.
Frameworks emit it; deployment systems consume it to construct an allowlist.

OpenAPI is deliberately **not** an input to this protocol. It can be used as a
parity check for typed API endpoints, but it cannot describe file-system web
routes, static build outputs, or exact public files. That decision and the
fail-closed posture behind it are recorded in
[`doc/adr/0001-route-inventory-is-not-openapi.md`](doc/adr/0001-route-inventory-is-not-openapi.md).

The normative artifacts are the JSON Schema
[`schemas/http-routes.json`](schemas/http-routes.json) (published at its `$id`,
`https://putnami.dev/schemas/putnami-http-routes-v1.json`) and the shared
corpus under [`fixtures/`](fixtures/).

## Producers and consumers

| Role | Implementation |
| --- | --- |
| Producer (Go) | `go.putnami.dev/http` — `ServerPlugin.Describe` canonicalizes typed API, manual, and static-mount routes into `schema/http-routes.json` (`go/framework/http/http_routes.go`). |
| Producer (TypeScript) | `@putnami/application` and `@putnami/web` emit per-project route fragments; the TypeScript extension aggregates them into `.gen/schema/http-routes.json` (`typescript/extension/internal/build/http_routes.go`) and commits that file verbatim to the same `schema/http-routes.json` (`typescript/extension/cmd/putnami-ts/build_generate.go`). |
| Consumer (in repo) | `sites/telemetry.putnami.dev` pins its committed inventory against what the running server describes, then proves every undeclared path and method is rejected. |
| Consumer (out of repo) | The deployment edge that builds a default-deny allowlist from the artifact. Ingestion and reconciliation stay outside this contract. |

A producer emits the whole inventory or none of it: `publicEdge` is required
even when false, so a consumer can prove a public prefix or template never
admits a private route.

## Wire shape

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-http-routes-v1.json",
  "protocol": "putnami.http-routes.v1",
  "routes": [
    {
      "match": "template",
      "path": "/users/{userId}",
      "methods": ["GET"],
      "publicEdge": true,
      "provenance": {
        "project": "example/web",
        "package": "@putnami/application",
        "sourceKind": "typed-api",
        "evidencePath": "src/api/users/[userId]/get.ts"
      }
    }
  ],
  "digest": "sha256:..."
}
```

`publicEdge` is required even when false. A complete inventory lets a validator
detect that a public prefix or template would accidentally admit a private
route. Provenance source kinds are closed: `typed-api`, `file-route`,
`static-mount`, `public-file`, and `manual`.

## Pages and actions

A consumer tells a file-system page from its action with `provenance.sourceKind`
and `methods` alone:

- A `file-route` route whose methods are `GET` and `HEAD` is a **page**.
- A `file-route` route whose methods are `POST`, at the same `match` and `path`
  as a page, is that **page's action**.

No other source kind is a page or an action, whatever its methods. The rule
adds no field and changes no version: it reads fields every v1 inventory
already carries.

`evidencePath` corroborates the rule and does not decide it. A page file can
produce several routes, because a finite static catch-all expands into one
`exact` route per concrete path, so a consumer that counts page files counts
the distinct `evidencePath` values of page routes, not the page routes. The
TypeScript web producer sets `evidencePath` on every `file-route`: the page
file for a page, and the `action.ts` beside it for an action.

## Match semantics and conversion

The three match kinds have intentionally narrow semantics:

- `exact` matches one path byte-for-byte. A trailing slash is significant, so
  `/docs` and `/docs/` are distinct exact paths.
- `template` matches segments against a template. A parameter is one complete
  `{name}` segment consuming exactly one non-empty segment and never `/`. A path
  may also carry **at most one** catch-all segment written `{name...}`, which
  matches one or more complete segments (slashes included) at a single position,
  optionally bounded by fixed or `{name}` segments before and after it (for
  example `/{module...}/@v/list`). Parameter and catch-all names match
  `[A-Za-z_][A-Za-z0-9_]*`; their names do not affect overlap or duplicate
  detection. Two catch-alls in one path, a catch-all outside a template match, or
  a catch-all that is not a complete segment are `http_routes.unsupported_pattern`.
- `prefix` is a byte prefix ending in `/`. `/assets/` owns only that subtree;
  `/assets` does not imply `/assets/`. The root prefix `/` is forbidden, so a
  static mount can never imply `/*`. This ban is unchanged: a catch-all is a
  per-route declaration a developer writes literally (`{name...}`), so unlike a
  root prefix it never arises implicitly and needs no extra opt-in gate.

Framework converters must translate native named-segment syntax (for example
`:id` or `[id]`) to `{id}`, and a named catch-all (for example `{path...}` or
`[...path]`) to `{path...}`. Only genuinely unrepresentable patterns still fail
closed: anonymous wildcards (`*`), regex groups, and optional/repeated
parameters. A framework must expand any other finite pattern into exact/template
routes or report `http_routes.unsupported_pattern`.

Paths are absolute and preserve a single trailing slash. Repeated separators,
dot segments, query strings, fragments, backslashes, control characters, and
raw non-ASCII bytes are invalid. Percent escapes use uppercase hex. Escaped
`/`, `\\`, `%`, `?`, `#`, controls, and unreserved characters are rejected to
avoid disagreement between proxies and runtimes; non-ASCII UTF-8 bytes and
other reserved data may be represented with canonical uppercase escapes.

Methods are a non-empty, duplicate-free subset of `DELETE`, `GET`, `HEAD`,
`OPTIONS`, `PATCH`, `POST`, and `PUT`. Converters uppercase native method names
before validation. `GET` does not implicitly add `HEAD`; an emitter adds both
only when the application actually serves both.

## Overlaps and fail-closed validation

Routes with the same match language and an intersecting method set are
duplicates. Template parameter names are ignored for this check, so
`/users/{id}` and `/users/{name}` are duplicates.

Different match languages may overlap when their methods and `publicEdge`
values agree; normal router precedence still applies inside the application.
If their methods intersect and their visibility differs, validation fails with
`http_routes.visibility_overlap`. This prevents a public `/assets/` prefix or
`/{tenant}/settings` template from widening access to a private route.

A catch-all over-approximates: it overlaps any route whose fixed segments are
consistent with the catch-all's own fixed prefix and suffix around the one-or-
more segments it absorbs. Overlap is computed on segment boundaries, never by
byte prefix, and it is deliberately conservative — a public catch-all can never
silently coexist with a private route it could match. Two routes are provably
disjoint (and so allowed) only when a bounding fixed segment differs or the
other route is too short for the catch-all to absorb a segment: a public
`/{module...}` flags a private `/internal/health`, while a public
`/{module...}/@v/list` stays disjoint from a private `/internal/health` and any
private route whose `@v/list` tail cannot be reproduced.

Validation emits stable, field-addressed diagnostic codes. Unknown fields,
unknown protocol identifiers, ambiguous patterns, unsafe escaping, duplicate
routes, and digest mismatches are all errors; consumers must not partially
apply an invalid inventory.

## Canonical serialization and digest

Canonicalization performs these steps:

1. stamp the canonical schema and protocol identifiers;
2. uppercase and lexicographically sort methods;
3. sort routes by semantic path (single-segment parameters replaced by `{}` and
   catch-alls by the distinct token `{...}`), match specificity (`exact`,
   `template`, `prefix`), original path, methods, visibility, and provenance —
   catch-all templates keep the `template` rank and order among their peers by
   this semantic path;
4. serialize `{ "protocol", "routes" }` with fields in declared order,
   two-space indentation, UTF-8, and one trailing newline;
5. set `digest` to `sha256:` plus the lowercase SHA-256 hex of those bytes; and
6. serialize the full manifest with the same formatting.

The schema URI and digest field are excluded from the digest projection.
Equivalent inventories therefore serialize byte-identically and produce the
same digest regardless of input route or method order.

The golden under `fixtures/equivalence/` pins the exact cross-language byte
form. `fixtures/valid/`, `fixtures/invalid/`, and `fixtures/expectations.json`
are the shared Go/TypeScript conformance corpus.

## Versioning and compatibility

The protocol identifier contains the major version. V1 consumers accept only
`putnami.http-routes.v1` and fail closed on any other identifier. Adding an
optional provenance field or a diagnostic code may be compatible; changing
matching, canonicalization, digest semantics, a required field, or a closed enum
requires a new major identifier and schema URL. Producers may emit only the
major version they implement.

The `{name...}` catch-all was added to the `template` grammar as a fail-closed
forward extension, not a new major. It grows no closed enum: the `match` set
stays `{exact, template, prefix}`. Every manifest that validated before still
validates to the same canonical bytes and digest — no previously-valid manifest
changes meaning. A pre-extension consumer rejects the new shape outright (it
reads `{name...}` as `http_routes.unsupported_pattern` and refuses the whole
inventory), which fails safe toward deny: an old edge denies module traffic
rather than admitting an unvalidated route.

## Non-goals

- framework-specific route discovery;
- OpenAPI generation or changes to existing route behavior;
- cloud release ingestion or load-balancer reconciliation; and
- provider resources, backend names, security policies, or deployment state.

## Ownership, support, and evidence

**Owner** — the `protocols` standardization layer. `protocols/http-routes`
(`go.putnami.dev/protocol/http-routes`) is the single owning project: the wire,
the schema, and the corpus change here first and every implementation follows.

**Support status** — `stable` in the workspace-root
[`putnami.support.json`](../../putnami.support.json) catalog, whose vocabulary
[`protocols/support`](../support/README.md) defines. Support status is a public
commitment, not a maturity stage.

**Evidence for that status**

- Two independent implementations run *this* corpus, not a copy: Go
  (`conformance_test.go`) and TypeScript
  (`typescript/framework/application/test/http-routes/conformance.test.ts`,
  which reads `protocols/http-routes/fixtures`).
- Canonical bytes and the digest are pinned across languages by
  [`fixtures/equivalence/http-routes.golden.json`](fixtures/equivalence/http-routes.golden.json).
- `version_test.go` pins `putnami.http-routes.v1` and the schema URL together;
  `drift_test.go` pins the Go types against the published schema's field set.
- A deployed workload consumes it: `sites/telemetry.putnami.dev` asserts the
  committed artifact matches the running server and that undeclared paths and
  methods are rejected.
- The one grammar extension shipped so far (`{name...}`) left every
  previously-valid manifest byte-identical, and a pre-extension consumer fails
  closed — the compatibility policy above is enforced, not aspirational.

## Feature and spec

The module owns the feature of its contract, `http-routes/route-inventory-contract`,
specified in [`specs/route-inventory-contract.json`](specs/route-inventory-contract.json).
The tests here prove its requirements, and so do four implementations: the Go
HTTP server (`go/framework/http`), whose `Describe` emits the inventory;
`@putnami/application`, whose TypeScript validator and serializer replay the
corpus and the golden; `@putnami/web`, whose page, action and asset routes pass
the strict parser; and the TypeScript extension (`typescript/extension`), which
merges the framework fragments into it. The workloads that declare routes keep the features of what
they deliver. The design intent lives in
[`doc/adr/0001-route-inventory-is-not-openapi.md`](doc/adr/0001-route-inventory-is-not-openapi.md).
