# ADR 0006 — The guard reads the Git candidate cut and is keyed on it

- **Status**: accepted. Amends decision 2 of
  [ADR 0003](0003-drift-is-the-generator-tasks-verdict.md)
- **Scope**: `@putnami/clientgen` (`tooling/clientgen-extension`), the
  `clientgen-workspace-check` task that `validate` and `clientgen-check` run

## Context

`clientgen-workspace-check` is a pure function of the workspace: it builds
nothing, renders nothing and spawns nothing. It still ran on every `validate`,
because it read files no cache key could hold:

- its source scan walked each project on disk, files Git ignores included;
- it found the member projects in `.putnami/workspace-index.json`, which Git
  ignores;
- it read each provider's `.gen/clientgen/config.json` and, for a provider with
  no contract sidecar, `.gen/schema/openapi.json`. A build writes both, and Git
  ignores both unless a project commits them.

A key over those files would describe whichever build last ran, not the tree.
The input `git:**` ([CLI ADR 0041](../../../cli/doc/adr/0041-git-candidate-file-inputs.md))
holds the repository's candidate cut: the tracked files and the untracked files
no ignore rule excludes. A task keyed on it is sound only when it reads nothing
else.

## Decision

**1. The check reads the candidate cut and nothing else.** Inside a Git work
tree every read goes through `gitcandidate.Tree`: the source scan, the provider
discovery, the committed manifests, the output directories, the inventories,
the census and each project's `putnami.json`. A file Git ignores is absent, as
it is from a clone. Outside a Git work tree the check reads the disk, and no
`git:` key exists to replay a verdict from.

**2. The membership comes from the job context.** The check takes the member
projects from `workspaceProjects`, which the CLI resolves from
`putnami.workspace.json` and the scope manifests its includes name. Those files
are candidates, so the key holds them. The CLI also folds the membership itself
into the key of every task keyed on the cut, so a membership that user config
or an ignored scope manifest changes moves the key too. A context without a
membership is a `clientgen.discovery` finding. The check never falls back to the index.

**3. A provider's inputs are the files it commits.** The contract is the
committed sidecar `schema/openapi.json`, and the targets are the committed
`client.putnami.json` manifests. A project that commits its
`.gen/clientgen/config.json` or `.gen/schema/openapi.json` still has them read,
because a tracked file is a candidate whatever the ignore rules say. Nothing
the verdict depends on comes only from an ignored file: discovery already
preferred the sidecar and the manifests, so that a cold clone and a built tree
reach one verdict.

**4. The task is keyed on `git:**`.** It declares the project-scoped input
`{"repository": {"from": "project", "files": ["git:**"]}}` and
`cache: {enabled: true, noOutput: true}`. A workspace-once task's project is
rooted at the workspace root, so the input holds the whole cut. The key holds
no commit, ref or absolute path: a checkout of the same files at another commit,
on another branch or in another directory is a hit.

## Consequences

- A `validate` whose candidate cut did not change replays the guard's verdict,
  findings and censused warnings included.
- An ignored source is not scanned. A handwritten transport in a file Git
  ignores does not fail the gate, as it cannot fail it on a clone.
- An unmarked third-party provider reads as first-party unless the project
  commits its `.gen/clientgen/config.json`. ADR 0003 already reported this on a
  cold clone; it is now the verdict on every tree.
- A membership change moves the key, even when no candidate file moves: a
  workspace include in the user's `~/.putnami/config.json` re-runs the guard.

## Alternatives rejected

- **A pattern list over the scan's read set.** It would hold the ignored index
  and build output, so the key would move with every build and still miss a
  new project directory.
- **Keep the guard uncached.** The check is pure; re-running it on an
  unchanged tree costs every `validate` its scan.
- **Read the index from the cut.** Git ignores it, so it is absent from every
  cut.
