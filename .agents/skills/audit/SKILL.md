---
name: audit
description: Scan projects for quality issues — create, update, and close GitHub issues; update scorecards. --prune removes dead weight per domain in one umbrella issue and one batch PR
---

# Audit

Scan projects for quality findings. Each finding becomes a GitHub issue with structured labels. Existing issues are updated or closed when findings change. Scorecards are refreshed at the end.

## Portable host contract

This is the canonical workflow for both Claude Code and Codex. Treat `/name`
references below as logical skill invocations: use `/name` on Claude Code and
`$name` on Codex. Use host-native tools with the stated capability rather than
requiring the literal Claude tool name. Deterministic helpers live under this
skill's `scripts/` directory.

## Arguments

| Flag | Scope |
|------|-------|
| `--all` | Full workspace audit (all domains, all groups) |
| `--domain <name>` | One domain (e.g. `go`, `typescript`) |
| `--project <name>` | One project (e.g. `go.putnami.dev/database`) |
| `--group <group>` | One property group across all projects |
| `--impacted` | Only projects with changes vs. main (default when no flag) |
| `--reconcile` | Verify open issues only — close resolved ones with commit/PR attribution |
| `--scorecard-only` | Skip scanning; only refresh scorecards from existing open issues |
| `--skip-scorecard` | Scan and create/update issues, but do not refresh scorecards |
| `--no-cache` | Ignore attestations — re-scan (project, group) pairs even when their content is unchanged |
| `--dry-run` | Print findings without creating/updating issues |
| `--prune` | Remove weight instead of filing findings: dead code, duplicates, dual versions, palliatives, historical references, justifying comments, test scaffolding, hidden config, stale docs. One umbrella issue and one batch PR per domain. See [Prune mode](#prune-mode). |
| `--typology <t>` | Prune only: restrict to one typology (`dead`, `duplicate`, `multi-version`, `palliative`, `historical-ref`, `comment`, `test-scaffold`, `config-surface`, `doc`) |
| `--apply` | Prune only: apply the safe removals and push the batch branch. Without it, prune scans and updates the umbrella issue only. |

Flags combine: `--group security --domain go` audits security in Go projects only.

**Prune mode** (`--prune`): none of the issue-per-finding lifecycle below applies. No fingerprints, no attestations, no scorecards. Jump to [Prune mode](#prune-mode).

**Reconcile mode** (`--reconcile`): Instead of scanning for new findings, only checks whether existing open issues are still valid. Resolved issues are closed with a comment attributing the fixing commit or PR. Combines with `--domain`, `--project`, `--group` to filter which issues to check. `--dry-run` prints what would be closed without acting.

**Scorecard-only mode** (`--scorecard-only`): Skip scanning entirely; run `bash .agents/skills/audit/scripts/scorecard.sh` (add `--domain <d>` to refresh a single domain) and exit. Scorecards are deterministic artifacts rendered from open issues — no model judgment involved. `fleet.sh` runs the script directly as its post-pass, and `/fix-loop` runs it when its backlog loop ends.

**Skip-scorecard** (`--skip-scorecard`): Run the full scan and issue lifecycle, but leave the scorecard refresh to someone else. Intended for parallel shards (see [Parallel runs](#parallel-runs)): the fleet's post-pass runs `scorecard.sh` once after all shards exit.

## Steps

### 1. Resolve scope

1. List the domains and their projects with `bash .agents/skills/audit/scripts/domains.sh` (`list` for names, `paths <domain>` for one domain's project paths). A domain is a top-level directory whose `putnami.json` declares the scope schema (`https://putnami.dev/schemas/putnami-scope.json`); its projects are that scope's `includes`. Every domain uses one **group priority order**: security, testing, design, performance, developer-experience, operations.
2. Determine which (domain, project, group) triples to audit based on arguments.
3. For `--impacted`: run `git diff --name-only main...HEAD` and match changed paths to projects.
4. Build a todo list of work items.
5. **Preflight — fail loud, never degrade silently.** If any TypeScript project is in scope, verify dependencies are installed (e.g. `node_modules` exists under the scoped projects); if missing, run `./putnamiw deps install`, and if that fails STOP and report — do not silently fall back to structural-only analysis (the 2026-06 TS wave did, and the degradation was invisible). If a property depends on tool output (coverage, `bun audit`), probe the tool once before scanning; if unavailable, either stop or note the caveat explicitly in every affected issue body and the wave narrative.

> **Branch**: if `--reconcile` is set, skip steps 2-4 and jump to [Reconcile mode](#reconcile-mode) below.
>
> **Branch**: if `--scorecard-only` is set, skip steps 2-4 and jump directly to [step 5 (Update scorecards)](#5-update-scorecards).

### 2. For each project in scope

Get project metadata (path, type, tags, dependencies) from the putnami MCP `describe_project` tool when available; fall back to `./putnamiw projects describe <project> --output=jsonl`.

**Resolve issue labels from project metadata** — map project tags and domain to labels:

| Source | → Labels |
| --- | --- |
| project tag `go` | `lang/go` |
| project tag `ts` | `lang/ts` |
| project tag `python` | `lang/python` |
| project tag `extension` | `role/extension` |
| project tag `web` | `role/site` |
| project tag `sample` or `e2e` | `role/sample` |
| project tag `protocol` | `role/framework` |
| domain `tooling` (no other role tag) | `role/tooling` |
| domain `go`/`typescript`/`python`/`protocols` (no extension/sample tag) | `role/framework` |
| domain `sites` (no other role tag) | `role/site` |

Store these as `<project-labels>` — they will be added to every issue created for this project.

**Attestation gate — skip unchanged work.** Audit judgments are cacheable outputs keyed by content, exactly like putnami task results. Compute the project's content hash once:

```bash
hash=$(.agents/skills/audit/scripts/attest.sh hash <project-path>)   # git tree hash; "-dirty" suffix if uncommitted changes
```

Before scanning each group in step 3, check the ledger:

```bash
.agents/skills/audit/scripts/attest.sh check <project-short> <group> "$hash" && skip
```

Exit 0 means this (project, group) was already scanned at exactly this content — skip it. Nothing new can be found and nothing can have been fixed, since the code is byte-identical. `--no-cache` bypasses the check. After completing a group's scan (findings filed, resolved issues closed), record it:

```bash
.agents/skills/audit/scripts/attest.sh record <project-short> <group> "$hash"
```

The helper refuses to record `-dirty` hashes, so a dirty working tree is always re-scanned. Report skipped pairs in the summary — a silent skip reads as coverage.

### 3. For each property group (in group priority order)

Scan the project for findings in each property. Use grep patterns, file reads, test output analysis, and structural code analysis appropriate to the project type (Go vs TypeScript).

**Audit depth**: do not just grep for patterns. Read the code, understand intent, trace data flow. A framework for experienced developers using AI must hold itself to a higher standard than pattern-matching can verify. When a property says "check", it means: read the relevant code, understand what it does, and evaluate whether it meets the bar.

**Mechanical properties are script-run, not model-run.** Run `.agents/skills/audit/scripts/mechanical.sh <project-path>` once per project. It deterministically emits JSONL findings for `complexity-file-size`, `complexity-nesting`, `complexity-dependencies` (import fan-out), raw-print `observability-logging` occurrences, and `design-errors` diagnostic codes passed to `diag.Errorf`/`Warningf` as string literals instead of namespaced `ErrorCode*` constants. Do not re-derive these by reading code — triage the script's output instead (e.g. a CLI output writer printing to stdout is not a logging violation; drop it) and take what survives through step 4 like any other finding. Your reading time goes to the semantic properties a script cannot check.

#### Property groups and what to check

---

**testing** (`test-coverage`, `test-contracts`, `test-concurrency`, `test-edge-cases`, `test-integration`, `test-regression`, `test-determinism`):

Framework users depend on every public API behaving as documented. Tests are the executable specification.

- `test-coverage` — Run `./putnamiw test <project> --output=jsonl --no-cache` and parse coverage. Flag exported functions/methods with 0% coverage. For framework packages, every exported symbol must have at least one test exercising its primary use case. Coverage below 60% on a framework package is severity high.

- `test-contracts` — **Every exported type, function, and interface must have dedicated tests that verify the contract, not the implementation.** Check that:
  - Public API signatures have tests covering normal inputs, edge inputs, and error returns
  - Type contracts are tested (e.g., if a function returns `Result<T>`, test both success and failure paths)
  - Go: exported functions in non-`_test.go` files have corresponding test functions
  - TS: exported symbols in barrel `index.ts` files have matching test files
  - Breaking a public contract without a test catching it is severity critical

- `test-concurrency` — **Go only**: check that packages with `sync.Mutex`/`sync.RWMutex`/channels have concurrent test scenarios. Look for:
  - Tests that spawn multiple goroutines accessing shared state
  - Use of `-race` flag in test configuration (should be enabled by default in CI)
  - Packages with mutex-protected state but no concurrent test: severity high
  - **TS**: check for async race conditions — concurrent promise resolution, shared mutable state across async boundaries

- `test-edge-cases` — Look for missing error path tests, nil/undefined input tests, empty collection tests, boundary values (0, -1, max int, empty string). Framework code must handle degenerate inputs gracefully. Check that error constructors and error wrapping paths are tested.

- `test-integration` — Verify cross-module integration tests exist. For frameworks: if module A depends on module B, there should be a test that exercises A through B's interface (not mocking B). Check `go/samples/` and `typescript/samples/` for integration-level coverage.

- `test-regression` — Check recent bug-fix commits (`git log --grep="fix"`) for corresponding test additions. A bug fix without a regression test is severity medium.

- `test-determinism` — Check for test flakiness signals: time-dependent assertions (`Date.now()`, `time.Now()` in assertions), uncontrolled randomness, port binding, file system race conditions. Tests must produce identical results on every run.

---

**security** (`security-injection`, `security-validation`, `security-auth`, `security-secrets`, `security-timeout`, `security-error-exposure`, `security-defaults`, `security-dependencies`, `security-serialization`):

Principle 4: "Security is foundational, not a layer." The framework must be secure by default, not by opt-in.

- `security-injection` — Grep for string concatenation/interpolation in SQL queries, shell commands, template rendering. Check that query builders use parameterized queries exclusively. In Go `database/` package: verify all queries use `$1` placeholders. In TS `database/`: verify query builder prevents raw SQL injection.

- `security-validation` — Check API handlers for input validation at system boundaries. Every endpoint that accepts user input must validate before processing. Check for:
  - Schema validation on request bodies (using runtime schema system)
  - Type coercion that could be exploited (string→number, array→string)
  - Missing validation on path parameters, query parameters, headers
  - Go: check `http.Handler` implementations for input parsing without validation

- `security-auth` — Verify authentication/authorization middleware coverage. Check that:
  - Routes handling sensitive data have security middleware applied
  - Token validation is not reimplemented per-handler (should use framework middleware)
  - Default deny: routes without explicit auth config should fail closed

- `security-secrets` — Grep for hardcoded secrets, API keys, passwords, tokens in source. Check `.env` files are gitignored. Check config loading doesn't have fallback defaults for secrets.

- `security-timeout` — Verify all I/O operations have bounded timeouts:
  - HTTP servers: read/write timeouts configured (Go `ServerConfig` defaults: verify they're applied)
  - HTTP clients: request timeouts, connection timeouts
  - DB queries: statement timeouts, connection pool limits
  - Missing timeout on any I/O operation: severity high

- `security-error-exposure` — Check error responses don't leak internals:
  - Stack traces in production HTTP responses
  - Internal file paths in error messages
  - Database column names or query text in user-facing errors
  - Go: check that `errors.Error` with `Stack()` doesn't serialize stack to HTTP response
  - TS: check `HttpException` doesn't include `cause` chain in response body

- `security-defaults` — **Does the framework default to secure?** Check:
  - HTTP server: are request size limits enforced by default? (Go: MaxBodySize 1MiB — good. Verify TS equivalent)
  - CORS: if no CORS middleware is configured, do cross-origin requests fail? (Browsers enforce, but check preflight handling)
  - Content-Type: are responses served with correct Content-Type? Is `X-Content-Type-Options: nosniff` set?
  - CSP: for `@putnami/web` (SSR), is a Content-Security-Policy header set by default?
  - Session cookies: are they HttpOnly, Secure, SameSite by default?
  - If the framework provides insecure defaults that users must override: severity high

- `security-dependencies` — **TS only**: check `package.json` dependencies for known vulnerabilities. Run `bun audit` or check advisory databases for direct dependencies. Go framework is stdlib-only (no check needed). For TS packages with runtime dependencies, each dependency is an attack surface. Flag packages with deep transitive dependency trees.

- `security-serialization` — Check for unsafe deserialization:
  - `JSON.parse` on untrusted input without schema validation
  - Prototype pollution vectors (object spread from user input, `Object.assign` with user data)
  - Go: check `json.Unmarshal` targets are typed (not `interface{}` for user input)
  - Template injection in SSR rendering

---

**performance** (`perf-algorithmic`, `perf-allocation`, `perf-io`, `perf-n-plus-1`, `perf-caching`, `perf-regex`, `perf-startup`, `perf-bundle`, `perf-hot-path`):

Principle 2: "Performance is a constraint, not an optimization." Framework overhead must be bounded and measurable.

- `perf-algorithmic` — Look for O(n²) or worse in code that processes collections of unknown size. Nested loops over arrays/slices, repeated linear searches where a map lookup would suffice. Check router path matching for linear vs trie-based lookup.

- `perf-allocation` — Check for allocations in hot loops:
  - `new Map/Set/Object/RegExp` inside loops or frequently-called functions
  - `JSON.parse`/`JSON.stringify` in request handlers (per-request allocation)
  - String concatenation in loops (use builder/buffer)
  - Go: check for `fmt.Sprintf` in hot paths (use `strings.Builder`), slice growth without pre-allocation

- `perf-io` — Check for synchronous I/O in async paths, sequential I/O that could be parallelized, missing batching of small operations. Look for `fs.readFileSync` in TS server code, blocking reads in Go handlers.

- `perf-n-plus-1` — Grep for database queries inside loops. Check repository patterns for methods that load related entities one-at-a-time. In Go `database/`: check for `Query`/`QueryRow` calls inside `for` loops. In TS `database/`: check for `repository.find()` inside iteration.

- `perf-caching` — Look for repeated expensive computations without caching. Check:
  - Config parsing on every request (should parse once)
  - Schema compilation on every validation (should compile once)
  - Template compilation on every render (should cache compiled templates)
  - Route resolution without caching (trie lookup is O(path length), acceptable)

- `perf-regex` — Check for `new RegExp()` or `regexp.Compile()` inside functions called per-request. Regex should be compiled once at module/package level. Shared compiled regex is safe for concurrent use in both Go and TS.

- `perf-startup` — **Framework cold-start performance**:
  - Check for import-time side effects (code that runs on `import`/`init()` before the app is ready)
  - Check for eager loading of optional features (should be lazy)
  - Go: check `init()` functions for expensive operations (file I/O, network calls)
  - TS: check top-level await, synchronous file reads at import time
  - For a framework, startup latency directly impacts developer experience and serverless cold starts

- `perf-bundle` — **TS web-facing packages only** (`@putnami/web`, `@putnami/ui`):
  - Check for barrel re-exports that prevent tree-shaking
  - Check for server-only code imported into client bundles
  - Check for large runtime dependencies that could be avoided
  - Verify `package.json` has proper `browser`/`default` conditional exports to separate server/client code

- `perf-hot-path` — **Per-request overhead in HTTP frameworks**:
  - Check middleware chain for unnecessary allocations per request
  - Check router lookup for per-request map/slice creation
  - Go: verify `context.Context` is passed through, not recreated per middleware
  - TS: verify middleware doesn't create closures capturing large objects per request
  - Check that DI resolution in request handlers uses cached instances (not re-resolving singletons per request)

---

**design** (`design-api-surface`, `design-coupling`, `design-patterns`, `design-abstractions`, `design-types`, `design-errors`, `design-stability`, `design-extensibility`, `design-consistency`, `complexity-file-size`, `complexity-function-size`, `complexity-nesting`, `complexity-dependencies`):

Principles 3 + 5: "Deterministic and reviewable behavior" + "Data ownership is non-negotiable." A framework's API is a contract with its users.

- `design-api-surface` — **Every exported symbol must be intentional.** Check:
  - Barrel `index.ts` files: is every re-export a deliberate public API?
  - Go: are exported types/functions in internal packages accidentally public?
  - Look for symbols exported only because they're used by tests (should use `_test` package in Go, `/testing` subpath export in TS)
  - Compare exports count across similar modules (e.g., all Go framework modules should have comparable surface area)

- `design-coupling` — Check for circular dependencies and excessive peer imports:
  - Run import graph analysis: count how many internal packages each file imports
  - Flag fan-out > 5 internal imports in a single file
  - Check for import cycles (A→B→A) which indicate design issues
  - Framework modules should depend downward (application→runtime→utils), never upward

- `design-patterns` — **Verify pattern consistency across the framework**:
  - Do all plugin implementations follow the same lifecycle interface? (Go: Generator/Warmer/Starter/Stopper; TS: generate/warmup/start/stop)
  - Do all repository implementations follow the same interface shape?
  - Do error handling patterns match? (Go: `errors.Wrap`; TS: `cause` chaining)
  - Is constructor naming consistent? (Go: `New<Type>`; TS: factory functions or classes)
  - Inconsistent patterns between similar modules: severity medium

- `design-abstractions` — Check for over/under-abstraction:
  - Interfaces with single implementations (Go: premature interface extraction)
  - Abstract base classes in TS (prefer composition via plugin system)
  - Leaky abstractions: implementation details visible through the public API
  - Check that abstractions earn their complexity: does the indirection serve a real use case?

- `design-types` — **Type quality (TS) / Type safety (Go)**:
  - TS: grep for `as any`, `@ts-ignore`, `@ts-expect-error` — each one must be justified with a comment explaining why the escape is necessary and what invariant the developer must maintain manually
  - TS: check that exported function return types are explicit (not inferred to complex anonymous types)
  - TS: verify generics have meaningful bounds (not `<T>` with no constraint where `<T extends SomeBase>` would be more correct)
  - Go: check for `interface{}` / `any` in public APIs where a typed interface would be more appropriate
  - Phantom types (`__brand`, `__type`) must have doc comments explaining the branding purpose
  - Unjustified type escape in framework code: severity high

- `design-errors` — **Error types must be typed, documented, and actionable**:
  - Go: verify error returns use the structured `errors.Error` type, not bare `fmt.Errorf`. Diagnostic codes passed as string literals to `diag.Errorf`/`Warningf` (rather than namespaced `ErrorCode*` constants) are now flagged mechanically by `mechanical.sh` — triage those from its output; reserve reading time for whether the codes/messages are actionable
  - TS: verify `HttpException` subclasses cover all failure modes. Check that error factories include enough context for an AI agent to understand what went wrong and how to fix it
  - Check error messages: can a developer (or AI) reading only the error message understand what happened, which input caused it, and what to do? If not: severity medium
  - Errors that say "invalid input" without saying which input or what's wrong: severity high

- `design-stability` — **Backward compatibility for public APIs**:
  - Check recent commits for removed or renamed exports (breaking changes)
  - Verify `@internal` / `@experimental` annotations on unstable APIs
  - Check that deprecated APIs have migration guidance in doc comments
  - Framework packages must not break public API without a major version bump
  - Go: check for removed exported functions/types in recent diffs
  - TS: check for removed entries in `package.json` `exports` map
  - Unannounced breaking change: severity critical

- `design-extensibility` — **Can users extend framework behavior without forking?**:
  - Check that plugins can add middleware, routes, DI bindings without modifying framework code
  - Check that key behaviors are interface-based (Go) or configurable (TS), not hardcoded
  - Look for `switch` statements on type that should be replaced with interface dispatch
  - Check that event systems allow user-defined event types
  - Sealed/final patterns where extension is expected: severity medium

- `design-consistency` — **Cross-module API consistency**:
  - Compare naming conventions across Go framework modules (e.g., `NewXxxPlugin` everywhere?)
  - Compare config struct patterns (do all use the same config-loading mechanism?)
  - Compare error handling patterns across modules
  - Check that similar operations have similar signatures (e.g., all `Start()` methods return `error`)
  - Inconsistency between modules that serve the same architectural role: severity medium

- `complexity-file-size` — Flag files exceeding 500 lines (Go) or 400 lines (TS). Framework files above this threshold likely have mixed concerns.

- `complexity-function-size` — Flag functions exceeding 50 lines or with cyclomatic complexity above 10. Framework functions must be readable; long functions with many branches are hard for both humans and AI to reason about.

- `complexity-nesting` — Flag nesting depth > 4 levels. Deep nesting signals missing early returns, missing extraction, or an overly complex algorithm.

- `complexity-dependencies` — Count imports per file. Flag files importing > 8 internal packages (fan-out too high, likely a god-file). Flag packages imported by > 10 files (fan-in too high, consider if it should be split).

---

**operations** (`observability-logging`, `observability-errors`, `observability-metrics`, `observability-tracing`, `prod-versioning`, `prod-config`, `prod-graceful`, `prod-migration`, `prod-health`):

Principle 1: "Observable by construction." Runtime behavior must be visible by default, not opt-in.

- `observability-logging` — Check for raw `console.log`/`fmt.Println` vs structured logging. Framework code must use the structured logger (`@putnami/utils` logger / `go.putnami.dev/logger`). Check that log calls include:
  - Log level appropriate to the message (not everything at INFO)
  - Structured fields (not string interpolation for key data)
  - Request context (trace ID, request ID) when inside a request handler

- `observability-errors` — Verify errors propagate context through the chain:
  - Go: `errors.Wrap(err, code)` preserves cause chain. Check for bare `return err` without wrapping (loses context about where the error was caught)
  - TS: `new Error(msg, { cause: err })` preserves chain. Check for `catch (e) { throw new Error(msg) }` that drops the cause
  - Framework errors must be diagnosable from the error alone, without needing to reproduce

- `observability-metrics` — Check for operations without measurable signals:
  - HTTP handler execution time, request count, error rate
  - DB query duration, connection pool utilization
  - DI container resolution time (for debugging startup issues)
  - Go: check for `emit.metric()` calls in framework operations
  - Missing metrics on a user-facing operation: severity medium

- `observability-tracing` — Check trace context propagation:
  - HTTP middleware should create/propagate trace spans
  - DB queries should be traced with the query (sanitized) as span name
  - Go: check `context.Context` carries trace data through the call chain
  - TS: check async context propagation in DI scope resolution

- `prod-versioning` — Check version management consistency. Verify `.gen/version.json` files are present and up-to-date. Check that version is exposed at runtime (health endpoint, startup log).

- `prod-config` — Look for hardcoded config values that should come from environment/config:
  - Port numbers, hostnames, timeouts in source code (should be in config structs)
  - Feature flags as boolean constants (should be runtime-configurable)
  - Environment-specific logic (`if (env === 'production')`) that should be config-driven

- `prod-graceful` — Check signal handling and resource cleanup:
  - Go: verify `os.Signal` handling with graceful shutdown (context cancellation, drain connections)
  - TS: verify `process.on('SIGTERM')` or equivalent lifecycle hooks
  - Check that DB connections, file handles, and goroutines are cleaned up on shutdown
  - Check shutdown timeout configuration (don't hang forever, don't kill immediately)

- `prod-migration` — Check for schema changes without migrations:
  - New table definitions or column additions should have corresponding migration files
  - Check migration ordering and idempotency
  - Data ownership principle: schema changes must be versioned and reversible

- `prod-health` — Verify health/readiness endpoints in applications:
  - Health endpoint should check downstream dependencies (DB, external services)
  - Readiness endpoint should reflect actual serving capability
  - Both should return structured responses (not just 200 OK)

---

**developer-experience** (`dx-api-docs`, `dx-examples`, `dx-getting-started`, `dx-migration`, `dx-site`, `dx-errors-actionable`, `dx-discoverability`, `dx-predictability`, `dx-cli-output`, `language`):

Principle 6: "Automation is a first-class user." The framework must be usable by both humans and AI agents. Documentation is not prose — it's executable specification.

- `dx-api-docs` — **Per-export documentation coverage.** Check that every exported function, type, and interface has a doc comment (Go: godoc; TS: JSDoc). The comment must describe:
  - What the function does (not how — that's the implementation)
  - Parameters and return value semantics
  - Error conditions and what they mean
  - `@internal` / `@experimental` for unstable APIs
  - Count: `exported symbols without doc comment / total exported symbols`. Ratio > 10% undocumented on a framework package: severity high

- `dx-examples` — Verify code examples exist and are current:
  - `doc/` folder contains runnable examples for key APIs
  - `go/samples/` and `typescript/samples/` projects exercise the framework end-to-end
  - Check that example imports match current `package.json` exports
  - Check that example code compiles (run build on sample projects)
  - Stale example that doesn't compile: severity high

- `dx-getting-started` — **Zero-to-working path.** Check that:
  - Each framework package has a `doc/` with at minimum a getting-started guide
  - The guide shows a complete, minimal working example (not fragments)
  - The example can be copy-pasted and run without modification
  - For AI agents: the getting-started guide should be sufficient to build a working app without reading source code

- `dx-migration` — Check for breaking changes without migration guidance:
  - Recent commits that modify exports, config shapes, or behavior
  - If breaking: is there a migration section in the doc, or a changelog entry?
  - Deprecated APIs should have `@deprecated Use X instead` with a concrete replacement

- `dx-site` — Verify `sites/putnami.dev/doc/` coverage for the project's features. Cross-reference exported APIs with documentation sections. New features without corresponding site docs: severity medium.

- `dx-errors-actionable` — **Can an AI agent self-correct from error messages alone?** Read error messages in the codebase and evaluate:
  - Does the error say what was expected vs what was received?
  - Does it identify which input/parameter caused the failure?
  - Does it suggest what to do? (e.g., "did you forget to call .use(plugin)?" or "register the provider before resolving")
  - Go: check `errors.New`/`errors.Newf` messages for context completeness
  - TS: check `throw new Error(msg)` and `HttpException` messages
  - Error that says "invalid" or "failed" without context: severity medium

- `dx-discoverability` — **Can an AI agent understand a module from its types alone?** Check:
  - Are types self-documenting? (e.g., `ServerConfig` fields have doc comments, not just `Port int`)
  - Are there type aliases that explain domain concepts? (e.g., `type Handler = (ctx: Context) => Response`)
  - Is the type graph navigable? (following types from the entry point should reveal the full API)
  - Do barrel exports organize symbols logically? (grouped by feature, not alphabetically)
  - Check `index.ts` / exported package symbols: can you understand the module's capabilities from the export list?

- `dx-predictability` — **API consistency for pattern matching.** Check:
  - Do similar modules have similar APIs? (e.g., Go `http.NewServerPlugin` and `database.NewPlugin` — same lifecycle pattern?)
  - Are options passed the same way? (config struct vs functional options vs method chaining — pick one per language)
  - Can a developer who learned one module predict the API of another?
  - Cross-module inconsistencies: severity low (but compound effect is high)

- `dx-cli-output` — **Machine-readable output for all operations.** Check:
  - CLI commands support `--output=jsonl` for structured output
  - Error output includes file, line, column for diagnostic tools
  - Progress reporting uses structured events, not free-form text
  - Missing structured output on a user-facing CLI command: severity medium

- `language` — **Every GitHub artifact, code comment, and doc is in English** (`.agents/constraints.md`). This property is script-run and workspace-wide, not per project. Run it once per audit run whenever the developer-experience group is in scope, and skip it for `--scorecard-only`:
  ```bash
  bash .agents/skills/check/scripts/english-only.sh files          # tracked files
  bash .agents/skills/check/scripts/english-only.sh tasks          # open, in-progress and blocked tasks, through `putnami tasks find`
  git fetch --prune origin
  bash .agents/skills/audit/scripts/english-only-proposals.sh      # open and draft proposals into origin's default branch, through `putnami proposals find`
  ```
  The proposals contract finds a proposal by its exact base and head, so `english-only-proposals.sh` takes the heads from origin's remote-tracking branches (hence the fetch) and scans each proposal's title and body with the detector's `text` mode. A proposal into another base is scanned only with `--base <branch>`. It asks about the 200 most recently committed heads (`--heads <n>` changes the bound, and the summary counts the heads left out); a head whose find answers `unavailable` is skipped, named on stderr and counted, and the scan goes on. Without an offender, an unavailable head makes the scan exit 2: the scan is incomplete, not clean. Every exit status means the same for all three scans: exit 0 is clean, exit 1 lists the offenders on stdout, and exit 2 is a tool failure — stop and report it, never read it as clean. The detector already skips test files, `testdata/` and `fixtures/`, allowlisted proper nouns, and its own tracking issue. Triage what remains: a foreign word quoted as an example inside an issue about language is a legitimate drop.
  File every surviving offender in **one** issue, never one per offender, with fingerprint `audit-fp: workspace/language/english-only` and title `[workspace] Non-English text in GitHub artifacts, code comments, or docs`. Route it as in 4b: when it is open, replace its Evidence with the current listing; when none exists or the last one closed as completed, create it with `--label "group/developer-experience" --label "prop/language" --label "area/all" --label "severity/medium" --label "priority/p2" --label "type/task" --label "status/audit-finding" --label "source/audit"`. `area/all` keeps it out of the domain scorecards; `status/audit-finding` puts it in the `/fix` queue. If the label is missing, create it first with `gh label create prop/language --force --description "Non-English text in a GitHub artifact, code comment, or doc"`. When all three scans are clean and the issue is open, close it as resolved (4e).

### 4. For each finding

#### 4a. Assign severity

Base severity from the finding's nature:
- **critical**: Security vulnerability, data loss risk, production blocker
- **high**: Significant gap, reliability risk
- **medium**: Quality improvement, fix when touching the area
- **low**: Cosmetic, minor, nice to have

Severity describes **impact only** — never inflate it for scheduling reasons. (The old severity-boost rule conflated the two axes and let boosted mediums skip triage; it is gone.)

Assign **priority** as a separate axis, from the position of the finding's group in the group priority order (step 1): positions 1-2 → `priority/p1`, positions 3-4 → `priority/p2`, positions 5-6 → `priority/p3`. Priority orders the fix queue; severity states impact. `/fix` sorts by severity first, then priority.

#### 4b. Fingerprint, duplicates, and waivers

Every finding gets a deterministic fingerprint, stamped into the issue body on its own line:

```
audit-fp: <project-short>/<property>/<slug>
```

`<slug>` identifies the primary subject — the exported symbol under scrutiny, or the file basename without extension (e.g. `audit-fp: go-http/security-timeout/client`). Derive it from the finding's *location*, never its prose, so re-discoveries in later waves produce the identical fingerprint.

**`<project-short>` naming convention** (used consistently in fingerprints, issue titles, and searches):

| Project name pattern | Short name |
| --- | --- |
| `@putnami/<name>` | `<name>` (e.g. `@putnami/database` → `database`) |
| `go.putnami.dev/protocol/<name>` | `go-protocol-<name>` (e.g. `go.putnami.dev/protocol/cache` → `go-protocol-cache`) |
| `go.putnami.dev/examples/<name>` | `go-examples-<name>` |
| `go.putnami.dev/<name>` | `go-<name>` (e.g. `go.putnami.dev/events` → `go-events`) |
| `@example/<name>` (TS samples) | `example-<name>` |
| `putnami.dev` | `putnami.dev` |

Match the most specific pattern first. Short names never contain `/` — nested name segments flatten to `-` so the fingerprint always has exactly three `/`-separated parts. (Issues filed before 2026-07-28 for `protocol/*` and `examples/*` projects may carry malformed slashed shorts like `go-protocol/cache`; when deduping those projects, search both forms.)

Search **all states** — closed issues are the system's memory of rejections:

```bash
gh issue list --search "\"audit-fp: <fp>\"" --state all --json number,title,state,stateReason,labels --limit 20
```

Fallback for pre-fingerprint issues (no hit above): `gh issue list --label "prop/<property>" --search "<project-short-name>" --state all --json number,title,state,stateReason,body --limit 50`, matching on same property label + project in title + similar file reference in body.

Route by what you find:

- **Open match** → **update** the existing issue (append new evidence, adjust severity). Do not create a duplicate.
- **Closed as completed** → the finding is back: a regression. File a new issue linking the old one (`Regressed: #<n>` in the body) and start severity no lower than the old issue's.
- **Closed as not planned, or labeled `wontfix`** → a **standing waiver**. Do NOT re-file; count it as "waived" in the summary. Override only when the evidence is materially new (different code path, demonstrated real-world impact) AND severity is high or critical — then file with a `Supersedes waiver: #<n>` line explaining exactly what changed since the rejection.

#### 4c. Adversarial check before filing

A false finding is not cheap — it costs a full `/fix` session downstream and erodes trust in the backlog. Before creating any **new** issue, re-read the evidence as a skeptic trying to refute it: does the code actually reach this path? Is the "missing" guard provided by a caller, middleware, or framework default? Does an existing test already pin this behavior? For `critical` and `high` findings, trace the concrete failure path end to end and put the trace in the Evidence section. Findings that don't survive the skeptic pass are dropped, not filed at lower severity.

#### 4d. Create or update issue

**New issue**:
```bash
gh issue create \
  --title "[<project-short>] <description>" \
  --label "group/<group>" --label "prop/<property>" \
  --label "scope/<domain>" --label "severity/<severity>" \
  --label "priority/<p1|p2|p3>" \
  --label "type/<type>" \
  --label "status/audit-finding" --label "source/audit" \
  --label "<project-labels>" \
  --body "<body>"
```

**Issue type** — set based on the finding's group:

- `group/security` → `type/bug`
- all other groups → `type/task`

**Auto-confirm rule**: if severity is `critical` or `high`, also add `status/confirmed` (remove `status/audit-finding`).

Issue body template:
```markdown
## Finding

<1-2 sentence description>

## Location

- **Project**: `<package-name>` (`<path>`)
- **File(s)**: `<file>:<line>` (repeat as needed)

## Evidence

<code snippet or test output>

## Severity Rationale

<why this severity level>

## Suggested Fix

<concrete suggestion>

---
_Property: `<property-id>` | Group: `<group>` | Domain: `<domain>`_
_Audit date: `<YYYY-MM-DD>`_

audit-fp: <project-short>/<property>/<slug>
```

#### 4e. Auto-close resolved findings

For existing open issues matching this project + property: if the finding no longer reproduces (code was fixed), close the issue with a comment noting it was resolved.

### 5. Update scorecards

> **Branch**: if `--skip-scorecard` is set, stop here and proceed directly to step 6. Scorecards will be refreshed by the fleet's post-pass or a later `--scorecard-only` run — see [Parallel runs](#parallel-runs).

Scorecard bodies are deterministic artifacts — never hand-write them:

```bash
bash .agents/skills/audit/scripts/scorecard.sh                # all domains + workspace rollup
bash .agents/skills/audit/scripts/scorecard.sh --domain go    # one domain (workspace rollup still refreshed)
```

The script rebuilds each domain scorecard (project × group matrix, by-group table, grades) and the workspace rollup from the current open issues. It finds each scorecard by its exact title among open issues (`[scorecard] <Domain> Health` per domain, for example `[scorecard] TypeScript Health`, and `[scorecard] Workspace Health` for the rollup) and creates it when none is open. `--dry-run` prints the bodies and edits nothing. Because it is a plain script, anything may run it: `/fix-loop`'s final step, `fleet.sh`'s post-pass, a cron — the dashboard never goes stale waiting for the next wave.

Then post the **wave narrative as a comment** on each in-scope domain scorecard issue — never in the body, which the script owns and regenerates:

```bash
gh issue comment <scorecard-number> --body "<wave narrative>"
```

The narrative records what was scanned, environment caveats (e.g. degraded tooling), notable findings, and what was **verified clean** — those clean attestations plus the comment trail are the wave-over-wave history the body cannot hold.

### 6. Prevention — graduate recurring finding classes

Count this wave's new findings per property. Any property that fired on **3+ projects** is a lint rule waiting to exist; `/fix` whacking instances one at a time is the expensive alternative. Draft a single graduation issue and put the draft in the wave summary, but **ask the user before filing** — constraints.md forbids unprompted tooling/process issues. In unattended runs, leave the draft in the summary for the next interactive session instead of filing. Once approved, file (or update, if one is already open):

```bash
gh issue create \
  --title "[workspace] Graduate <property> recurrences into a deterministic guard" \
  --label "type/task" --label "prop/<property>" --label "area/all" \
  --label "source/audit" --label "status/audit-finding" --label "severity/medium" \
  --body "<the instances found this wave + the proposed lint rule / mechanical.sh check / framework guard>"
```

`area/all` keeps these meta-issues out of the domain scorecards. This is principle 6 — automation as first-class user — applied to the audit itself: kill the class, not the instance.

### 7. Print summary

```
Audit complete: <N domains>, <N projects>, <N groups>

  Created: N new issues
  Updated: N existing issues
  Closed:  N resolved issues
  Waived:  N (standing wontfix — not re-filed)
  Skipped: N (project, group) pairs unchanged since last attestation
  Unchanged: N

Scorecards updated: workspace + <N> domains
```

## Reconcile mode

`--reconcile` verifies existing open issues instead of scanning for new findings:

1. List candidates: `gh issue list --label "source/audit" --state open --json number,title,body,labels --limit 200`, narrowed by `--domain` / `--project` / `--group` label filters when given.
2. For each issue, locate the finding from the `audit-fp:` line and the Location section, then check whether it still reproduces at HEAD.
3. **Resolved** → attribute the fix (`git log --oneline -- <file>` to find the commit/PR), comment `Resolved by <sha or PR link>`, then `gh issue close <number> --reason completed`.
4. **Still valid** → leave open; refresh the Evidence section if the code moved.
5. Finish with the scorecard refresh (step 5) unless `--skip-scorecard`.

`--dry-run` prints what would be closed without acting.


## Prune mode

`/audit --prune --domain <d>` removes weight. It answers one question per candidate: what breaks if this goes away? When the answer is "nothing", it goes away. The regular property rubric finds work to add; prune finds work to delete. The two never run together.

The repository must be explainable, not merely green. Putnami's consumers are this repository and the checkouts listed in `PUTNAMI_CONSUMER_REPOS`. A symbol, package, flag or page that none of them uses has no reason to exist.

### Typologies

| Typology | Candidate | Truth to restore | Verdict |
| --- | --- | --- | --- |
| `dead` | package or export with zero importer across this repository and the consumer checkouts | delete; unexport when only its own package uses it | auto, except methods (may satisfy an interface) and sample-only importers |
| `duplicate` | the same function defined in two or more projects | one copy, in the lowest module that both can import | owner names the surviving copy |
| `multi-version` | legacy / v1-v2 / compat / fallback branches | the newer path, alone | owner names the survivor |
| `palliative` | a guard explained by an incident; a test pinning a commit or a stamped version | fix the root cause or delete the guard; a test proves a contract, never history | owner, except pinned repository state (auto) |
| `historical-ref` | a comment, doc or ADR citing an issue, PR or commit | the rule, stated in the present tense, or nothing | auto |
| `comment` | a comment that justifies the code instead of describing it; a lint escape (`//nolint`, `as any`, `@ts-ignore`, `biome-ignore`); a file over 30 % comment lines | a comment states the contract; a lint escape means the code or the rule changes | justification comments auto; escapes and density need the owner |
| `test-scaffold` | test-only packages, fakes, mocks, harnesses; a project with test lines above twice its source | tests through the public contract, on the real component | owner |
| `config-surface` | a `PUTNAMI_*` variable read in code and documented nowhere | a documented config key, or deletion | owner |
| `doc` | an ADR that is superseded, rejected or never adopted; a page linking to a path that no longer exists | ADRs describe the released product, one per settled decision; dangling links go | dangling links auto; ADRs need the owner |

Module granularity and god files (`release_set.go`, `result-v2.ts`) are reported as observations in the umbrella issue, never as prune PRs: splitting is design work, and the module graph is what `--impacted` and the monorepo sell.

### Models

Judgment and edits run on different tiers, on both hosts. Tier equivalence is settled in `.agents/constraints.md`: Fable 5.1 ↔ `gpt-6-astra`, Opus 5.5 ↔ `gpt-6-sol`, Sonnet 5 ↔ `gpt-6-luna`.

| Step | Claude Code | Codex | Why |
| --- | --- | --- | --- |
| Index, scan, gates | script / CLI | script / CLI | no model |
| P3 triage of every `auto: false` row, and of `auto: true` rows in `dead`, `duplicate`, `multi-version`, `palliative`, `test-scaffold` | `prune-triage`: Fable 5.1, high | `prune-triage`: `gpt-6-astra`, high | a wrong deletion breaks a consumer repository, where this repository's gate cannot see it |
| P5 edits, one call per typology | `prune-apply`: Sonnet 5, medium | `prune-apply`: `gpt-6-luna`, medium | every row is prescribed; the `--projects` gate is the proof |
| Orchestration: slicing, umbrella issue, commits, the one `--impacted` gate | this skill's session (Opus 5.5) | the session model (`gpt-6-astra`) | git and GitHub state stay in one place |

The workers are hand-authored with the skill: `.claude/agents/prune-triage.md`, `.claude/agents/prune-apply.md`, `.codex/agents/prune-*.toml` (registered in `.codex/config.toml`). Hand each worker a slice, never a domain: at most 150 candidate rows per `prune-triage` call and one typology per `prune-apply` call, and run one orchestrator session per (domain, typology) on the large domains (`tooling`, `typescript`).

### Steps

**P1. Index.** Run `prune.sh repos` and read the HEAD dates. A consumer checkout older than 7 days is fetched first (`git -C <repo> pull --ff-only`); a missing checkout stops the run (set `PUTNAMI_CONSUMER_REPOS`). Then `prune.sh index` once per session: 40 s over every repository, symbol and import tables under `.putnami/audit/prune/`.

**P2. Scan.** For each domain in scope, `prune.sh scan all $(bash .agents/skills/audit/scripts/domains.sh paths <domain>) > .putnami/audit/prune/<domain>.jsonl` (`--typology` narrows). The script emits candidates, not findings: every `auto: false` row needs a reading, and `auto: true` rows still get the skeptic pass in P3.

**P3. Triage.** Delegate to `prune-triage` in slices of at most 150 rows: `auto: true` rows of `historical-ref`, `comment` justifications, `doc` dangling links and `config-surface` duplicates skip triage and go straight to P5, where `prune-apply` runs the skeptic pass itself. Every other row is triaged. The worker returns one JSONL line per row with one of three outcomes:
- **apply** — the evidence is complete. A `dead` export with zero references in every consumer repository, a comment that explains history, a link to nothing. These are applied in P5 without asking.
- **verdict** — the removal is right but the choice is the owner's: which copy survives, which branch of a dual path, whether an ADR still describes the product. These become rows in the umbrella issue.
- **drop** — a false positive. Name why in one line (interface implementation, extension binary consumed by manifest, symbol reached by reflection or by a JSON key). Dropped rows are listed at the bottom of the umbrella issue so the next wave does not re-read them.

Triage rules per typology: a Go method with no caller by name is checked against the interfaces of its package before deletion. An export used only by samples or templates counts as unused: samples follow the framework, not the reverse. For `duplicate`, the surviving copy is the one in the lowest module both consumers already import; if no such module exists, the finding stays a verdict. For `multi-version`, the newer path survives by default; the row says which files carry the old one. For `comment`, keep the one sentence that states the contract and delete the rest; a lint escape becomes either a code change or a rule change in `biome.json` or `.golangci.yml`, never a longer comment. For `palliative`, the row names the incident, states whether the root cause is still reachable, and estimates the fix in files; the owner picks fix, delete or keep.

**P4. One umbrella issue per domain.** Title `[prune] <domain>`, labels `type/task`, `scope/<d>`, `source/audit`, `status/confirmed`, `<project-labels>`. The body is regenerated on every batch with `gh issue edit --body`, never appended: a counts table (applied / verdict / dropped per typology, LoC delta so far), then one verdict table per typology with a checkbox per row (`- [ ] path — one-line choice`), then the observations (modules, god files), then the dropped list. The owner ticks a row to approve it and writes the choice on the same line when there are two. The issue is the decision surface; the PR is the delivery surface. No per-finding issue, ever.

**P5. One batch branch per domain.** Branch `prune/<domain>` from `origin/main`, draft PR opened at the first commit and kept open while batches land. One commit per typology, in the order `dead`, `multi-version`, `duplicate`, `palliative`, `test-scaffold`, `comment`, `historical-ref`, `config-surface`, `doc`: deletions first, so later typologies do not touch files that are about to go. Domains run in parallel; typologies within a domain run in sequence, because they touch the same files.

Each commit message names what it deletes, in English (D-002, D-003). The type is `refactor`, a Conventional Commit type, because a prune keeps behavior; a typology that drops a published export uses `refactor(<domain>)!`:

```
refactor(<domain>): prune <typology> — <N> removals

Deletes: <package or file list, or "exports: A, B, C in pkg">
Keeps: <the surviving copy or path, when a verdict chose one>
Verdicts: #<umbrella> rows <ids>
```

Edits are delegated to `prune-apply`, one call per typology, with the `apply` rows and the ticked verdict rows as its input. The worker edits and runs the `--projects` gate; the orchestrator reads its `Deletes:` block into the commit message, verifies the gate record it names, and commits. Rows the worker escalates go back to the umbrella issue as verdicts.

Gate cadence, the expensive part, is what makes batches cheaper than PRs:
- after each commit: `./putnamiw lint,test,build --projects <changed projects>`; a red here is fixed in the same commit, never carried;
- before ready-for-review, once: `./putnamiw lint,test,build,validate --impacted --enforce-coverage`; a load flake is re-run alone with `--retry-failed`, never waited out with a timeout;
- no `/fix` finalizer, no per-finding session.

At the start of every batch, re-read the umbrella issue: ticked verdict rows join the next commit of their typology. The PR is marked ready when every typology has had its pass and no verdict row is left open, or when the owner says ship. The PR body lists the deletions per typology with the LoC delta from `git diff --shortstat origin/main...`, links the umbrella issue, and closes it on merge.

**P6. Summary.** Print, per domain: candidates, applied, verdict (open / ticked), dropped, LoC delta, PR URL, umbrella issue URL. A prune typology that fired in three or more domains is a rule waiting to exist: draft it for `.agents/constraints.md` or a lint rule in the summary, and ask before filing (step 6 above applies).

## Parallel runs

A full workspace audit can be split across multiple backgrounded Claude Code or
Codex sessions, one per `(domain, group)` slice. The orchestrator script lives
at `.agents/skills/audit/scripts/fleet.sh` and computes its slice matrix from
the domains that `domains.sh` reads from the workspace's top-level scopes.

### How slicing works

The matrix is computed from each domain's project count:

- A domain with **≤6 projects** runs as **one combined shard** (`/audit --domain <d> --skip-scorecard`) — all groups for that domain in a single agent.
- A domain with **>6 projects** is **split per group** (`/audit --domain <d> --group <g> --skip-scorecard`) — one agent per group, launched in group priority order.

For today's scopes that yields 26 slices: `python` (5 projects) and `sites` (2) as single shards; `go` (38), `protocols` (39), `tooling` (11) and `typescript` (35) each split into 6 group shards. `fleet.sh --dry-run` prints the current matrix with each domain's project count.

### Why `--skip-scorecard`

Scorecards (the `[scorecard] … Health` issues) are the only shared write surface across shards. Letting each shard refresh them would race. Shards skip the refresh; the orchestrator runs `scorecard.sh` once after all shards exit — a plain script, no model session needed. Attestation writes never race: each (project, group) pair belongs to exactly one shard and gets its own file under `.putnami/audit/attestations/`.

### Dispatch

```bash
.agents/skills/audit/scripts/fleet.sh --host claude # Claude Code fleet
.agents/skills/audit/scripts/fleet.sh --host codex  # GPT-6 Astra fleet
.agents/skills/audit/scripts/fleet.sh --dry-run     # print the matrix only
.agents/skills/audit/scripts/fleet.sh --domain go   # only the 6 go slices
.agents/skills/audit/scripts/fleet.sh --group security
.agents/skills/audit/scripts/fleet.sh --max-parallel 6
.agents/skills/audit/scripts/fleet.sh --no-cache
.agents/skills/audit/scripts/fleet.sh --skip-scorecard-pass
```

Each shard uses the selected host CLI and writes stdout and stderr under
`.audit-runs/<timestamp>/`. Codex shards explicitly request
`gpt-6-astra` at xhigh effort and fail preflight when the installed CLI does
not expose that model. The final `summary.txt` lists per-slice exit status and
points to any failed shard's log.

### When to run the parallel mode vs the single-process `/audit`

- **`/audit --impacted`** — fast, default for routine work; only re-audits projects touched on the current branch.
- **`/audit --domain <d>`** — one domain end-to-end in a single session; use when you want to read the audit live.
- **`fleet.sh`** — full workspace refresh, or any time the matrix would take > ~30 minutes serially. Attestations make re-runs cheap: shards skip every (project, group) pair whose content hasn't changed since its last scan, so a routine fleet run re-reads the drift, not the world. Reserve `--no-cache` for rubric changes (new properties, changed thresholds) where old attestations no longer mean "clean".
