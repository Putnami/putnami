# ADR 0100 — Admission charges the CPU it grants

- **Status**: accepted, amended 2026-11-25
- **Date**: 2026-11-24
- **Scope**: `@putnami/cli` (`internal/jobs/scheduler`), `run.orchestration`
  of the `result-v2` contract (`go.putnami.dev/protocol/cli`, one more
  member), features `cli/job-planning-execution` and
  `cli/diagnostics-results`

## Context

The scheduler grants each subprocess a CPU ceiling, exported as
`PUTNAMI_CPU_BUDGET` and `GOMAXPROCS`, and admits it against the machine's
CPU capacity. The two numbers were distinct: a task with no history was
granted every CPU and charged one, and a task with history was charged its
measured occupancy under a larger ceiling. The worker count was twice the
CPUs, three times for a light plan. Nothing reported the sum of the ceilings
running or the machine load.

The rejected option is the one this ADR replaces, a ceiling above the
admission charge. Measurement: `./putnamiw lint,test,build,validate --all
--no-cache --enforce-coverage` at `11783c14`, on a 4-core Linux container
(Intel Xeon @ 2.10 GHz, no cgroup quota) with the task history of a first
run: a run of 17 min 18 s and 3,727 s of task CPU (`run.cpu.actualMs`, session
`20261124-233204-59d6ad`), and a one-minute load read from `/proc/loadavg`
every 15 s outside the run of 16.7 on average and 28.7 at its peak, 4.2 and
7.2 times the CPUs.

Rejected cold rules, each measured by the same gate with an empty task
history on the same machine, against 720 s for `8e2cf03f` (session
`20261125-034836-446584`): charging a cold task what it is granted, seeded by
its task kind (the median occupancy of same-named tasks of other projects,
else half the capacity for a heavy step and one CPU), took 762 s (session
`20261125-040158-f3add1`): the tasks used 2.75 CPUs on average against 3.1,
since a cold task fills less than the whole CPU it is charged. The same with a
quarter of the capacity for a heavy step took 739 s (session
`20261125-041606-070f74`). Admitting up to 1.5 times the CPUs, with as many
workers, took 988 s (session `20261125-042912-330c4e`).

## Decision

1. **Admission charges the ceiling of a task with history.** A dispatch whose
   task has history reserves the CPU ceiling its subprocess is granted, and
   its memory, once its cache lookup missed and before its cache lease and its
   task-output locks, and releases them when the subprocess ends; a task that
   loses the lease gives them back while it waits for the lease owner, unless
   a member of its batch owns a lease, and a follower of a shared node while
   its leader runs. A batch's reservation covers its members that execute and
   those not yet looked up, and shrinks without a wait as members hit the
   cache, so a hit, coalesced or not, ends holding none and is not listed
   among the reservations. A smaller batch whose learned memory exceeds the
   larger one's takes the difference only when it fits at once, never waiting,
   and otherwise runs on what it holds. A batch reserves its shared class
   ceiling once. Those ceilings never sum above the CPU capacity, the logical
   CPUs clamped to a cgroup bandwidth quota. Waiting subprocesses start in the
   coordinator's longest-first order, the first never overtaken. A dispatch
   reserves only its named claims: a cache hit, which starts no subprocess,
   reserves no CPU, and the worker count bounds how many lookups run at once.
   A serve step, which runs until the run is interrupted, reserves no CPU: a
   CPU it held would never return to the finite tasks waiting for it.
2. **A task with no history keeps the first-cold rule.** It is granted the
   whole capacity and reserves one CPU, raised by a configured weight, until
   every tool a job starts honors its grant. A weight scales a history-backed
   task's occupancy before the critical-path lift, never the lift itself, and
   a learned weight lifts the ceiling no higher than the highest occupancy
   the task's history measured, rounded up.
3. **The worker count follows the capacity.** `auto` and `max` run one worker
   per CPU of capacity, `eco` half; usable memory may lower that share. A
   serve step holds its worker until the run is interrupted, so each one adds
   a worker. An explicit count wins. `eco` loses its cap of 8 workers and its
   light-plan raise: 4 CPUs with a light plan run 2 workers instead of 4, and
   64 CPUs run 32 instead of 8.
4. **A run reports what it granted.** `run.orchestration.capacity` samples the
   sum of the running ceilings and the one-minute load from the run's start
   to its end.

## Consequences

