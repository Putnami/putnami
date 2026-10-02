# ADR 0004 — A publication job packs and the engine uploads

- **Status**: accepted
- **Scope**: `putnami-extension-sdk` (`publicationoutbox`, `npmpublish`,
  `gomodpublish`, `putpublish`, `oci.PushLayout`), the publication outbox
  contract in
  [`protocols/extension`](../../../../protocols/extension/README.md#publication-outbox),
  and the publication jobs of `@putnami/go` and `@putnami/typescript`

## Context

A managed publication writes npm versions, Go module versions, OCI images and
Put registry versions to registries that a bearer authorizes. A publication job runs repository code:
its packer, its scripts and its toolchain. A job that holds the bearer can use
it for anything the bearer allows, and a job that uploads decides which bytes
the registry receives.

## Decision

### 1. The job packs managed members into an outbox and uploads none of them

The engine names a private directory in `PUTNAMI_PUBLICATION_OUTBOX`. The job
writes each managed member's artifacts there and writes `outbox.json` last,
with `publicationoutbox.Writer`. The job holds no bearer. Members that a route
does not manage publish themselves and are not listed.

### 2. The descriptor is strict and bounded

`outbox.json` is the contract in `protocols/extension/publication_outbox.go`
and `schemas/publication-outbox.json`:

- unknown, duplicate and null members are refused;
- at most 64 members in 256 KiB;
- each member names its ecosystem (`npm`, `go`, `oci`, `put` or `archive`),
  coordinate, version and project, and carries exactly the block its ecosystem
  names; a `put` or `archive` member carries the `put` block;
- each artifact file carries its SHA-256 digest and size, within the
  ecosystem's cap: an npm tarball up to 256 MiB, a Go zip up to 500 MiB, a Put
  manifest payload up to 4 MiB and a Put blob up to 512 MiB;
- an OCI image names its layout directory, its repository and its expected
  manifest digest, which addresses its content;
- a `put` block names the manifest's media type, the manifest payload file,
  and up to 32 blobs with their media types, in the
  [Put write protocol](../../../../protocols/put/README.md).

An npm member carries no access level and no dist-tag, so repository code
chooses neither visibility nor channel. It names the registry the job resolved
(`--registry`, else `registries.npm.publish`, else npm's own registry), which
the managed npm rules accept: HTTPS, or HTTP to a loopback host, with no
credential, query or fragment. The engine refuses it when the project's
`registries.npm.publish` names another registry. A Go member carries no
registry endpoint: the engine reads `registries.go.origin`. An OCI member names
its repository, and the engine checks that host against
`registries.oci.publish` and against the registry its bearer is for. A `put`
or `archive` member carries no registry endpoint: the engine reads
`registries.put.registry`, else the default Put registry.

### 3. Paths stay inside the outbox

A path is relative, clean and slash-separated, with no `..` segment, no
absolute form, and never `outbox.json`. The reader walks each path from the
outbox root and refuses a symbolic link at any step. An OCI layout holds only
regular files and directories. The writer, the reader and `oci.PushLayout` open
each file without following a symbolic link and without waiting on a FIFO, then
require the regular file the walk found, so an entry replaced after the walk
fails the read instead of blocking the upload.

### 4. The engine uploads the bytes it verified

The engine reads the outbox after the job exits. `publicationoutbox.Read`
parses the descriptor, and `Outbox.ReadFile` reads each artifact once, refusing
a size or a digest that differs from the descriptor. The uploader receives the
bytes that were hashed, so a file changed after the check is never uploaded.
`Outbox.VerifyFile` applies the same checks while streaming the file through
the digest, so the engine can refuse every blob of a Put member before it asks
for the bearer without holding the blobs in memory; the upload still reads
each blob with `Outbox.ReadFile`.
`oci.PushLayout` hashes the manifest bytes it uploads and refuses a digest other
than the expected one before any request.

### 5. Uploaders take the bearer as an argument

`npmpublish`, `gomodpublish`, `putpublish` and `oci.PushLayout` receive the
bearer from their caller. None reads it from the environment or starts a process to obtain it.
Each sends it only to the registry it was given: requests refuse redirects and
bypass ambient proxies, and the OCI keychain answers only for the repository's
host. No error or report carries the bearer: Go and Put errors carry no
response body, npm error excerpts are redacted, and OCI errors are rewritten
without it.

### 6. Reuse is decided by digest

A version that the registry already serves at the artifact's digest is reused;
one served at another digest is refused. npm reads the tarball back, Go reads
the zip and go.mod back, OCI asks for the manifest by digest, and Put reads
the version's manifest back after a conflict and accepts only the same media
type and payload bytes. A Put member's digest is the SHA-256 of the manifest
payload the registry stores, which a publisher computes before it uploads.

## Rejected alternatives

- **Give the job a short-lived bearer.** It is still a bearer in repository
  code, and the registry still receives whatever bytes the job chooses.
- **Let the descriptor name access, dist-tags, or a registry the workspace
  does not declare.** The repository would choose how its artifacts are
  published, or send them somewhere its workspace contradicts. An npm member
  names its registry only so the engine uploads where the job resolved it, and
  the bearer still reaches only a registry the provider's credential serves.
- **Trust the descriptor's digests without reading the files.** A file can
  change between the job's write and the upload.

## Consequences

- The publication code moves into the SDK. The jobs call the same packages
  through thin wrappers until they write an outbox.
- Release archives, config members, migrations and site-content bundles are
  outbox members of ecosystem `archive` or `put`. Under `publication-v1` the
  engine fails a release in which it did not upload a selected one, so a job
  that publishes one itself packs it instead.
- A new ecosystem needs a block in the descriptor, its caps, and an uploader
  that follows rule 5.
