# Putnami framework audit profile

The managed `/audit` skill reads this file before it scans
(`options["@putnami/intelligence"].audit.profile`). It states how the portable
rubric applies to this repository. It never changes the workflow itself.

This repository is a framework, not a service. Its users are experienced
developers who build with AI agents, and every exported symbol is a contract
with them. Read every property as "does the framework hold this bar, and does
it give the applications built on it this property by default".

The framework principles the properties cite:

1. Observable by construction: runtime behavior is visible by default.
2. Performance is a constraint: framework overhead is bounded and measurable.
3. Deterministic and reviewable behavior.
4. Security is foundational: secure by default, not by opt-in.
5. Data ownership is non-negotiable: schema changes are versioned and reversible.
6. Automation is a first-class user: humans and agents both use the framework,
   and documentation is an executable specification.

## Commands and preflight

- Run the CLI as `./putnamiw` (see `.agents/constraints.md`).
- When a TypeScript project is in scope, check that its dependencies are
  installed. When they are missing, run `./putnamiw deps install`; when that
  fails, stop and report.
- `sec-dependencies` needs `bun audit`. Probe it once before scanning.

## Labels

Role labels come from the project tags and the scope:

| Source | Label |
| --- | --- |
| tag `extension` | `role/extension` |
| tag `web` | `role/site` |
| tag `sample` or `e2e` | `role/sample` |
| tag `protocol` | `role/framework` |
| scope `tooling`, no other role tag | `role/tooling` |
| scope `go`, `typescript`, `python` or `protocols`, no extension or sample tag | `role/framework` |
| scope `sites`, no other role tag | `role/site` |

Issue types: a `group/security` finding gets `type/bug`. Every other finding,
the language issue, a graduation issue and a prune umbrella issue get
`type/task`.

## Filing rules

- `.agents/constraints.md` forbids filing a tooling or process issue without
  the user's approval. A step 6 graduation issue is drafted in the wave summary
  and filed only once the user approves it. An unattended run leaves the draft
  for the next interactive session.
- `--prune`: set `PUTNAMI_CONSUMER_REPOS` to the downstream checkouts. Run one
  orchestrator session per (scope, typology) on the `tooling` and
  `typescript` scopes. Worker tiers follow the model equivalence in
  `.agents/constraints.md`. Prune commit messages follow decisions D-002 and
  D-003 in `decisions.json`.

## Properties in this repository

The repository checks script (`.agents/audit/checks.sh`) emits
`test-contracts`, `design-errors`, `design-errors-fmt-errorf`, `sec-timeout`
and `design-stability` findings. Triage them like `mechanical.sh` output.

### security

- `sec-authz-enforcement`: routes that handle sensitive data apply the
  framework's security middleware. A handler does not re-implement token
  validation. A route without an explicit auth configuration fails closed.
- `sec-timeout` (added): every I/O operation has a bounded timeout. HTTP
  servers apply their read and write timeouts (verify the Go `ServerConfig`
  defaults are applied). HTTP clients set request and connection timeouts.
  Database access sets statement timeouts and pool limits. A missing timeout on
  any I/O operation: severity high.
- `sec-injection`: the Go `database` package uses `$1` placeholders for every
  query; the TypeScript query builder cannot emit raw SQL from input.
- `sec-validation`: request bodies are validated with the runtime schema
  system; check type coercion (string to number, array to string) and
  unvalidated path parameters, query parameters and headers.
- `sec-defaults`: Go enforces a 1 MiB request body limit by default; verify the
  TypeScript equivalent. Check preflight handling without CORS middleware,
  `X-Content-Type-Options: nosniff`, a default Content-Security-Policy on
  `@putnami/web` server rendering, and `HttpOnly`, `Secure` and `SameSite` on
  session cookies. An insecure default that users must override: severity
  high.
- `sec-dependencies` (added, TypeScript only): run `bun audit` on packages
  with runtime dependencies. Each dependency is an attack surface; flag deep
  transitive trees. Go framework packages use the standard library only.
- `sec-serialization`: also check template injection in server rendering.

### testing

- `test-coverage`: every exported symbol of a framework package has at least
  one test of its primary use. Coverage below 60 % on a framework package:
  severity high.
- `test-contracts`: every exported type, function and interface has tests for
  normal input, edge input and error returns. In Go, exported functions have
  test functions; in TypeScript, symbols exported from a barrel `index.ts`
  have test files. Breaking a public contract without a failing test: severity
  critical.
- `test-integration`: when module A depends on module B, a test exercises A
  through B's interface without mocking B. `go/samples/` and
  `typescript/samples/` carry the integration coverage.

### performance

