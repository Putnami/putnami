# Workflow records and handoffs

`build` coordinates an authorized outcome. `spec`, `design`, `prototype`,
`review`, `adr` and `check` are focused interventions within its one execution
loop. `plan`, `execute`, `fix`, `epic` and `code-review` remain compatible
entries. The workflow source is in `src/skills/`; both hosts receive the same
content through the contributor extension. The exact v1 field contract is
[workflow.md](../src/skills/build/references/workflow.md), and the native CLI
command is described in the [CLI commands](../../cli/doc/03-commands.md).

Start with the requested observable outcome and acceptance. Native feature
declarations and specs supply requirement IDs and expected verification
checks. The local workflow JSON is an ignored projection that references
those exact versions and partitions their requirements into current, later
and excluded work. Excluding an outcome-changing requirement needs its actual
mandate or scope-decision reference. For example, a first export unit may cap
data size, but the requested large-data export remains a later requirement;
completing the first unit cannot complete the whole outcome.

The map has useful delivery units, milestone context and cycles. A unit states
its observable result, boundary, projects, covered requirements, dependencies,
required delivery stage and actual receipt. A milestone tracks coordination;
it is not a receipt. The next cycle names exact tasks, inputs, expected
inspectable result, environment and proof entrypoint. Later cycles remain
proportionate. A settled small fix reuses current intent and design, with a
reason when a new prototype or ADR has no role. The canonical stage inventory
still records `intent`, `spec`, `design`, `prototype`, `review`, `adr`,
`candidate` and `handoff`, each with inputs, expected output, exit criteria and
a supported result or explicit non-applicability reason. A ready dossier
requires a passed candidate stage.

Material specs, designs, prototypes and ADRs receive independent challenge of
their exact source and context versions. The reviewer writes an original
versioned report with stable findings, scenarios, consequences and
corrections. Later correction and re-review, counterevidence, or an
authorized deferral is appended as a resolution; changing an index digest
alone cannot renew an old opinion. Unrelated edits leave an artifact opinion
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

`fix/scripts/finalize-pr.sh --partial-delivery` publishes a partial unit with
a non-closing parent reference. It rejects closing directives in caller
title/body/branch commits and skips the parent task's delivered transition.
The normal closing template and transition are for the complete mandated
outcome only. The helper still enforces its existing proof, gate, English,
proposal read-back and comment controls in both modes. Publication, merge,
release and deployment remain bounded by the invocation's authority.

These records provide traceable local coordination, not a trusted authorship
service. The CLI checks structure and bindings; independent reviewers judge
semantic scope coverage and material decisions. A local checkpoint that was
not transferred cannot promise another workspace a working asset.
