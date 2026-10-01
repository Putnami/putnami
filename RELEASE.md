# Release Planning

Release decisions live here so they do not become part of the SDD protocols or
the current support catalog. This document records the intended release gate;
it does not announce a release or extend today's support promise.

Current package and protocol classifications remain authoritative in
[`putnami.support.json`](putnami.support.json). The current license is always the
checked-in [`LICENSE.md`](LICENSE.md).

This document is the *plan*. How a release is actually cut — the cadence, what a
release may contain, the checklist it must pass, rollback, and security releases
— is [RELEASING.md](RELEASING.md). What the release promises a user is the
[First Public-Release Contract](README.md#first-public-release-contract).

## First OSS release candidate

The candidate binary matrix is:

| Operating system | amd64 | arm64 |
| --- | --- | --- |
| macOS (`darwin`) | Candidate | Candidate |
| Linux | Candidate | Candidate |
| Windows | Candidate | Not built |

Each target must pass the public golden-path smoke on an actual matching runner
before it is advertised as supported. Candidate runs block promotion; the four
macOS and Linux targets also monitor `latest` nightly through the public
installer URL. The Windows host exists only for a run, so it has no nightly job.
Post-publication automation, when configured, belongs to an external release
execution plane because the repository CI schema has no schedule or
operating-system matrix. The release owner owns escalation and the documented
five-runner manual fallback whether or not that plane is available. Without
that automation, the fallback includes the same four-target `latest` run
nightly until automation is configured or restored.

## Release rehearsal

Before any release step runs, one repeatable rehearsal answers a single
question over the current tree: could this tree become the intended public
release? The rehearsal is read-only. It never builds, publishes, tags, pushes,
rewrites history, or changes repository visibility.

Run it through the owning project's gate:

```
./putnamiw lint,test,build --projects @putnami/cli --enforce-coverage
```

It checks ten evidence items — version and artifact manifest, checksums and
provenance, support report, migration report, scrub result, golden-path result,
governance checklist, label/scope bootstrap plan, publish order, and rollback
plan — and records an explicit GO or NO-GO.

- The declared inputs, publish order, rollback points and fresh-root metadata
  live in
  [`tooling/cli/internal/cli/testdata/release-plan.json`](tooling/cli/internal/cli/testdata/release-plan.json).
- The recorded verdict lives in
  [`tooling/cli/doc/reports/release-rehearsal.md`](tooling/cli/doc/reports/release-rehearsal.md).
- The operator guide is
  [`tooling/cli/doc/20-release-rehearsal.md`](tooling/cli/doc/20-release-rehearsal.md).
- The digest, builder identity, signature, reproducibility, and immutable
  rollback rules are the durable
  [`release provenance policy`](tooling/cli/doc/release-provenance.md).

**NO-GO is a valid result.** The rehearsal never fails because a release input
is missing; it fails when the recorded verdict stops matching the tree. A
blocked check names the missing input and the issue that owns it.

The rehearsal deliberately stops before the private-archive/public-root switch.
That switch is the final operation, and it runs only after a human reads the
recorded verdict. It renames the private repository to the archive name and
publishes the released tree as one signed root commit in a new repository. The
archive keeps the full private history and is never published. Until the new
repository is public, reversing the switch means deleting it and renaming the
archive back; after that, it means setting the new repository back to private.
No artifact is deleted.

## Compatibility before v1.0.0

Before v1.0.0, breaking changes are allowed when the release notes or migration
documentation state the required user changes. A pre-1.0 minor release does not
promise backward compatibility with earlier minor releases.

The exact per-format read windows, migrations, deliberate rejections, and
immutable prior-release evidence are maintained in
[`tooling/cli/doc/21-compatibility-and-migration.md`](tooling/cli/doc/21-compatibility-and-migration.md)
and its accepted [compatibility-budget ADR](tooling/cli/doc/adr/0010-compatibility-budget.md).

## License transition

Putnami is currently distributed under FSL-1.1-MIT. The project intends to make
v1.0.0 available under the MIT License, but that change is the final release
step: until the checked-in license is changed, FSL-1.1-MIT controls. This intent
is separate from the automatic future-license grant already contained in
`LICENSE.md`.
