# Lint

**Command:** `putnami lint [project]`

Runs `golangci-lint` and `staticcheck` on your Go code. Their exact versions
are published in [`../tools/versions.json`](../tools/versions.json), so CI can
pre-bake the same toolchain before the lint gate runs.

## Overview

- Runs `golangci-lint` then `staticcheck` (both by default) as two separate processes.
  They cover different rules: the bundled golangci config does **not** enable golangci's
  `staticcheck` linter, so the standalone tool is not redundant with it.
- Reuses only PATH or managed binaries that match the pinned version; stale
  binaries cannot mask the expected tool
- Installs the pinned tool via `go install` only when no compatible binary is
  available; pre-install with `putnami deps install --tag go` for offline CI
- `golangci-lint` supports auto-fix (`--fix` enabled by default)
- Config resolution walks up from project root to workspace root, falling back to the bundled default config
- Retries up to 3 times on `ETXTBSY` (a transient OS-level error on Linux)

**Activation:** Any project containing `go.mod` or `*.go` files.

## Execution Flow

1. **golangci-lint** (if `--tool` is `golangci-lint` or `all`):
   - Resolve binary: matching system PATH → matching managed tools dir → pinned `go install`
   - Resolve config (see [Config Resolution](#config-resolution))
   - Run `golangci-lint run [--fix] [--new] [--config] [--timeout]`
   - Parse output and emit diagnostics
   - Run the [skip guard](#skip-guard) over the project's `_test.go` files, unless `--skip-guard=false`
2. **staticcheck** (if `--tool` is `staticcheck` or `all`):
   - Resolve binary: matching system PATH → matching managed tools dir → pinned `go install`
   - Run `staticcheck ./...` — no check-selection flag, so staticcheck's own default
     set applies (see [Staticcheck's check set](#staticchecks-check-set)). When the
     scheduler batched several projects, one process runs every member's package
     pattern from the shared `go.work` root instead (see
     [Staticcheck batching](#staticcheck-batching))
   - Parse output and emit diagnostics
3. **Documentation links** (if `--tool` is `all` and `--docs-links` is on): see
   [Documentation links](#documentation-links)

### Source mutation is declared, not inferred

The split between the read-only pass and the fixing pass is stated in the
manifest rather than discovered after a job runs. `lint` schedules
`lint-golangci-fix` when `--fix` is set and `lint-golangci-readonly` when it is
not; only the fixing task passes `--fix` to `golangci-lint`, so only it declares
`mutatesSources` — together with the project-scoped `sources` write resource the
planner serializes conflicting jobs on. `staticcheck` has no fix mode, so its
single task serves both pipelines read-only. None of the three produces output
files. That declaration is what lets the planner and the cache know *before* a
job runs that its inputs may not survive it.

After a successful fixing pass, Putnami recomputes its keyed project-source
digest. An already clean tree keeps the same digest and its status can be reused
on the next lint; a pass that changed keyed sources is marked non-restorable,
so every worktree still applies its own edits.

## GolangCI-Lint batching

When the scheduler has several compatible `golangci-lint` jobs ready at the same
time, it runs them as one process instead of one per project. `golangci-lint`
applies a single `--config` per invocation, so the batch groups the selected
projects by their effective config and governing `go.work` root, runs one
process per group over every module root (with `--path-mode abs`), and
attributes each finding back to the owning project. Formatting drift (the
`fmt --diff` gate) and `--fix` write-back are preserved per project.

This amortizes `golangci-lint`'s fixed startup and package-loading cost across
the group. Grouping stays opportunistic — a project is never held behind a
cohort barrier — and is capped at `maxProjects: 12` to keep peak memory bounded.

## Staticcheck batching

`staticcheck` batches too, under the same opportunistic policy and the same
`maxProjects: 12` cap. Where `golangci-lint` needs a config partition,
`staticcheck` does not, and that difference decides the whole design.

**How a batch invokes the tool.** The extension groups the selected projects by
their **governing `go.work` root** — the nearest `go.work` walking up from each
project — and runs **one process per group, from that root**, with one package
pattern per member:

```bash
# instead of: for m in …; do ( cd $m && staticcheck ./... ); done
cd <go.work root>
staticcheck ./go/framework/errors/... ./go/framework/http/... ./tooling/cli/...
```

`GOWORK` is pointed at that same `go.work`, exactly as a solo run in any member
directory would resolve it. A project with **no** governing `go.work` is its own
group and runs `staticcheck ./...` from its own root — byte-for-byte the solo
invocation.

**Why `go.work` root is the only grouping key.** `staticcheck` resolves
`staticcheck.conf` **per package**, from that package's own directory hierarchy,
and applies the resulting check selection to each analysis *result*. A
`staticcheck.conf` next to one member therefore narrows that member's subtree and
only that subtree, batched or not — so unlike `golangci-lint`'s single
`--config`, a shared process cannot leak one project's configuration into
another's. The only thing a group must actually agree on is the directory the one
process runs from and the workspace file it resolves through.

**Attribution.** `staticcheck` reports paths relative to its working directory,
so a batched finding says `go/framework/errors/wrap.go:5:6` where the same solo
run says `wrap.go:5:6`. Each finding is joined back to the group root and charged
to the project with the longest matching root, then rendered workspace-relative —
the same path the solo stream carries. Nested modules need no special handling:
`go`'s pattern expansion stops at a module boundary, so `./go/framework/migration/...`
never yields packages from a nested `migration/migratecli` module, just as the
solo `./...` in that directory does not. A position-less
`-: pattern ./x/...: …` load error names exactly one member and fails exactly
that member. A run that fails with nothing attributable fails the whole group and
carries the raw tail.

**A toolchain mismatch skips one member, not the group.** When the pinned
`staticcheck` is older than a file's Go version requires it cannot type-check
that file, and the solo pass reports the project as *skipped* rather than failed.
The batch reproduces that per **member**: the `file requires newer Go version …`
line carries a position, so it is attributed like any other finding, and only the
member it names is silenced — wholesale, dropping that member's other findings
too, because its solo run drops them as well. Its batch-mates keep reporting
normally. Go version requirements are per file and per module, so this is a
routine mid-upgrade state, not an all-or-nothing property of the one binary:
suppressing the whole group would hide real findings *and* cache each member's
undeserved green under its own key. The group is skipped as a whole only when the
abort is the run's only content.

**Per-project identity is unchanged.** Each member still declares its own
project-scoped inputs and writes its own cache entry under its own key; the batch
key is a *dispatch* grouping and never reaches the cache key. A member whose
sources changed misses while its batch-mates hit, and warm runs spawn nothing at
all. (Adding the `batchable` block does move the task-contract digest once, by
design — cache key v5 covers the batch policy, so entries written under the old
contract cannot serve the new one.)

**No analyzer narrowing.** The batch passes patterns and nothing else. `-checks`
was measured and rejected as a cost lever — see
[Staticcheck's check set](#staticchecks-check-set).

Measured: one process over a 12-module group costs 3.16 s cold / 0.58 s warm against
8.56 s / 3.45 s for twelve processes — 2.7× cold, 5.9× warm — and ~0.29 s per
invocation is pure process and package-listing overhead that buys no analysis.

## Documentation links

`lint` checks every relative link and anchor in the project's `README.md`
files and `doc/` trees. A link to a file that does not exist, a path whose case
differs from the name on disk, or an anchor that names no heading of its
Markdown target is an `error` diagnostic coded `docs-links`, and it fails the
task. Links to web pages and site routes are not checked. Inside a Git work
tree the step reads the repository's candidate cut, so a link to a file Git
ignores is broken, as it is in a clone. A link may name any file of the
workspace, so the step is cached on the input `git:**`: any change to a tracked
or unignored file reruns it, and an unchanged tree replays it whatever the
commit or the branch.

Turn it off for one run with `--docs-links=false`, or for a project with
`"options": { "lint": { "docs-links": false } }` in `putnami.json`, which
also takes the project out of the link check `validate-workspace` runs. The rule
itself, shared by every language extension, is documented in the
[extension SDK](../../../tooling/extension-sdk/docslinks/README.md).

## Skip guard

A test that is skipped for no reason, or because it is flaky or fails on CI,
hides a failure. The skip guard refuses those skips. It runs with the
`golangci-lint` tasks, so `--tool staticcheck` alone does not run it.

| Form | Result |
|------|--------|
| `t.Skip`, `t.Skipf` or `t.SkipNow` outside any `if`, `switch` case or `select` case | error |
| A skip whose reason names flakiness or CI, such as `t.Skip("flaky on CI")` | error |
| A skip under a condition that names CI or flakiness, such as `if os.Getenv("CI") != ""`, `if isFlaky` or `if os.Getenv(EnvCI) != ""` | error |
| A skip in a file whose build constraint names no platform, such as `//go:build integration` with an unconditional skip | error |
| A skip under any other `if`, `switch` case or `select` case condition, such as a platform check (`runtime.GOOS`), a missing dependency (binary, database binding, environment variable) or `testing.Short()` | allowed |
| A skip in the `else` branch of a condition that names CI, when the skip's own condition does not | allowed |
| A skip in a file that some platform does not build: a `//go:build` or `// +build` line such as `linux`, `!windows`, `unix` or `cgo` (not `integration \|\| linux`), or a `_windows_test.go` style name | allowed |

The guard does not judge what a condition checks: any condition that does not
name CI or flakiness counts as a guard. Review a new guarded skip as you would
any other test change.

The receiver is any `*testing.T`, `*testing.B`, `*testing.F` or `testing.TB`
parameter, and a suite's `T()` call. A skip in a subtest or a helper counts by
its own conditions, so a helper that always skips is refused. A
`//go:build ignore` file is never compiled, so the guard does not read it. The
guard reads the files the task's cache key reads: it skips `testdata`, `vendor`, nested
modules and the directories git ignores.

A skip that stays on purpose carries a reviewed exception, on the same line or
on a comment line of its own just above:

```go
if testing.Short() || os.Getenv("CI") != "" {
	//putnami:allow-skip CI provides no docker daemon to this package
	t.Skip("needs a local docker daemon outside CI")
}
```

The lint reports each reviewed exception as a warning, so the review stays
visible. An exception without a reason is an error.

To turn the guard off for a workspace, set the option in `putnami.workspace.json`:

```json
{ "options": { "@putnami/go:lint": { "skip-guard": false } } }
```

The same key in a project's `putnami.json` turns it off for that project.

## Usage

### Run both tools (default)

```bash
putnami lint .
```

### Auto-fix issues

```bash
putnami lint . --fix
```

### Run a single tool

```bash
putnami lint . --tool golangci-lint
putnami lint . --tool staticcheck
```

### Lint only new/changed code

```bash
putnami lint . --tool golangci-lint --new
```

### Custom config file

```bash
putnami lint . --config ./.golangci-strict.yml
```

### Set an explicit tool timeout

```bash
putnami lint . --timeout 8m
```

An explicit value takes precedence over the scheduler-derived value — see
[Timeouts](#timeouts).

## API Reference

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--fix` | `true` | Auto-fix issues where possible (golangci-lint only) |
| `--config <path>` | — | Explicit golangci-lint config file (highest priority) |
| `--new` | `false` | Only lint new/changed code (golangci-lint `--new` flag) |
| `--tool <name>` | `all` | Which tool to run: `golangci-lint`, `staticcheck`, or `all` |
| `--timeout <duration>` | derived from the scheduler deadline; `8m` fallback | Explicit timeout passed to golangci-lint; keep it below the scheduler deadline |
| `--skip-guard` | `true` | Refuse a test skip that runs unconditionally or names flakiness or CI. See [Skip guard](#skip-guard) |
| `--docs-links` | `true` | Check every relative link and anchor in the project's documentation. Runs when `--tool` is `all`. See [Documentation links](#documentation-links) |
| `--remote-build-cache` | `true` | Back the Go build cache with the run's shared object cache when a cache provider offers one. `--remote-build-cache=false` keeps the local Go cache alone. See [Remote build cache](./build.md#remote-build-cache) |

## Timeouts

Two deadlines govern a `golangci-lint` job, and they are deliberately not equal:

| Deadline | Value | Owner | Armed when |
|----------|-------|-------|-----------|
| Scheduler | 600000 ms (10 min) default; a project task override or batch leader can change it | `lint-golangci-fix` / `lint-golangci-readonly` in `putnami.extension.json`, resolved by the CLI | the extension process is spawned |
| Tool | derived from `PUTNAMI_TASK_DEADLINE_MS`; `run.timeout: 8m` is the fallback | golangci-lint | `golangci-lint run` begins |

**The tool deadline must stay strictly below the scheduler deadline.** The
scheduler's clock starts first, so if the two are equal the scheduler always
wins and the job reports an opaque `job timed out` with no cause. With the tool
deadline lower, golangci-lint reports its own `Timeout exceeded: try increasing
it by passing --timeout` first, which names what was slow. The shipped-default
rule is enforced by `TestGolangciToolDeadlineStaysBelowItsSchedulerDeadline`,
which reads both values from the shipped files and walks the normalized pipeline.

When Putnami invokes this extension it exports the resolved scheduler deadline
as `PUTNAMI_TASK_DEADLINE_MS`. If neither the command parameters nor argv set
`--timeout`, the extension derives `deadline - min(120s, deadline/5)` and passes
that value to golangci-lint. The derivation applies to per-project deadline
tuning and to a batch leader's already scaled deadline, so it keeps the tool
below the scheduler rather than leaving a static 8-minute value to bind a
smaller override. An explicit `--timeout` deliberately wins, so it remains the
caller's responsibility to keep it below the task deadline.

The 2-minute gap is the budget for the work inside the scheduler's clock but
outside golangci's: extension spawn and handshake, resolving or installing the
pinned binary, the `fmt --diff` gate the fixing path runs first (7.7 s with a
cold cache on 10 cores, and CI runners are CPU-quota limited), diagnostic
parsing and result reporting.

Passing `--timeout` overrides the derived value. A `--timeout` at or above the
effective scheduler deadline re-creates the collision above, so keep it below
the resolved task deadline rather than assuming the 10-minute default.

Both golangci tasks carry the **same default** 600000 ms deadline. They run the same
tool over the same sources under one shared config that exposes a single tool
timeout, and the fixing pass does strictly more work, so a lower ceiling on
either one would silently become the binding limit for both.

`lint-staticcheck` keeps the 300000 ms default. It was not in the audited set,
`staticcheck` has no timeout of its own, and no observation shows it under
deadline pressure — but its value is in the same locked table, so raising it
later is an equally reviewed change. A batch leader still scales it by the group
size, as it does for every batched task; there is no tool-side deadline to derive
from it, because `staticcheck` has none.

### Why 600000 ms

The previous 300000 ms ceiling was reached *exactly* in production — the
`intelligence-server` check-only lint ended at `300002 ms` with `job timed out`
after 646 cache hits — while the bundled config's `run.timeout` was also 5 m.
Two identical deadlines is why no structured cause was reported.

Measurement set (`n` is too small to call a p95, so these are observed
**maxima**; measured 2026-08-03 on a 10-core Apple Silicon laptop):

| Run | Cache state | Duration |
|-----|-------------|----------|
| `tooling/cli` (167,897 LOC), 10 cores | scratch `GOCACHE` + `GOLANGCI_LINT_CACHE` (cold) | 26.6 s |
| `tooling/cli`, `GOMAXPROCS=2`, `--concurrency 2` | scratch caches (cold) | 86.6 s |
| `putnami lint --no-cache --fix=false` × 3 (`@putnami/cli`) † | warm toolchain cache | 19.2 s, 2.9 s, 1.9 s |
| `putnami lint --no-cache --fix=false` (`@putnami/go`, `@putnami/typescript`, `go.putnami.dev/database`) † | warm toolchain cache | 6.4 s, 5.0 s, 4.5 s |
| Production `lint~golangci-lint-check-only` | cloud runner | terminated at 300 s (true duration unknown) |

† whole-command wall time, an upper bound on the lint step alone.

`n = 8` local runs. The binding limit is the 480000 ms tool deadline, ~5.5× the
86.6 s maximum; the 600000 ms scheduler deadline is ~6.9×. The production
failure is *censored* — the termination hid its true duration — so it is
evidence of an undersized ceiling, not a duration sample; the scheduler raise is
exactly 2× its censored lower bound.

### Batched runs

When the scheduler batches `n` projects into one `golangci-lint` process it
uses the largest member deadline and scales it by `n`. That resolved leader
deadline is exported to this runtime, so the derived golangci timeout scales
with the same batch instead of silently binding on the static config value.

> **Cloud activation.** Raising these deadlines does not change anything for a
> Cloud consumer until that consumer upgrades its pinned `@putnami/go` version
> in its lock; the runner reads both the task deadline and the bundled
> `.golangci.yml` from the locked extension. Publication and the Cloud
> lock/runner rollout are tracked in the downstream qualification issue, not
> here. The rollback path is the same lever in reverse: pin the previous
> extension version.

## Config Resolution

golangci-lint config is resolved in this order (first match wins):

1. `--config` flag (explicit override)
2. `.golangci.yml` or `.golangci.yaml` in the project root
3. `.golangci.yml` or `.golangci.yaml` in the workspace root
4. Bundled default config from `@putnami/go` (`config/.golangci.yml`)

staticcheck does not require a config file and Putnami passes it none. It still reads
`staticcheck.conf` if one exists, resolved per package from that package's own directory
hierarchy — so a `staticcheck.conf` next to a package narrows only that subtree.

## Default Configuration

The bundled `config/.golangci.yml` enables 15 linters (`linters.default: none` plus the
list below). `golangci-lint run -v` prints the resolved set as
`[lintersdb] Active 15 linters: …` if you need to confirm it for a given project.

**Correctness & bugs:**
- `govet` — a named subset of Go vet checks, not all of them: `assign`, `atomic`, `bools`,
  `composites`, `copylocks`, `errorsas`, `httpresponse`, `loopclosure`, `lostcancel`,
  `nilfunc`, `printf`, `shift`, `sortslice`, `stdmethods`, `stringintconv`, `tests`,
  `unmarshal`, `unreachable`, `unsafeptr`, `unusedresult`. `enable-all` is `false`, so
  `structtag`, `nilness`, `shadow` and `fieldalignment` are **not** run.
- `errcheck` — unchecked errors (including type assertions and blank identifiers)
- `unused` — unused code (this is `honnef.co/go/tools/unused`, i.e. staticcheck's `U1000`)
- `ineffassign` — assignments with no effect
- `errorlint` — error wrapping correctness

**Code hygiene:**
- `revive` — opinionated style rules
- `misspell` — spelling mistakes in comments and strings
- `unconvert` — unnecessary type conversions
- `unparam` — parameters that are always called with the same value
- `prealloc` — slice preallocation suggestions

**Security:**
- `gosec` — security issues (tuned for application context)

**Style:**
- `gocritic` — opinionated micro-optimisations and style
- `bidichk` — dangerous bidirectional Unicode sequences

**Formatters:**
- `goimports`, `gofmt`

**Excluded paths:** directories named `vendor`, `node_modules`, `dist` or `.putnami`
inside the project.

- `run.relative-path-mode: wd` makes each pattern match the path relative to the directory
  golangci-lint runs in, not to the config file, which sits in the extension's install
  directory outside the workspace. The directories above the workspace are never part of
  a matched path.
- A solo run executes from the project directory. A batched run executes from the
  `go.work` directory; a project whose `go.work`-relative path an exclusion path pattern
  matches (`paths`, `paths-except`, or a rule's `path` or `path-except`), such as a project
  under `dist/` or `internal/`, runs from its own directory instead. Batched and solo runs
  then report the same findings for every pattern that matches at any directory depth, as
  the bundled ones do; a pattern anchored at the run directory, such as `^vendor/`, does
  not.
- Each pattern is a regular expression anchored on whole directory names, and it never
  matches a `..` segment. `internal/binding/` and `pkg/distance/` stay linted. When you run
  golangci-lint yourself from a symlinked directory, it reports paths that climb to the
  nearest directory the resolved and the symlinked paths share; no pattern matches them.
  `putnami lint` runs from the resolved directory.
- `build` and `bin` are not excluded: an output directory holds no Go file, so such an
  entry can only hide a Go package of that name.

If you write your own config, anchor each pattern the same way. Keep golangci-lint's
default `relative-path-mode` (`cfg`) for a config inside the project. Set `wd` for a config
several projects share, at the workspace root or outside the workspace: under `cfg`, its
patterns would also match the directories between the config and each project.

### Staticcheck's check set

Putnami runs `staticcheck ./...` with **no `-checks` flag**, so the effective set is
staticcheck's built-in default: **141 of the 149 checks** the pinned v0.7.0 binary ships.

| Class | Available | Default |
|-------|----------:|--------:|
| `SA*` — correctness | 95 | 94 |
| `S1*` — simplification | 35 | 35 |
| `ST1*` — style | 18 | 11 |
| `U1000` — unused | 1 | 1 |
| `QF*` — quickfix | 0 | — |

Off by default: `SA9003`, `ST1000`, `ST1003`, `ST1016`, `ST1020`, `ST1021`, `ST1022`,
`ST1023`. The `QF*` quickfix analyzers are **not compiled into the `staticcheck` command**
at all — they ship for editor integrations — so `-checks=QF1001` answers
`Couldn't find check QF1001`. Confirm any of this against your own pinned binary with
`staticcheck -list-checks` and `staticcheck -explain <check>`.

Note that `-checks` is a **post-analysis display filter**: staticcheck always runs every
analyzer and only then drops the diagnostics you did not ask for. Narrowing the list changes
what is reported, never what is computed, so it is not a performance lever.

### Overriding the default

Create `.golangci.yml` in your project root:

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - govet
    - ineffassign
    - unused

formatters:
  enable:
    - gofmt
    - goimports
```

You *can* add golangci's own `staticcheck` linter here — in golangci-lint v2 it subsumes
the former `gosimple` and `stylecheck` and covers `S1*`, `SA*` and `ST1*`. The bundled
default deliberately does not: running both analyzers in one process was measured and
rejected. Merging them cut wall time by 42%, but cost 58% more task CPU, 52% more process
CPU, and 297% more peak resident memory — a bad trade on a shared runner, where the
scheduler's constraint is CPU and memory rather than one job's wall clock. Enabling it in a
project config recreates that trade for that project, and does not stop the standalone
`staticcheck` task from running.

## Tool Comparison

| | golangci-lint | staticcheck |
|--|--|--|
| Auto-fix | Yes (`--fix`) | No |
| Configuration | `.golangci.yml` | Optional `staticcheck.conf`, resolved per package |
| Scope | Meta-linter — 15 linters in the bundled config | 141 checks (`S1*`, `SA*`, `ST1*`, `U1000`) |
| Strength | Breadth — style, hygiene, security, and correctness | Depth — subtle correctness bugs and deprecations |

Use both together (the default) for maximum coverage. They are genuinely complementary, and
**the bundled config does not enable golangci's `staticcheck` linter**, so the standalone
tool is the *only* source of `S1*`/`SA*`/`ST1*` coverage — including every `SA1019`
deprecation warning, the whole `S1*` simplification class, and `SA5008` (invalid struct
tags, which the bundled `govet` subset does not cover because `structtag` is not enabled).

The overlap that does exist comes from other bundled linters reimplementing the same rule:
`unused` is staticcheck's `U1000`; `ineffassign` overlaps `SA4006`; `govet`'s `assign`,
`unmarshal`, `printf`, `sortslice` and `shift` overlap `SA4018`, `SA1014`, `SA1006`/`SA5009`,
`SA1028` and `SA9006`; `gocritic`'s `dupSubExpr` overlaps `SA4000`. That is 9 checks of 141.

## Boundaries

- **Scope**: project-only — workspace-level runs are not supported
- **Auto-fix**: only golangci-lint supports `--fix`; staticcheck always requires manual fixes
- **Tool versions**: [`../tools/versions.json`](../tools/versions.json) is the
  single source of truth for tool package, version, and build Go version.
- **Tool install path**: `golangci-lint` and `staticcheck` are installed to `.putnami/extensions/@putnami-go/tools/` and shared across the workspace
- **Concurrent install protection**: tool installation uses file locking to prevent races when multiple projects lint in parallel
- **Batching**: both tools batch — `golangci-lint` by effective config **and** governing `go.work` root (see [GolangCI-Lint batching](#golangci-lint-batching)), `staticcheck` by governing `go.work` root alone (see [Staticcheck batching](#staticcheck-batching))
