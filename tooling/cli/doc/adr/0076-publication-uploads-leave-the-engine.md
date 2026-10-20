# ADR 0076 — Publication uploads leave the engine

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/credentialprovider`,
  `internal/jobs/publication`, `internal/cli`, `go.mod`)
- **Supersedes in part**: [ADR 0057](0057-publication-authority-stays-in-the-engine.md),
  decision 3 (the engine asks the `publish` credential and uploads each
  member in its own process) and the consequence that the engine carries the
  SDK's upload clients

## Context

Under ADR 0057, each upload node asks the credential provider session for the
`publish` credential of a member's registry, then uploads the packed npm
tarball, Go module zip, OCI layout or Put registry member in the engine's
process with the SDK's clients. The engine therefore holds a publish bearer
during every upload, and the core links a registry client library,
go-containerregistry, to push OCI layouts.

The registry protocol now defines `uploader-v1`: an `upload` op on the same
session, which a provider echoes only beside `publication-v1`
([`protocols/registry/uploader.go`](../../../../protocols/registry/uploader.go)).
The request names the plan digest, the registry URL, the kind of a Put
registry member, the outbox root and the member the engine read from the
outbox. It carries no credential. The answer names the digest the registry
stores and, for a release archive, the digest of each platform.

## Decision

1. **The engine offers `uploader-v1`.** `initialize` offers
   `credential-v1`, `publication-v1` and `uploader-v1`. The provider's echo
   decides, as for `publication-v1`.
2. **The upload node keeps every engine check, then sends `upload`.** It
   still validates the outbox descriptor, refuses a member, version or
   project the plan does not assign, resolves the member's registry from the
   workspace declarations, compares an npm registry with
   `registries.npm.publish` and an OCI repository host with
   `registries.oci.publish`, and checks an OCI member's layout digest against
   the image candidate its package step built. It derives registry hosts and
   schemes with `go.putnami.dev/protocol/oci`. It then sends one `upload` per
   member to the session that opened the plan, and asks for no credential.
   The provider reads every file of the member from the outbox and hashes it
   again, so the re-hash that ADR 0057 ran in the engine now runs where the
   bytes are read for the upload, and a mismatch is an
   `artifact_digest_mismatch` refusal.
3. **The answer must be the packed artifact.** The answered digest must equal
   the packed one: the npm tarball, the Go module zip, the OCI manifest or the
   Put registry manifest. Only a release archive may answer platforms, and
   they must be the platforms its packed manifest names. The node then writes
   `published-image.json` for an OCI member and reports one
   `published-member` event, as before.
4. **One bounded attempt.** One upload is bounded by 10 minutes plus 1 minute
   per whole 256 MiB of the files its member declares. The outbox contract
   bounds every file, so the bound is finite. An OCI member declares a layout
   directory and no file, so it gets 10 minutes. The upload is not retried.
   An upload the provider leaves unanswered may have reached the registry, so
   the session ends and every later op of the run fails. A refusal is an
   answer: it names its code, fails the member, and leaves the session open.
5. **A session that cannot upload fails before the first hook.** When a
   session echoes `publication-v1` without `uploader-v1`, and the run is
   hosted or enables the `publish` purpose, the provider's start fails the
   command before the first hook. The error is an invalid configuration
   (exit code 2) that names the extension that declares the provider and asks
   to upgrade it. A session without `publication-v1` keeps the release-set
   provider path.
6. **The core links no registry client.** The engine imports no upload
   client of the SDK, and `tooling/cli/go.mod` requires no OCI registry
   client library.

ADR 0055 is unchanged: no repository process reads a publish credential, and
now the engine reads none either.

## Rejected alternatives

- **Keep the in-engine upload beside `uploader-v1`.** Two upload paths would
  need two sets of proofs, and the engine would keep a publish bearer and a
  registry client for a provider that is about to be upgraded.
- **Retry an unanswered upload.** The registry may already hold the first
  attempt; a second one could conflict with it, and the session already
  overstayed its bound.
- **Fall back to the engine's own upload when the provider lacks
  `uploader-v1`.** It keeps the bearer in the engine, which is what this
  decision removes. Failing before the first hook names the fix instead.

## Consequences

- A hosted publish needs a credential provider that serves `uploader-v1`.
  An older provider fails the command before any repository code runs.
- The engine never asks for the `publish` credential. The `publish` purpose
  now only enables the publication session and starts the provider before
  the first hook.
- The registry protocols the provider speaks (npm, Go module proxy, OCI and
  `put-write/v1`) live in the provider's extension, not in the core.
- The digest comparison, `published-image.json` and the `published-member`
  events stay in the engine, so the release evidence is the same as under
  ADR 0057.
