# Repository policy

Repository-specific conventions live in one block of the workspace
configuration, `options["@putnami/contributor"]` in `putnami.workspace.json`.
Every member is optional; an absent member takes the default below. The block
holds conventions, never credentials, host permissions or provider settings:
provider settings (a repository name, a state-to-label mapping) belong to the
provider's binding in `options.collaboration`.

Read it once per run from the workspace root:

```bash
jq '.options["@putnami/contributor"] // {}' putnami.workspace.json
```

| Member | Default | Used by | Meaning |
|---|---|---|---|
| `version` | `1` | all | Policy format. Another value stops the workflow with a clear message. |
| `tasks.source` | none | fix, fix-loop, epic, code-review | The `source` of the references the tasks provider issues, so `fix <id>` and `epic <id>` can address a task by its id. Without it, pass the whole reference. |
| `proposals.source` | none | code-review | The `source` of the references the proposals provider issues, so `code-review <id>` can address a proposal by its id. |
| `tasks.backlog.states` | `["open"]` | fix, fix-loop | Semantic states the backlog selection reads. |
| `tasks.backlog.labels` | `[]` | fix, fix-loop | Labels every backlog task must carry. |
| `tasks.filters` | `{}` | fix, fix-loop | Named filter flags: `{"<flag>": "<label template with {value}>"}` turns `--<flag> <value>` into a label filter. |
| `tasks.rank` | `[]` | fix, fix-loop | Ordered labels; a task ranks by the earliest one it carries, then by age. |
| `tasks.tiers` | `{}` | fix, execute | Keys `light`, `standard` and `heavy`, each naming a label: a task carrying the label declares that tier. |
| `tasks.planLabels` | `[]` | plan | Labels a task created from a plan receives. |
| `tasks.claimAssignees` | `[]` | fix | Assignees a claim sets when the provider offers `assign`, in the provider's own notation. |
| `states.claimed` | `in_progress` | fix | Semantic state a claimed task moves to. |
| `states.delivered` | `in_progress` | finalizer | Semantic state a task moves to once its proposal is published. |
| `workers.light`, `workers.standard`, `workers.heavy` | `fix-light`, `fix-standard`, `fix-heavy` | execute, fix, epic | Worker profile delegated for each tier. |
| `workers.analyst` | `epic-analyst` | epic | Read-only analyst profile. |
| `verification.language` | none | check, finalizer | `en` runs the English-only detector on everything a change publishes; absent, no language rule is enforced. |
| `verification.documentation` | `[]` | check | Documentation roots a user-facing change must update, besides each project's `doc/`. |
| `verification.ciGate.checks` | `[]` | execute, finalizer | Names of the hosted checks that stand for the gate. Empty, the gate always runs locally. |
| `verification.ciGate.load` | `0.7` | execute, finalizer | Load ratio (`machine-load.sh`: one-minute load average divided by CPUs) above which the final gate goes to the hosted checks. |
| `verification.ciGate.timeoutMinutes` | `60` | finalizer | How long the finalizer waits for the hosted checks before it stops; invoking it again keeps waiting. |
| `verification.ciGate.pollSeconds` | `30` | finalizer | Interval between two reads of the hosted checks. |
| `publication.draft` | `false` | finalizer | Keep proposals as drafts at finalization. Every proposal opens as a draft (`--draft`) whatever this says. |
| `publication.bodyMaxBytes` | none | finalizer | Largest proposal body the finalizer publishes; the body becomes the squash commit message. Absent, only the contract's 65536 bytes apply. |
| `publication.taskReference` | none | finalizer | Line that links a proposal to its task, with `{id}` replaced by the task reference id (for example a closing keyword the provider understands). The finalizer adds it to the commit message and to the proposal body. |
| `integrations.contentBump` | none | content-bump | `{"lock", "command", "projects", "reference"}` of a pinned-content lock the `content-bump` workflow refreshes. Without it, that workflow reports that it is not configured. |

## Worker profiles and models

A worker profile is a host-agnostic body (`.claude/agents/<name>.md`,
`.codex/agents/<name>.toml`) with host metadata. The profiles this extension
ships carry a reference host model mapping in that metadata. A repository that
needs other models or tools selects its own profiles per tier under `workers`
instead of editing the shipped files; a locally edited shipped file is kept and
reported by the next install, never overwritten. Host permissions, model
access and credentials stay with the workspace and the host.

## What the policy cannot do

It cannot grant authority: publication, merge and deployment still need the
current mandate and the repository's normal controls. It cannot change a
provider's behavior: a state that has no label on the provider side, or a
capability the provider lacks, is reported as the provider answers it.
