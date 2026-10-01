# Site Content Bundle Protocol

`go.putnami.dev/protocol/sitecontent` defines `site-content-bundle/v1`, the wire contract for content produced in one repository and merged into a Putnami site in another repository.

The publishable blob is ONE `tar.gz` archive:

- `bundle.json` at the archive root: the manifest, described by `schemas/site-content-bundle.json`. It is the reserved manifest carrier (`ManifestEntryName`), not a payload file — exempt from the files/mount rules.
- every payload file (regular files only).

Embedding the manifest is part of the contract: one digest addresses manifest and payload together, so the manifest cannot escape the consumer's content-address pin. A payload-only blob with a separately stored manifest is NOT a valid bundle. The reasoning and the rejected alternatives are recorded in [`doc/adr/0001-manifest-travels-inside-the-bundle.md`](doc/adr/0001-manifest-travels-inside-the-bundle.md).

Lifecycle: produce -> publish -> pin -> merge -> overlay. A producer assembles the archive with `BuildArchive` (deterministic bytes, self-verified) and publishes it by digest; the site pins that digest. The site verifies the blob with `VerifyArchive` (extract `bundle.json`, gate on `formatVersion` major, verify the payload against the manifest), validates URL-prefix ownership, and overlays files only when there are no collisions.

The normative artifacts are the JSON Schema [`schemas/site-content-bundle.json`](schemas/site-content-bundle.json) (published at its `$id`, `https://putnami.dev/schemas/protocol/sitecontent/site-content-bundle.json`) and the shared corpus under [`fixtures/`](fixtures/): `fixtures/valid` and `fixtures/invalid` cover the manifest, `fixtures/payload` the payload rules, and `fixtures/archive` the assembled `tar.gz`.

## Producers and consumers

| Role | Implementation |
| --- | --- |
| Producer (out of repo) | Putnami Cloud's docs pipeline builds a bundle with `BuildArchive` and publishes it to the put registry. No producer lives in this repository. |
| Consumer (in repo) | `sites/putnami.dev` pins a digest in `content.lock.json`, fetches by digest, verifies, and overlays into `.gen/public/docs` (`src/lib/content/`). The user-facing behaviour is documented in [`sites/putnami.dev/doc/02-concepts/site-content-bundles.md`](../../sites/putnami.dev/doc/02-concepts/site-content-bundles.md). |
| Reference implementation | This package: `BuildArchive`, `VerifyArchive`, `VerifyPayload`, `ValidateMergePlan`, and `ValidatePrefixOwners` are the semantics both sides must reproduce. |

## Versioning and compatibility

Two versions with different jobs:

- `FormatVersion` (`major.minor`, stamped into every `bundle.json`) versions the
  wire. Manifest objects are closed shapes: unknown fields are rejected, so
  adding even an optional field, or changing a shape, enum, or payload rule,
  requires a major bump. A minor bump is limited to revisions an existing
  consumer can validate under the unchanged schema and rules. An archive
  consumer first reads only the `formatVersion` envelope; when its major is
  newer, the consumer ignores the rest of that unknown manifest shape and
  falls back to baked content rather than guessing. `CompatibleFormatVersion`
  is the readable-major check, and it is contract, not implementation.
- `ProtocolVersion` versions this package's Go surface and is pinned by
  `conformance_test.go`, so a bump is a reviewed act.

Because a consumer pins a digest and falls back on a newer major, a producer can
ship a new major without coordinating a deploy: the site keeps serving the last
content it understands.

## Manifest

```json
{
  "$schema": "https://putnami.dev/schemas/protocol/sitecontent/site-content-bundle.json",
  "formatVersion": "1.0",
  "name": "cloud-docs",
  "mounts": [{ "urlPrefix": "/docs/cloud" }],
  "source": { "repo": "acme-platform", "commit": "abc123" },
  "files": [
    {
      "path": "docs/cloud/index.html",
      "digest": "1a31f208b4e0b7759528b7af8580d7beb300ba849259987113d8b8d7718ff4a5"
    }
  ]
}
```

`formatVersion` is `major.minor`. The schema is closed for the current major;
unknown fields are invalid. Consumers built for major `1` inspect the version
envelope before applying that schema and must ignore bundles with a newer major
and fall back to baked content.

## Payload Rules

- The archive is `tar.gz`; the reserved `bundle.json` root entry carries the manifest and is skipped by the payload rules below.
- Entries are regular files only; symlinks, hardlinks, devices, and directories are rejected.
- Paths are forward-slash relative paths with no absolute path, backslash, empty segment, `.`, or `..`.
- Every payload file must be listed exactly once in `files`.
- Every listed file must exist in the payload with a matching sha256 bare-hex digest.
- Every file URL (`"/" + path`) must fall under one declared mount.

## Merge Rules

Mounts partition site URL space. Equal, parent, or child prefixes overlap and are hard errors within a bundle, across bundles, and between bundle mounts and site-local prefixes. Use `ValidateMergePlan` or `ValidatePrefixOwners` so producers and consumers apply identical collision rules.

## Ownership, support, and evidence

**Owner** — the `protocols` standardization layer. `protocols/sitecontent`
(`go.putnami.dev/protocol/sitecontent`) is the single owning project: the
manifest, the payload rules, and the merge rules change here first, and both
repositories follow.

**Support status** — `preview` in the workspace-root
[`putnami.support.json`](../../putnami.support.json) catalog, whose vocabulary
[`protocols/support`](../support/README.md) defines. Public and usable, but not
yet carrying a stable cross-implementation commitment.

**Evidence for that status**

- The contract is in production: `sites/putnami.dev` pins a real bundle digest
  in `content.lock.json` and fails its build when verification fails.
- Published JSON Schema, a strict Go parser, and a fixture corpus that covers
  the manifest, the payload rules, and the assembled archive, including
  traversal, symlink, unlisted-file, and digest-mismatch rejection.
- What is *missing* for `stable`, and why the status is not higher: the
  consumer's implementation
  (`sites/putnami.dev/src/lib/content/sitecontent.ts`) is a hand-written
  TypeScript port that reproduces this package's rules and error codes but is
  tested against fixtures **ported** from this corpus rather than the corpus
  itself, so the two can drift without a test failing. The producer lives in
  another repository and runs no conformance suite here. Promotion needs the
  TypeScript port to consume `protocols/sitecontent/fixtures` directly, the way
  the platform and http-routes ports already do.
- Nothing silently regresses meanwhile: a bundle that fails verification fails
  the site build in CI, and a newer format major is skipped in favour of baked
  content.

**Owning user feature** — none in this repository. The user-visible outcome —
documentation produced elsewhere appears in the site's navigation and search —
is owned by `sites/putnami.dev`, which documents it in
[`doc/02-concepts/site-content-bundles.md`](../../sites/putnami.dev/doc/02-concepts/site-content-bundles.md);
this package owns only the wire that outcome travels on. Declaring a separate
product feature for the wire would split one user outcome across two owners. The
durable decisions live in [`doc/adr/`](doc/adr/) rather than in a spec, which by
contract details exactly one authored feature.
