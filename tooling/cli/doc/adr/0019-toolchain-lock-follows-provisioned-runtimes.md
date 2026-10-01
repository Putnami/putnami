# ADR 0019 — Toolchain locks follow provisioned runtimes

- **Status**: accepted
- **Scope**: the workspace lock's toolchain pins (`tooling/cli` install, init,
  `projects create`, `deps`, runtime toolchain resolution) and the Go and
  TypeScript extensions' `workspace-install`

## Context

A Putnami job runs with the exact toolchain release the workspace lock pins.
The pins must reach every workspace that needs them, including a fresh one and
one whose host has no toolchain installed, and must never name a runtime no
workflow provisions. Go and Bun are the provisioned runtimes. Producing
Node-compatible output uses Bun's compiler and needs no Node toolchain.

## Decision

1. **Only Go and Bun are pinned.** The lock writer derives them from `go.work`
   and `package.json#packageManager`. `package.json#engines.node` is not a
   Putnami toolchain declaration. The workspace protocol still reads Node
   entries in v3/v4 locks; the next refresh drops them. The TypeScript
   extension's `--target node` stays supported: it is a compilation target, not
   a provisioned runtime.
2. **A refresh replaces the toolchain map, but keeps the pins declared
   extensions resolve.** A declared extension's runtime reads its toolchains
   from the lock by identity, whether or not the workspace has a project of
   that language. The refresh keeps, verbatim and without a metadata request,
   a pin no declaration derives while a declared extension's runtime resolves
   its identity (`RuntimeToolchainLocks` in `internal/extension`: the
   toolchains of every task and job, the runtime's `runToolchains`, and for a
   local source the prepare toolchains). A declaration wins over the committed
   pin. A pin that neither a declaration nor a declared extension resolves is
   dropped. The refresh never invents a version. A workspace that cannot load
   fails the refresh and leaves the committed pins unchanged.
3. **`workspace-install` provisions toolchains.** A toolchain alias that only
   it needs resolves as optional: a missing pin, a missing host-platform digest
   or no matching candidate yields the unavailable identity, and the job runs.
   The alias is probed again once the run ends, however it ended: a toolchain
   the run installed is reported at info level, a candidate still rejected as a
   warning. An alias another active command also needs resolves strictly, and
   a job of any other command refuses a required alias the run resolved
   unavailable while a lock exists. The unavailable identity keys the run apart
   from every pinned run, and the lifecycle job is uncached. The prepare
   toolchains of an extension declared by path resolve the same way only in a
   run whose every command is `workspace-install`.
4. **Every entry point reaches its pins.**
   - `putnami init` ends with the refresh `putnami install` ends with, after
     its last dependency install.
   - An explicit `putnami install` pins, before its installers, each toolchain
     `go.work` or `package.json#packageManager` declares at a release the lock
     does not pin, then refreshes at the end.
   - The implicit install a command runs in a fresh checkout restores the
     committed lock without a refresh, except that it pins each declared
     toolchain the lock does not pin yet. It keeps every committed entry,
     stamps no CLI protocol version, creates no lock, and makes no request when
     no pin is missing. A declaration that differs from a committed pin needs an
     explicit `putnami install`.
   - The TypeScript extension's `workspace-install` writes
     `package.json#packageManager` as `bun@<version>` of the Bun it installs
     with when the workspace declares none.
5. **Commands that need `go` use the pinned Go.** `putnami projects create`
   (for a Go template) and `putnami deps add|remove` find the go command the
   way a task finds its run toolchains (`jobs.RunToolchainEnvironment`): the
   release the lock pins, through the extension's candidates and environment
   bindings; failing that, a `go` on PATH. When neither exists, they write a
   missing `go.work` from the new project's go directive (create only), pin
   each toolchain `go.work` declares and the lock lacks, run every extension's
   `workspace-install`, and resolve again, otherwise fail with the reasons.
   They do not run `putnami install`. Create drops the memoized workspace after
   it writes the new project, so later steps plan over the new project. The CLI
   names no Go release and no install location; the lock and the extension own
   both.
6. **The Go extension accepts the pinned release exactly** when the lock pins
   one that meets the workspace's go directive; a `go` on PATH that reports
   another release does not satisfy `putnami install`.
7. **Only a planned job needs its pin.** Runtime synchronization leaves
   unresolved a required alias the lock does not satisfy. Once the plan is
   final, and before any job runs or reaches the cache, the engine resolves the
   aliases of the planned jobs strictly
   (`jobs.RequirePlannedRuntimeToolchains`), checking a job of another command
   before a `workspace-install` job. A planned job that needs a missing pin
   fails the run; a run that plans none goes on. The error names both ways to
   get the pin: add a project that declares the toolchain, or run
   `putnami install`. Three cases resolve strictly before planning and still
   fail without the pin: an engine-owned provider launch, the prepare
   toolchains of an extension declared by path, and a job with workspace-once
   activation (`workspace-sync`, `cache-clean`, `cache-gc`).

## Consequences

- Install and pin never contact `nodejs.org` or interpret npm-semver ranges.
- A fresh `putnami init` followed by `putnami install` reaches its pins in one
  pass, and a host with only Putnami installed can create a Go project.
- A workspace that declares `@putnami/go` by path and has no Go project still
  fails `putnami build`; only the source workspace declares it that way.
- Reintroducing a managed Node runtime needs a provisioning owner, an exercised
  workspace and a deliberate protocol change.
