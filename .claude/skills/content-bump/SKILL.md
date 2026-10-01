---
name: content-bump
description: Refresh a pinned-content lock the repository policy declares to its channel head, validate the pinned projects, and publish a proposal
disable-model-invocation: true
allowed-tools: Bash, Read, Grep, Glob, TodoWrite
argument-hint: [--bundle <name>] [--no-pr] [--dry-run]
---

# Content Bump

This is the canonical workflow for Claude Code and Codex. Use host-native tools
by capability; explicit invocation is `/content-bump` on Claude Code and
`$content-bump` on Codex. This workflow is explicit-only on both hosts; Codex
enforces this through `agents/openai.yaml`.

Some repositories pin published content (documentation bundles, fixtures,
datasets) by digest in a committed lock and move that pin on purpose: the lock
is the only committed artifact, and committing its diff is what changes what
the repository serves. This workflow refreshes such a lock when newer content
was published, validates the projects that read it, and publishes one
proposal. When nothing moved it stops cleanly — no branch, no proposal.

It is an optional integration. The repository declares it in its
[policy](../../../.agents/skills/execute/references/policy.md) under
`integrations.contentBump`:

| Member | Meaning |
|---|---|
| `lock` | Workspace-relative path of the committed lock. |
| `command` | Argument vector that resolves every pinned bundle to its channel head, rewrites the lock and primes the local digest cache. It exits non-zero and leaves the lock untouched when a resolve fails. |
| `projects` | Projects whose lint and build validate the new lock. |
| `reference` | Optional line the commit and proposal carry, such as the tracking task. |

Without that member, report "content-bump is not configured for this
repository" and stop.

## Workspace CLI

Run commands from the consumer workspace root. Before using the commands below,
select the executable wrapper when available, otherwise the installed CLI:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Resolve this again in each new shell or delegated worker; do not assume shell
variables survive host tool calls. Use `"$PUTNAMI_CLI"` for Putnami commands.

## Arguments

| Flag | Effect |
|------|--------|
| `--bundle <name>` | Only resolve the named bundle; may repeat, forwarded to `command` as `--bundle <name>`. Default: every bundle in the lock. |
| `--no-pr` | Bump + validate, then stop with the change left uncommitted for manual review. |
| `--dry-run` | Report whether a bump is available, then revert the lock. No build, no commit, no proposal. |

## Steps

### 1. Resolve and bump the lock

Run the policy's `command` from the workspace root, adding each `--bundle`.
Check the exit status, then read stdout:

- **Non-zero exit or an `error:` line on stderr** → the resolve failed and the
  lock is untouched. Report the error and **stop**; never publish a failed
  resolve.
- **Nothing to move** → report "the pinned content is up to date" and **stop**.
- **One line per moved bundle** (name, version and digest before and after) →
  capture these transitions for the proposal body and continue.

Confirm the change with Git, the authoritative signal:

```bash
git status --porcelain -- <lock>
```

An empty result means no change: report up to date and **stop**. Only the lock
may be modified.

`--dry-run` stops here: print the pending transitions (or "up to date") and
restore the lock with `git restore -- <lock>`.

### 2. Validate

```bash
"$PUTNAMI_CLI" lint,build --projects <projects, comma-separated>
```

If it fails, do **not** publish: report the failure, restore the lock
(`git restore -- <lock>`), and stop. A lock that does not build is never
committed.

`--no-pr` stops here: leave the lock modified, report the transitions, and stop
— no branch, commit, or push.

### 3. Publish the proposal

Branch from the fetched trunk, commit **only** the lock, push, and publish the
proposal through the proposals contract:

```bash
git fetch origin
git switch -c bump/content-<short-digest> origin/<trunk>
git add -- <lock>
git commit -m "chore: bump the pinned content lock" \
  -m "<one line per transition: name from → to (short digest)>" \
  -m "<reference, when the policy declares one>"
git push -u origin bump/content-<short-digest>
jq -n --arg head "bump/content-<short-digest>" --arg base "<trunk>" --rawfile body bump-body.md \
  --arg commit "$(git rev-parse HEAD)" \
  '{change: {base: $base, head: $head, headCommit: $commit}, title: "chore: bump the pinned content lock", body: $body}' >bump-proposal.json
"$PUTNAMI_CLI" proposals upsert --input-file bump-proposal.json --output=json
```

The body lists each bundle transition (name, version from→to, short digest),
states that lint and build passed for the listed projects, and carries the
policy's `reference`. The upsert is keyed by base and head, so a repeat updates
the same proposal. On `unresolved`, run `proposals find` for that base and head
before anything else; on `unsupported`, report the pushed branch and that no
proposals provider is bound.

### 4. Report

Print the outcome: the bumped bundles and the proposal reference (and URL when
the provider supplies one), or "already up to date — nothing to do", or the
validation failure with its output.
