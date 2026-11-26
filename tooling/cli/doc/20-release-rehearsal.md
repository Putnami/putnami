# Release Rehearsal

The release rehearsal answers one question over the current tree: could this
tree become the intended public release? It is read-only. It never builds,
publishes, tags, pushes, rewrites history, or changes repository visibility.

The private-archive/public-root switch is excluded. It stays the final
human-approved operation and runs only after a human reads the recorded verdict.

## Run it

```bash
./putnamiw lint,test,build --projects @putnami/cli-documents --enforce-coverage
```

To read the evidence dump directly:

```bash
cd tooling/cli-documents
PUTNAMI_NO_RELAUNCH=1 go test ./ \
  -run TestReleaseRehearsalReportRecordsTheCurrentVerdict -v
```

The `-v` log prints the point-in-time numbers that are deliberately not part of
the recorded verdict: candidate file count, release series, builder archive
matrix, publishable artifacts per channel, publish order, and scrub findings.

## Read the verdict

The recorded verdict lives in
[`doc/reports/release-rehearsal.md`](reports/release-rehearsal.md),
inside a generated block delimited by `<!-- rehearsal:generated:begin -->` and
`<!-- rehearsal:generated:end -->`. Each row is one required evidence item:

| Evidence item | What it checks |
| --- | --- |
| version and artifact manifest | The declared release series matches the workspace version, the declared targets match the builder's archive platform matrix, and the candidate cut resolves at least one publishable artifact. |
| checksums and provenance | The durable provenance policy names SHA-256, the authoritative registry headers, builder identity, the current unsigned-artifact policy, the current non-reproducible archive boundary, and immutable rollback; the installer and release smoke retain their fail-closed digest paths. |
| support report | The support catalog parses, uses only known statuses, classifies nothing twice, and classifies the CLI. |
| migration report | The canonical compatibility guide and accepted budget ADR retain every public format, and each lock, machine-result, and extension-contract corpus has non-empty, re-derivable provenance whose recorded SHA-256 matches its immutable fixtures. |
| scrub result | The public-cut scrub reports no finding beyond its reviewed baseline, and the private workspace state is guarded by the committed ignore file. |
| golden-path result | The release smoke exercises install → init → serve → HTTP → stop. |
| governance checklist | The minimum public governance surface exists, the checked-in licence is the declared one, and a neutral contributor CI path exists. |
| label/scope bootstrap plan | The fresh root declares complete metadata, and its labels cover every scope and every property group named by the label-axis authority. |
| publish order | Every (artifact, channel) pair the tree publishes is claimed by exactly one publish step or one recorded exclusion, and no declared pattern is dead. |
| rollback plan | Every publish step names a rollback point and its exact reversal, every exclusion gives a reason, and the final switch stays excluded and owned by a human. |

A blocked row names the missing input and the issue that owns it.

## NO-GO is a valid result

The rehearsal **never** fails because a release input is missing. It fails when
the recorded verdict stops matching the tree. That is what makes it repeatable:
the same tree always renders the same block.

When the gate reports a mismatch it prints the computed block. Replace the
generated block in the report with it, adjust the surrounding prose, and commit.
Never hand-edit a verdict: the next run recomputes it.

## An uncommitted file already moves the verdict

The candidate cut is the tracked tree **plus every untracked, non-ignored
file**. Creating `GOVERNANCE.md` in your working tree satisfies a requirement
immediately, so the gate reports a mismatch before you commit anything. That is
intended — the tree in front of you is the tree being judged — but it has two
consequences:

1. Re-record the block in the same change that adds the release input. Other
   changes that touch the same report serialize on it.
2. A scratch file at a path the plan names reds the drift check where it
   runs. Delete it or ignore it rather than re-recording around it.

## The drift check runs on main, without a cache

