---
name: code-review
description: Code review a change proposal — check correctness, conventions, tests, docs, and flag risks
model: claude-opus-5-5[1m]
allowed-tools: Bash, Read, Grep, Glob, Task, TodoWrite, Write, ToolSearch, mcp__putnami__putnami_search, mcp__putnami__putnami_symbol, mcp__putnami__putnami_context, mcp__putnami__putnami_impact, mcp__putnami__putnami_related, mcp__putnami__putnami_summary, mcp__putnami__list_projects, mcp__putnami__describe_project, mcp__putnami__deps, mcp__putnami__find_owner, mcp__putnami__why_impacted, mcp__putnami__topo_sort, mcp__putnami__impacted, mcp__putnami__get_diagnostics
argument-hint: [<proposal-id> | <proposal-reference> | <branch>]
---

# Code Review

Review behavior, contracts, tests, documentation, and risks in a fresh bounded
context. Read [the execute loop](../../../.agents/skills/execute/references/loop.md) and
[its records contract](../../../.agents/skills/execute/references/records.md), and reach proposals
through [the collaboration contracts](../../../.agents/skills/execute/references/collaboration.md). A delegated review
returns a durable local result to that loop; it does not wait for a human to
copy findings or start corrections.

## Portable host and authority

Treat `/name` as the host-native skill invocation (`$name` on Codex). Review
runs at the repository's review quality: this skill's host metadata carries the
model the repository chose, and any collection worker stays at the same
quality. Add a specialized pass only
for a distinct risk or interaction; do not require a fixed number of passes,
findings, or votes.

The reviewer owns its review result, never implementation changes, commits,
branch switches, merges, or task transitions. Do not check out a proposal in a
shared workspace. Read the pinned objects or use an isolated review worktree
when execution requires a checkout. Fresh context is an independence practice,
not an access-control barrier when agents share credentials and writable files.

Resolve the CLI from the consumer workspace root, again in each new shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

## Input and result routing

- `<id>` or `<reference>`: a proposal id (with the policy's `proposals.source`)
  or a whole `{source, id}` reference; read it with `proposals status`. For a
  task, find the proposals of its branches with `proposals find`. Review each
  linked scope without silently combining different revisions.
- `<branch>`: review against the repository trunk and locate its proposal with
  `proposals find`.
- No argument: review the current branch and locate its proposal if present.
- An execute delegation supplies exact base/head/tree identity, change perimeter, scope
  owner positions, acceptance criteria, evidence references, and the local result
  destination. These supplement source inspection; implementation prose is not
  an independent review conclusion.

Record the resolved repository, base, head, proposal state, dirty tree identity
when applicable, and review scope before reading. A merged proposal is
read-only; report a closed unmerged proposal's state instead of publishing
approval. An absent linked proposal is
an unresolved input, not a clean review. Do not replace a supplied revision with
whatever happens to be the latest branch head.

Default to a local result when delegated by execute or reviewing a branch.
An explicitly invoked standalone review of an open proposal retains authority
to publish its review through `proposals review`. The coordinator decides any
additional publication
under the existing mandate; this skill grants no merge or deployment authority.

## Review procedure

1. Read workspace instructions, `.agents/constraints.md`, the request, relevant
   decisions, architectural contracts, and the diff at the pinned revision.
   Start code discovery with Putnami MCP; use the local graph/CLI when stale or
   unavailable. Inspect affected consumers and dependencies beyond the diff.
2. Account for every changed file and other relevant integration surface.
   Read relevant files in full. Record each covered file and its purpose in
   the review; explicitly justify exclusions such as reproducibly generated
   copies inspected through their source and parity evidence. A large diff
   does not authorize silent omission: bound the pass and report unfinished
   coverage, or divide independent surfaces with clear ownership.
3. Examine success and failure paths, changed invariants, compatibility,
   concurrency, authorization, migrations, lifecycle cleanup, performance,
   meaningful tests, and documentation as applicable. Check responsibilities
   and available strategic capabilities as well as whether the code runs.
   Ground objections in a constraint and offer a viable resolution where possible.
   Read the proposal title and body as the squash commit they become: they
   state the net change of the branch as it stands, briefly, and name what it
   deletes. A body that describes an earlier state, narrates commits or review
   rounds, or carries proof or file lists is a finding, and so is any text that
   names an agent or model.
4. Inspect the producer evidence supplied with exact references, commands,
   selections, configuration/policy versions, and revision binding. Reuse
   applicable evidence under its producer's rules. Do not run an unconditional
   test pass followed by a full uncached gate. Request or run only missing or
   invalid required checks and targeted experiments needed to establish a
   finding. Follow the canonical workspace gate policy; `validate` may already
   expand to `validate-workspace` in the consumed extension. When execute
   delegates the review, its final impacted gate runs after the review: do
   not run or request it.
5. Coordinate any execution with the owner of the frozen tree. No agent may
   edit source while checks and review produce evidence on that version.
   Mutating lint or generation belongs before the freeze; run it in isolation
   when required. Use existing resource admission, and avoid simultaneous
   expensive checks unless their isolation and available capacity are known.
6. Write the original local report and return its review metadata, coverage and
   findings using the records contract. The coordinator alone assembles the
   dossier; do not concurrently edit that shared projection. Preserve
   stable finding IDs across rounds, severity, location, reproducible scenario
   or reason, and resolution (`open`, `fixed`, `refuted` with evidence, or
   `deferred` by the identified competent authority). A claimed fix needs a
   revision and verification; the reviewer need not invent findings to fill
   a category. Separate checked coverage, exclusions, and missing evidence.
7. Return the exact report/index references and revision to execute. Corrections
   remain in the same run/proposal where coherent. Review affected code and its
   interactions again after corrections; preserve prior history and identify
   conclusions that remain applicable under the records contract.

## Finding severity and publication

Use the repository's policy when defined. Otherwise retain the existing
critical / moderate / minor vocabulary: critical covers correctness or security
failures and broken required checks; moderate covers substantiated design,
edge-case, or performance concerns; minor covers nonblocking polish. Explain
impact rather than classifying by the amount of code. The local index uses the
existing Cloud severity vocabulary: map minor to low, moderate to medium, and
critical to critical; use high only when applicable policy defines that level.
Retain the original label and rationale in the report. Severity is distinct
from the overall verdict.

For a published review, map the evidence to the contract verdicts `approve`,
`request_changes`, or `comment` (needs discussion). Approval requires complete
relevant coverage, valid required
checks, and no unresolved blocking findings or reservations. Missing coverage
or proof cannot become approval merely because no defect was found. Report
incomplete evidence and decisions explicitly. The execute verifier, not this
free-form review verdict, computes any `Putnami verified` status.

Do not automatically create tasks for findings on the work under review.
Record them in the current review and correction loop. A separate task is for
work deliberately deferred under existing authority. When standalone
publication is authorized, write the concise review to a file and publish it
once, keyed by the reviewed revision so a repeat returns the same review:

```bash
jq -n --argjson ref '<proposal reference>' --rawfile body review.md \
  --arg verdict '<approve|request_changes|comment>' --arg commit '<reviewed head commit>' \
  '{ref: $ref, verdict: $verdict, body: $body, commit: $commit,
    idempotencyKey: ("review:" + $ref.id + ":" + $commit)}' >review-request.json
"$PUTNAMI_CLI" proposals review --input-file review-request.json --output=json
```

Include revision, local report reference or authorized published trace,
findings, coverage gaps, and checks. Never publish a private local path as
though it were accessible evidence. The provider may refuse a verdict (for
example an approval by the proposal's author): report its answer as it is.
