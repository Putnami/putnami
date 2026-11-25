# ADR 0100 — Admission charges the CPU it grants

- **Status**: accepted
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
