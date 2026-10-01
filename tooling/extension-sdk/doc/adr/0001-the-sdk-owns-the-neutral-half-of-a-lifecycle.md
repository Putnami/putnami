# ADR 0001 — The SDK owns the neutral half of every shared lifecycle

- **Status**: accepted
- **Scope**: `putnami-extension-sdk` (`tooling/extension-sdk`)

## Context

Several extension responsibilities are only partly language-specific. Evicting a
machine cache to a budget is arithmetic plus a cross-process lock; what an entry
*is* belongs to the extension. Aggregating infrastructure requirements over a
closure is a graph merge; runtime compatibility belongs to the language.
Provisioning a test database is container lifecycle and lease management;
which projects need one belongs to the closure. Recording which channels a
package step produced is a file inside the directory that step owns; what a
channel contains belongs to the packager.

When each extension writes its own whole half, the neutral parts drift, and the
drift stays invisible until someone reads the implementations side by side.

## Decision

For each shared lifecycle, the SDK supplies the neutral half as a library and
the extension supplies only its ecosystem's knowledge.

- `cachepolicy`: recency sort, low watermark, grace window, collector lock and
  freed-bytes summary. The extension supplies what one entry is.
- `infraagg`: closure merge, overrides, authored-over-default precedence and the
  atomic manifest write. The extension supplies its runtime compatibility hook.
- `dbtestenv`: the `auto`/`require`/`skip` policy, the closure datasource merge,
  the digest-keyed container with reuse and reaping, and the invocation-scoped
  binding artifact ([ADR 0002](0002-a-secret-lives-in-one-artifact.md)). The
  extension registers the producer and finalizer on its test pipeline.
- `genresult`: every path a generation manifest serializes is project-relative
  and slash-separated.
- `pkgmeta`: the readers of the package-to-publish contract,
  `WriteChannelRecord`, `ReadChannelIndex`, `PackageMetadata` (the archive
  publication manifest the uploader reads) and `ArchivePlatforms`, the one
  archive platform matrix. Its Docker types separate the package-owned local
  OCI candidate from publish-owned, remotely verified immutable evidence.
- `imagepkg` and `dockerpublish`: package resolves inputs and assembles a local
  OCI layout; publish resolves the target and credentials, verifies the
  registry digest, and records the immutable reference in its own output.
- `hostenv`: the one list of managed-runtime identity variables a test
  subprocess must not inherit, and the scrub that removes them.
- `privatebroker`: the loopback registry broker endpoint every publisher reads.

A helper belongs here when the orchestrator handshake or a shared lifecycle
requires it. General-purpose Go utility code does not.

## Invariants

- A lifecycle whose neutral half exists in the SDK has exactly one
  implementation of that half.
- The neutral half never encodes a language-specific fact.
- `WriteChannelRecord` writes a packager's record inside the output directory it
  owns. The channel index is derived over those records and is never a file.
  `metadata.json` has one writer per project, which states the whole document.
- The identity-scrub list holds deployment identity only; credentials,
  endpoints and project or region selectors are preserved.
- A generation manifest never serializes an absolute path.
- Package never resolves an output registry or emits remote evidence; publish
  never rewrites the package-owned candidate bytes.
- A project base is consumed through its typed local OCI layout and candidate
  digest; publication is not a prerequisite for local composition.

### Private registry broker

When an invocation supplies a numeric-loopback registry broker URL, every
request of that registry kind goes to the broker, and the publisher asks the
credential seam about the broker host. `dockerpublish` reads
`PUTNAMI_REGISTRY_OCI_URL`, the Go module publisher
`PUTNAMI_REGISTRY_GOMOD_URL` (`/go`), and the npm publisher
`PUTNAMI_REGISTRY_NPM_URL` (`/npm`).

- Published coordinates and evidence keep the canonical managed registry.
- A malformed endpoint fails the publication; there is no fallback to the
  remote registry. Without the variable, the direct HTTPS path applies.
- Under the OCI broker, only the managed destination is accepted: an external
  target such as `ghcr.io` is refused before network access. Publish external
  images in a separate invocation.
- The rule covers every image publication, including a Go or TypeScript
  workload's `--docker`. The publisher writes content by digest and applies the
  version as the only tag, because the broker admits no content tag. A
  daemon-built candidate without an OCI layout is refused.

## Rejected alternatives

- **Each extension implements the whole lifecycle.** The halves diverge
  silently.
- **Move the neutral half into the CLI.** Puts language lifecycles back in core
  and makes every policy change a CLI release.
- **One library per lifecycle.** The parts share the context, emitter and
  manifest types; splitting them creates a version matrix nobody asked for.
- **Document the convention and rely on review.** A documented convention is
  what diverged.

## Consequences

- A new lifecycle primitive is an SDK change plus its adoption in every
  extension owning that lifecycle, in one change. A half-adopted primitive is
  what this ADR prevents.
- The SDK depends on the protocol modules and follows their release cadence; an
  extension cannot pin an old SDK with a new protocol.
- Genuinely different halves stay separate: a Go and a TypeScript packager still
  assemble different artifacts.