- The same no-cache gate at `2e3a9e6e`, against `ea881dbd` before, on a 4-core
  Linux container (Intel Xeon 2.10 GHz, no cgroup quota), the runs one after
  another, each once the one-minute load fell below 2.5, read the same way:
  from an empty task history, 598 s against 620 s, with a one-minute load of
  14.4 on average, 21.0 at the 90th percentile and 23.3 at its peak, against
  14.9, 21.2 and 26.0 (sessions `20261125-060126-5e8f86` and
  `20261125-062749-b5ee15`); from the 29-entry history of the first run, 535 s
  against 537 s, with a load of 13.0, 19.4 and 20.9 against 13.2, 20.1 and
  25.0 (sessions `20261125-061258-214be1` and `20261125-072200-8515ec`).
- An unchanged workspace, from one warm task history: 64.9 s and 65.1 s
  against 74.3 s and 75.9 s before (sessions `20261125-062515-aeaa32` and
  `20261125-062620-84a6c5`, against `20261125-073202-21b3fd` and
  `20261125-073316-38d72a`).
- The load still exceeds 1.5 times the CPUs: a task with no history is still
  granted the whole machine, and tools start more work than their grant,
  such as `go test` on `/go/extension` at 2.7 CPUs under a ceiling of 2. The
  load meets its bound once every tool a job starts honors its budget and a
  cold task is charged what it is granted.
- A worker waiting for CPU holds no cache lease and no task-output lock, so a
  task holding CPU, or a session it started, never waits for a lease or a lock
  its waiter holds. A task that executes after its lease wait timed out takes
  its CPU without a lease; one that runs after the leader it followed did not
  publish waits for CPU while it holds its own lease, and that wait is bounded
  by the lease wait of whoever waits for it.
- The historical ceiling of a task only rises, and admission now charges it:
  a task once granted more than it now uses keeps reserving it until its
  history is reset.
- The critical-path lift still raises the long pole's ceiling; its cores now
  come from the work admission holds back, not from oversubscription.
- Tools that do not honor `PUTNAMI_CPU_BUDGET` still exceed their grant; the
  load the report samples shows them.

## Amendment — 2026-11-25: admission charges measured occupancy

### Context

Measured on a 4-CPU Linux container (Intel Xeon @ 2.10 GHz, no cgroup quota)
after this ADR: a task with history ran slower than from an empty history.
The `--no-cache` gate restricted to 14 projects
(`/go/framework/{client,api,database,app,openapi,grpc,telemetry}`,
`/typescript/framework/{client,application,web,analytics,cli-protocol,ui}`,
`/tooling/cli-documents`) took 93.2 s from the history of a first run against
84.5 s from none. Three causes, read from the runs' task events, capacity
samples and an admission trace:

- **Rounding.** A ceiling is whole CPUs, so a task that measured 1.1 CPUs was
  charged 2: the history of that selection charged 345 CPU-seconds for 222
  used.
- **Head-of-line waiting.** The first waiter is never overtaken and started
  only once its whole charge fitted: behind a lint charged 3.2 CPUs with 2.5
  free, every waiter idled until a test ended, up to 25 s; with charges of 1.0
  to 1.5 CPUs, three tasks left less than one CPU free and the fourth worker
  waited 1 to 4 s at a time.
- **A starved tail.** The last task of a run kept the ceiling its history
  measured while it shared the machine, one CPU, for its last 15 s alone.

### Decision

1. **Admission charges measured occupancy.** Admission counts thousandths of a
   CPU. A task with history is charged the occupancy it measured, CPU over
   wall time, at its granted ceiling when its history holds an observation
   there, scaled by its weight, at least a tenth of a CPU and at most its
   ceiling. A ceiling the critical-path lift raised is charged whole. A task
   with no history is still charged one CPU, scaled by its weight.
2. **A waiting subprocess starts while any CPU is unreserved.** The first
   waiter is still never overtaken, but it starts once CPU is free even when
   its charge exceeds what is free; the charges then exceed the capacity by
   less than its own, and no other waiter starts until they are below it.
3. **A task with history is granted at least the free CPUs.** Its planned
   ceiling is raised to the CPUs the ceilings of the running subprocesses
   leave free, so the last task of a run has the whole machine. The raise is
   not charged: it replaces the rule of decision 1 above that ceilings never
   sum above the CPU capacity, as a ceiling a task fills only while it runs
   alone. Only the planned, unweighted ceiling teaches the history its floor.
