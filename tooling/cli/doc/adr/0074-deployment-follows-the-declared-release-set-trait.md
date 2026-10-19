# ADR 0074 — Deployment and publication follow declared traits

- **Status**: proposed
- **Scope**: `@putnami/cli` (`internal/engine`, `internal/jobs/publication`,
  `internal/jobs/cachekey`, `internal/jobs/remotecache`,
  `internal/jobs/scheduler`, `internal/jobs/planner`, `internal/machine`,
  `internal/extension`, `internal/cli`) and `@putnami/cli-model` (`jobs`)
- **Amends**: [ADR 0017](0017-one-release-set-plan-against-the-channel-head.md)
  decision 2 and [ADR 0021](0021-publish-version-and-deploy-follow-distribution-v2.md)
  decisions 13 and 14, which name the `deploy` command
- **Decisions**: D-021 of the root `decisions.json`

## Context

ADR 0073 removed the development verbs from the core, and contract 11 gave
commands the `consumesReleaseSet` trait, which the `@putnami/dev` vocabulary
gives to `deploy`. The engine still recognized a deployment by the command
name `deploy`: release-set deploy mode, the distribution policy read, the
head measurement, strict `--impacted` narrowing, the verification half of a
mixed session, environment binding and the release barrier. It recognized a
publication by the command name `publish`, and a release-set member's package
job by the command name `package`, although every first-party `publish`
declares the registry side effect and every member declares its package
publisher and step.

## Decision

1. **A command binds a release set when its resolved traits declare
   `consumesReleaseSet`; the core names no deploy verb.** At run level,
   `extension.CommandConsumesReleaseSet` reports whether a loaded job of the
   command carries the trait, and the release-set options carry the answer
   for the run. At job level, a planned job reads its own
   `Traits.ConsumesReleaseSet`. Such a run reads the distribution policy, is
   never measured against the channel head, widens to the members instead of
   being scoped, has no verification half, and narrows strictly under
   `--impacted` with a channel. With `--env`, only its consuming jobs name the
   workload candidates and receive the workload contract, and each of them
   waits for the release barrier beside a registry publication. A command of
   any name that declares the trait does all of this; a `deploy` that does
   not declare it does none of it.
2. **`--env` and `--release` are flags of a release-set consumer.** The core
   reads them only for a run of a command whose traits declare
   `consumesReleaseSet`, so only such a run, or a task that declares the
   flag itself, keeps them out of the undeclared-flag warning. Any other run
   warns that they are undeclared, as it warns for any flag nothing reads: a
   `deploy` without the trait does not ignore `--env` in silence.

Publication and packages follow the same rule: registry side effects replace
the `publish` name, and declared member routes replace the `package` name.

3. **A run publishes when it names a command whose loaded jobs declare the
   registry side effect; a job publishes when its command traits declare
   it.** `extension.CommandPublishes` answers for a command and
   `jobs.PublishesToRegistry` for a planned job. The run-level answer decides
   the release-set mode, the tag line, the selection check, the distribution
   policy read, the dry-run probe report, archive accounting, the verification
   half of a mixed session and the cache-trust floor. With `--dry-run`, such a
   command alone executes with a synthesized `dry-run` parameter. The
   job-level answer decides the member publication gate, a dependent
   publish's projects, the publication side of the release barrier and which
   uncoordinated jobs a release drops. The machine summary's image build time
   is the project's docker task whose result records no publication.
4. **A member's package job is the command that its publisher's publish
   command runs first, that its package publisher declares with the
   `injectPublishChannels` trait and a step of the member's package step.**
   The publish command is the member's ecosystem profile `publish`, and
   `extension.MemberPackageCommand` resolves the route. The coordinator binds
   the release-set plan to that command's jobs, and the selection fingerprint
   keys its step. No command, or more than one, refuses the member at
   planning.
5. **An extension project may run on itself the commands whose traits inject
   its publish channels**, whatever their names.
6. **The remote cache never shares a job with external effects**: its command
   traits declare the registry or cloud side effect, or its task declares a
   registry or cloud effect (`jobs.HasExternalEffects`). Cache trust rises for
   the registry side effect only. The portable path, the publication outbox,
   the release-plan handoff and the refusal of an unauthorized publication
   read the same predicate.
7. **The engine classifies no task by `runner.PublicationCommands` or
   `cache.SideEffectingTask`.** Both stay in the protocols for version-1
   requests and platform consumers, and the executor still applies the
   protocol's own version-1 publication check to a version-2 request whose
   plan declares a registry or cloud effect. The adapter captures the
   ancestry snapshot at process start for a command that publishes or
   consumes the release set, or for an extension group whose subcommand runs
   such a command, and the engine reads it itself for a bound request that
   carries `invocation.publication`.
8. **The synthesized coordinator nodes keep their `publish~` identities.**
   `publish~open` and `publish~release-set` are session-record identities and
   carry declared traits, so every trait reader classifies them. The release
   node waits for the `publish~open` node as before.
9. **Rejected**: a `package` field on the ecosystem profile, which needs a
   twelfth contract rung; a `packageCommand` field in the SDK member
   declaration, which changes the Go, TypeScript and Cloud emitters; and the
   package publisher's command other than publish that declares the step,
   which is ambiguous where `validate` or `build` declares a package step id,
   as the Cloud and Go manifests do.

## What this amends

- ADR 0017 decision 2: the combined session that keeps its own selection and
  widens the publish selection is a registry publication beside any
  release-set consumer.
- ADR 0021 decisions 13 and 14: what they state of `deploy --env` holds for
  every command whose traits declare `consumesReleaseSet`, and for no other.

## Consequences

- No first-party manifest declares a `deploy` command, and every first-party
  command that reads `--env` or `--release` declares the flag, so no
  first-party run changes.
- A third-party `deploy` whose resolved traits do not declare
  `consumesReleaseSet` loses release-set deploy mode, environment binding,
  the barrier and strict narrowing, and a run of it warns that `--env` and
  `--release` are undeclared. Its traits lack the trait when its manifest
  declares traits without it, or declares none and no loaded vocabulary
  gives `deploy` the trait, as `@putnami/dev` does. Its manifest declares
  `consumesReleaseSet` at contract 11, or the workspace loads a vocabulary
  that gives `deploy` the trait.
- Every first-party `publish` declares the registry side effect, every
  first-party `package` and `publish` declares `injectPublishChannels`, and
  each first-party publish runs `package` first, so every first-party member
  route resolves to `package` and no first-party run changes. Cloud
  `validate` declares `injectPublishChannels` too, but it runs at workspace
  level, so no project runs it on itself.
- A third-party `publish` whose resolved traits name no registry side effect
  loses release-set coordination, the tag line, the dry-run execution and
  probe report, archive accounting, the cache-trust floor and the ancestry
  capture, the remote cache may share its jobs, and a bound request without
  `invocation.publication` runs it instead of refusing it. A third-party
  `deploy` whose traits declare neither the cloud side effect nor
  `consumesReleaseSet` loses the ancestry capture and that refusal too. A
  member whose package route no loaded command declares is refused at
  planning. Its manifest declares the trait at contract 11, or the workspace
  loads a vocabulary that gives it, as `@putnami/dev` does.
- The engine reads no ancestry on its own for a run without
  `invocation.publication`: a terminal run relies on its adapter's capture,
  which reads the commands it parsed, or the flat command that an extension
  group's subcommand runs.
