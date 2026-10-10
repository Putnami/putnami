# Release rehearsal

**Verdict: GO.** All ten release checks pass. The tree satisfies every
non-destructive release input this rehearsal owns.

This report is the output of one repeatable, non-destructive rehearsal. It does
not release anything. The private-archive/public-root switch is excluded from
the rehearsal and stays the final human-approved operation.

## What the rehearsal is

`TestReleaseRehearsalReportRecordsTheCurrentVerdict` in
[`tooling/cli-documents/release_rehearsal_test.go`](../../../cli-documents/release_rehearsal_test.go)
runs ten checks over the candidate cut — the tracked tree plus every non-ignored
new file — and renders the verdict block below. The gate fails when the recorded
block stops matching the tree, not when a check is blocked. A NO-GO is a valid,
recordable result; a stale record is not.

Run it with the project gate:

```
./putnamiw lint,test,build --projects @putnami/cli-documents --enforce-coverage
```

Or read the evidence dump directly:

```
cd tooling/cli-documents
PUTNAMI_NO_RELAUNCH=1 go test ./ -run TestReleaseRehearsalReportRecordsTheCurrentVerdict -v
```

The declared inputs live in
[`tooling/cli/internal/cli/testdata/release-plan.json`](../../internal/cli/testdata/release-plan.json):
the release series and licence, the fresh-root metadata, the label and scope
bootstrap set, the publish order, the rollback points and their reversals, and
the per-evidence requirements with the issue that owns each missing input.

## Verdict

<!-- rehearsal:generated:begin -->
| Evidence | Check | Result | Blocking input |
| --- | --- | --- | --- |
| version and artifact manifest | `version-and-artifact-manifest` | pass | — |
| checksums and provenance | `checksums-and-provenance` | pass | — |
| support report | `support-report` | pass | — |
| migration report | `migration-report` | pass | — |
| scrub result | `scrub-result` | pass | — |
| golden-path result | `golden-path-result` | pass | — |
| governance checklist | `governance-checklist` | pass | — |
| label/scope bootstrap plan | `label-scope-bootstrap-plan` | pass | — |
| publish order | `publish-order` | pass | — |
| rollback plan | `rollback-plan` | pass | — |

**Verdict: GO** — all 10 release checks pass.
<!-- rehearsal:generated:end -->

## Why every check passes

The migration check now reads the canonical compatibility guide and accepted
budget decision. It parses the provenance records for the lock, machine-result,
and extension-contract prior-release corpora, requires each corpus to be
non-empty and re-derivable, and recomputes every fixture's SHA-256. A placeholder
directory, changed historical bytes, or an unrecorded fixture is therefore a
blocker.

The checksum and provenance check reads the implemented installer and release
smoke paths plus the durable
[`release-provenance.md`](../release-provenance.md) contract. That contract names
SHA-256, the authoritative registry response headers, the required builder
identity record, today's unsigned-artifact policy, today's non-reproducible
archive boundary, and immutable rollback. The check does not turn the registry
digest into a signature or claim reproducibility the archive packager does not
provide.

The other eight checks continue to pass against the same candidate cut:
version/matrix/artifact identity, support classification, public scrub, the
install → init → serve → HTTP → stop golden path, governance, fresh-root labels
and scopes, complete publish ordering, and reversible rollback data.

## Evidence measured at this run

The numbers below are point-in-time and deliberately outside the generated
block: they would otherwise force a re-record on every unrelated project
addition. Re-read them from the test's log output at any time.

| Quantity | Value |
| --- | --- |
| Candidate files scanned | 8285 |
| Release series | 0.3.0 (from `tooling/CHANGELOG.md`) |
| Builder archive matrix | `darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64` |
| Publishable artifacts | 101 across `archives=19 docker=4 go=63 npm=15 template-archives=7` |
| Publish order | protocols → extension-sdk → go-framework → typescript-framework → language-extensions → intelligence-extensions → cloud-extensions → cli → contributor-extensions → templates |
| Public-cut scrub findings | 0 unresolved beyond the reviewed baseline |

The `docker` channel carries four artifacts — the documentation site, the
telemetry service, and two samples. They are deployment artifacts of this
repository, not release artifacts, so the plan excludes them from the publish
order with that reason recorded.

## Publish order and rollback

Every step publishes only after the artifacts it resolves, so a rollback never
strands a consumer. Each rollback point and its exact reversal are declared in
the release plan and asserted by the `rollback-plan` check.
The agent-readiness archive follows its published protocol, framework, SDK, and
language-extension inputs and precedes the CLI that resolves it.

The extension SDK (`go.putnami.dev/sdk/extension`, project
`putnami-extension-sdk`) is explicitly enrolled after the protocols and before
the language extensions. Its Go version follows the same immutable-module
rollback policy as the protocols; complete release sets must include it for
downstream extension builds.

| Order | Step | Channel | Reversal |
| --- | --- | --- | --- |
| 1 | protocols | `go` | Superseding patch version plus a `retract` directive. A published module version is immutable. |
| 2 | go-framework | `go` | Same as protocols. |
| 3 | typescript-framework | `npm` | Superseding patch version plus a deprecation of the bad version. |
| 4 | language-extensions | `archives` | Repoint the extension archive channel at the previous version. |
| 5 | intelligence-extensions | `archives` | Repoint the agent-readiness archive channel at the previous version. |
| 5a | cloud-extensions | `archives` | Repoint the Cloud CLI extension archive channel at the previous version. |
| 6 | cli | `archives` | Repoint the CLI download channel at the previous version. |
| 7 | templates | `archives`, `template-archives` | Repoint the template archive channel at the previous version. |

The final switch — renaming the private repository to the archive name and
publishing the released tree as one signed root commit in a new repository — is
**excluded**. Its rollback point is the archived private repository and its
verified history bundle, both untouched by this rehearsal and by every step
above. Until the new repository is public, its reversal is deleting it and
renaming the archive back; after that, it is setting the new repository back to
private. The archive is never published and no artifact is deleted, so the
switch stays reversible until an external clone of the public root exists.

## Non-destructive by construction

`TestReleaseRehearsalPerformsNoDestructiveOperation` reads the rehearsal's own
source and fails if it names a subprocess, a filesystem mutation, or a
publishing verb. The rehearsal reads the candidate cut through the public-cut
helpers and reads files; it cannot build, publish, tag, push, rewrite history,
or change repository visibility.

## Related decisions

- [ADR 0009 — the release rehearsal is a recorded verdict, not a release step](../adr/0009-release-rehearsal-recorded-verdict.md)
- [`tooling/cli-documents/specs/release-rehearsal.json`](../../../cli-documents/specs/release-rehearsal.json)
- [`RELEASE.md`](../../../../RELEASE.md)
