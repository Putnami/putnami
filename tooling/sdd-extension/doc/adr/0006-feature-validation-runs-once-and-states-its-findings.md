# ADR 0006 — Feature validation runs once and states its findings as fragments

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `@putnami/sdd` (`features-validate`, now a step of
  `validate-workspace`, and the `validate` command, which keeps
  `specs-validate`)
- **Implements**: ADR 0134 of `@putnami/cli` (a run fails on the findings of
  the projects it selected) for the feature validation, and a first part of
  ADR 0006 of `protocols/evidence` (evidence is fragments that tasks emit and
  a collector streams)

## Context

`features-validate` was the `features` step of `validate`, a per-project
command. Each copy validated one project, but its read set is the workspace:
the feature identities of every project, the evidence at every root, and the
files every source binding hashes. So each copy was keyed on the workspace's
Git candidate cut, the input `git:**`. This repository ran 123 copies, and
every commit re-ran all of them.

ADR 0134 of the CLI lets one workspace task state its findings as `finding`
fragments in `putnami-evidence.jsonl`. After the session, the CLI fails the run
on the error findings of the projects the run selected. The task does not
decide, and it does not read the selection.

## Decision

1. **One task, a step of `validate-workspace`.** `features-validate` is the
   `features` step of the workspace-once `validate-workspace` command, and
   `validate` keeps `specs-validate` alone. `validate` already runs
   `validate-workspace` through `alsoRuns`, so `validate --projects <p>`,
   `validate --impacted` and the canonical gate still run the feature
   validation.
2. **It validates the projects `validate` activates on.** A project qualifies
   when its root holds `putnami.features.json`, a `features/*.json` document or
   a `specs/*.json` document in the candidate cut, and its resolved extensions
   or direct dependencies name this extension. The task evaluates each one
   exactly as its own task did: a selection of that one project, read through
   the candidate cut, every project at the tree base version `0.0.0`, and a
   revision that names no commit. The evaluations run in parallel on the CPUs
   the scheduler grants the job, and share one reader, which answers each read
   once, and one strict parse of each authored document. Each evaluation
   validates every project's manifests; `protocols/features` answers that
   validation once per manifest content in the process, and an evaluation
   reads feature identities in the authored order instead of copying each
   manifest into canonical order. Their answers merge in project order, so the
   bytes do not depend on how many run at once.
3. **It states every finding, located at the file it names.** Each diagnostic
   of each evaluation becomes one `finding` fragment of source format
   `putnami-sdd`. The rule is the diagnostic code, and the severity and message
   are the diagnostic's own. The subject path is the workspace-relative file
   the diagnostic names, and the member after `#` is the symbol. Equal
   fragments are one. A diagnostic that names no file is placed at the file it
   is about: the `featureAuthority` warning at the project's `putnami.json`,
   and any other at the `putnami.features.json` of the project whose
   evaluation reported it. A finding that concerns several files is stated at
   each: a feature id declared twice at every declaration, and a contribution
   that does not resolve at every capability manifest that declares it and
   every evidence record that references it. A string longer than the 4096
   bytes of `protocols/evidence` is cut at a character boundary. The task writes `putnami-evidence.jsonl`, a required file of
   its command output, on every run, empty when it finds nothing.
4. **The task does not decide.** It publishes no diagnostic and succeeds
   whatever the findings say. It fails only when it cannot read its inputs or
   write the file: the candidate cut does not open, an evaluation reports a
   finding about the workspace view, the reader or the revision, which no
   project's file holds, or the findings exceed the 50000 fragments one file
   holds, which the failure states. It declares `selection: none`: the CLI hands it no
   selection and keys it on no selected project, so it writes the same
   fragments under every selection and one cache entry serves every one. The
   CLI's evidence verdict fails the run once per selected project with an
   error finding, as `<project>:evidence~verdict`, prints one line per counted
   project and rule with the count of its warnings, and records each warning
   in the session.
5. **The key is an interim.** The task keeps the input `git:**`, the
   workspace's candidate cut, and so re-runs on every change of the
   repository. The collector of ADR 0006 of `protocols/evidence` replaces it:
   one identity task keyed on the identity files, and per-project checks keyed
   on their own project.

## What a narrowed run reports

A run fails on the error findings located in the files of the projects it
selected, and on those located in a file no project owns. An unscoped run
counts every project.

Before, a project's task failed on every error its evaluation reported,
wherever the file. Five checks of an evaluation report a file outside its own
project. Each finding now counts for the project that owns the file it names,
and a finding that concerns several files is stated at each:

- **A manifest or a feature document that cannot be read or parsed.** Every
  evaluation reads every project's authored manifests, and it reports one
  that it cannot parse, or a root that holds both forms. Every project's task
  used to fail on it. It now fails a run that selects the project that holds
  the file.
- **A feature id declared twice.** The finding names the later declaration in
  path order, and both declaring projects' evaluations report it. It is stated
  at every declaration, so it fails a run that selects either declaring
  project, as before.
- **A record followed across a project boundary.** An evaluation follows
  another root's evidence document that carries a record for one of its
  features, and the capability manifest of a project that owns a contribution
  such a record references. A finding in one of those files counts for the
  project that holds it.
- **A contribution that does not resolve.** The finding is stated at every
  capability manifest that declares the contribution and at every evidence
  record that references it, so it fails a run that selects any of their
  projects.
- **A file at the workspace root.** The workspace root is a root of every
  evaluation, so every project's task reported its findings. They now count
  for the root project, or, in a workspace without one, under every
  selection, as before.
- **A finding about the inputs.** A finding about the workspace view, the
  reader or the revision names no project's file. The task fails on it
  instead of stating it, so no selection passes over it.

## Rejected alternatives

- **One unscoped evaluation.** Its findings are not the per-project ones. A
  structural error anywhere suppresses every other project's assessment
  warnings, and it would assess projects `validate` does not activate on.
- **The per-project tasks, writing fragments.** They would keep 123 copies
  keyed on the whole cut.
- **Publishing the findings as task diagnostics as well.** A narrowed run
  would show, and the task would fail on, the errors of projects it did not
  select.

## Consequences

- One task replaces the 123 copies. On this repository it writes 2,578
  warning fragments and no error. After an untracked file is added, its
  process runs 0.7 to 1.2 seconds under every selection, where the copies ran
  0.2 seconds for a run that selects one features project, 0.8 for five, 4.7
  for 28 and 21.5 for all 123. A run narrowed to one features project is
  therefore slower, until the per-file keys of the evidence collector let the
  task skip what did not change.
- The task's cache entry holds the fragment file, about 800 KiB here, and one
  entry serves every selection.
- An unscoped run prints one warning line per project and rule, 129 here, and
  the session records each warning.
- `putnami features validate`, the interactive command, still reads the disk
  and reports the HEAD commit. It shares the engine, so it too states a
  duplicate id at every declaration and an unresolved contribution at every
  capability manifest that declares it.