- `perf-bundle` (added, `@putnami/web` and `@putnami/ui` only): barrel
  re-exports that defeat tree shaking, server-only code in client bundles,
  avoidable large runtime dependencies, and missing `browser`/`default`
  conditional exports.
- `perf-hot-path`: router lookup allocates no map or slice per request; DI
  resolution in handlers reuses cached singletons.
- `perf-startup`: framework cold start is developer experience and serverless
  latency. Check Go `init()` functions and TypeScript top-level await or
  synchronous reads at import time.

### design

The workload properties do not apply here: `design-thin-workload`,
`design-route-registration`, `design-provider-ownership` and
`design-retired-roots`. In their place:

- `design-api-surface` (added): every exported symbol is intentional. Check
  barrel re-exports, Go exports in internal packages, and symbols exported only
  for tests (use a `_test` package in Go, a `/testing` subpath export in
  TypeScript).
- `design-coupling` (added): import cycles and upward dependencies.
  Framework modules depend downward (application, then runtime, then
  utilities).
- `design-patterns` (added): plugins share one lifecycle (Go
  Generator/Warmer/Starter/Stopper; TypeScript generate/warmup/start/stop);
  repositories share one interface shape; constructors are named alike (Go
  `New<Type>`). Inconsistent patterns between similar modules: severity medium.
- `design-abstractions` (added): interfaces with one implementation,
  abstract base classes where the plugin system composes, implementation
  details visible through the public API.
- `design-extensibility` (added): users add middleware, routes and DI
  bindings without forking; a type switch that interface dispatch should
  replace. Sealed behavior where extension is expected: severity medium.
- `design-consistency` (added): modules with the same architectural role
  share naming, configuration loading, error handling and signatures.
  Inconsistency: severity medium.
- `design-types`: phantom types (`__brand`, `__type`) carry a doc comment. An
  unjustified type escape in framework code: severity high.
- `design-errors`: Go returns the structured `errors.Error`, never bare
  `fmt.Errorf` (`design-errors-fmt-errorf` from the checks script). Diagnostic
  codes are namespaced `ErrorCode*` constants. An error that says "invalid
  input" without naming the input: severity high.
- `design-stability`: a public API does not break without a major version.
  Check removed Go exports and removed `package.json` `exports` entries in
  recent diffs, `@internal`/`@experimental` on unstable APIs, and migration
  guidance on deprecated APIs. An unannounced breaking change: severity
  critical.

### operations

The deployment properties do not apply here: `ops-deploy-idempotency` and
`ops-provisioning-order`. `ops-cold-start` is covered by `perf-startup`. The
other properties ask what the framework gives applications by default:

- `ops-logging`: framework code uses `@putnami/utils` logger or
  `go.putnami.dev/logger`, with a level that fits the message.
- `ops-errors`: Go wraps with `errors.Wrap(err, code)` instead of a bare
  `return err`; TypeScript passes `{ cause: err }`.
- `ops-metrics`: HTTP handler time, request count and error rate, database
  query time and pool use, and DI resolution time. Go emits them with
  `emit.metric()`.
- `ops-tracing`: middleware creates spans, database queries are traced with
  the sanitized query as span name, and TypeScript async context survives DI
  scope resolution.
- `ops-config-resolution`: ports, hosts and timeouts come from configuration
  structs; no boolean feature constants and no `if env === 'production'`
  branches.
- `ops-health`: the health endpoint checks downstream dependencies, readiness
  reflects serving capability, and both answer a structured body.
- `ops-versioning` (added): `.gen/version.json` exists and is current, and
  the version is exposed at runtime (health endpoint, startup log).

### developer-experience

`dx-runbook` and `dx-config-schema` do not apply here. In their place:

- `dx-doc-coverage`: more than 10 % of a framework package's exported
  symbols without a doc comment: severity high.
- `dx-examples` (added): each key API has a runnable example in `doc/`, the
  samples exercise the framework end to end, and example imports match the
  current exports. A stale example that does not compile: severity high.
- `dx-getting-started` (added): each framework package's `doc/` has a
  complete minimal example that runs when copied, enough for an agent to build
  a working app without reading the source.
- `dx-migration` (added): a breaking change has a migration section or a
  changelog entry; `@deprecated` names a concrete replacement.
- `dx-site` (added): `sites/putnami.dev/doc/` covers the project's features.
  A new feature without a site page: severity medium.
- `dx-discoverability` (added): types document themselves (fields carry doc
  comments), type aliases name domain concepts, and the export list shows what
  the module can do.
- `dx-predictability` (added): similar modules take options the same way
  (configuration struct, functional options or chaining: one per language).
  Severity low per finding.
- `language`: on. `.agents/constraints.md` requires English everywhere.
