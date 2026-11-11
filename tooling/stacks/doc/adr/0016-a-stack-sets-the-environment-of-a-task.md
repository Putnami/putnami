# ADR 0016 — A stack sets the environment of a task

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `tooling/stacks` (`@putnami/stacks`), first `@putnami/java`

## Context

Some builds read the location of an SDK from the environment, not from the
step that runs them: the Android Gradle plugin reads `ANDROID_HOME`. The core
gives a stack three hooks, and none sets such a variable for every step of a
task:

- `Provide` (ADR 0003, ADR 0004) answers entries with the toolchain it
  installs, but the core calls it only when the toolchain on `PATH` is missing
  or outside the stated versions.
- `Tools` (ADR 0004, ADR 0006) appends directories to `PATH`, and the core
  does not call it offline.
- `Step` (ADR 0014) adapts the program and the arguments of a step, not its
  environment.

Whether a build needs the SDK, and whether the extension may install it, can
also depend on a choice the workspace's owner commits, such as accepting the
SDK's licenses.

## Decision

- `Stack.Env` answers `NAME=VALUE` entries for the steps of one task of a
  component, after its toolchain and tools are selected. Each entry replaces
  the task's own value of `NAME`.
- It receives an `EnvRequest`: the task, the workspace root, the component's
  metadata, the task's environment, the extension's block of the workspace's
  options (`options` of `putnami.workspace.json` under the extension's name),
  and whether the run is offline.
- The jobs of the extension that select the component's toolchain, a
  workspace install, a `serve`, a `run` or a `package`, call it too, with no
  task: their steps read the same variables.
- An offline run calls it too: the stack decides what it may provide without
  the network.
- Its error, or an entry with no `=`, fails the task with
  `stacks.toolchain_missing`.

## Consequences

- A stack that installs an SDK, or reads one the machine holds, sets its home
  in one place for every step of every task and job, overrides included.
- The workspace's options reach the hook from the job context, so the stack
  reads the policy the orchestrator loaded.
- The entries `Env` answers do not reach the cache key: they locate a tool, as
  the toolchain's home does. The extension's block of the workspace's options
  also reaches its tasks as parameters, which the key folds, so a change to
  the block reruns the extension's tasks once.