The gate only detects drift when it runs. Every file of the candidate tree can
move the verdict, and only a whole-tree input (`git:**`) keys that; such an
input selects `@putnami/cli-documents`, and the prerequisite steps of the
projects it depends on, for every change of the repository. The project
declares none. The drift check and the public-cut release gate run only when
`PUTNAMI_WHOLE_TREE_GATES` is set, and skip otherwise:

- the [whole-tree gates workflow](../../../.github/workflows/whole-tree-gates.yml)
  sets the variable on every push to `main` and runs the project with
  `--no-cache`, so a changed tree is never answered from a cached run;
- a pull request runs them only when it changes `tooling/cli-documents`,
  `tooling/windows-vet`, that workflow, the recorded report, the release plan
  or the public-cut baseline or allowlist. A pull request that moves the
  verdict without recording it therefore passes its own gate and reds the
  workflow on `main`: the pull request that re-records the block runs the
  drift check itself.

Run the drift check on any tree with:

```bash
PUTNAMI_WHOLE_TREE_GATES=1 ./putnamiw test --projects @putnami/cli-documents --no-cache
```

The variable is declared under `options.test.envInputs` in
[`tooling/cli-documents/putnami.json`](../../cli-documents/putnami.json), so a
run with it never restores the skip a run without it stored. The tests that
always run read the release plan and the public-cut baseline and allowlist,
which `options.test.filePatterns` names. `TestReleaseRehearsalNeverReplaysACachedVerdict`
enforces the declarations and the workflow's trigger, variable and cache
switch. The public-cut input regression compares the scanner's candidate set
with what `git:**` collects and proves that a new unrelated document moves that
key while ignored local state does not.

## The release plan

[`internal/cli/testdata/release-plan.json`](../internal/cli/testdata/release-plan.json)
is the human-owned, reviewed declaration of what the release needs. It is data
only — the decoder rejects unknown fields, so no field can carry something
executable.

It declares:

- **release** — the series, the current licence, the intended future licence,
  and the target platforms.
- **freshRoot** — the public root's default branch, visibility, description,
  topics, scopes, bootstrap labels, and label axes with their authorities.
- **publishOrder** — the ordered publish steps, each with its channels, its
  artifact patterns, its rollback point and its exact reversal.
- **excluded** — artifact/channel pairs the release deliberately does not
  publish, each with a recorded reason.
- **finalSwitch** — the excluded private-archive/public-root switch, its human
  owner, its rollback point and its reversal.
- **checks** — per evidence item, the requirements that must hold, each naming
  the input in plain language and the issue that owns it.

Requirement kinds:

| Kind | Meaning |
| --- | --- |
| `file` | The path exists in the candidate cut. |
| `tree-prefix` | At least one candidate path sits under the prefix. |
| `phrase` | The named file contains the phrase after whitespace normalization. |
| `ignore-entry` | The committed `.gitignore` guards the path. `.context/`, `.context`, `/.context/` and `/.context` all count; a `!` line does not. |

The set of ten evidence items is owned by the gate, not by the plan. The plan
cannot add, rename, or drop one, and a check with neither a requirement nor an
assertion is reported as proving nothing.

## When a check changes

- **The input lands somewhere else.** Update the requirement's target in the
  plan, re-run, re-record.
- **A new publishable artifact appears.** Give it a position in `publishOrder`,
  or record it under `excluded` with a reason. The `publish-order` check fails
  until one of the two is true.
- **The builder's platform matrix changes.** Update `release.targets`. The
  `version-and-artifact-manifest` check compares the two.

## Related

- [ADR 0009 — the release rehearsal is a recorded verdict, not a release step](adr/0009-release-rehearsal-recorded-verdict.md)
- [Release provenance policy](release-provenance.md)
- [Compatibility and Migration](21-compatibility-and-migration.md)
- [`tooling/cli-documents/specs/release-rehearsal.json`](../../cli-documents/specs/release-rehearsal.json)
- [`RELEASE.md`](../../../RELEASE.md) — the release contract this rehearsal reads
