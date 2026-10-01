# CI Change Plans

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
