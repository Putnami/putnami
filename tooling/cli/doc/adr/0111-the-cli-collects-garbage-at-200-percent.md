# ADR 0111 — The CLI collects garbage at 200 percent

- **Status**: accepted
- **Date**: 2026-11-26
- **Scope**: `@putnami/cli` (`cmd/putnami`: every process started from the
  `putnami` binary)

## Context

An unchanged `lint,test,build,validate --all` of this repository keeps a few
hundred megabytes of plan, cache keys, memoized input digests and task records
live in the engine. A CPU profile of such a run, on 2026-11-26 at `1d552f15`
on a 4-core Linux container, put 6.9 s of the engine's 40 s in the garbage
collector's mark workers at Go's default target of 100 percent.

## Decision

1. **The `putnami` binary sets a garbage collection target of 200 percent**
   (`debug.SetGCPercent(200)`) when it starts, before any command.
2. **`GOGC` decides when it is set**, including `GOGC=off`, as it does for any
   Go program. `GOMEMLIMIT` keeps its meaning: the runtime collects when the
   heap reaches the limit, whatever the percent.
3. The target applies to every `putnami` process, the ones the engine starts
   (task and provider call-backs, registry-credential starts, git hooks)
   included. It does not reach the other programs the engine starts.

Measured with `tooling/cli` at `1fab9ddc` (before) and `9b97274a` (after), on
the same container: one priming run, then unchanged runs of
`./putnamiw lint,test,build,validate --all`, three before and two after (the
first run after the change re-executed tasks, as a new CLI build does, and is
left out). Wall is the `time` of the `./putnamiw` call; engine CPU is
`run.orchestration.cpu.engineMs` and the ready wait the largest
`scheduler.readyWait[].maxMs` of each `session.json`; peak memory is the
engine process's `VmHWM`, sampled every 0.3 s.

- Engine CPU: 35.8 s to 31.3 s (median).
- Wall: 23.2 s to 22.5 s.
- Maximum ready wait: 8.7 s to 8.3 s.
- Engine peak resident memory: 474 MB to 742 MB.

## Rejected alternatives

- **400 percent.** Engine CPU fell to 32.1 to 32.6 s and the wall to 23.4 to
  23.5 s in two runs with `GOGC=400` at `1d552f15`: no gain over 200, for a
  larger heap.
- **A soft memory limit instead (`GOMEMLIMIT`).** A limit has to be chosen per
  machine; a percent scales with the workspace. A machine short of memory can
  still set one.
- **Leave Go's default.** A sixth of the engine's CPU on an unchanged run.

## Consequences

- The engine's peak memory grows about 1.6 times on this repository. A
  machine short of memory sets `GOGC=100`, or a `GOMEMLIMIT` that keeps most
  of the CPU gain.
- A `GOGC` a user exports reaches every Go program the engine starts, not only
  the engine.
