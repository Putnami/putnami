# ADR 0004 — A publication job packs and the engine uploads

- **Status**: accepted
- **Scope**: `putnami-extension-sdk` (`publicationoutbox`, `npmpublish`,
  `gomodpublish`, `oci.PushLayout`), the publication outbox contract in
  [`protocols/extension`](../../../../protocols/extension/README.md#publication-outbox),
  and the publication jobs of `@putnami/go` and `@putnami/typescript`

## Context

A managed publication writes npm versions, Go module versions and OCI images to
registries that a bearer authorizes. A publication job runs repository code:
its packer, its scripts and its toolchain. A job that holds the bearer can use
it for anything the bearer allows, and a job that uploads decides which bytes
the registry receives.

## Decision

### 1. The job packs into an outbox and uploads nothing

The engine names a private directory in `PUTNAMI_PUBLICATION_OUTBOX`. The job
writes each managed member's artifacts there and writes `outbox.json` last,
with `publicationoutbox.Writer`. The job holds no bearer. Members that a route
does not manage publish themselves and are not listed.

### 2. The descriptor is strict and bounded

`outbox.json` is the contract in `protocols/extension/publication_outbox.go`
and `schemas/publication-outbox.json`:

- unknown, duplicate and null members are refused;
- at most 64 members in 256 KiB;
- each member names its ecosystem (`npm`, `go` or `oci`), coordinate, version
  and project, and carries exactly the block its ecosystem names;
- each artifact file carries its SHA-256 digest and size, within the
  ecosystem's cap: an npm tarball up to 256 MiB, a Go zip up to 500 MiB;
- an OCI image names its layout directory, its repository and its expected
  manifest digest, which addresses its content.

An npm member carries no access level and no dist-tag, and npm and Go members
carry no registry endpoint. Repository code chooses neither visibility, nor
channel, nor destination. An OCI member names its repository, and the engine
checks that host against the registry its bearer is for.

### 3. Paths stay inside the outbox

A path is relative, clean and slash-separated, with no `..` segment, no
absolute form, and never `outbox.json`. The reader walks each path from the
outbox root and refuses a symbolic link at any step. An OCI layout holds only
regular files and directories.

### 4. The engine uploads the bytes it verified

The engine reads the outbox after the job exits. `publicationoutbox.Read`
parses the descriptor, and `Outbox.ReadFile` reads each artifact once, refusing
a size or a digest that differs from the descriptor. The uploader receives the
bytes that were hashed, so a file changed after the check is never uploaded.
`oci.PushLayout` hashes the manifest bytes it uploads and refuses a digest other
than the expected one before any request.

### 5. Uploaders take the bearer as an argument

`npmpublish`, `gomodpublish` and `oci.PushLayout` receive the bearer from their
caller. None reads it from the environment or starts a process to obtain it.
Each sends it only to the registry it was given: requests refuse redirects and
bypass ambient proxies, and the OCI keychain answers only for the repository's
host. No error or report carries the bearer: Go errors carry no response body,
npm error excerpts are redacted, and OCI errors are rewritten without it.

### 6. Reuse is decided by digest

A version that the registry already serves at the artifact's digest is reused;
one served at another digest is refused. npm reads the tarball back, Go reads
the zip and go.mod back, and OCI asks for the manifest by digest.

## Rejected alternatives

- **Give the job a short-lived bearer.** It is still a bearer in repository
  code, and the registry still receives whatever bytes the job chooses.
- **Let the descriptor name registry endpoints, access or dist-tags.** The
  repository would choose where and how its artifacts are published.
- **Trust the descriptor's digests without reading the files.** A file can
  change between the job's write and the upload.

## Consequences

- The publication code moves into the SDK. The jobs call the same packages
  through thin wrappers until they write an outbox.
- Release archives uploaded through `cloud-publish-archives` are not outbox
  members.
- A new ecosystem needs a block in the descriptor, its caps, and an uploader
  that follows rule 5.