4. **A task with no history keeps decision 2 of the original decision.** It
   is granted the whole capacity and charged one CPU: no cold rule measured
   below keeps the load under 1.5 times the CPUs without lengthening the full
   gate.
5. A request that charges no CPU, a dispatch's claims or a serve step, is
   admitted whatever the CPU charges, as before.

### Measurements

The selection gate, `./putnamiw lint,test,build,validate --projects
<selection> --no-cache --enforce-coverage`; A = `main` at `00810ec6`, B = the
option, one git worktree each, each run alone once the one-minute load fell
below 2.0, A and B alternating: from an empty history three runs each, one
seeding run each, then two runs each from the seeded history. The full gate
replaces `--projects <selection>` with `--all`, from an empty history, once
the load fell below 1.0. Medians; the one-minute load is read every 15 s from
`/proc/loadavg`, mean.

Chosen, decisions 1 to 4 at `d9ab7157`:

- Selection from an empty history: 69.2 s [69.2–75.0], load 4.0, against A
  68.2 s [68.0–72.0], load 3.9.
- Selection from the seeded history: 63.8 s [62.8–64.8], load 3.4, against
  A 83.6 s [81.6–85.6], load 2.8. Warm is 8 % faster than cold, where A is
  23 % slower (sessions `20261125-194858-7b0aae`, `20261125-195253-0df0d3`).
- Full gate: 541.0 s, load 13.6 (session `20261125-201955-cbb0cc`), against
  A 527.4 s, load 14.6 (`20261125-203126-d969bc`); A ranged 514.2 to 558.3 s
  over six runs. The first full run of B's tree took 623.2 s: its Go build
  cache was cold for the changed CLI packages, which a Putnami `--no-cache`
  run does not reset.

Rejected for a task with history, each a cumulative step towards the chosen
one, with a task with no history granted only the CPUs the running ceilings
leave free (see below):

- Decision 1 alone, the planned ceiling kept: warm 68.4 s against cold
  61.0 s, the tail at one CPU (sessions `20261125-161933-86d184`,
  `20261125-162313-444ea7`).
- Adding decision 3: warm 70.0 s against cold 61.0 s, the head-of-line wait
  (`20261125-164320-e61b20`, `20261125-164725-6a6e0e`).
- A waiter starting once a whole CPU is free instead of decision 2: warm
  65.8 s against cold 63.4 s, the fourth worker waiting on less than one free
  CPU (`20261125-170836-57fbfc`, `20261125-171230-9ea945`).
- Decision 2 itself in that series: warm 57.9 s against cold 67.0 s and A
  warm 77.2 s (`20261125-173508-684049`, `20261125-173901-df3db9`).

Rejected for a task with no history, on the full gate:

- **The CPUs the running ceilings leave free, at least one**: load 5.2 on
  average, 7.3 at its peak, but 826.4 s (session `20261125-182206-8d4959`)
  and a failure: the machine is busy whenever a cold task starts, so nearly
  every task got one CPU, `/tooling/cli` tests ran 422 s against 211 s and
  one of them, which needs parallelism, failed. On the selection the same
  rule took 61.0 to 67.0 s, as fast as A, with a load of 3.0 to 3.2.
- **Those free CPUs, at least half the machine**: 605.6 s, load 9.3
  (session `20261125-190532-aedad0`), `/tooling/cli` tests 318 s.

Both runs were the first of their trees, so their Go build cache was cold for
the changed CLI packages, which cost the chosen tree 82 s on its first run;
the long pole alone, a task whose ceiling is fixed when it starts and whose
length no history tells, lengthened them more. The kind seed, quarter seed
and 1.5-times admission this ADR's context rejected lengthened it too.

The runs, their reports, sessions, task histories and load samples are in
`evidence/1450-cold/` of commit `bfe46a81` (branch `epic-925/1450-cold`).

### Consequences

- On the 14-project selection, a run from the history of one first run is no
  longer slower than a cold one, and 24 % faster than before. Neither the
  full gate from a history nor later generations of a history were measured:
  a task granted more than its planned ceiling teaches its history the
  occupancy it reached there, which may raise its next ceiling and charge.
- A task with history keeps a deterministic planned ceiling that live peers
  only raise; it records an observation at the ceiling it was granted, and
  its history's ceiling floor stays the planned one.
- The charges may exceed the CPU capacity by less than one waiter's charge,
  and the ceilings granted by the CPUs left free.
- From an empty history the full gate still holds a load of about 3.5 times
  the CPUs: a cold task is granted the whole machine until its first run is
  measured.
