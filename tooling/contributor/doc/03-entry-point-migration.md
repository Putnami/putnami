# Entry-point migration and evidence limits

## Entry points

Every entry point of the two superseded artifacts still exists: no skill name
was removed in this release. Some arguments were, and the next table lists
them.

| Before | Now | Status |
|---|---|---|
| `plan`, `execute`, `fix`, `epic`, `check`, `code-review`, `fix-loop`, `content-bump` (maintainer artifact) | Same names, from `@putnami/contributor` | Canonical |
| `putnami-plan` (public artifact) | Alias of `plan`, local plan only | Deprecated |
| `putnami-change` (public artifact) | Alias of `execute`, local mandate only | Deprecated |
| `putnami-check` (public artifact) | Alias of `check`, local evidence only | Deprecated |
| `putnami-review` (public artifact) | Alias of `code-review`, local findings only | Deprecated |

An alias names its canonical skill and keeps the restriction its entry point
always had. The aliases are removed in a later release, once every built-in
starter and pinned consumer has moved to `@putnami/contributor` and one
release has shipped with both names; that release note names the removal.

## Arguments and helper interfaces

| Before | Now |
|---|---|
| `fix <issue-number>`, `epic <issue-number>` | `fix <task>`: an id resolved with the policy's `tasks.source`, or a whole `{source, id}` reference |
| `fix --group <g>`, `--domain <d>` | Filters the policy's `tasks.filters` defines (this repository keeps `--group` and `--domain`), or `--label <label>` |
| `plan --issue` | `plan --task` (`--issue` is its deprecated spelling) |
| `plan --domain <audit-group>` | Removed: scopes come from native project selection, labels from `--label` |
| `epic --no-fable` | `epic --no-analyst` |
| `code-review <pr-number>` | `code-review <proposal id or reference or branch>` |
| `finalize-pr.sh --issue <n>` | `--task-source <source> --task-id <id>`, optional; `--issue` fails with a message naming the replacement |
| `finalize-pr.sh` prints `PR_URL=` | Prints `PROPOSAL_REF=`, and `PROPOSAL_URL=` when the provider supplies one |
| Proof recorded as an issue comment | Proof recorded in the proposal body's `## Verification` section |
| `english-only.sh github` | `english-only.sh tasks`; `github` is a deprecated alias that no longer scans proposals |
| `session-cap.sh` names a mission file, blocks at 300 tool calls, counts subagent calls | It names the memory checkpoint, warns once at 250 of the orchestrator's own calls, never blocks; the harness's auto-compaction bounds the context |

## Workspace declarations

A workspace that still declares `@putnami/agent-workflows` or
`@putnami/maintainer-workflows` is refused by every agent-content command, with
nothing written; the error names the migration. The CLI no longer installs
separately declared artifacts, and both projects are deleted. To switch:

1. Declare `@putnami/contributor` in `extensions`, and run `putnami install`.
2. Review the move: `putnami migrate agent-content @putnami/contributor`.
3. Apply it: `putnami migrate agent-content @putnami/contributor --apply`. The
   two `agentArtifacts` entries become `extension:@putnami/contributor`, their
   lock pins go, and this clone's ownership records merge into the extension's.
4. Commit `putnami.workspace.json`, `putnami.lock.json` and the host files.

A clone that already materialized the old artifacts keeps their ownership
records after it pulls the switch, and every command refuses the contributor
opt-in until they move. Run the same `--apply` once in that clone; in this
repository that is `./putnamiw migrate agent-content @putnami/contributor --apply`.
It keeps and releases any file you edited, and afterwards
`putnami context generate` and `putnami install` change nothing. See the
[CLI migration guide](../../cli/doc/18-agent-workflows.md#migrating-separate-artifacts-to-an-extensions-content).

## Publication and compatibility window

`@putnami/contributor` is published with the framework release set, like every
other extension, and has the `preview` support status. Its project sets
`options.publish.archives`, and `@putnami/scaffold` packages it under the
`agent-content` step. It reaches a channel once a release set that includes it
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
  `@putnami/maintainer-workflows` either migrates with this CLI (above) or
  stays on an earlier CLI release, which installs its pins exactly as before
  ([CLI ADR 0048](../../cli/doc/adr/0048-migrating-agent-artifacts-to-extension-content.md),
  section 7).

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

- `putnami tree verify` checks the consistency of declared, local inputs: tree binding,
  producer records, review coverage and scope notes. It does not authenticate
  authors, prove scope completeness, or replace a producer's full schema
  validator. Agents sharing write access can alter notes and policy.
- The finalizer records the caller's proof; it cannot establish that the proof
  is truthful, current or complete. Only a dossier the verifier accepts
  supports `Putnami verified`, and only for the local, declared scope.
- The finalizer's "verified proposal" covers what the proposals contract
  returns: base, head, head commit and state; the assignees and labels the
  upsert answered with, which the proposal read back must still carry; and
  the task's state. The upsert answer already includes what the provider
  applies from its own settings (`assignAuthor` and `inheritLabelPrefixes` on
  the GitHub provider, which refuses an upsert whose addition GitHub dropped).
  The finalizer does not read provider settings, so it cannot see a provider
  that silently ignores its own setting; that is the provider's suite to
  prove. A provider that does not report assignees or labels is not verified
  on them, and the finalizer prints that it did not verify them. The
  superseded helper called GitHub directly and compared both with this
  repository's own label prefixes.
- A provider answer is as strong as the provider: `checked` preconditions do
  not stop a concurrent writer between the read and the write, an assignee is
  not an exclusive lease, and a provider without hosted checks reports them as
  unsupported.
- Memory is context, never evidence: a checkpoint references gate, review and
  qualification records and proves nothing they do not.
- The helper suites run against a CLI built from the tree and the local
  provider. They do not exercise a hosted provider; that proof belongs to each
  provider's own suite.
