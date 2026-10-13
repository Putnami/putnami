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
| `tasks.source` | none | build | The task provider's source for an explicitly named task ID; otherwise pass the full `{source,id}` reference. |
| `proposals.source` | none | review | The proposal provider's source for an explicitly named proposal ID; otherwise pass its full reference. |
| `tasks.tiers` | `{}` | build | Keys `light`, `standard` and `heavy`, each naming a label: a named task carrying the label declares that tier. |
| `tasks.claimAssignees` | `[]` | build | Assignees set for a named task claim when the mandate authorizes claim and the provider offers `assign`. |
| `states.claimed` | `in_progress` | build | Semantic state an explicitly authorized task claim moves to. |
| `states.delivered` | `in_progress` | proposals finalize | Semantic state a task moves to once its complete-outcome proposal is published. |
| `workers.light`, `workers.standard`, `workers.heavy` | `fix-light`, `fix-standard`, `fix-heavy` | build | Worker profile delegated for each tier. |
| `workers.analyst` | `epic-analyst` | build | Read-only analyst profile for coordinated work. |
| `verification.language` | none | check, proposals finalize | `en` runs the English-only detector on everything a change publishes; absent, no language rule is enforced. |
| `verification.documentation` | `[]` | check | Documentation roots a user-facing change must update, besides each project's `doc/`. |
| `verification.ciGate.checks` | `[]` | build, proposals finalize | Names of the hosted checks that stand for the gate. Empty, build runs the gate locally before finalization. |
| `verification.ciGate.load` | `0.7` | build | Load ratio (one-minute load average divided by CPUs) above which build selects the hosted gate. |
| `verification.ciGate.timeoutMinutes` | `60` | proposals finalize | How long the command waits for the hosted checks before it stops; invoking it again keeps waiting. |
| `verification.ciGate.pollSeconds` | `30` | proposals finalize | Interval between two reads of the hosted checks. |
| `publication.draft` | `false` | proposals finalize | Keep proposals as drafts at finalization. Every proposal opens as a draft (`--draft`) whatever this says. |
| `publication.bodyMaxBytes` | none | proposals finalize | Largest proposal body the command publishes; the body becomes the squash commit message. Absent, only the contract's 65536 bytes apply. |
| `publication.taskReference` | none | proposals finalize | Line that links a complete outcome's proposal to its task, with `{id}` replaced by the task reference id (for example a closing keyword the provider understands). The command adds it to the commit message and proposal body only for normal delivery; `--partial-delivery` uses a non-closing parent reference and skips the delivered transition. |
| `integrations.contentBump` | none | content-bump | `{"lock", "command", "projects", "reference"}` of a pinned-content lock the `content-bump` workflow refreshes. Without it, that workflow reports that it is not configured. |

Stored `tasks.backlog`, `tasks.filters`, `tasks.rank` and `tasks.planLabels`
values do not cause `build` to select backlog work or create tasks. They may
remain in older workspace policy until its owner removes them; this extension
does not consume them.

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
