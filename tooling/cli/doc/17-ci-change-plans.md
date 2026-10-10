# CI Change Plans

Two commands report the impacted plan of a checked-out commit range:

| Command | Document | Commands planned | Use it when |
| --- | --- | --- | --- |
| `putnami impact-plan <commands>` | ImpactPlan | the list you name | an extension needs the changed files, impacted projects and tasks of a change |
| `putnami change-plan` | ChangePlan v1 | the fixed gate `lint,test,build,validate` | a CI plane admits a change and must bind it to a repository and a digest |

Both run the same revision checks and the same engine plan. The ChangePlan is
the projection of the ImpactPlan for the fixed gate: see
[Impact plans](#impact-plans).

`putnami change-plan` produces a deterministic, immutable v1 admission plan
for a checked-out commit range. CI and Cloud use it to decide which quality-gate
tasks must run without reimplementing Putnami's impact graph.

```bash
putnami change-plan --base origin/main --output=json
```

The command requires a clean worktree. `--head` defaults to the checked-out
`HEAD` and, when supplied, must resolve to it. `--base` must be an ancestor of
HEAD. Both are resolved and emitted as full commit IDs; changed files are the
direct `base..head` commit diff, never staged or untracked worktree files.
Rename detection is disabled, so a move always contributes both its removed
source path and added destination path regardless of local Git configuration.

Structured output is a protocol result-v2 envelope. The ChangePlan itself is
the envelope's `data` field:

```json
{
  "version": 1,
  "generator": {"name": "putnami", "version": "..."},
  "repository": {"remote": "origin", "url": "https://host/org/repo.git"},
  "baseSHA": "...",
  "headSHA": "...",
  "changedFiles": ["..."],
  "impact": {
    "directProjects": [{"id": "/app", "name": "app", "path": "app"}],
    "projects": ["..."],
    "transitiveDependents": ["..."]
  },
  "tasks": [{"identity": {"key": "/app:lint"}, "deadlineMs": 300000, "resourceClass": {"heavy": false, "cpuWeight": 1}}],
  "cache": {"status": "available", "entries": []},
  "digest": "sha256:..."
}
```

`impact.projects` is the full impacted closure. `directProjects` owns at least
one changed path; `transitiveDependents` is the remaining closure. Every task
is an existing planner node for the `lint,test,build,validate`
command set, the same gate CI runs, and carries a typed identity, resolved hard
deadline, declared scheduling resources and ordering edges. A task with no hard
deadline makes plan emission fail closed.

## Digest and canonical form

For v1, the digest is `sha256:` plus the lowercase SHA-256 hex digest of the
UTF-8 bytes emitted by Go's `encoding/json` for this exact payload, in this
field order:

```text
{
  "domain":"putnami/change-plan/v1",
  "version", "generator", "repository", "baseSHA", "headSHA",
  "changedFiles", "impact", "tasks"
}
```

Arrays are deduplicated and sorted before serialization: paths lexically,
projects by ID, tasks by task identity key, ordering edges lexically, and
resources by scope then ID. `baseSHA` and `headSHA` are full commit IDs: 40 or
64 lowercase hexadecimal characters.

The document, its canonical form and its validation are a published contract,
`go.putnami.dev/protocol/ci` (`protocols/ci`). Go consumers import
`ChangePlan`, `ChangePlanCanonicalBytes`, `RecomputeChangePlanDigest`, and
`ValidateChangePlan` from it; the CLI builds every plan with the same package.
Other consumers can use the rule above and check themselves against the
conformance corpus in `protocols/ci/fixtures/change-plan`. A consumer must
reject an unknown `version`, a malformed full SHA, non-canonical ordering or a
duplicate entry, a bad digest, or a document whose head is not the checked-out
revision it is admitting.

## Cache summary

`cache` is explicitly advisory and is excluded from the digest. Local cache
presence can change when another run warms or evicts an entry, so it must never
change admission work or allow a runner to skip a planned task. `status` is one
of:

- `available`: every cacheable task has a cache key and local presence hint.
- `disabled`: the caller supplied `--no-cache`.
- `unavailable`: a key would depend on ambient environment/runtime input, key
  computation failed, or cache presence could not be checked. The static
  `reason` identifies which case; no partial entries are emitted.

Cache keys are emitted only when they can be recomputed from the checked-out
revision and the CLI's declared state. Environment values, source contents,
command arguments, working directories, output paths, invocation locators, and
secrets are never included in the document.

## Impact plans

`putnami impact-plan` is the seam an extension uses to ask the engine what a
change impacts, for the commands the extension names, without importing the
CLI:

```bash
putnami impact-plan test,build --base origin/main --output=json
```

1. The one positional argument is the command list, comma-separated. Aliases
   resolve as they do for `putnami <command>`. A command named twice, and an
   alias that expands to several commands, are usage errors.
2. `--base`, `--head` and `--no-cache` mean what they mean for
   `change-plan`, and the same revision checks apply: a clean worktree, a head
   that is the checked-out `HEAD`, and a base that is its ancestor.
3. `--baseline`, `--impacted`, `--projects` and `--all` are refused: the range
   selects the projects.
4. No repository remote is needed.

The structured result's `data` member is the ImpactPlan:

```json
{
  "version": 1,
  "generator": {"name": "putnami", "version": "..."},
  "commands": ["test", "build"],
  "baseSHA": "...",
  "headSHA": "...",
  "changedFiles": ["..."],
  "impact": {"directProjects": ["..."], "projects": ["..."], "transitiveDependents": ["..."]},
  "tasks": ["..."],
  "cache": {"status": "available", "entries": []}
}
```

Every member except `commands` has the type, order and meaning it has in a
ChangePlan. `commands` is the list you named, in your order. The tasks are
every task the engine plans for that list over the impacted closure, including
the tasks of a companion command that an extension's `alsoRuns` adds to a
command you named. The document has no `repository` and
no `digest`; the document you derive from it holds both. Without `--output`,
the command prints a short summary: the command list, the range, and the
number of changed files, impacted projects and tasks.

Go consumers import `ImpactPlan` and `ValidateImpactPlan` from
`go.putnami.dev/protocol/ci`. `ChangePlanFromImpactPlan(plan, repository)`
projects a plan onto the ChangePlan of the same range: `putnami change-plan`
is that projection for the fixed gate and the `origin` remote, so the two
documents never disagree about one range. The conformance corpus is
`protocols/ci/fixtures/impact-plan`.
