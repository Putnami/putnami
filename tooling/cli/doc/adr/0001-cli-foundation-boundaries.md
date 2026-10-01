# ADR 0001 — CLI foundation boundaries: catalog, result, engine

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli`)

## Context

The CLI publishes the same facts through many surfaces: dispatch, help,
completion, telemetry, JSON, JSONL, MCP, session files and watch. When each
surface keeps its own command table, derives its own counts from raw scheduler
events, or builds its own scheduler, the surfaces drift apart and nothing
detects it.

## Decision

### 1. The command catalog is the single command vocabulary

One catalog, `internal/commandmeta` (`catalog_data.go`: `catalog` and
`globalFlags`), owns paths and subcommands, task aliases, categories,
summaries, examples, related commands, typed flags with defaults and
positionals, structured-output capability and workspace requirements.

Dispatch, text/man/Markdown/structured help, shell completion, alias
validation, JSON capability and the telemetry vocabulary derive from it. The
handler registry in `internal/cli` only binds names to functions, and
`registerCommand` panics on a name the catalog does not declare. A second
hand-maintained table describing commands is a defect.

### 2. One canonical result model, one reducer

`TaskStatus` (success, failed, canceled, skipped) and `ReuseKind` (none,
local-cache, remote-cache, coalesced) are orthogonal. One reducer derives task
and session counts, diagnostics, failures, artifacts, cache summaries, timings
and the aborted state from the canonical events.

Every consumer reaches its wire through an adapter over that model. An adapter
that needs a field the model lacks means the model is wrong; the adapter never
re-parses events. [ADR 0006](0006-cli-model-and-command-verticals.md) places
the model in a module that cannot reach the executor.

### 3. One engine: `Engine.Run(ctx, Request, EventSink) (SessionResult, error)`

The engine owns the workspace and config snapshot, extension resolution,
project selection, planning, preflight, lifecycle hooks, cache setup,
scheduling, session recording, run markers, profiling and cleanup.

The scheduler is an internal execution stage. No adapter constructs or
configures a scheduler. Terminal, extension aliases, MCP, watch and lifecycle
jobs are adapters over `Engine.Run`.

`Request.Stdout` carries every human notice the engine prints ("No projects
matched", the `--plan` table, the auto-selection line, a blocked preflight's
envelope). Nil means `os.Stdout`. MCP sets it because MCP stdout is the
JSON-RPC frame stream, and one stray notice breaks the framing.

### 4. Telemetry seam (binding)

The engine owns an injected observer on `Request`, not telemetry. Only the
terminal adapter supplies an observer; MCP, watch, lifecycle and extension
aliases pass nil. A nil observer is a total no-op: no allocation, no buffer
write.

- `TrackSessionStart` fires only through the terminal adapter's
  `sessionObserver.RunPlanned`, after planning succeeds.
- `TrackSessionEnd` fires once per process in `runTerminalSession`, which
  re-reads persisted consent first.

Rationale: the policy is "never send before notice". A watch iteration or an
MCP tool call is not a user-initiated session. If the engine reported directly,
every adapter would start reporting from surfaces that never had consent.
Adding an observer to a non-terminal adapter needs a new ADR and a consent
review.

### 5. The deliberate self-executions stay

The engine runs work in-process; no run stage re-enters the CLI. The following
self-executions cross a process, version or workspace boundary and have no
in-process equivalent. Do not delete them as recursion.

| Site | Why it stays |
| --- | --- |
| `launch.Relaunch` | Re-execs into the workspace-pinned binary; the point is to run different code. `PUTNAMI_LAUNCHED` guards loops, `PUTNAMI_NO_RELAUNCH` opts out. |
| `launch.IsExemptCommand` / `IsExemptInvocation` | Recovery paths (`pin`, `version list`/`use`, `upgrade --from-source`, `migrate vnext`, `extensions … --user`) run as the invoked binary, because the pinned one may be broken or predate the flag. |
| `extensions.TemplatesTest` | Runs `putnami install` and `build,test` against another, rendered workspace. The nested run is the test subject. |
| `PUTNAMI_WORKSPACE_BOOTSTRAPPED` (`lifecycle/bootstrap.go`) | Install jobs may spawn `putnami`; only an environment variable reaches a grandchild. Bootstrap itself calls install in-process (`installRunner`). |
| `PUTNAMI_ARTIFACTS_ENSURED` (`lifecycle/ensure.go`) | `ensuredWorkspaces` dedupes within a process; the variable dedupes across a nested `putnami` an extension job spawns. |

## Enforcement

- `internal/cli/structural_baseline_test.go` pins the catalog declarations and
  exact call-site counts (`jobs.RunPlan`, `jobs.Plan`, `jobs.ReduceRun`, the
  scheduler constructor, the reducer, `map[string]any` lines). Call-site pins
  fail when a count rises and when it falls without the pin moving, so every
  movement is reviewed. Counts the CLI drives down are ceiling-only.
- `TestCatalog_CoversCommandRegistry` checks catalog and registry agree.
- `internal/cli/engine_boundary_test.go` rejects scheduler construction
  outside the engine.
- `internal/engine/seam_test.go` rejects telemetry imports in the engine and
  observers on non-terminal adapters. `internal/engine/stdout_seam_test.go`
  allows exactly one `os.Stdout` reference, inside `Request.stdout()`.

## Rejected alternatives

- Synchronized command tables: they drift without one derivation point.
- Adapters that own scheduler setup or parse events: each creates its own
  execution and result contract.
- Telemetry inside the engine: non-terminal adapters are not consented
  sessions.

## Consequences

- The surface goldens (`surface_golden_test.go`, `testdata/surface/`) and the
  parse-acceptance table pin help, man, Markdown, structured help, completion
  and valid invocations. They run in-process only: the suite runs with
  `race: true`, where one subprocess re-exec costs about 1 s.
- Invalid input is a usage error (exit 2): an unknown flag for the command, a
  value flag without a value, a bad `--retry`, an unknown `--max-parallel`
  mode. Validation tests pin it, not the goldens.
