# 0006: Version 1 of the contracts is stable

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`)
- **Amends**: [ADR 0002](0002-the-contracts-are-published-as-preview.md), decision 2

## Context

ADR 0002 published the module as `preview`, because every consumer lived in
this repository. That has changed. Putnami Cloud, a separate repository with
its own releases, requires the published module without `replace`: its source
domain serves the tasks contract on GitHub, Jira and Linear, and its
intelligence domain reads memory through it. The local, GitHub, GitLab, Jira
and Linear providers in this repository run the shared scenarios.

A team outside this repository needs to rely on the documents it writes and
reads for a year. `preview` lets a minor release break them.

## Decision

1. **The module is `stable` from release 0.5.0.** The workspace-root
   [`putnami.support.json`](../../../../putnami.support.json) records it, and
   `reviewedStableSubjects` in `protocols/support` pins it with no `default`
   or `parity` claim. Every criterion of support ADR 0002 §1 holds: Putnami
   Cloud consumes the module across a repository boundary; the catalog tests
   pin each contract version; the fixture corpus runs in `conformance_test.go`;
   a new member opens a new contract version; and `providertest` is the shared
   corpus every provider passes.
2. **Within a contract version, a document never changes.** Every request,
   result, response, envelope and binding that is valid under version 1 stays
   valid, with the same meaning. A new member, a new required operation, a
   narrowed value or a new outcome opens version 2. New optional operations and
   their documents may still join version 1.
3. **A frozen corpus holds the promise.** `fixtures/v1/` holds version 1
   documents as written on 2026-11-07, one or more per contract and document
   kind. A test parses every file and pins every file's SHA-256. Changing a
   frozen file fails the test: the fix is a new contract version, not an edited
   fixture.
4. **A version stays supported for a defined time.** Until 1.0, a minor
   release may end a version only with a migration documented in the release
   notes. From 1.0, a version stays readable for 12 months after its successor
   ships.
5. **The Go helpers and the guides are not part of the promise.** `Serve`,
   `ReadProviderOffer` and `providertest` follow the module's Go API and may
   change in a minor release before 1.0.

## Rejected alternatives

- **Stay `preview` and write the promise in the README.** The support catalog
  is the authority a reader checks, and `preview` there says a minor release
  may break the documents.
- **Promote each contract separately.** Status is per subject, and the four
  contracts ship as one module with one parser and one schema set.

## Consequences

- A team outside this repository can bind, serve and read version 1 and keep
  it working for 12 months past its successor from 1.0.
- A change to a version 1 document fails the frozen corpus and the support
  pin, so it cannot ship as cleanup.
- Losing Putnami Cloud as a consumer, or the frozen corpus, demotes the module
  to `preview` under support ADR 0002 §2.
