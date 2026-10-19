# ADR 0074 — Deployment follows the declared release-set trait

- **Status**: proposed
- **Scope**: `@putnami/cli` (`internal/engine`, `internal/jobs/publication`,
  `internal/extension`, `internal/cli`)
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
mixed session, environment binding and the release barrier.

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
   waits for the release barrier beside a `publish`. A command of any name
   that declares the trait does all of this; a `deploy` that does not
   declare it does none of it.
2. **`--env` and `--release` are flags of a release-set consumer.** The core
   reads them only for a run of a command whose traits declare
   `consumesReleaseSet`, so only such a run, or a task that declares the
   flag itself, keeps them out of the undeclared-flag warning. Any other run
   warns that they are undeclared, as it warns for any flag nothing reads: a
   `deploy` without the trait does not ignore `--env` in silence.

Publication and packages follow the same rule in a second change, which adds
its decisions to this record: registry side effects instead of the `publish`
name, declared member routes instead of the `package` name, the self-publish
exception, the remote cache, the protocol classifiers and the coordinator
identities.

## What this amends

- ADR 0017 decision 2: the combined session that keeps its own selection and
  widens the publish selection is a `publish` beside any release-set
  consumer.
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
- The `publish` command name still decides the publication side of the
  barrier and of the release-set options until the second change.
  `runner.PublicationCommands` (`publish`, `deploy`) still decides the
  ancestry snapshot, the portable and publication paths and the publication
  outbox, as [ADR 0073](0073-the-core-declares-no-development-verb.md)
  records.
