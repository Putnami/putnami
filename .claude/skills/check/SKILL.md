---
name: check
description: Run the Putnami gate, applicable local execution proof, the documentation audit and the repository language rule for a change
allowed-tools: Bash, Read, Grep, Glob, Task, TodoWrite
argument-hint: [<project-path-or-name> | --all] [--skip-docs]
---

# Change Qualification

Run the full pre-commit validation suite, qualify applicable runtime behavior
from the current worktree, and audit documentation coverage for impacted
changes. Read the repository's conventions from
[its policy](../../../.agents/skills/execute/references/policy.md).

## Portable host contract

This is the canonical workflow for both Claude Code and Codex. Use host-native
tools by capability. When another skill is named, invoke it with `/name` on
Claude Code or `$name` on Codex. Its evidence obligations also apply through
the repository's own rules (`.agents/constraints.md` when present) when an
implementation request does not name this skill.

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

- No arguments: run on `--impacted` projects (default), once, before declaring the change complete
- `<project>`: run on a specific project (by name or path, or `.`); use this while iterating
- `--all`: run on all projects
- `--skip-docs`: skip the documentation gap audit

## Project selection

Determine the project selection flag based on the arguments:
- If a **specific project** is passed → use `--projects <project>`
- If `--all` is passed → use `--all`
- If **no argument** is passed → use `--impacted`

The `--impacted` default is for the one final run before you declare the
change complete. It selects the changed projects and every project that
depends on them, which is too large to repeat on each iteration: one edited
line in a widely used library can select hundreds of jobs with `--impacted`
and a handful with `--projects <that library>`. Preview the difference with
`--plan`. While iterating, pass the projects you changed;
a named project keeps the upstream steps it needs and skips its dependents.
Name several projects in one comma-separated `--projects` value.

## Steps

### 1. Identify targeted projects

Run `"$PUTNAMI_CLI" build <selection> --plan` to understand which projects will be checked.

### 2. Run or reuse the applicable gate

When execute calls this step, first inspect the exact referenced producer session
and report under its records contract. Reuse a current applicable result; do not
start another gate merely because a review completed. Derive blocking roots and
flags from the workspace CI policy. `validate` already includes the SDD
`validate-workspace` companion. The command below is the ordinary default, not
an override of additional repository checks. During final evidence collection,
use execute's immutable base SHA and freeze the tree. This check verdict alone
is not the computed Putnami verified stamp.


```
"$PUTNAMI_CLI" lint,test,build,validate <selection> --enforce-coverage
```

Where `<selection>` is the project selection flag determined above (`--projects <project>`, `--all`, or `--impacted`).

Coverage is measured and enforced by default, so `--enforce-coverage` is redundant. Keep passing it anyway: it is explicit, it still means exactly this, and it keeps the gate identical on a runner pinned to an older CLI. Never pass `--no-enforce-coverage` here — that is the escape hatch for local work in progress, and it would let a coverage drop through the gate.

If any job fails:
- Re-run the **failing job only** with `--output=jsonl --no-cache` to get structured diagnostics; this per-job diagnostic run is not the gate's one `--retry-failed` retry, and it does not replace the gate
- Parse the JSONL output for `diagnostic` events (file, line, column, message, severity)
- Present a clear summary of failures grouped by project and job
- Suggest fixes for each failure

### 3. Qualify applicable local behavior

Decide applicability from the changed projects and their reachable workloads,
not from whether the user named this step. A changed project reaches a workload
when it is one, or when a workload's `runsWith` closure includes it. Run the
proof for each reachable workload. A pure documentation or isolated library
change that reaches no workload is `NOT APPLICABLE`, with the reason.

The local proof is one command per reachable workload, run on the actual
worktree, including uncommitted changes:

```bash
"$PUTNAMI_CLI" qualify <project> --target local --output=json
```

It composes the workload with every workload it runs with (production-mode, no
watch, per-run databases, real migrations at boot), waits on the typed ready
event, sends the smoke contract derived from the route inventory, and tears the
composition down. A TypeScript workload without a committed route inventory
needs `"$PUTNAMI_CLI" build --projects <id>` first; otherwise the verdict is
`unsupported`. Do not hand-author a receipt or drive the smoke yourself.

Read the verdict from the result's `data`:

- `state` must be `passed`; it is the only pass, and the only state that exits
  `0`. Every other state (`failed`, `unsupported`, `timed_out`,
  `composition_failed`, `digest_mismatch`, ...) is not a pass: report `MISSING`
  or `BLOCKED` with the failing phase's diagnostic.
- `binding` (`kind: tree`, `fingerprint`, `dirty`, `headSHA`) names the tested
  tree. `binding.fingerprint` must equal the gate session's `tree.fingerprint`
  from step 2 (`.putnami/sessions/<id>/session.json`); a different value means the proof and the gate covered different
  trees, and the proof is stale.
