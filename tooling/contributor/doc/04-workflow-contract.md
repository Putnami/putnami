# Workflow records and handoffs

`build` coordinates an authorized outcome. `spec`, `design`, `prototype`,
`review`, `adr` and `check` are focused interventions within its one execution
loop. The workflow source is in `src/skills/`; both hosts receive the same
content through the contributor extension. The exact v1 field contract is
[workflow.md](../src/skills/build/references/workflow.md), and the native CLI
command is described in the [CLI commands](../../cli/doc/03-commands.md).

Start with the requested observable outcome and acceptance. Native feature
declarations and specs supply requirement IDs and expected verification
checks. The local workflow JSON is an ignored projection that references
those exact Git-tracked versions, refuses symlinked refs, and partitions their
requirements into current, later
and excluded work. Excluding an outcome-changing requirement needs its actual
mandate or scope-decision reference. For example, a first export unit may cap
data size, but the requested large-data export remains a later requirement;
completing the first unit cannot complete the whole outcome.

The map has useful delivery units, milestone context and cycles. A unit states
its observable result, boundary, projects, covered requirements, dependencies,
required delivery stage and actual receipt. A milestone tracks coordination;
it is not a receipt. Unresolved unit decisions block its ready claim. The
next cycle names native provider task refs as opaque `{source,id}` pairs with
an optional paired revision and actual `tasks get` receipt, or an exact local
task record `{version:1,id,inputs:[ref],nextResult}`; a display string cannot
substitute for that identity. It also
names inputs, expected
inspectable result, environment and proof entrypoint. Later cycles remain
proportionate. A settled small fix reuses current intent and design, with a
reason when a new prototype or ADR has no role. The canonical stage inventory
still records `intent`, `spec`, `design`, `prototype`, `review`, `adr`,
`candidate` and `handoff` **for each unit**, each with a nonempty
`entryCondition` stating its observable starting prerequisite, exact inputs,
expected output, exit criteria and a supported result or explicit
non-applicability reason. A
current unit reaches ready only after each of its stages passes or is
not applicable, with candidate passed. Later units may retain pending stages.

Material specs, designs, prototypes and ADRs receive independent challenge of
their exact source and context versions. The reviewer writes an original
versioned report with stable findings, scenarios, consequences and
corrections. Later correction and re-review, counterevidence, or an
authorized deferral is appended as a resolution; changing an index digest
alone cannot renew an old opinion. Preserve historical examined artifact and
context bytes under immutable versioned paths. When an examined path changes
in place, a `challenge.snapshots` entry pairs the old exact ref with a copy at
a distinct path and identical digest. Later independent reviewer reports
belong in `challenge.revisions`; the latest binds the current source and
context, and a fixed finding requires a later review of different artifact
bytes. Stable earlier findings remain in the chain. Unrelated edits leave an artifact opinion
current when its examined inputs are unchanged. Any covered edit changes the
whole-tree candidate binding and invalidates its review/gate proof.

A prototype answers one material question within an effort ceiling. Its
record points at inputs, inspectable assets and instructions, observations,
simulations, assumptions, conclusion and remaining uncertainty. Simulated
behavior stays labeled. A reused implementation from the experiment goes
through ordinary review and the gate. The handoff names the mandate, unit,
native scopes, receiver and responsibility, exact inputs, expected output,
acceptance, original unresolved finding IDs and next action. Repository or
provider versioned refs travel across hosts; ignored local assets must be
transferred or reported unavailable to the dependent stage. Resume reconciles
the current provider revision, tree and producer refs instead of replaying a
conversation.
For the handoff unit, its `handoff.inputs` includes every exact next-cycle
provider receipt and local task record ref; a provider task without a receipt
is only a planning reference, not a transferred task body.

Check `putnami tree verify --help` for `--workflow` before relying on a new
record. `tree verify --workflow <file>` can validate and project planning
progress, including pending work; it never grants the ready stamp. An optional
exact `workflow` ref on the v2 dossier makes the native verifier require
complete current requirements from the actual producer session and its
`spec-verification.json` report, in addition to the existing dossier's
candidate review, gate, qualification and acceptance. Older CLIs can ignore
an unknown dossier member, so a successful old `--record` call is not workflow
proof. Missing, duplicate, unknown, skipped, failed and stale native checks
remain incomplete. A source-level candidate result does not establish merge,
package publication, deployment or adoption; each later stage needs its own
actual receipt.

`putnami proposals finalize --partial-delivery` publishes a partial unit with
a non-closing parent reference. It rejects closing directives in caller
title, the fully normalized proposal body and branch commits before a write,
and skips the parent task's delivered transition.
The normal closing template and transition are for the complete mandated
outcome only. Native finalization still enforces proof, gate, English,
proposal read-back and comment controls in both modes. Publication, merge,
release and deployment remain bounded by the invocation's authority.
Provider label inheritance that relies on a closing task reference may not
run for a partial proposal; read back the actual proposal labels and report
their state rather than claiming the parent's labels transferred.

These records provide traceable local coordination, not a trusted authorship
service. The CLI checks structure and bindings; independent reviewers judge
semantic scope coverage and material decisions. A local checkpoint that was
not transferred cannot promise another workspace a working asset.
