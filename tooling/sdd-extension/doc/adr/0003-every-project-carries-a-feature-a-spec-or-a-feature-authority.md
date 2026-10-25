# ADR 0003 — Every project carries a feature, a spec or a featureAuthority

- **Status**: accepted
- **Scope**: `@putnami/sdd` (`feature-coverage-validate`, a step of
  `validate-workspace`), every workspace that sets
  `options.sdd.featureCoverage`, and the protocol modules of this repository

## Context

A feature in `putnami.features.json` and a spec under `specs/` link a
project's code to the outcome it serves. A project that has neither can state
a reviewed `featureAuthority` in its `putnami.json`: an `owner` that names the
feature it serves elsewhere, or `none` with the reason it has no feature.

One check read these answers: `specs.missing_feature_link`, a warning of
`putnami specs validate`. It assesses only the projects that declare a
publication channel or that the support catalog lists, and only in an unscoped
run. The validate gate never runs it. On `main` at 7144249f, 29 of 151
projects carried no feature, no spec and no `featureAuthority`, and the
warning named 7 of them.

## Decision

1. **A step of `validate-workspace` judges every project.**
   `feature-coverage-validate` reads every member the job wire carries, with
   or without a `putnami.json`. A project passes when its own root declares a
   feature or hosts a spec, when its `putnami.json` declares
   `featureAuthority.none` with a reason, or when it declares a
   `featureAuthority.owner` that names an authored feature.
2. **`featureAuthority` is the only exemption.** No rule by tag, type or path
   exempts a project. A template, a sample, a generated client and a protocol
   answer the same question, each in its own `putnami.json`, where a reviewer
   reads the answer.
3. **An owner that names no authored feature covers nothing.** The finding
   names the owner. Otherwise the field becomes the cheapest way to silence
   the step.
4. **The debt list is committed and exact.** `options.sdd.featureCoverage.debt`
   in `putnami.workspace.json` lists the ids of the projects that do not
   answer yet, and a listed project passes. An entry whose project answers
   fails, and so does an entry that names no project, so the pull request that
   pays a debt removes its entry.
5. **The list only shrinks, by review.** The step reads no history, so it does
   not refuse a new entry. A new entry is an edit of `putnami.workspace.json`
   that a reviewer sees, and D-025 states the rule.
6. **The workspace opts in.** Without `options.sdd.featureCoverage`, the step
   passes with a note that counts the projects that would fail. A block the
   step cannot read fails closed: an unknown member, a missing `debt`, an
   entry that is not a project id, or a duplicate entry. In this repository,
   the check of D-025 fails a `putnami.workspace.json` that drops the list.
7. **The step reads the candidate cut.** It reads the manifests and the specs
   through the Git candidate files, so an ignored document covers nothing, and
   the task is cached on the input `git:**`. That key also holds the workspace
   membership with each project's configuration, so declaring a
   `featureAuthority` moves it.

D-026, settled the same day, decides how the protocols answer:

8. **A protocol owns the feature of its contract.** Its spec states what every
   implementation must do: the wire format, the versions, the validation and
   the semantics. Each project that implements the protocol depends on it and
   proves those requirements in its own tests, under a check of its own,
   because the spec gate blocks a check that two projects report. A protocol
   that carries `featureAuthority.none` today keeps it until epic #565 gives
   it a feature.

## Alternatives rejected

- **Extend the specs ratchet.** `specs.baseline.json` freezes requirements
  that exist. It has no entry for a project that declares nothing, so it
  cannot see the gap.
- **A project-scoped step.** A project-scoped task runs only on the projects
  that install `@putnami/sdd`. A project that declares nothing is the most
  likely not to install it, and the step would skip it without a word.
- **A separate debt file with its own protocol.** The list belongs to the
  workspace's SDD configuration. A new file needs a format, a schema and a
  reader, for one list of ids.
- **An exemption by tag or by type.** A rule such as "a sample needs no
  feature" exempts every future sample without a review. A `featureAuthority`
  states one project's reason where a reviewer reads it.
- **Read the history to refuse a growing list.** The step would need the base
  commit, which the `git:**` key does not hold, so a cached verdict would not
  follow the base. Review already sees the edit.

## Consequences

- A new project fails the gate until it declares a feature, a spec or a
  `featureAuthority`, or until a reviewer accepts a debt entry for it.
- A `featureAuthority` now passes a gate, not a warning: a reviewer reads
  every new `none` reason and every new `owner`.
- The `specs.missing_feature_link` warning stays. It applies the same rule to
  fewer projects.
- A manifest or a spec that fails to parse covers nothing in this step. The
  project-scoped validate steps report the document itself.
- In this repository, 8 projects start in the debt list. Epic #565 removes
  them.
