# ADR 0015 — Finalize uploads its screenshots through the proposals contract

- **Status**: accepted
- **Scope**: `@putnami/contributor` (`change finalize`),
  `go.putnami.dev/protocol/collaboration` (the proposals contract, version 1)
  and the `@putnami/cloud` collaboration provider
- **Amends**: [ADR 0014](0014-screenshots-are-the-proof-of-a-web-change.md),
  consequence 2: the captures no longer stay local
- **Extends**: [collaboration ADR 0006](../../../../protocols/collaboration/doc/adr/0006-version-1-is-stable.md),
  decision 2, which lets an optional operation join version 1

## Context

`change finalize` checks the screenshots of a ready web change and writes one
`screenshots:` line in the verification comment. The files stay under
`.putnami/` on the machine that took them, so a reviewer reads a count and
sees no picture.

Putnami Delivery now stores a change's screenshots and shows them on the
change page. It takes them in two steps under a person's session: the
manifest, which it answers with the digests it does not hold, then each
missing file.

`change finalize` reaches a backend only through the collaboration contracts.
The proposals contract has no operation that carries a file.

## Decision

1. **The proposals contract gains one optional operation, `screenshots`.**
   The request names the proposal, the commit the manifest captures and the
   path of the manifest file, whose capture files lie beside it. The provider
   reads the files itself: the provider process runs on the machine that holds
   them, and a document bounded at 4 MiB cannot carry up to 512 MiB of images.
   The result names the proposal, the commit, the project, the captures the
   manifest lists and the files the call uploaded. The operation is mutating
   and destructive, since a later manifest of the same project and head
   replaces the first. It joins version 1: no existing document changes.
2. **One call uploads one manifest.** How a provider stores the files is its
   own matter. A provider that learns which files it lacks sends only those,
   so a repeat after a failure resumes instead of starting again.
3. **`change finalize` uploads after the publication and before the
   verification comment.** It sends each manifest it accepted once, with the
   published head. A waiver, a draft and a change that touches no page send
   nothing.
4. **An absent capability warns; a refusal fails.** When the provider does not
   offer the operation, or answers `unsupported`, the run warns and goes on
   with the same verification comment. Any other failure stops the run with
   the provider's message, before the comment and the task transition: the
   screenshots are part of what the comment says was verified.
5. **`@putnami/cloud` serves it against Putnami Delivery.** It posts the
   manifest as written, then puts each missing file. A provider that does not
   serve the route yet (404 on the manifest) and a process that holds only a
   machine token both answer `unsupported`, with their own reason. Until the
   published Cloud schema declares the two routes, the call is hand-written
   and listed in `cloud/extension/clientgen.framework.json`.

## Consequences

- A reviewer sees the screenshots of a ready web change on the change, when
  the bound provider stores them.
- A workspace bound to a provider without the operation, such as
  `@putnami/github-collaboration`, publishes as before, with one warning.
- A hosted run on a machine token publishes without uploading. The captures
  of such a run reach the change only when a person finalizes.
- One provider launch uploads every file of a manifest inside the provider's
  time budget. A large first upload that runs out of time answers
  `unresolved`; running finalize again sends what is still missing.
- A CLI reads a provider's declaration against its own catalog, and one
  that does not know an operation withdraws that provider's whole contract
  version. A CLI older than this change therefore stops serving the
  proposals contract of an `@putnami/cloud` that declares `screenshots`. A
  workspace that pins the two separately moves the CLI pin first, or both in
  the same pull request. Dropping an unknown optional operation instead of
  withdrawing the contract would lift this constraint for every later
  operation; it is a separate decision on the protocol.
- The contract carries a local path. A provider that runs elsewhere than the
  caller cannot serve the operation; no such provider exists.

## Alternatives rejected

- **Two operations, one for the manifest and one per file.** They mirror the
  HTTP routes of one backend, and start the provider once per file, often
  dozens of times for one change, each start resolving the session again.
- **The Cloud extension uploads from a command of its own.** The upload would
  follow finalize instead of preceding the verification comment, and the
  build workflow would name a provider's command.
- **Carry the files in the request.** A request is one JSON document of at
  most 4 MiB.
