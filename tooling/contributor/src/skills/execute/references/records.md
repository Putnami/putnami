# Local dossier and existing evidence

This is an ignored local index, not a new public review protocol,
controller or storage service. It references existing producer records and
attributed Markdown notes. Keep snapshots and original outputs under
`.context/execute/<run>/`; append revisions instead of editing another author's
report. The current dossier is a projection, not a separately maintained plan.

Cloud's existing `ReviewReportV1` stays unchanged: its hosted revision, reviewer,
summary and findings can be referenced as a whole file. Do not add local coverage,
finding lifecycle or consultation fields to its closed wire contract, forge a hosted
stamp, or assume its review-only ledger is a general execution store. Local
review has no hosted authentication. Map minor→low, moderate→medium,
critical→critical; retain hosted high and original severity in the report.

## Commands

The checker is the consumer Putnami CLI's `tree verify` command. It needs Git
and a Putnami CLI that has `tree verify`; it needs no Bun, no Node.js and no
workspace of its own. A CLI without the command answers `unknown subcommand:
tree verify`: upgrade it. The checker obtains the native dry-run plan, but never
executes gate tasks, edits source, or calls a collaboration provider. It prints
one JSON document and exits 0 when the evidence is verified; otherwise it prints
`{"verdict":"not-verified","reason":"..."}` on one line and exits 1.

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
"$PUTNAMI_CLI" tree verify --snapshot --base origin/main > .context/execute/run/candidate.json
"$PUTNAMI_CLI" tree verify --ref .context/execute/run/review-1.md
"$PUTNAMI_CLI" tree verify --record .context/execute/run/candidate.json
"$PUTNAMI_CLI" tree verify --gate .putnami/sessions/<id>/session.json --report .putnami/reports/<id>.json --base origin/main
```

Create the ignored run directory first. Paths are absolute or relative to the
workspace root. `--snapshot` emits a **pending** index; never report it as verified.
`--ref` emits `{path, sha256}` for existing bytes. `--gate` applies the dossier's
gate rules to one session and report, without a dossier: it is how the
finalizer reuses a gate that ran on the live tree. It ignores the baseline
the session recorded: the native plan against the base decides the selection.
It requires the CI policy's commands and the finalizer's own four (`lint`,
`test`, `build`, `validate`), so a reused gate is never narrower than the one
the finalizer would run. Preserve producer outputs
unchanged. A changed reference, policy or tree requires renewed evidence, not
merely recalculating hashes to conceal stale opinions.

## Fields consumed by the checker

The snapshot supplies `version: 2`, `binding {fingerprint, headSHA, baseSHA}`,
`changedFiles`, and `policies` references to repository rules/configuration and
architectural manifests. The fingerprint is the existing Putnami producer's;
HEAD is included. The diff includes dirty and untracked non-ignored files.
Version 1 dossiers and records mixing the legacy `domains`, `domain`, or
`guardian` fields with version 2 are rejected. Keep historical ignored records
unchanged; create a new snapshot and obtain current consultation evidence for
this schema. There is no automatic migration.

Fill the remaining fields from actual work:

| Field | Content |
|---|---|
| `objective` | Reference to the accepted intention, scope and decision authority |
| `scope` | Reference to the reviewed change perimeter, workload/acceptance inventory, consulted scopes and reasons for exclusions or no affected consultation |
| `implementers` | Nonempty list of actual implementation session IDs |
| `scopes` | List of consulted scope contributions described below; proportional to affected boundaries |
| `review` | Independent review metadata below, referencing the original output |
| `gate` | `{session: ref, report: ref}`: exact v2 producer session and matching v2 report |
| `qualification` | `{required: [project IDs], results: [refs], notApplicable: ref or null}`; no required workload needs an explicit reviewed non-applicability note |
| `acceptance` | Nonempty `[{requirement, status: "passed", evidence: ref}]` from actual functional/harness results, distinct from smoke |

A consultation entry has `scope`, `owner` (consulted agent session ID), nonempty
`context` refs,
`proposal` ref, `design {position, note: ref}` and
`realization {position, note: ref, fingerprint}`. Each opinion also carries
`proposalSha256` (the examined proposal digest) and `contextSha256` (sorted
digests of its supplied context). These come from the consulted agent's original output;
swapping newer references beside an unchanged opinion must fail. Both current positions must
be `accepted`. Keep previous objections in
`objections [{note: ref, resolution: ref, authority}]`; resolution notes name the
compatible proposal, receiving-scope acceptance or actual arbitration. The
verifier requires a reference/attribution, not an authenticated authority.
The `owner` names the agent consulted for this scope; it does not denote an
architecture team or grant authority. Use existing Putnami project/scope IDs
and architecture manifests as contextual contracts where relevant, without
creating a new registry. An empty `scopes` list needs its rationale in the
reviewed top-level `scope` reference.

Review metadata:

```json
{
  "author": "independent-session-id",
  "fingerprint": "the exact candidate fingerprint",
  "report": {"path": "original-review.md", "sha256": "digest of original bytes"},
  "coverage": [{"path": "changed/path", "status": "reviewed", "reason": "behavior and interaction examined"}],
  "findings": []
}
```

Coverage names every changed file exactly once. `excluded` requires a specific
reason (for example generated output examined through its source and parity
check); `unreviewed` prevents verification. Summarize contracts, cross-scope
interactions, tests, uncertainties and context versions in the referenced report.

Each finding has `id`, `severity` (`low|medium|high|critical`), `location`,
`scenario`, `state` and `resolution` reference. `open` blocks; `fixed` requires
correction/re-review evidence; `refuted` requires counterevidence; `deferred`
requires an explicit `authority` and an approval reference in `resolution`.
Missing required gate, qualification or acceptance cannot be waived this way.
Original reports remain intact when the projection shows a later resolution.

## What is computed, and what is declared

The checker compares current tree and policy hashes, exact file coverage,
references, positions/findings, gate success and session/report IDs, coverage
enforcement, and qualification project/tree/state/cleanup. Gate selection must
be all projects or impacted against the index's **immutable base SHA**; use that
SHA explicitly in the gate's `--baseline`. Impacted selection is correctly
recorded as scoped by the producer; that is not an error. Required command roots
come from the current CI v3 policy. The verifier recomputes its unfiltered
impacted dry-run plan through the native CLI and requires every planned task
in the gate records; impacted mode alone does not rule out extra filters.
`--record <file> --base <target>` also checks the dossier merge-base against the
intended proposal base; the finalizer supplies `origin/<base>` whenever that
ref exists, so snapshot against the same remote ref.
Successful task records also establish
implicit companion commands, including `validate-workspace`.

Exit 0 emits `verdict: "Putnami verified"`, `stage: "ready"` and
`assurance: "local-declared-scope"`. Keep that qualification with the visible
stamp. It does not certify a deployment or downstream adoption.

This bounded script checks the fields it consumes, not every native producer
schema/cross-field invariant. CI flags other than `--enforce-coverage` and
`--continue-on-error` fail closed until supported. It assumes unmodified outputs from the installed
producers; it is not a replacement for their Go validators. Metadata not carried
by those records (such as test-name filters), required-scope
completeness, semantic review quality, author identity, historical immutability
and authorization of exceptions remain reviewed conventions. A hash identifies
bytes, not a trusted author. Shared writers can rewrite the index or verifier.
These limits must remain visible in pilot results; do not activate an automatic
merge policy from this local stamp alone.
