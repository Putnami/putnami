---
name: review
description: Independently challenge a material artifact or frozen candidate and preserve original findings
---

# Review

Accept a material spec, design, prototype, ADR or frozen implementation
candidate. Work in a distinct context from its author. Read the mandate,
exact examined artifact and relevant source and context versions; a title or
summary alone is not review coverage. Candidate code review also follows the
[build check stage](../../../.agents/skills/build/references/check.md), its
[records contract](../../../.agents/skills/build/references/records.md) and the v2 dossier contract.
Use the repository's review-quality host profile; add a specialized pass only
for a distinct risk, without requiring a fixed count of passes or findings.
Resolve a proposal ID through the policy's `proposals.source`, or accept an
exact `{source,id}` reference. A branch review uses its pinned base and head;
no argument reviews the current branch. A task reference resolves its linked
proposals without combining different revisions. Record the repository, base,
head, proposal state, tree identity and scope before reading. An absent
proposal or unavailable revision is an unresolved input, not a clean review.

Resolve the consumer CLI in each shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Start code discovery with Putnami and use its local graph fallback when
needed. For a candidate, account for every changed file and relevant consumer;
read affected files in full and justify any exclusion, including generated
copies inspected through source and parity proof. Bound a large pass and report
unfinished coverage. Examine success and failure paths, compatibility,
concurrency, authorization, migrations, cleanup, performance, tests and docs
as applicable. Inspect the proposal title and body as the squash commit and
report stale or incomplete net-change prose and any missing deletion statement.
Challenge outcomes and exclusions, native requirement coverage,
responsibility boundaries, invariants, experiment assumptions, compatibility,
proof entrypoints and likely failure scenarios. State what was actually
examined and what remains unavailable. Give each material finding a stable ID,
location, scenario, consequence and concrete correction. Write an original,
attributed report with exact artifact and relevant context references as
specified by the [workflow contract](../../../.agents/skills/build/references/workflow.md).
The author may append a correction and re-review, counterevidence or an
actual mandate-authorized deferral; preserve the original report and the exact
examined bytes at a versioned path or digest-matched snapshot. A corrected
artifact needs a later independent review that binds the current source and
context and retains earlier findings. Changed examined bytes reopen the
opinion. Unrelated tree edits do not invalidate a
design opinion, while any covered edit invalidates whole-tree candidate proof.

Inspect supplied producer records by exact path, command, selection, policy
version and tree binding. Reuse valid proof under its producer rules; request
only a missing check or targeted experiment. Coordinate any execution with the
owner of the frozen tree and use resource admission. The build coordinator owns
the final impacted gate and dossier. In a candidate report, give each changed
file or justified excluded surface, stable finding ID, severity, location,
reproducible scenario, consequence, correction and current resolution.
Severity is P1 for correctness, security, data loss or a broken required
check, P2 for a substantiated design, edge-case or performance defect, or a
contradiction with an ADR, and P3 for polish. P1 and P2 block; P3 does not.
Approval requires complete relevant coverage, valid required checks and no
unresolved P1 or P2 finding or reservation. Missing coverage or proof remains
explicit. The native verifier, rather than a free-form opinion, computes
`Putnami verified`. The local review index uses provider severity vocabulary:
map P3 to low, P2 to medium and P1 to critical, retaining the original label
and rationale in the report. Use high only where repository policy defines it.

Review is read-only on product and collaboration state. It never creates a
task, publishes a review or grants merge/deployment authority unless the
invocation separately and explicitly authorizes that external act. A delegated
review returns a local report; standalone publication to an open proposal
requires separate explicit authorization and an exact reviewed revision. Never
check out a proposal in a shared workspace. If publication is authorized,
write a concise review body to a file and send one identity-keyed
`proposals review` request with the exact `{source,id}` proposal reference,
reviewed commit, verdict (`approve`, `request_changes` or `comment`) and
`idempotencyKey: "review:<id>:<commit>"`. The provider may refuse approval
by the proposal's author; report its answer as-is. Never publish a private
local path as if it were accessible evidence. Return
the exact report reference, reviewed version, coverage, findings and missing
inputs. The author or build coordinator owns repair and progression.
