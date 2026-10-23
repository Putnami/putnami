# ADR 0072 — The spec gate and task scopes follow declared outputs and command kinds

- **Status**: proposed
- **Scope**: `@putnami/cli-model` (`jobs`) and `@putnami/cli`
  (`internal/specgate`, `internal/engine`, `internal/jobs/cachekey`,
  `internal/workspace_state`, `internal/output`)
- **Supersedes in part**: in [ADR 0026](0026-spec-gate-evidence-scope.md), the
  producer rule of §2 ("The producer is the `test` job whose declaration names
  the report") and the `test` plan of §7; in
  [ADR 0070](0070-a-task-scope-keeps-a-job-by-its-declared-side-effects.md),
  decision 2 ("`validate` and `validate-workspace` stay never narrowed by
  name"). The rest of both holds.
- **Decisions**: D-006 and D-021 of the root `decisions.json`
- **Amended**: 2026-10-23, §6, the first consequence and the
  `impact_scope_test.go` proof: a kept criteria job keeps the jobs it depends
  on and no verification job the scope does not name.

## Context

D-006 settles that the core CLI declares no development verb. D-021 settles
that a manifest declares the kind of each command it adds, with the traits
that replace verb-name checks. CLI contract 11 adds that field: a command's
traits may declare the kind `verification`, `criteria`, `diagnostics` or
`files`. Discovery resolves each command's traits: the manifest's own when it
declares them, otherwise its verb's in the merged vocabulary. `@putnami/dev`
gives `test` the verification kind and `validate` the criteria kind. Cloud
declares both on its own commands, and `@putnami/sdd` declares the criteria
kind on `validate` and `validate-workspace`.

Two reserved command-output files carry the spec gate's evidence:
`putnami-feature-verification.json`, the observation report, and
`spec-criteria.json`, the criteria projection. A task declares each one in its
v3 declaration. The Go, TypeScript and Python `test` tasks declare the report,
and the SDD `validate` task declares the projection.

The core still decided by command name:

- a job wrote the report only under `test`, and the projection only under
  `validate`;
- the collector read observations from every `test` job;
- a warm hit restored the two artifact records only under those two commands;
- coverage rows came only from `test` jobs, the replay served only a session
  naming `test`, and the session report's strict reader refused any other
  command;
- the gate attached only to a session naming `test`, recovery planned `test`
  to find a candidate's cached report, and applicability planned `validate`;
- a task scope never narrowed `validate` and `validate-workspace`
  (`neverNarrowedCommands`), and kept the `test` jobs a kept projection
  producer judges.

## Decision

1. **A producer is a declaring task of a command of its kind.** A job writes
   the report when its command has the verification kind and its task
   declares a command-output file at the reserved filename. A verification job
   with no pipeline step and no declaration is the whole command, so it counts
   too. A job writes the projection when its command has the criteria kind and
   its task declares the projection file. The command's name never decides.
   `ProducesVerificationReport` and `ProducesSpecCriteriaProjection` in
   `cli-model/jobs` stay the only copies of the rule. The collector, the cache
   lookup and the warm-hit restore all ask them.
2. **The gate attaches to a session that names a verification command** of a
   loaded extension (`CommandsOfKind`). A verification command whose pipeline
   declares no report stays under judgment: its projects read as consulted and
   silent, and block under enforce (ADR 0026 §2). The kind is the fail-closed
   activation rule, so no `test` arm remains.
3. **Recovery plans the verification commands** over the candidate set and
   keys them in one pass. With no verification command, every candidate is
   consulted and silent.
4. **Applicability plans the criteria commands that declare the projection**
   (`SpecCriteriaProjectionCommands`). A selected project that disables one of
   them hides whether a producer applies, which is an error as before. With no
   such command, no producer applies to any project.
5. **Coverage follows the verification kind.** A coverage row comes from a job
   of a verification command whose result carries a valid coverage summary,
   one row per project and command. A replay serves a session that names the
   command that recorded it, and the live view attaches it to that command.
   The strict reader accepts a row under any named command.
6. **A task scope never narrows a criteria job**, and a kept criteria job
   keeps the jobs it depends on and no other. It keeps no verification job the
   scope does not name: the spec gate reads that job's report from its cache
   entry, and a check no entry answers warns (ADR 0026 §5). The side-effect
   floor of ADR 0070 decision 1 is unchanged.
7. **A workspace that loads the first-party manifests and `@putnami/sdd`
   behaves as before.** With them and the `@putnami/dev` vocabulary, the
   verification commands are `test`, the criteria commands are `validate` and
   `validate-workspace`, and the only command that declares the projection is
   `validate`. A test loads every first-party manifest, applies the
   vocabulary, and pins that a task scope keeps exactly the jobs the names
   kept. Without `@putnami/sdd`, no command declares the projection, and
   applicability answers as the consequences below state.

## Rejected alternatives

- **Keep a `test` arm for activation.** It keeps a verb name in the core,
  which D-006 removes, and the kind gives the same fail-closed activation.
- **Attach the gate when the plan holds a producer.** It changes first-party
  behavior: a `test` session whose narrowed or empty plan holds no producer
  keeps a record today and would lose it.
- **Plan every criteria command for applicability.** A project that disables
  `validate-workspace` would then fail applicability, although that command
  never writes the projection.
- **A new manifest field naming the producer command.** The declaration and
  the kind already state the fact; a second field would drift from them.

## Consequences

Behavior changes only for a manifest whose command name and kind disagree:

- A `test` command without the verification kind, because its manifest
  declares traits without `kind` or the workspace loads no vocabulary that
  gives one, no longer carries the gate, recovery, coverage rows or replay.
- A `validate` or `validate-workspace` command without the criteria kind is
  narrowed by a task scope like any command, and is no projection producer.
- A command of any name with the verification or criteria kind gains these
  behaviors.
- The collector reads observations only from report producers. A pipeline
  step of a verification command that declares nothing but emits the report
  artifact no longer counts, so its project reads as silent.
- A workspace with no projection producer and a project that disables
  `validate` used to answer "unknown" for applicability. It now answers that
  no producer applies.
- A session report may hold a coverage row from a verification command other
  than `test`. An older CLI refuses such a report. The report is local session
  state, so the risk is a machine that mixes CLI versions on one session store.
- A version 1 portable request is re-planned by its executor. A submitter and
  an executor on the two sides of this change disagree on a command whose name
  and kind disagree. The executor then refuses the request rather than run a
  different graph, as the seam already does for any graph difference.
- The unconsulted-candidate messages keep their wording ("the test task that
  writes its verification report"), which is outside this decision. The run
  summary renders coverage on the line of a verification-kind command once the
  live view follows the declared kinds.

## How this is proven

- `cli-model/jobs/plan_contract_test.go`: the producer rule by kind and
  declaration, and the command lists.
- `internal/engine/spec_gate_test.go`: activation by a `check` command of the
  verification kind and none by a `test` command without it; applicability
  under a `criteria` command.
- `internal/engine/spec_gate_cache_test.go`: recovery from a `check` entry, and
  none without the kind.
- `internal/jobs/scheduler/task_capture_test.go`: the warm-hit restore by
  kind.
- `internal/workspace_state/session_report_test.go` and
  `internal/output/live_test.go`: coverage recorded and replayed under
  `check`.
- `internal/engine/impact_scope_test.go`: a criteria command of any name is
  never narrowed, a `validate` without the kind is, every first-party command
  keeps what it kept, and a kept criteria job keeps no verification job the
  scope does not name.
- `internal/cli/e2e/specgate/spec_gate_matrix_test.go`: `criteria,check --all`
  verifies, blocks a contradicted check, and blocks again on the warm rerun.