- `cleanup.state` must be `clean`; `partial` names leftovers in `leftovers`.

Generated-client changes still need provider declaration → generation → compile
or load the emitted client → real bound consumer call; the composition covers
that call when the consumer runs with the provider. Keep internal providers and
consumers real, and name every double at a genuine external boundary. Rerun
after any relevant change.

If the proof is absent, skipped, impossible, not `passed`, or covers
stale/different code, report `MISSING` or `BLOCKED` and the precise gap. It
cannot contribute a ready verdict.

### 4. Audit documentation gaps (unless `--skip-docs` is passed)

Check that impacted packages have appropriate documentation:

1. Get the list of changed files from git, against the change's base:
   ```
   git diff --name-only origin/<base>...HEAD
   ```

2. For each changed package, check:
   - **Package `doc/` folder exists** — every package should have a `doc/` directory
   - **New exports have docs** — if new public APIs were added (new exports in `package.json` or new exported functions/types), check that corresponding documentation exists in the package's `doc/` folder
   - **Changed behavior is documented** — if function signatures, config options, CLI flags, or default values changed, verify the docs reflect the change
   - **Published docs updated** — for user-facing changes, check that each documentation root the policy lists under `verification.documentation` has a corresponding update

3. Report findings:
   - List packages with missing or outdated documentation
   - For each gap, specify what documentation is missing and where it should go
   - Distinguish between **required** updates (new APIs, changed behavior) and **recommended** updates (improved examples, cross-references)

### 5. Language rule

Run this step only when the policy sets `verification.language` to `en`; with
no language rule, report it as not configured. The detector checks everything
this change publishes:

```bash
bash .agents/skills/check/scripts/english-only.sh commits origin/<base>..HEAD
bash .agents/skills/check/scripts/english-only.sh files --diff origin/<base>...HEAD
```

The proposal text is checked only when the proposals contract answers `ok`
with a proposal for the branch. Read the answer first, then decide. `jq -j`
ends its output without a newline, so a native Windows jq, which ends lines with
CRLF, leaves no carriage return in the captured values:

```bash
envelope="$("$PUTNAMI_CLI" proposals find --output=json \
  --input '{"change":{"base":"<base>","head":"<branch>"},"states":["draft","open"]}')" || true
outcome="$(jq -j '.outcome // "no envelope"' <<<"$envelope" 2>/dev/null || echo "no envelope")"
found="$(jq -j '.result.items // [] | length' <<<"$envelope" 2>/dev/null || echo 0)"
if [ "$outcome" = ok ] && [ "$found" -gt 0 ]; then
  jq -r '.result.items[0] | .title + "\n\n" + (.body // "")' <<<"$envelope" |
    bash .agents/skills/check/scripts/english-only.sh text --label "proposal title and body"
else
  echo "proposal title and body: NOT CHECKED (outcome $outcome, $found proposal(s))"
fi
```

Compare against `origin/<base>`, not a local branch that may be stale. Run
each command on its own so each exit status stays visible. `NOT CHECKED` is
never a pass: an `unsupported` outcome means the proposals contract is unbound,
`unavailable` means the provider could not answer, and no proposal means there
is no text to check yet. Exit 1 lists each offender with its signals and lines:
rewrite the text in English (reword commit messages before pushing). Exit 2 is
a tool failure, never a pass. The detector already skips test files, fixtures
and allowlisted proper nouns, and the publication helper refuses a non-English
proposal title or body on its own when the policy sets the rule.

### 6. Summary

Present a final summary:

```
## Check Results

### Lint, Test, Build
- lint:    PASS/FAIL (N projects)
- test:    PASS/FAIL (N projects)
- build:   PASS/FAIL (N projects)
- validate: PASS/FAIL (N projects)

### Local Execution Proof
- status: PASSED / NOT APPLICABLE / MISSING / BLOCKED
- workloads: <each reachable workload, or the NOT APPLICABLE reason>
- verdict: <state> · binding <fingerprint> dirty <true|false> head <headSHA> · cleanup <state>
- gate tree: <gate session tree.fingerprint> (equal to binding.fingerprint: yes/no)
- disclosed external doubles: <summary or none>

### Documentation Audit
- N packages checked
- N documentation gaps found
  - N required updates
  - N recommended updates

### Language Rule
- commits, changed files: PASS/FAIL/NOT CONFIGURED (N offenders)
- proposal title and body: PASS/FAIL/NOT CHECKED (<outcome or no proposal>)/NOT CONFIGURED

### Verdict
QUALIFIED FOR REVIEW / NEEDS ATTENTION
```

`QUALIFIED FOR REVIEW` requires the ordinary gate plus `PASSED` local proof
whenever it is applicable, and no language-rule offender when the policy sets one. `NOT APPLICABLE` needs a concise reason. If evidence
is missing or blocked, keep checkpoint commit/push/draft status explicit and
list the gap in priority order; those durable backups remain allowed but are
not qualification or completion.
