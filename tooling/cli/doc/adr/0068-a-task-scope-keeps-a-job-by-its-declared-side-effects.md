# ADR 0068 — A task scope keeps a job by its declared side effects

- **Status**: proposed
- **Scope**: `@putnami/cli` (`internal/engine` plan narrowing)
- **Supersedes in part**: [ADR 0044](0044-selection-is-task-level.md),
  decision 5, the bullet "`publish`, `deploy`, `validate` and
  `validate-workspace` are never narrowed", for `publish` and `deploy`
- **Decisions**: D-006 of the root `decisions.json`

## Context

An `--impacted` selection can hold a project for some of its tasks only
([ADR 0044](0044-selection-is-task-level.md)). The plan then drops every job
of that project that the scope does not name, except the jobs of four
commands, chosen by name: `publish`, `deploy`, `validate` and
`validate-workspace`.

A command name is not a declaration. An extension that releases through a
command named `ship` has its release jobs dropped by a diff.
D-006 moves every development verb out of the core, so a rule keyed on a
verb's name no longer belongs there.

The four names stand for two different rules:

- `publish` and `deploy` change state outside the workspace. The release-set
  coordinator decides which members a session releases; the diff decides
  what the session verifies. The command traits already state this:
  `sideEffects` is `registry` or `cloud`, declared by the manifest or given
  by the well-known verb's default when the manifest declares no traits.
- `validate` and `validate-workspace` check what the session verified. On a
  project that a `^` edge makes task-scoped, the scope names the `test` tasks
  behind the changed `generate` and never `validate~features` or
  `validate~specs`, so `test` runs and the spec gate gets no criteria
  projection for that project. No declaration states this relation. Every
  first-party `validate` and `validate-workspace` task declares file inputs,
  so a rule such as "a task that declares no file input is never narrowed"
  keeps none of them.

## Decision

1. **A job whose command traits declare side effects is never narrowed by a
   task scope.** The rule reads the job's resolved traits
   (`JobDefinition.Traits.SideEffects` is not empty), so it follows the
   manifest's declaration whatever the command's name. The predecessors of a
   kept job are kept, as before.
2. **`validate` and `validate-workspace` stay never narrowed by name.** They
   are the last name-keyed arm of the narrowing. Proposal 1 states what
   replaces it.
3. **Every first-party command keeps what it kept.** No first-party manifest
   declares traits on `publish`, which therefore takes the `registry` default
   of its well-known verb; `deploy` takes `cloud`; no other first-party
   command declares side effects. A test loads every first-party manifest and
   pins, command by command, that the new rule keeps exactly the jobs the
   four names kept.

## Proposals

These are not decided here. Each one changes what a first-party `--impacted`
plan holds, so each needs the owner's decision.

1. **Replace the `validate` arm with a declaration.** Two routes exist:
   - a command trait that says the command checks the session. The protocol
     has no such field yet; it would come with the other new trait fields in
     one contract rung, and `@putnami/sdd` would declare it;
   - a relation derived from the reserved outputs: the task that writes the
     criteria projection runs wherever a task that writes the verification
     report runs. It needs no new field, but the Go, TypeScript, Cloud and
     client-generator `validate` tasks would then narrow by the inputs they
     declare.
2. **A task that declares no file input and depends on no task is never
   narrowed.** It errs wide, the bias ADR 0044 decision 3 states. It changes
   first-party plans: the Scaffold `package~template` and
   `package~agent-content`, the Cloud `package~cloud-config-member` and
   `package~cloud-site-content`, and the TypeScript `version~version` tasks
   declare no input, so they would run on every task-scoped project that
   plans their command.
3. **Read task-level effects too.** The publication gates classify a job by
   its command traits or by its task's declared `registry` and `cloud`
   effects (`HasExternalEffects`). The narrowing reads the traits only. Using
   the same classification would also keep the Cloud `cloud-deploy`,
   `cloud-publish-config` and `cloud-publish-archives` jobs.

## Consequences

- A command with any name that declares side effects keeps its jobs on a
  task-scoped project. A declaration adds side effects but never removes a
  well-known verb's, so `publish` and `deploy` keep their jobs whatever their
  manifest declares. `--impacted` plans,
  and the `impact-plan` output built from them, follow the declaration.
- A portable request carries its task scopes, and the executing side
  re-plans with the same rule to prove the graph equal. A submitter and an
  executor on the two sides of this change disagree only on a manifest whose
  traits contradict its command's name, and that request is refused rather
  than run on a different graph.
- The core still names `validate` and `validate-workspace` in the narrowing
  until proposal 1 is decided.
