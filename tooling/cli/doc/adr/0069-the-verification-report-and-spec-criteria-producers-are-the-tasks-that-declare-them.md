# ADR 0069 — The verification report and spec criteria producers are the tasks that declare them

- **Status**: proposed
- **Scope**: `@putnami/cli-model` (`jobs`) and `@putnami/cli`
  (`internal/specgate`, `internal/engine`, `internal/jobs/cachekey`,
  `internal/workspace_state`, `internal/output`)
- **Supersedes**: in [ADR 0026](0026-spec-gate-evidence-scope.md), the
  producer rule of §2 ("The producer is the `test` job whose declaration names
  the report") and the `test` plan of §7. The rest of ADR 0026 holds.

## Context

Two reserved command-output files carry the spec gate's evidence:
`putnami-feature-verification.json`, the observation report, and
`spec-criteria.json`, the criteria projection. A task declares each one in its
v3 declaration. The first-party manifests already do: the Go, TypeScript and
Python `test` tasks declare the report, and the SDD `validate` task declares
the projection.

The CLI still decided by command name in six places. A job wrote the report
only under `test`, and the projection only under `validate`. The collector read
observations only from `test` jobs. A warm hit restored the two artifact
records only for those commands. Coverage rows came only from `test` jobs, the
replay served only `test`, and the session report's reader refused any other
command. The gate attached only to a session naming `test`. The engine planned
`validate` to learn which projects a projection producer applies to, and `test`
to find a candidate's cached report. A third-party command under another name
that declares either file was ignored.

## Decision

1. **A producer is the task that declares the file.** A job writes the report
   or the projection when its task declares a command-output file at the
   reserved filename, whatever its command. `ProducesVerificationReport` and
   `ProducesSpecCriteriaProjection` in `cli-model/jobs` stay the only copies of
   the rule. The collector, the cache lookup and the warm-hit restore all ask
   them.
2. **The producer commands come from the job map.** A command is a report or
   projection producer command when one of its pipeline steps runs a declaring
   task (`VerificationReportCommands`, `SpecCriteriaProjectionCommands`). The
   planner still decides, per project, whether a listed command plans a
   producer.
3. **The gate attaches to a session that names a report producer command** of
   a discovered extension, or `test`. With the first-party extensions that set
   is `test`, so every first-party session attaches as before. The `test` arm
   keeps a `test` pipeline that declares no report under judgment: its
   projects read as consulted and silent and block under enforce (ADR 0026
   §2). Without the arm, that workspace would skip the gate and pass enforce
   on no evidence.
4. **Applicability plans the projection producer commands.** A selected
   project that disables one of them hides whether a producer applies, which
   is an error as before. With no projection producer command, no producer
   applies to any project.
5. **Recovery plans the report producer commands** over the candidate set and
   keys them in one pass. With no report producer command, every candidate is
   consulted and silent.
6. **Coverage follows the measurement.** A coverage row comes from any job
   whose result carries a valid coverage summary, one row per project and
   command. A replay serves a session that names the command that recorded
   it, and the live view attaches it to that command. The session report's
   strict reader accepts a row from any named command.
7. **Two name-keyed arms stay.** A job with no declaration and no pipeline
   step counts as a report producer only when its command is `test`. Proposal
   for the owner: drop this fallback, since every first-party producer
   declares the report. The `test` activation arm of point 3 stays until the
   gate has another fail-closed activation rule.

## Rejected alternatives

- **Attach the gate when the plan holds a producer.** It changes first-party
  behavior: a `test` session whose narrowed or empty plan holds no producer
  keeps a record today and would lose it.
- **Attach the gate only on report producer commands.** It fails open for a
  `test` pipeline that declares no report.
- **A new manifest field naming the producer command.** The declaration
  already states the fact; a second field would drift from it.

## Consequences

- Behavior change: a session report may hold a coverage row from a command
  other than `test`. An older CLI refuses such a report. The report is local
  session state, so the risk is `putnami sessions report` on a machine that
  mixes versions.
- A workspace with no projection producer and a project that disables
  `validate` used to answer "unknown" for applicability. It now answers that
  no producer applies.
- The unconsulted-candidate messages keep their wording ("the test task that
  writes its verification report").
- The run summary still renders coverage only on the `test` line. That
  rendering is outside this decision.

## How this is proven

- `cli-model/jobs/plan_contract_test.go`: the producer rule under any command
  and the producer command lists.
- `internal/engine/spec_gate_test.go`: activation by a `check` command, and
  applicability under a `criteria` command.
- `internal/engine/spec_gate_cache_test.go`: recovery from a `check` entry.
- `internal/jobs/task_capture_test.go`: the warm-hit restore under other
  commands.
- `internal/workspace_state/session_report_test.go` and
  `internal/output/live_test.go`: coverage recorded and replayed under `check`.
- `internal/cli/e2e/specgate/spec_gate_matrix_test.go`: `criteria,check --all`
  verifies, blocks a contradicted check, and blocks again on the warm rerun.
