# ADR 0003 — Drift is the generator task's verdict; the guard reads committed inputs

- **Status**: accepted, amended by
  [ADR 0006](0006-the-guard-reads-and-keys-on-the-git-candidate-cut.md)
- **Scope**: `@putnami/clientgen` (`tooling/clientgen-extension`), the client
  outputs of `@putnami/go` `build-describe` and `@putnami/typescript`
  `build-generate`

## Context

`putnami validate` must reject a committed client that is not what the current
contract generates, and a newly handwritten first-party transport, without
anyone remembering a second command. Inside `lint,test,build,validate` the build
has already regenerated every target, or a cache hit restored it, before a
`validate` contribution runs, so a guard that reads the worktree then cannot
see a hand edit. Rebuilding every provider and rendering every target again
inside the guard costs about 10 s per `validate` and cannot be cached.

Each generator task already declares the directory it writes, and the engine
captures it after a run and swaps it in on a hit. The generator is the one
component that sees the pre-write bytes while they still exist.

## Decision

**1. The engine judges drift on the generator's declared output.**
`clientgen-go` and `clientgen-ts` declare `drift: "fail"` on their `client`
output ([`protocols/extension` ADR 0004](../../../../protocols/extension/doc/adr/0004-a-declared-output-may-police-its-own-drift.md)).
So do `@putnami/go` `build-describe` and `@putnami/typescript`
`build-generate`, which write the same directories first in a session: the
policy sits on the FIRST writer, or that writer erases the evidence. The engine
snapshots the directory immediately before the task writes or a hit restores,
compares afterwards, and fails the task with `generated-output-drift` naming the
changed files. The worktree keeps the regenerated client; the remedy is a
commit.

**2. The guard builds nothing and renders nothing.** `clientgen-workspace-check`
judges what no generator task can: every committed manifest against the
committed provider contract (`contractSha256`, operation coverage, service
identity, language), every committed generated file against its recorded hash,
generated-marked files the manifest does not list, the two root inventories,
and the handwritten-transport scan of every consumer source. It reads committed
inputs only: the project index, the committed `schema/openapi.json` sidecar
(over whatever a build last wrote under `.gen`), and the committed
`client.putnami.json` manifests, which also name a provider's targets on a cold
clone. A cold clone and a tree the session just built reach one verdict. The
guard is not cached: its read set is every production source. ADR 0006 amends
this decision: the guard takes its member projects from the job context instead
of the project index, reads the Git candidate cut only, and is keyed on the
input `git:**`.

**3. Activation is workspace-once.** The guard is a `validate` command with
`activation: "workspace-once"`. The planner matches it without consulting any
project's extension list, so no project opts out by not depending on
`@putnami/clientgen`, and exactly one guard is planned per session.

**4. `validate` waits on the `!clientgen` barrier.** The planner plans
`clientgen` for every selected project that declares this extension and orders
the guard after those leaves; `clientgen` depends on `build`. A selected
provider regenerates all its targets, cross-language ones included, under the
engine's drift judgment before the guard reads the tree. A consumer-only
selection plans no generation: drift comes only from a provider or generator
change, and both select the provider under `--impacted`.

**5. Debt is censused by exact count, never by class.** The guard blocks on
every handwritten transport callsite that neither `clientgen.framework.json` nor
`clientgen.external.json` claims. A `clientgen.pending.json` census at the
workspace root, when present, records a count per exact (verdict, file,
transport, symbol); the guard reports those at warning severity, reads the
document strictly, and fails any entry that no longer matches, so the census
can only shrink. This repository has no census.

## Consequences

- Every `putnami validate` in a workspace that installs `@putnami/clientgen`
  scans that workspace's sources. The scan is the guard's remaining cost.
- A new handwritten first-party transport fails the gate. The way forward is
  the generated binding or an inventory entry naming the owner and the reason.
- `drift` is part of the task contract, so changing it is a cold miss.
- `thirdParty` lives only in `.gen/clientgen/config.json`, which a build
  writes. On a cold clone the guard reads an unmarked third-party provider as
  first-party and reports `clientgen.missing-first-party-marker` until
  `putnami build --projects <provider>` runs.
- A provider that `disable.tags` keeps out of `--impacted` (both sample
  providers here carry `e2e`) has its drift judged only when selected
  explicitly. The committed-input checks still run for it on every `validate`.
- `typescript/samples/10-service-to-service/clients/go` holds hand-authored
  tests inside `clientgen-go`'s captured output, so a hit restores the producing
  run's copy and drift reports it. The fix is a declaration naming only the
  generated closure.

## Alternatives rejected

- **Judge HEAD through `git show`.** Misses a drifted worktree, fails an
  uncommitted correct regeneration, and on CI the worktree is HEAD anyway.
- **An engine stage that captures the pre-session tree for the guard.** Keeps
  the nested build and the render, the cost this decision removes.
- **Keep a nested session, made cheaper.** Planning and restoring the
  providers' closure alone stayed above the 5 s target.
- **Cache the guard.** Its read set, every consumer source, cannot be listed.
  ADR 0006 reverses this: the read set is the candidate cut, which `git:**`
  holds.
- **Allowlist debt by folder, file or transport.** A class-based exemption keeps
  matching callsites nobody enumerated, including new ones.
