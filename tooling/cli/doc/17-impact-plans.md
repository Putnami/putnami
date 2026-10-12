# Impact Plans

`putnami impact-plan` is the seam an extension uses to ask the engine what a
change impacts, for the commands the extension names, without importing the
CLI:

```bash
putnami impact-plan test,build --base origin/main --output=json
```

It reports the impacted plan of a checked-out commit range: the changed files,
the impacted projects, and every task the engine plans for the named commands.
It plans and executes nothing else.

1. The one positional argument is the command list, comma-separated. Aliases
   resolve as they do for `putnami <command>`. A command named twice, and an
   alias that expands to several commands, are usage errors.
2. A command that no discovered extension declares is a usage error that names
   it, so a misspelled or uninstalled command never yields an empty plan. A
   declared command that plans no task over the range gives a valid empty
   plan.
3. `--baseline`, `--impacted`, `--projects` and `--all` are refused: the range
   selects the projects.
4. No repository remote is needed.

## Revisions

The command requires a clean worktree. `--head` defaults to the checked-out
`HEAD` and, when supplied, must resolve to it. `--base` must be an ancestor of
HEAD. Both are resolved and emitted as full commit IDs; changed files are the
direct `base..head` commit diff, never staged or untracked worktree files.
Rename detection is disabled, so a move always contributes both its removed
source path and added destination path regardless of local Git configuration.
A plan whose planning changed the checkout is refused.

## The document

Structured output is a protocol result-v2 envelope. The ImpactPlan itself is
the envelope's `data` member:

```json
{
  "version": 1,
  "generator": {"name": "putnami", "version": "..."},
  "commands": ["test", "build"],
  "baseSHA": "...",
  "headSHA": "...",
  "changedFiles": ["..."],
  "impact": {
    "directProjects": [{"id": "/app", "name": "app", "path": "app"}],
    "projects": ["..."],
    "transitiveDependents": ["..."]
  },
  "tasks": [{"identity": {"key": "/app:test"}, "deadlineMs": 300000, "resourceClass": {"heavy": false, "cpuWeight": 1}}],
  "cache": {"status": "available", "entries": []}
}
```

`commands` is the list you named, in your order. `impact.projects` is the full
impacted closure. `directProjects` owns at least one changed path;
`transitiveDependents` is the remaining closure. The tasks are every task the
engine plans for the command list over the impacted closure, including the
tasks of a companion command that an extension's `alsoRuns` adds to a command
you named. Each task carries a typed identity, a resolved hard deadline,
declared scheduling resources and ordering edges. A task with no hard deadline
makes plan emission fail closed.

Arrays are deduplicated and sorted before serialization: paths lexically,
projects by ID, tasks by task identity key, ordering edges lexically, and
resources by scope then ID. `baseSHA` and `headSHA` are full commit IDs: 40 or
64 lowercase hexadecimal characters. Environment values, source contents,
command arguments, working directories, output paths, invocation locators, and
secrets are never included in the document.

The document has no `repository` and no `digest`; the document you derive from
it holds both. Without `--output`, the command prints a short summary: the
command list, the range, and the number of changed files, impacted projects
and tasks.

## Cache summary

`cache` is advisory. Local cache presence can change when another run warms or
evicts an entry, so it must never change admission work or allow a runner to
skip a planned task. `status` is one of:

- `available`: every cacheable task has a cache key and local presence hint.
- `disabled`: the caller supplied `--no-cache`.
- `unavailable`: a key would depend on ambient environment/runtime input, key
  computation failed, or cache presence could not be checked. The static
  `reason` identifies which case; no partial entries are emitted.

Cache keys are emitted only when they can be recomputed from the checked-out
revision and the CLI's declared state.

## Consumers

The document, its canonical form and its validation are a published contract,
`go.putnami.dev/protocol/ci` (`protocols/ci`). Go consumers import
`ImpactPlan` and `ValidateImpactPlan` from it; the CLI builds every plan with
the same package. Other consumers can check themselves against the conformance
corpus in `protocols/ci/fixtures/impact-plan`.

A consumer that admits a change by a repository-bound digest derives a
ChangePlan: `ChangePlanFromImpactPlan(plan, repository)` projects a plan onto
the ChangePlan of the same range and stamps its digest. The core CLI emits no
ChangePlan. The ChangePlan, its digest and its corpus are documented in
[`protocols/ci`](../../../protocols/ci/README.md#the-change-plan).
