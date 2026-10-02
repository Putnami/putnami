---
name: audit
description: Scan Putnami projects for quality/correctness/security findings — create, update, and close GitHub issues; refresh scorecards. --prune removes dead weight per scope in one umbrella issue and one batch proposal
model: claude-opus-5-5[1m]
allowed-tools: Bash, Read, Grep, Glob, Task, TodoWrite, ToolSearch, mcp__putnami
argument-hint: [--all | --scope SCOPE | --project PROJECT | --group GROUP | --impacted] [--reconcile | --scorecard-only | --prune [--typology T] [--apply]] [--skip-scorecard] [--no-cache] [--dry-run]
disable-model-invocation: true
---

# Audit

Scan Putnami projects for quality findings. Each finding becomes a GitHub issue with structured labels. Existing issues are updated or closed when findings change. Scorecards are refreshed at the end (when enabled).

## Portable host contract

This is the canonical workflow for both Claude Code and Codex: invoke it with `/audit` on Claude Code or `$audit` on Codex. The `@putnami/intelligence` extension ships it; `putnami install` materializes it when the workspace lists `extension:@putnami/intelligence` in `agentArtifacts`. Helper scripts live under `.agents/skills/audit/scripts/` on both hosts; run them from the workspace root through `bash`. When the `putnami` MCP tools appear deferred on Claude Code, load them in one ToolSearch call before discovery. Run semantic audit judgment on the strongest available model.

The audit unit is a Putnami **scope**: a directory whose `putnami.json` declares projects (`putnami scopes list`). Repository settings live in the workspace manifest under `options["@putnami/intelligence"].audit`; `bash .agents/skills/audit/scripts/config.sh` resolves them:

| Key | Meaning | Default |
|---|---|---|
| `priority` | Property groups in fix-queue order for every scope | `security, operations, testing, design, performance, developer-experience` |
| `scopes.<scope>.priority` | That scope's own order | the workspace `priority` |
| `labelPrefix` | Issue label of a scope is `<labelPrefix><scope>` | `scope/` |
| `scorecards` | Maintain one scorecard task per scope plus a workspace rollup | `false` |
| `profile` | Path of the repository profile, from the workspace root | `.AI/audit/profile.md` |
| `checks` | Path of a repository bash script, from the workspace root; `mechanical.sh` runs it with the project path and its JSON lines join `mechanical.sh` output | none |

Before resolving scope, load the repository profile: `bash .agents/skills/audit/scripts/config.sh profile` prints its path, and exits 1 when no profile is set and the default file is absent. It exits 2 when a configured profile is missing or `jq` is not installed: stop and report, never scan without the profile the workspace names. The profile contains repository-specific architecture, rubric, label, and operational guidance and extends this portable workflow without modifying it. It may add properties to a group, sharpen a property's bar for that repository, or map project tags to role labels.

Judge each project against its own repository architecture and constraints. Repository-specific boundaries, providers, infrastructure, and incident-derived rules belong in the profile; when no profile exists, apply the portable property definitions below without inventing local conventions.

## Arguments

