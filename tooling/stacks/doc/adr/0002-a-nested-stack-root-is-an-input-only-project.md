# ADR 0002 — A nested stack root is an input-only project

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `rust/extension`
  (`@putnami/rust`) and the stack extensions that follow it
- **Amends**: [ADR 0001](0001-one-extension-per-stack-on-a-shared-core.md),
  its consequence on components whose stack root is neither the workspace
  root nor their own directory

## Context

A repository often holds a build of its own below the workspace root: a
Cargo workspace under `services/billing` whose members are
`services/billing/core` and `services/billing/http`. Its members share that
directory's `Cargo.toml`, `Cargo.lock`, `.cargo/config.toml`, nextest
configuration and `target/`. ADR 0001 leaves such a member out with
`stacks.stack_root_unsupported`: a task input selector answers files under
the project only, and the manifest's static `from: "workspace"` input names
files at the workspace root only, so the nested root's files could reach no
key, and a member would keep a cache hit across a lockfile change.

## Decision

The probe answers the directory of a nested stack root as a project of its
own, an **input-only project**, when the root holds no component: a virtual
Cargo workspace. It carries the stack's tag and its metadata names its root
and no component. Its tasks run no step and read no report. Its
`<stack>:<task>` selector answers the stack-root files the task reads, in its
own directory, which it is the root of.

Each component of that root gets a dependency on the input-only project. Every
stack task depends on the same task upstream, so the root's files reach each
member's key through the upstream key of the same task: a `Cargo.lock` change
reruns the members' test and build, and their lint keeps its key.

A stack root that is itself a component, such as a nested Cargo workspace
whose root manifest has a `[package]`, takes the same edge from each member,
unless the root component depends on that member: the edge would close a
cycle, so that member is left out with `stacks.stack_root_unsupported`.

The workspace root keeps the static `from: "workspace"` input of ADR 0001: a
project at the workspace root would own every file no deeper project claims.

## Rejected alternatives

- **A selector answer with workspace-relative files.** The cleanest model, but
  a change to the input-resolve protocol and to every core that reads it, for
  a case the project graph already expresses.
- **A digest of the root's files in the probe metadata.** The metadata
  reaches every task's key, so lint would rerun on a lockfile change; and the
  core re-probes when a candidate directory's inputs move, and the root is no
  candidate until it is a project.
- **Each member as its own stack root.** Its steps run in the shared
  `target/`, so its reports and key would both be wrong.

## Consequences

- A nested workspace adds one project per virtual root, with no step to run.
- A change to the root's files reaches its members through ordinary impact
  propagation over the edge.
- A member of a non-virtual nested root that the root component depends on
  is still left out.
