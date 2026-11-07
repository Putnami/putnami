# ADR 0001 — One extension per stack, on a shared core

- **Status**: accepted
- **Date**: 2026-11-07
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `rust/extension`
  (`@putnami/rust`) and the stack extensions of #934 to #940

## Context

Putnami runs tasks through full extensions for Go, TypeScript and Python. A
team on another stack cannot use its runner and cache, and a full extension
written from scratch costs 20 to 40 agent-hours per stack. `putnami observe`
already finds each stack's components, and the evidence readers already read
its reports. The design is in [the design note](../01-design.md).

## Decision

Each stack gets its own extension in its language root (`rust/extension`,
`php/extension`, …): a manifest, an embedded stack profile and a call to the
stack core, `go.putnami.dev/tooling/stacks`. The core answers the workspace
probe from the stack's observe detector, resolves the profile with the
project's own `putnami.json` override, answers the `<stack>:lint|test|build`
input selectors, and runs the tasks: the toolchain check, the steps, the
reports. Its conformance kit holds each extension's manifest to its profile.

- The observe detectors move to the public package
  `go.putnami.dev/tooling/observe/detect`, and `@putnami/observe` becomes
  importable from every scope.
- Rust is the reference stack.
- A toolchain mismatch fails the task; a project may set `toolchain.check` to
  `warn`. The key carries the expected version, never the machine's.
- A project's override lives in its own `putnami.json`, under the stack
  extension's option namespace.

## Rejected alternatives

- **One `@putnami/stacks` extension for every stack.** One generic option
  namespace, and a project that later moves to a full extension changes its
  options; a stack is not found under its own name.
- **Full extensions from scratch.** The probe, the selectors, the runner and
  the report reading written once per stack.
- **The machine's toolchain version in the key.** It needs a lock-resolved
  toolchain, so a pin and an install per stack.

## Consequences

- A new stack is a profile, a manifest and a detector adapter, held by the
  conformance kit.
- Stack-root files reach the key through each task's static `from:
  "workspace"` input, so an override can narrow them but not add one; files
  under the project reach it through the selector, which an override changes
  freely.
- A component whose stack root is neither the workspace root nor its own
  directory is left out with `stacks.stack_root_unsupported`.
