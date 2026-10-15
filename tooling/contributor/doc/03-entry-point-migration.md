# Entry-point migration and evidence limits

## Entry points

The extension now installs `build`, `spec`, `design`, `prototype`, `review`,
`adr` and `check`. `build` owns the single shared implementation and evidence
loop. `content-bump` remains a separately configured explicit-only utility.
This is an owner-authorized breaking change in the current pre-1.0 line.

| Before | Now | Status |
|---|---|---|
| `build`, `spec`, `design`, `prototype`, `review`, `adr`, `check` | Same names, from `@putnami/contributor` | Installed |
| `execute` | `build` for the one implementation, review and evidence loop | Removed; no inherited external authority |
| `plan` | `build` for outcome mapping and a precise next cycle | Removed; no implicit task creation |
| `fix` | `build <named-task>` under an explicit task mandate | Removed; no implicit backlog selection or claim |
| `epic` | `build` with coordinated delivery units and milestones | Removed; no separate epic controller |
| `code-review` | `review` for an independent candidate review | Removed; publication requires separate explicit authority |
| `putnami-plan`, `putnami-change`, `putnami-check`, `putnami-review` | Canonical intervention named above | Removed; no alias invocation |
| `fix-loop` | No automatic batch successor | Removed |
| `content-bump` | Same configured utility | Retained, explicit-only |

`build` accepts a named task as an input when one is supplied. Claiming it
requires explicit authorization; choosing a backlog task, creating a task,
merging or deploying are never implied by a request. Publishing the run's own
proposal is the default, as with the former `fix`; pass `--local` to stay
local.

## Arguments and helper interfaces

Removed skill-specific flags have no direct replacement. `build` takes an
intention, named task or proposal reference, `--resume <record>`,
`--tier light|standard|heavy`, `--base <ref>` and `--dry-run` under its own
mandate. `review` accepts an exact proposal reference, branch or current
candidate. Re-express a former `plan --scope` or label filter as explicit
native project scope and acceptance, rather than relying on a removed skill
to select work. A former `fix --group` or backlog loop has no automatic
successor; select and authorize each task through the provider before asking
build to deliver it. Native `proposals finalize` and `contributor language`
interfaces are documented in the [CLI commands](../../cli/doc/03-commands.md).
The former `fix/scripts/finalize-pr.sh`, `machine-load.sh`,
`tree-fingerprint.sh` and `session-cap.sh` are removed from installed content.
Use `proposals finalize`, `contributor machine-load` and `tree fingerprint`;
the host runtime handles session limits. The active audit integration still
calls `check/scripts/english-only.sh`: that path remains as a thin relay to
`contributor language`, with no separate language heuristic.

## Workspace declarations

Stored task backlog filters, ranks and plan labels from the removed workflows
do not make build select tasks or create them. Repository owners may remove
those unused policy values during migration. `content-bump` still reads its
configured `integrations.contentBump` block and remains explicit-only.

A workspace that still declares `@putnami/agent-workflows` or
`@putnami/maintainer-workflows` is refused by every agent-content command, with
nothing written. The CLI no longer installs separately declared artifacts,
both projects are deleted, and no command moves them. To switch:

1. Declare `@putnami/contributor` in `extensions`, and replace the two
   `agentArtifacts` entries with `extension:@putnami/contributor`.
2. Delete this clone's ownership records of the two artifacts. The refusal
   names each one.
3. Run `putnami install`. It drops the two artifacts' pins from
   `putnami.lock.json`. A file that differs from the content is a collision
   that writes nothing: move or delete it, then rerun.
4. Commit `putnami.workspace.json`, `putnami.lock.json` and the host files.

A clone that already materialized the old artifacts keeps their ownership
records after it pulls the switch, and every command refuses the contributor
opt-in until they are deleted. Delete the records the refusal names, then run
`./putnamiw install` in this repository. See
[Replacing separate artifacts](../../cli/doc/18-agent-workflows.md#replacing-separate-artifacts-with-an-extensions-content).

## Publication and compatibility window

`@putnami/contributor` is published with the framework release set, like every
other extension, and has the `preview` support status. Its project sets
`options.publish.archives`, and `@putnami/go` packages its content with its
executable. It reaches a channel once a release set that includes it
advances that channel: `canary` first, `stable` and `latest` at the next
promotion. Until the channel a workspace resolves serves it:

- A workspace that declares the extension by path, as this repository does,
  installs its content with this release's CLI.
- Every built-in starter still writes `@putnami/contributor` in `extensions`
  and `extension:@putnami/contributor` in `agentArtifacts`. `putnami init`
  reports that the content could not be installed, names `putnami install`,
  and finishes the workspace without workflow files. `putnami install` fails
  at its extensions phase in that workspace, because the channel does not
  serve the extension yet. To install the rest of the workspace now, remove
  both entries, or resolve `canary`, and add them back once the channel
  serves it.
- A workspace that declared `@putnami/agent-workflows` or
  `@putnami/maintainer-workflows` either switches by hand with this CLI
  (above) or stays on an earlier CLI release, which installs its pins exactly
  as before.

Which CLI reads the content:

- The extension manifest declares `agentContent`, which needs extension
  contract 5. This release's CLI is the first that reads it.
- An earlier CLI refuses the published package with "requires a newer
  putnami", and reads the `extension:@putnami/contributor` opt-in as an
  unpinned artifact, so its `putnami install` fails in a workspace that opted
  in ([extension ADR 0006](../../../protocols/extension/doc/adr/0006-agent-content-is-an-additive-contract.md)).
  Upgrade the CLI before adding the opt-in.

Consumer adoption is proven separately, from a repository that consumes the
published extension; this repository's self-consumption does not prove it.

## Evidence limits

- `tree verify --workflow` validates a versioned local progress projection;
  it never emits the candidate-ready verdict. Confirm that capability before
  relying on an optional workflow reference in a v2 dossier, since an older
  CLI can ignore an unknown dossier member. Current native requirements and
  producer check reports constrain completion; a unit's task count or
  polished artifact does not.
- `putnami tree verify` checks the consistency of declared, local inputs: tree binding,
  producer records, review coverage and scope notes. It does not authenticate
  authors, prove scope completeness, or replace a producer's full schema
  validator. Agents sharing write access can alter notes and policy.
- Native finalization records the caller's proof; it cannot establish that the proof
  is truthful, current or complete. Only a dossier the verifier accepts
  supports `Putnami verified`, and only for the local, declared scope.
- Native finalization's "verified proposal" covers what the proposals contract
  returns: base, head, head commit and state; the assignees and labels the
  upsert answered with, which the proposal read back must still carry; and
  the task's state. The upsert answer already includes what the provider
  applies from its own settings (`assignAuthor` and `inheritLabelPrefixes` on
  the GitHub provider, which refuses an upsert whose addition GitHub dropped).
  The command does not read provider settings, so it cannot see a provider
  that silently ignores its own setting; that is the provider's suite to
  prove. A provider that does not report assignees or labels is not verified
  on them, and the command reports that it did not verify them. The
  superseded helper called GitHub directly and compared both with this
  repository's own label prefixes.
- A provider answer is as strong as the provider: `checked` preconditions do
  not stop a concurrent writer between the read and the write, an assignee is
  not an exclusive lease, and a provider without hosted checks reports them as
  unsupported.
- Memory is context, never evidence: a checkpoint references gate, review and
  qualification records and proves nothing they do not.
- The native command suites run against the local provider. They do not
  exercise a hosted provider; that proof belongs to each provider's own suite.