| Flag | Scope |
|------|-------|
| `--all` | Full workspace audit (all scopes, all groups) |
| `--scope <path>` | One scope from `putnami scopes list` |
| `--project <name>` | One exact Putnami project id |
| `--group <group>` | One property group across all projects |
| `--impacted` | Only projects with changes vs. `main` (default when no flag) |
| `--reconcile` | Verify open issues only — close resolved ones with commit/PR attribution |
| `--scorecard-only` | Skip scanning; only refresh scorecards from existing open issues |
| `--skip-scorecard` | Scan and create/update issues, but do not refresh scorecards |
| `--no-cache` | Ignore attestations — re-scan (project, group) pairs even when their content is unchanged |
| `--dry-run` | Print findings without creating/updating issues |
| `--prune` | Remove weight instead of filing findings: dead code, duplicates, dual versions, palliatives, historical references, justifying comments, test scaffolding, hidden config, stale docs. One umbrella issue and one batch proposal per scope. See [Prune mode](#prune-mode). |
| `--typology <t>` | Prune only: restrict to one typology (`dead`, `duplicate`, `multi-version`, `palliative`, `historical-ref`, `comment`, `test-scaffold`, `config-surface`, `doc`) |
| `--apply` | Prune only: apply the safe removals and push the batch branch. Without it, prune scans, triages and updates the umbrella issue only. |

Flags combine: `--group security --scope identity` audits security in the identity scope only.

**Prune mode** (`--prune`): none of the issue-per-finding lifecycle below applies. No fingerprints, no attestations, no scorecards. Jump to [Prune mode](#prune-mode).

**Reconcile mode** (`--reconcile`): Instead of scanning for new findings, only checks whether existing open issues are still valid. Resolved issues are closed with a comment attributing the fixing commit or PR. Combines with `--scope`, `--project`, `--group` to filter. `--dry-run` prints what would be closed without acting.

**Scorecard-only mode** (`--scorecard-only`): Skip scanning entirely; run `bash .agents/skills/audit/scripts/scorecard.sh` (add `--scope <s>` to refresh a single scope) and exit. Scorecards are deterministic artifacts rendered from open issues.

**Skip-scorecard** (`--skip-scorecard`): Run the full scan and issue lifecycle, but leave the scorecard refresh to someone else. Intended for parallel shards: the fleet's post-pass runs `scorecard.sh` once after all shards exit.

## Bootstrap (first run in this repo)

The audit taxonomy is not part of the default label set. On the **first** mutating run (or when `gh label list` is missing them), create the labels once. Under `--dry-run`, report missing labels but do not create them.

```bash
# groups
for g in security operations testing performance design developer-experience; do
  gh label create "group/$g" --color 5319e7 --force >/dev/null 2>&1 || true
done
# severities (impact only)
gh label create "severity/critical" --color b60205 --force; gh label create "severity/high" --color d93f0b --force
gh label create "severity/medium"  --color fbca04 --force; gh label create "severity/low"  --color c2e0c6 --force
# priorities (fix-queue ordering, separate from severity)
for p in p1 p2 p3; do gh label create "priority/$p" --color 5319e7 --force; done
# lifecycle + provenance (status/* are shared with $fix and $fix-loop)
for s in audit-finding confirmed in-progress needs-review; do gh label create "status/$s" --color 0e8a16 --force; done
gh label create "source/audit" --color ededed --force
# roles + languages
for r in workload lib cli site infra; do gh label create "role/$r" --color bfdadc --force; done
for l in go ts python; do gh label create "lang/$l" --color c5def5 --force; done
# per-scope labels + area/all for workspace-wide meta-issues
for label in $(bash .agents/skills/audit/scripts/config.sh scopes | jq -r '.label') area/all; do
  gh label create "$label" --color 1d76db --force; done
```

**Attestations** (`.agents/skills/audit/scripts/attest.sh`) cache "this (project, group) was scanned at content hash X" under `.putnami/audit/attestations/` — Putnami CLI state, gitignored, per-machine. No provisioning needed; they populate as waves run and let unchanged projects be skipped.

**Scorecards** are tasks of the bound tasks provider, one per scope plus one workspace rollup. They are off until the workspace sets `"scorecards": true`; until then the audit runs the full scan/issue lifecycle and skips the scorecard refresh with a note. Once on, `scorecard.sh` finds or opens each scorecard through `putnami tasks create` with the idempotency key `scorecard:<scope label>` (`scorecard:workspace` for the rollup), so no file records task numbers. The provider matches a key per account: a CI identity and a person each keep their own scorecards, so run scorecards from one identity.

## Steps

### 1. Resolve scope

1. Run `bash .agents/skills/audit/scripts/config.sh scopes` for every scope's projects, short names, label and priority order. Generated projects (tag `generated` or `generated-client`) are not listed and not audited. A failure stops the audit: never guess scopes.
2. Determine which (scope, project, group) triples to audit from the arguments. `bash .agents/skills/audit/scripts/config.sh scope-of <project-id>` names a project's scope; a project outside every scope is reported, not audited.
3. For `--impacted`: run `git diff --name-only main...HEAD` and match changed paths to projects.
4. Build a todo list of work items.
5. **Preflight — fail loud, never degrade silently.** If scoped projects need dependencies, run `putnami deps install` when they are absent; if that fails STOP and report rather than silently falling back to structural-only analysis. If a property depends on optional tool output (coverage or a dependency-vulnerability scan), probe the repository-declared Putnami command once before scanning; if unavailable, either stop or note the caveat explicitly in every affected issue body and the wave narrative.

> **Branch**: if `--reconcile` is set, skip steps 2-4 and jump to [Reconcile mode](#reconcile-mode).
>
> **Branch**: if `--scorecard-only` is set, skip steps 2-4 and jump to [step 5](#5-update-scorecards).

### 2. For each project in scope

Run `putnami projects describe <project> --output=jsonl` (or `putnami describe_project`) to get metadata (path, tags, dependencies).

**Resolve issue labels from project metadata:**

| Source | → Labels |
| --- | --- |
| tag `go` | `lang/go` |
| tag `ts` | `lang/ts` |
| tag `python` | `lang/python` |
| path `.../workloads/...` | `role/workload` |
| path `.../libs/...` | `role/lib` |
| tag `cli` | `role/cli` |
| tag `web` | `role/site` |
| tag `infra` | `role/infra` |
| always | the scope label (`config.sh label <scope>`) |

Store these as `<project-labels>` — added to every issue for this project.

**Attestation gate — skip unchanged work.** Audit judgments are cacheable outputs keyed by content, exactly like putnami task results. Compute the project's content hash once:

```bash
hash=$(bash .agents/skills/audit/scripts/attest.sh hash <project-path>)   # git tree hash, folded with DIRECT deps; "-dirty" suffix if uncommitted (or describe unavailable)
```

Before scanning each group in step 3, check the ledger (`<project-short>` is the project's short name from step 4b):

```bash
bash .agents/skills/audit/scripts/attest.sh check <project-short> <group> "$hash" && skip
```

Exit 0 means this (project, group) was already scanned at exactly this content — skip it. Nothing new can be found and nothing can have been fixed, since the code is byte-identical. `--no-cache` bypasses the check. After completing a group's scan (findings filed, resolved issues closed), record it:

```bash
bash .agents/skills/audit/scripts/attest.sh record <project-short> <group> "$hash"
```

The hash also folds each **direct dependency**'s tree object and the **rubric hash** (`SKILL.md` + `mechanical.sh` + `config.sh` + the repository profile and checks script when set), so a dep change, a rubric edit or a profile edit invalidates attestations implicitly — no manual `--no-cache`. If `putnami projects describe` is unavailable the dep set is unknown, so the hash is marked `-dirty` and the project re-scans rather than skip on a partial hash.

**Shared Intelligence tier.** When the root Putnami manifest enables `options["@putnami/cloud"].intelligence`, ephemeral web sessions and CI reuse each other's verifications through `putnami cloud audit-attest` (keyed by repo + project + group + input hash + rubric hash). After a local miss `check` consults Intelligence and seeds the local ledger on a hit; `record` mirrors best-effort. This remains a pure, fail-open optimization — unavailable Intelligence degrades to local-only and never fails an audit. `fleet.sh` batch-prefetches the ledger in one ordered request before dispatch (also skipped under `--no-cache`).

The helper refuses to record `-dirty` hashes, so a dirty working tree is always re-scanned. Report skipped pairs in the summary — a silent skip reads as coverage.

### 3. For each property group (in scope priority order)

Scan for findings in each property. **Read the code, understand intent, trace data flow** — do not just grep. Putnami projects can run real production traffic and infrastructure; a pattern match is not evidence. When a property says "check", read the relevant code and evaluate whether it meets the bar. Ground findings in the repository profile, constraints, commit history, and runbooks. When hosted Intelligence tools are configured, prefer their context, impact, related, search, and symbol tools for structural orientation; grep stays for literal patterns.

**Mechanical properties are script-run, not model-run.** Run `bash .agents/skills/audit/scripts/mechanical.sh <project-path>` once per project. It deterministically emits JSONL findings for `complexity-file-size`, `complexity-nesting`, `complexity-dependencies` (import fan-out), and raw-print `ops-logging` occurrences, then runs the repository checks script when the workspace sets `checks`; those lines carry the property ids the profile defines. Do not re-derive these by reading code — triage the script's output instead (e.g. a CLI output writer printing to stdout is not a logging violation; drop it) and take what survives through step 4 like any other finding.

#### Property groups and what to check

---

**security** (`sec-authz-enforcement`, `sec-token-identity`, `sec-secrets`, `sec-injection`, `sec-validation`, `sec-error-exposure`, `sec-defaults`, `sec-boundary-leak`, `sec-serialization`):

Treat authentication as a generic subdomain: its language is clients, principals, scopes, grants, tokens, and audiences. Repository-specific domain boundaries and identity infrastructure belong in the repository profile, when present.

- `sec-authz-enforcement` — On Go `api.Endpoint(...)`, `.Secure(scope)` is runtime enforcement and the single source of truth for HTTP permission scopes (TS: `.secure(...)`). Check that every route touching sensitive data declares its scope there, that handlers don't re-implement the same JWT scope check, and that default auth-exclude paths are narrow (`/_/health`, not `/_/` — which would publish `/_/openapi.json`). Routes that fail open: severity critical.
- `sec-token-identity` — Token and workload-identity handling: precedence, verification, introspection, issuer/audience matching, exchange allowlists, and tenancy binding. Identity flows fail closed and callers request only the audience and scope they need. A missing allowlist entry or over-broad audience: severity high.
- `sec-secrets` — Check for hardcoded secrets, keys, and tokens in source or configuration. Secret-bearing values use the repository's protected configuration path, never committed files or undocumented plain-environment fallbacks.
- `sec-injection` — SQL/shell/template concatenation. Postgres access must use parameterized queries (`$1` placeholders / query builder), never string-built SQL. Any raw interpolation of request data into a query: severity critical.
- `sec-validation` — Public and infra-facing API handlers must validate input at the boundary (schema on bodies, typed path/query params). Do not put required inputs in DELETE request bodies — use path/query params (proxies drop DELETE bodies).
- `sec-error-exposure` — Responses must not leak internals: stack traces, file paths, SQL text, or DB column names in user-facing errors. Go `errors.Error` `Stack()` must not serialize to HTTP; TS `HttpException` must not expose the `cause` chain in the body.
- `sec-defaults` — Secure-by-default: request-size limits, timeouts, `X-Content-Type-Options: nosniff`, cookie `HttpOnly/Secure/SameSite` on the web surfaces. Insecure default that users must opt out of: severity high.
- `sec-boundary-leak` — DDD boundary integrity: generic subdomains must not embed product-domain literals or rules, and one domain must not import another domain's internals. Cross-domain concept leak in a generic subdomain: severity high.
- `sec-serialization` — Unsafe deserialization: `json.Unmarshal` into `interface{}` for untrusted input, `JSON.parse` without schema, prototype-pollution vectors.

---

**operations** (`ops-deploy-idempotency`, `ops-provisioning-order`, `ops-config-resolution`, `ops-cold-start`, `ops-retry-backoff`, `ops-graceful`, `ops-migration`, `ops-health`, `ops-logging`, `ops-errors`, `ops-metrics`, `ops-tracing`):

Operational reliability is defined by how a project deploys, provisions, resolves configuration, and observes itself: observable by construction, idempotent, and recoverable. Apply repository-specific platform conventions from the repository profile.

- `ops-deploy-idempotency` — Deploy/release paths must be safe to re-run, serialize conflicting work, wait for prerequisites, and recover cleanly after partial failure. A release step that collides on concurrent deploy or strands intermediate writes: severity high.
- `ops-provisioning-order` — Resource lifecycles must declare dependencies and provision in order. Check ad-hoc chains that assume prior state and conflated provisioning/serving identities.
- `ops-config-resolution` — Configuration, secrets, and managed-resource values must compose through the repository's declared resolution seams without hidden environment side channels, duplicate providers, or blank-default regressions.
- `ops-cold-start` — Startup paths must tolerate cold or temporarily unavailable peers with bounded retry/backoff instead of one eager request that makes the service fail to boot.
- `ops-retry-backoff` — Cross-service I/O needs a timeout and bounded retry with backoff where retry is safe. Missing timeout on any cross-service I/O: severity high.
- `ops-graceful` — Signal handling and cleanup: `SIGTERM`/context-cancel drains in-flight work, closes pools/handles, bounded shutdown timeout. Jobs must exit non-zero on failure so the deployer sees it.
- `ops-migration` — Schema changes ship a feature-owned `migration.Source` (embedded SQL under `<feature>/internal/migrations/`) surfaced via `MigrationContributor`; no central migration list edits. Check ordering/idempotency and that composer plugins aggregate sub-plugin sources.
- `ops-health` — Health, liveness, readiness, and version endpoints use the project's standard platform support. Readiness reflects real serving capability, not a static success response.
- `ops-logging` — Use the project's structured logger rather than raw printing. Log calls carry level, structured fields, and the relevant request/tenant/trace context.
- `ops-errors` — Errors preserve cause and add actionable context at boundaries. An operational failure must be diagnosable from logs alone.
- `ops-metrics` — User-facing operations emit signals: HTTP latency/count/error-rate, DB query duration, pool utilization. Missing metrics on a serving path: severity medium.
- `ops-tracing` — Trace context propagates through middleware and DB calls; `tenant_id`/`workspace_id` are first-class span attributes on every request (architecture §5).

---

**testing** (`test-coverage`, `test-contracts`, `test-concurrency`, `test-edge-cases`, `test-integration`, `test-regression`, `test-determinism`):

- `test-coverage` — Run `putnami test <project> --output=jsonl --no-cache` and parse coverage. Flag exported Reader/Writer methods and handlers with 0% coverage. Coverage below 60% on a lib carrying domain logic: severity high.
- `test-contracts` — Every feature `Reader`/`Writer` interface and every API endpoint has tests covering normal, edge, and error paths — testing the contract, not the implementation. Breaking a published contract (API scope, DTO, registry protocol) without a test catching it: severity critical.
- `test-concurrency` — Go: packages with `sync.Mutex`/channels or deploy-concurrency guards need concurrent test scenarios and `-race`. TS: async races over shared mutable state. Concurrency-guard code with no concurrent test: severity high.
- `test-edge-cases` — Missing error-path, nil/empty, and boundary tests. Error constructors and wrapping paths must be exercised.
- `test-integration` — Real-dependency integration where it matters: DB-backed repositories tested against Postgres (not mocked), storage against a real/emulated bucket, deploy planner against fixture manifests. A repository with only mocked tests: severity medium.
- `test-regression` — Recent fix commits (`git log --grep=fix`) should each add a regression test. Repeated deploy/config/provider regressions must be pinned. A bug fix without a regression test: severity medium.
- `test-determinism` — No time/randomness/port/filesystem races in assertions. Cache/content-addressing tests must be byte-deterministic.

---

**performance** (`perf-algorithmic`, `perf-allocation`, `perf-io`, `perf-n-plus-1`, `perf-caching`, `perf-regex`, `perf-startup`, `perf-hot-path`):

- `perf-algorithmic` — O(n²)+ over collections of unknown size (route matching, manifest/blob lists, project graphs).
- `perf-allocation` — Allocations in hot loops / per-request: `fmt.Sprintf` on hot paths, per-request `JSON` marshal without need, slice growth without pre-alloc.
- `perf-io` — Synchronous/sequential I/O that should be parallel or batched; buffering an unbounded blob or request body in memory instead of streaming or redirecting it.
- `perf-n-plus-1` — DB queries inside loops (`Query`/`QueryRow` in `for`, `repository.find()` in iteration).
- `perf-caching` — Repeated expensive work uncached: config parse per request (use the per-pod resolve cache), schema compile per validation, missing remote-cache reuse.
- `perf-regex` — `regexp.Compile`/`new RegExp` inside per-request functions; compile once at package level.
- `perf-startup` — Import/initialization side effects, eager loading of optional features, and expensive work before readiness increase startup and scale-out latency.
- `perf-hot-path` — Per-request middleware/DI overhead: re-resolving singletons per request, closures capturing large objects, `context.Context` recreated per middleware.

---

**design** (`design-feature-decomposition`, `design-exposition`, `design-reader-writer`, `design-thin-workload`, `design-route-registration`, `design-provider-ownership`, `design-api-contract`, `design-domain-boundary`, `design-retired-roots`, `design-types`, `design-errors`, `design-stability`, `complexity-file-size`, `complexity-function-size`, `complexity-nesting`, `complexity-dependencies`):

The repository's declared architecture and profile are the contract. New code follows them; existing code migrates when touched — flag violations in **new or modified** code, not untouched legacy. When no local architecture is declared, apply the portable Putnami conventions below without inventing retired paths or ownership rules.

- `design-feature-decomposition` — Libraries decompose by **vertical feature**, not horizontal layer. Flag top-level `handlers/`/`service/`/`repository/`/`types`/shared `migrations/` that mix concepts (architecture §2 anti-pattern) in new/changed libs.
- `design-exposition` — Controlled exposition: Go concretes live under `<feature>/internal/`; TS privacy via `package.json#exports`. A workload importing `@putnami/<lib>/src/...` or an unexported subpath, or an accidentally-exported internal: severity high.
- `design-reader-writer` — Each feature publishes `<Feature>Reader` (side-effect-free) and `<Feature>Writer`; read-only middleware never sees a Writer.
- `design-thin-workload` — Workloads compose feature plugins and own only boundary concerns (middleware order telemetry→auth→tenancy→handlers, `/healthz`, OpenAPI). Handlers depend on Reader/Writer interfaces, never concrete `*Service`. Provider/business behavior in a workload: severity medium.
- `design-route-registration` — One route-registration path per workload. Straddling `go.putnami.dev/api` endpoints and per-feature `RegisterRoutes`/ad-hoc HTTP mounts lets OpenAPI drift from production routing: severity high.
- `design-provider-ownership` — External integrations are packaged by provider ownership first: one provider feature owns its callbacks, tokens, persistence, and migrations. Introduce a shared broker/interface only when a real second provider exists.
- `design-api-contract` — Prefer interoperable HTTP contracts: identifiers in path/query not DELETE bodies; expose OpenAPI intentionally with narrow auth-exclude. Consumers get facts + leased tokens, never raw credentials.
- `design-domain-boundary` — DDD boundaries: generic subdomains (auth/IAM) stay agnostic of core-domain concepts; a domain never depends "upward" or sideways into another domain's internals; integrate via published contracts (`go.putnami.dev/protocol/*`). (Also flagged under `sec-boundary-leak` when it's a security surface.)
- `design-retired-roots` — Do not add files under roots declared retired by the repository profile or constraints. When no retired roots are declared, do not infer them.
- `design-types` — Go: `interface{}`/`any` in public APIs where a typed interface fits. TS: unjustified `as any`/`@ts-ignore`/`@ts-expect-error` (each needs a comment), missing explicit return types, unbounded generics. Config-schema structs must stay in the owning workload package (moving one out empties its generated schema).
- `design-errors` — Structured, actionable errors: Go `errors.Error` with defined code constants (not bare `fmt.Errorf`); messages that name the offending input and the remedy. "invalid input" with no context: severity medium.
- `design-stability` — Observable identities (package names, image names, service hosts, artifacts, and protocol shapes) must not change silently. A rename that misses a live override or consumer is an unannounced break: severity high.
- `complexity-file-size` — Flag files > 500 lines (Go) / 400 (TS).
- `complexity-function-size` — Functions > 50 lines or cyclomatic complexity > 10.
- `complexity-nesting` — Nesting depth > 4.
- `complexity-dependencies` — Files importing > 8 internal packages (god-file fan-out) or packages imported by > 10 files (fan-in — consider splitting).

---

**developer-experience** (`dx-doc-coverage`, `dx-adjacent-docs`, `dx-runbook`, `dx-cli-output`, `dx-errors-actionable`, `dx-config-schema`, `language`):

Automation is a first-class user; operators run these services in production.

- `dx-doc-coverage` — Exported Go/TS symbols carry doc comments (godoc/JSDoc) describing behavior, params, and error conditions. `@internal`/`@experimental` on unstable APIs.
- `dx-adjacent-docs` — User-facing changes update adjacent docs in the same owning domain (constraints rule). A new CLI verb, config field, or endpoint without doc update: severity medium.
- `dx-runbook` — Operational surfaces such as deploys, break-glass access, secret rotation, and cutovers have a current runbook in the repository's documented location. Incident procedures that live only in tribal knowledge: severity medium.
- `dx-cli-output` — CLI commands support `--output=jsonl`; errors include file/line/column for diagnostic tools; progress uses structured events. Do not name an extension command flag after a putnami builtin (it silently shadows).
- `dx-errors-actionable` — Can an operator/AI self-correct from the error alone? It should say what was expected vs received, which input failed, and what to do (e.g. "identity is not entitled — add SA to the token-exchange allowlist").
- `dx-config-schema` — Config structs generate a truthful schema (`putnami describe`). Flag config that won't round-trip or that relocated out of its workload package and emptied its schema block.
- `language` — Every GitHub artifact, code comment, and doc is in English. This property is script-run and workspace-wide, not per project. It applies when the shared detector `.agents/skills/check/scripts/english-only.sh` exists (the `@putnami/contributor` extension installs it); without it, report the property as not checked. Run it once per audit run whenever the developer-experience group is in scope, and skip it for `--scorecard-only`:
  ```bash
  bash .agents/skills/check/scripts/english-only.sh files          # tracked files
  bash .agents/skills/check/scripts/english-only.sh tasks          # open, in-progress and blocked tasks, through `putnami tasks find`
  git fetch --prune origin
  bash .agents/skills/audit/scripts/english-only-proposals.sh      # open and draft proposals into origin's default branch, through `putnami proposals find`
  ```
  The proposals contract finds a proposal by its exact base and head, so `english-only-proposals.sh` takes the heads from origin's remote-tracking branches (hence the fetch) and scans each proposal's title and body with the detector's `text` mode. A proposal into another base is scanned only with `--base <branch>`. It asks about the 200 most recently committed heads (`--heads <n>` changes the bound, and the summary counts the heads left out); a head whose find answers `unavailable` is skipped, named on stderr and counted, and the scan goes on. Without an offender, an unavailable head makes the scan exit 2: the scan is incomplete, not clean. Every exit status means the same for all three scans: exit 0 is clean, exit 1 lists the offenders on stdout, and exit 2 is a tool failure — stop and report it, never read it as clean. The detector already skips test files, `testdata/` and `fixtures/`, allowlisted proper nouns, and its own tracking issue. Triage what remains: a foreign word quoted as an example inside an issue about language is a legitimate drop.
  File every surviving offender in **one** issue, never one per offender, with fingerprint `audit-fp: workspace/language/english-only` and title `[workspace] Non-English text in GitHub artifacts, code comments, or docs`. Route it as in 4b: when it is open, replace its Evidence with the current listing; when none exists or the last one closed as completed, create it with `--label "group/developer-experience" --label "prop/language" --label "area/all" --label "severity/medium" --label "priority/p2" --label "status/audit-finding" --label "source/audit"`, plus the issue type the profile names. It is always `priority/p2`: it spans every scope, so no scope's priority order applies. `area/all` keeps it out of the scope scorecards. If the label is missing, create it first with `gh label create prop/language --force --description "Non-English text in a GitHub artifact, code comment, or doc"`. When all three scans are clean and the issue is open, close it as resolved (4e).

### 4. For each finding

#### 4a. Assign severity

- **critical**: security vuln, data loss, production blocker (fails-open auth, credential leak, non-idempotent destructive deploy)
- **high**: significant reliability/security gap
- **medium**: quality improvement, fix when touching the area
- **low**: cosmetic, minor

Severity describes **impact only** — never inflate it for scheduling reasons. (An older severity-boost rule conflated impact with priority and let boosted mediums skip triage; it is gone.)

Assign **priority** as a separate axis, from the position of the finding's group in the scope's priority order (`config.sh priority <scope>`): positions 1-2 → `priority/p1`, positions 3-4 → `priority/p2`, positions 5-6 → `priority/p3`. Priority orders the fix queue; severity states impact.

#### 4b. Fingerprint, duplicates, and waivers

Every finding gets a deterministic fingerprint, stamped into the issue body on its own line:

```
audit-fp: <project-short>/<property>/<slug>
```

`<slug>` identifies the primary subject — the exported symbol under scrutiny, or the file basename without extension (for example `audit-fp: payments/api/sec-token-identity/authorize`). Derive it from the finding's *location*, never its prose, so re-discoveries in later waves produce the identical fingerprint.

**`<project-short>` convention** — read it from `config.sh scopes` (`.shorts`) or `config.sh short <project-id>`; never derive it by hand. It is `<scope>/<leaf>`, where `<leaf>` is the final path segment, or `<scope>/<path under the scope>` when two audited projects of the scope share a leaf. Use it consistently in fingerprints, issue titles, and searches:

| Project | Short name |
| --- | --- |
| `/payments/libs/core` | `payments/core` |
| `/payments/workloads/api` | `payments/api` |
| `/web/apps/console` | `web/console` |
| `/web/libs/cli` and `/web/workloads/cli` | `web/libs/cli`, `web/workloads/cli` |

Search **all states** — closed issues are the system's memory of rejections:

```bash
gh issue list --search "\"audit-fp: <fp>\"" --state all --json number,title,state,stateReason,labels --limit 20
```

Fallback for pre-fingerprint issues (no hit above): `gh issue list --label "prop/<property>" --search "<project-short>" --state all --json number,title,state,stateReason,body --limit 50`, matching on same `prop/` label + project in title + similar file reference.

Route by what you find:

- **Open match** → **update** the existing issue (append new evidence, adjust severity). Do not create a duplicate.
- **Closed as completed** → the finding is back: a regression. File a new issue linking the old one (`Regressed: #<n>` in the body) and start severity no lower than the old issue's.
- **Closed as not planned, or labeled `wontfix`** → a **standing waiver**. Do NOT re-file; count it as "waived" in the summary. Override only when the evidence is materially new (different code path, demonstrated real-world impact) AND severity is high or critical — then file with a `Supersedes waiver: #<n>` line explaining exactly what changed since the rejection.

#### 4c. Adversarial check before filing

A false finding is not cheap — it costs a full `$fix` session downstream and erodes trust in the backlog. Before creating any **new** issue, re-read the evidence as a skeptic trying to refute it: does the code actually reach this path? Is the "missing" guard provided by a caller, middleware, or framework default? Does an existing test already pin this behavior? For `critical` and `high` findings, trace the concrete failure path end to end and put the trace in the Evidence section. Findings that don't survive the skeptic pass are dropped, not filed at lower severity.

#### 4d. Create or update issue

**New issue**:
```bash
gh issue create \
  --title "[<project-short>] <description>" \
  --label "group/<group>" --label "prop/<property>" \
  --label "<scope-label>" --label "severity/<severity>" \
  --label "priority/<p1|p2|p3>" \
  --label "status/audit-finding" --label "source/audit" \
  <one --label per entry in <project-labels>> \
  --body "<body>"
```

**Issue type by group**: follow the repository profile and existing issue taxonomy. If neither defines a type policy, use `bug` for demonstrated defects and do not invent new taxonomy labels.

**Auto-confirm rule**: if severity is `critical` or `high`, swap `status/audit-finding` for `status/confirmed` so `$fix` or an explicit `$fix-loop --label status/confirmed` can pick it up.

Issue body template:
```markdown
## Finding

<1-2 sentence description>

## Location

- **Project**: `<name>` (`<path>`)
- **File(s)**: `<file>:<line>`

## Evidence

<code snippet or test output>

## Severity Rationale

<why this impact level — the concrete failure and who it hits>

## Suggested Fix

<concrete suggestion>

---
_Property: `<property-id>` | Group: `<group>` | Scope: `<scope>`_
_Audit: `<YYYY-MM-DD>`_

audit-fp: <project-short>/<property>/<slug>
```

#### 4e. Auto-close resolved findings

For existing open issues on this project + property that no longer reproduce, close with a comment noting the resolution (and the fixing commit/PR if identifiable).

### 5. Update scorecards

> **Branch**: if `--skip-scorecard` is set, stop here and go to step 6. Scorecards will be refreshed by the fleet's post-pass or a later `--scorecard-only` run.

Scorecard bodies are deterministic artifacts — never hand-write them:

Write the **wave narrative** to a file, then pass it to the script:

```bash
bash .agents/skills/audit/scripts/scorecard.sh --narrative-file <file>                  # all scopes + workspace rollup
bash .agents/skills/audit/scripts/scorecard.sh --scope identity --narrative-file <file> # one scope (rollup still refreshed)
```

The script rebuilds each scope scorecard (project × group matrix, by-group table, grades, and the narrative as its "Last wave" section) and the workspace rollup from the open tasks carrying each scope label, through `putnami tasks find`, `create` and `update`. With scorecards off it prints a note and changes nothing.

The narrative records what was scanned, environment caveats (e.g. degraded tooling), notable findings, and what was **verified clean**. Each run replaces the previous one; attestations keep the per-pair history.

### 6. Prevention — graduate recurring finding classes

Count this wave's new findings per property. Any property that fired on **3+ projects** is a lint rule waiting to exist; fixing instances one at a time is the expensive alternative. File (or update, if one is already open) a single graduation issue. When the repository profile or constraints require the user's approval before a tooling or process issue is filed, put the draft in the wave summary instead, and file it only once approved:

```bash
gh issue create \
  --title "[workspace] Graduate <property> recurrences into a deterministic guard" \
  --label "prop/<property>" --label "area/all" \
  --label "source/audit" --label "status/audit-finding" --label "severity/medium" \
  --body "<the instances found this wave + the proposed lint rule / mechanical.sh check / framework guard>"
```

`area/all` keeps these meta-issues out of the per-scope scorecards (`scorecard.sh` queries each scope label). Kill the class, not the instance.

### 7. Print summary

```
Audit complete: <N scopes>, <N projects>, <N groups>

  Created:   N new issues
  Updated:   N existing issues
  Closed:    N resolved issues
  Waived:    N (standing wontfix — not re-filed)
  Skipped:   N (project, group) pairs unchanged since last attestation
  Unchanged: N

Scorecards: updated <N> | off
```

## Reconcile mode

Triggered by `--reconcile`. Do not scan for new findings. For each open `source/audit` issue in scope (`gh issue list --label source/audit --state open ...`, plus any `--scope`/`--group`/`--project` filter), locate the finding from its `audit-fp:` line and Location section, re-read the referenced file(s), and decide if it still reproduces. If resolved, close it:

```bash
gh issue close <number> --comment "Resolved as of <sha> (<commit subject or PR>). Finding no longer reproduces at <file>:<line>."
```

`--dry-run` prints the close list without acting. Finish with a summary of checked / closed / still-open counts.

## Prune mode

`$audit --prune --scope <s>` removes weight. It answers one question per candidate: what breaks if this goes away? When the answer is "nothing", it goes away. The regular property rubric finds work to add; prune finds work to delete. The two never run together.

The repository must be explainable, not merely green. Its consumers are this repository and the checkouts listed in `PUTNAMI_CONSUMER_REPOS` (colon-separated; an empty value when no other repository consumes this one). A symbol, package, flag or page that none of them uses has no reason to exist.

### Typologies

| Typology | Candidate | Truth to restore | Verdict |
| --- | --- | --- | --- |
| `dead` | package or export with zero importer across this repository and the consumer checkouts | delete; unexport when only its own package uses it | auto, except methods (may satisfy an interface), sample-only importers and test-only references |
| `duplicate` | the same function defined in two or more projects | one copy, in the lowest module that both can import | owner names the surviving copy |
| `multi-version` | legacy / v1-v2 / compat / fallback branches | the newer path, alone | owner names the survivor |
| `palliative` | a guard explained by an incident; a test pinning a commit or a stamped version | fix the root cause or delete the guard; a test proves a contract, never history | owner, except pinned repository state (auto) |
| `historical-ref` | a comment, doc or ADR citing an issue, PR or commit | the rule, stated in the present tense, or nothing | auto |
| `comment` | a comment that justifies the code instead of describing it; a lint escape (`//nolint`, `as any`, `@ts-ignore`, `biome-ignore`); a file over 30 % comment lines | a comment states the contract; a lint escape means the code or the rule changes | justification comments auto; escapes and density need the owner |
| `test-scaffold` | test-only packages, fakes, mocks, harnesses; a project with test lines above twice its source | tests through the public contract, on the real component | owner |
| `config-surface` | a `PUTNAMI_*` variable read in code and documented nowhere (the scan knows only this prefix) | a documented config key, or deletion | owner |
| `doc` | an ADR that is superseded, rejected or never adopted; a page linking to a path that no longer exists | ADRs describe the released product, one per settled decision; dangling links go | dangling links auto; ADRs need the owner |

Module granularity and god files are reported as observations in the umbrella issue, never as prune proposals: splitting is design work.

### Workers

Judgment and edits run on different tiers. This extension ships both workers with the skill: `prune-triage` (read-only, strongest tier, high effort) and `prune-apply` (edits, light tier, medium effort), under `.claude/agents/` and `.codex/agents/`. On Codex, the repository registers them in `.codex/config.toml`.

| Step | Who | Why |
| --- | --- | --- |
| Index, scan, gates | script / CLI | no model |
| P3 triage of every `auto: false` row, and of the `auto: true` rows of `dead` and `palliative` | `prune-triage` | a wrong deletion breaks a consumer repository, where this repository's gate cannot see it |
| P5 edits, one call per typology | `prune-apply` | every row is prescribed; the `--projects` gate is the proof |
| Orchestration: slicing, umbrella issue, commits, the one `--impacted` gate | this skill's session | git and GitHub state stay in one place |

Hand each worker a slice, never a scope: at most 150 candidate rows per `prune-triage` call and one typology per `prune-apply` call, and run one orchestrator session per (scope, typology) on large scopes.

### Steps

**P1. Index.** Run `bash .agents/skills/audit/scripts/prune.sh repos` and read the HEAD dates. A consumer checkout older than 7 days is fetched first (`git -C <repo> pull --ff-only`); a missing checkout stops the run (set `PUTNAMI_CONSUMER_REPOS`). Then `prune.sh index` once per session: symbol and import tables under `.putnami/audit/prune/`. The workspace's own packages are the Go module paths and scoped npm names it tracks, so the index needs no configuration.

**P2. Scan.** For each scope in scope, list its projects and scan them:

```bash
bash .agents/skills/audit/scripts/prune.sh scan all \
  $(bash .agents/skills/audit/scripts/config.sh scopes | jq -r --arg s "<scope>" 'select(.scope == $s) | .projects[]') \
  > .putnami/audit/prune/<scope>.jsonl
```

`--typology` narrows the first argument. The script emits candidates, not findings: every `auto: false` row needs a reading, and every `auto: true` row still gets a skeptic pass: in P3 for `dead` and `palliative`, in P5 for the rest.

**P3. Triage.** Delegate to `prune-triage` in slices of at most 150 rows: `auto: true` rows of `historical-ref`, `comment` (justifications) and `doc` (dangling links) skip triage and go straight to P5, where `prune-apply` runs the skeptic pass itself. Every other row is triaged. The worker returns one JSONL line per row with one of three outcomes:
- **apply** — the evidence is complete. A `dead` export with zero references in every consumer repository, a comment that explains history, a link to nothing. These are applied in P5 without asking.
- **verdict** — the removal is right but the choice is the owner's: which copy survives, which branch of a dual path, whether an ADR still describes the product. These become rows in the umbrella issue.
- **drop** — a false positive. Name why in one line (interface implementation, extension binary consumed by manifest, symbol reached by reflection or by a JSON key). Dropped rows are listed at the bottom of the umbrella issue so the next wave does not re-read them.

Triage rules per typology live in the `prune-triage` worker. An export used only by samples or templates counts as unused: samples follow the code they demonstrate, not the reverse.

**P4. One umbrella issue per scope.** Title `[prune] <scope>`, labels: the scope label (`config.sh label <scope>`), `source/audit`, `status/confirmed`, `<project-labels>`, plus the issue type the profile names for maintenance work. The body is regenerated on every batch with `gh issue edit --body`, never appended: a counts table (applied / verdict / dropped per typology, LoC delta so far), then one verdict table per typology with a checkbox per row (`- [ ] path — one-line choice`), then the observations (modules, god files), then the dropped list. The owner ticks a row to approve it and writes the choice on the same line when there are two. The issue is the decision surface; the proposal is the delivery surface. No per-finding issue, ever. Under `--dry-run`, print the body instead of creating or editing the issue, then go to P6.

**P5. One batch branch per scope.** Only with `--apply`; without it, go to P6 once the umbrella issue is current. Branch `prune/<scope>` from `origin/main`, a draft proposal opened at the first commit and kept open while batches land. One commit per typology, in the order `dead`, `multi-version`, `duplicate`, `palliative`, `test-scaffold`, `comment`, `historical-ref`, `config-surface`, `doc`: deletions first, so later typologies do not touch files that are about to go. Scopes run in parallel; typologies within a scope run in sequence, because they touch the same files.

Each commit message names what it deletes, in English. The type is `refactor`, a Conventional Commit type, because a prune keeps behavior; a typology that drops a published export uses `refactor(<scope>)!`:

```
refactor(<scope>): prune <typology> — <N> removals

Deletes: <package or file list, or "exports: A, B, C in pkg">
Keeps: <the surviving copy or path, when a verdict chose one>
Verdicts: #<umbrella> rows <ids>
```

Edits are delegated to `prune-apply`, one call per typology, with the `apply` rows and the ticked verdict rows as its input. The worker edits and runs the `--projects` gate; the orchestrator reads its `Deletes:` block into the commit message, verifies the gate record it names, and commits. Rows the worker escalates go back to the umbrella issue as verdicts.

Gate cadence, the expensive part, is what makes batches cheaper than one proposal per finding:
- after each commit: `putnami lint,test,build --projects <changed projects>` (`./putnamiw` when the workspace has one); a red here is fixed in the same commit, never carried;
- before ready-for-review, once: `putnami lint,test,build,validate --impacted --enforce-coverage`; a load flake is re-run alone with `--retry-failed`, never waited out with a timeout;
- no per-finding session.

At the start of every batch, re-read the umbrella issue: ticked verdict rows join the next commit of their typology. The proposal is marked ready when every typology has had its pass and no verdict row is left open, or when the owner says ship. Its body lists the deletions per typology with the LoC delta from `git diff --shortstat origin/main...`, links the umbrella issue, and closes it on merge.

**P6. Summary.** Print, per scope: candidates, applied, verdict (open / ticked), dropped, LoC delta, proposal URL, umbrella issue URL. A prune typology that fired in three or more scopes is a rule waiting to exist: draft it for the repository constraints or a lint rule in the summary, and follow step 6 for filing.

## Parallel runs

A full workspace audit may split independent `(scope, group)` slices across bounded subagents when collaboration tools are available. On Claude Code, `bash .agents/skills/audit/scripts/fleet.sh` runs them as background `claude -p` sessions and performs the scorecard post-pass.

- A scope with **≤4 projects** runs as one combined shard (`$audit --scope <s> --skip-scorecard`).
- A scope with **>4 projects** is split per group (`$audit --scope <s> --group <g> --skip-scorecard`) in priority order.
- Never exceed the session's available collaboration slots; keep one slot for the root orchestrator.

Shards use `--skip-scorecard` because the scorecards are the shared write surface. The root orchestrator runs `scorecard.sh` once after every shard exits. Each (project, group) pair belongs to exactly one shard and gets its own attestation file.

### When to use which

- **`$audit --impacted`** — fast, default for routine work; only re-audits touched projects.
- **`$audit --scope <s>`** — one scope end-to-end, live in the session.
- **Parallel shards** — full-workspace refresh; attestations keep re-runs bounded to changed inputs. Reserve `--no-cache` for intentional full hygiene waves.
