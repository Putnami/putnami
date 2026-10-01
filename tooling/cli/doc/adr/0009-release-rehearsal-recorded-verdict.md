# ADR 0009 — The release rehearsal is a recorded verdict, not a release step

- **Status**: accepted
- **Scope**: the release rehearsal in `tooling/cli-documents`
  (`@putnami/cli-documents`), its release plan in
  `tooling/cli/internal/cli/testdata/release-plan.json`, and the recorded report
  `tooling/cli/doc/reports/release-rehearsal.md`

## Context

Several steps of a public release are one-way doors. A published Go module
version cannot be withdrawn, a published npm version can only be deprecated,
and a repository that was public once may already be cloned. A reversible step
is reversible only if someone wrote down the rollback point first.

The question "could this tree become the intended public release?" must have an
answer anyone can re-derive for the tree in front of them, not a narrative true
for one commit. The mechanism must also be able to say NO-GO without failing
the build, or the pressure to reach a green gate becomes pressure to weaken a
check.

## Decision

1. **The rehearsal is a test that renders a verdict, not a gate that demands
   GO.** `TestReleaseRehearsalReportRecordsTheCurrentVerdict` computes ten
   check results over the candidate cut, renders one generated block, and
   compares it with the block committed in the report. It fails on **drift**
   (the recorded verdict no longer matches the tree), never on the verdict
   itself. NO-GO is a committable outcome; an unrecorded change to a release
   input is the defect.
2. **Evidence items are code-owned, requirements are plan-owned.** The ten
   evidence items are a fixed table in the test. The plan parser rejects an
   unknown check id and a plan that omits a required one, and the runner
   reports a check whose declared evidence text drifted. The plan owns the
   requirements: which file, tree prefix, document phrase or ignore entry each
   item needs, and who owns a missing one. A check with neither an assertion
   nor a declared requirement is a blocker, not a pass.
3. **Reuse the scrub, never reimplement it.** The scrub result comes from the
   existing public-cut scanner, its committed baseline and its allowlist, and
   the rehearsal enumerates the candidate cut through the same helper, so both
   gates see the same tree.
4. **Non-destructive is enforced.**
   `TestReleaseRehearsalPerformsNoDestructiveOperation` reads the rehearsal's
   own source and fails if it names a subprocess, a filesystem mutation or a
   publishing verb. The plan is decoded with unknown fields rejected, so no
   field can smuggle something executable in. The scan is file-scoped on
   purpose: the reused public-cut helpers run `git ls-files`, and the enforced
   property is that nothing the rehearsal runs can change the tree or publish.
5. **Every verdict-bearing file is a cache-key input.** A drift check served
   from a cache entry keyed without the file reports success without running.
   `@putnami/cli-documents` declares every Git candidate file of the
   repository (`git:**`,
   [ADR 0041](0041-git-candidate-file-inputs.md)) as test inputs, and `TestReleaseRehearsalVerdictInputsAreDeclaredCacheKeyInputs`
   asserts, with `store.SelectsPath` (the matcher the cache key uses), that the
   report, the plan, every file a check reads and every requirement target is
   covered. A requirement naming an uncovered path fails with the pattern to
   add.
6. **The recorded block carries statuses, never volatile counts.** It holds
   check ids, pass or blocked status, blocking inputs and owners. Artifact
   counts, candidate file counts and scrub totals are logged by the test and
   quoted in the report's prose, so unrelated work does not force a re-record.
7. **The publish order is validated against the tree.** Every (artifact,
   channel) pair resolved from the candidate cut is claimed by exactly one
   publish step or one recorded exclusion, and every declared pattern must
   match something.

## Consequences

- Changing a release input (the licence, the support catalog, the builder
  platform matrix, the governance surface, the ignore guard) forces a fresh
  recorded verdict.
- The candidate cut includes untracked, non-ignored files, so a locally drafted
  release input turns the rehearsal red until the block is re-recorded.
- The rehearsal proves nothing about built artifacts. Digest and provenance
  verification belongs to the installer and release-smoke work, and the
  rehearsal reports that dependency as a blocker.
- The private-archive/public-root switch is excluded by construction and
  recorded as excluded; the `rollback-plan` check fails if that exclusion is
  flipped, so the last human-approved step cannot be automated by editing data.
- A phrase requirement couples the plan to wording in another file; the owner
  changes both in the same change.

## Rejected alternatives

- **A `putnami release rehearse` command.** It puts a release verb in the
  public command surface and makes the read-only property a runtime concern.
- **Failing the build on NO-GO.** It turns an honest report into an obstruction
  and rewards weakening checks.
- **Generating the report file from the test.** Writing a file breaks the
  read-only invariant and lets a run change the record a human approves. The
  test prints the block; a human commits it.
