# Package

The package command creates distribution packages from build output. It supports two channels: **npm** (registry tarball) and **docker** (container image).

## Overview

- Prepares npm packages from transpiled library and type declaration output
- Builds Docker images from compiled executables with a minimal distroless base
- Handles workspace dependency resolution at the git-derived version of each package's line
- Inherits workspace metadata (author, license, repository) into output packages
- Writes structured metadata for downstream CI/CD consumption

## Usage

### Basic Usage

```bash
# Create npm package
putnami package . --npm

# Build Docker image
putnami package . --docker

# Both
putnami package . --npm --docker

# Preview what would be packaged
putnami package . --npm --dry-run
```

### Common Patterns

```bash
# Override the npm registry for a one-off publish
putnami package . --npm --registry https://npm.mycompany.com

# Docker with an extra local tag
putnami package . --docker --docker-tag v1.0.0

# Docker for a specific platform
putnami package . --docker --platform linux/arm64
```

To use a first-class image project as the base, set
`options.package.dockerBaseProject` to its project ID. Putnami adds that as a
graph edge, packages the base first, and composes from its typed local OCI layout
and candidate digest. No registry is needed between the two package tasks. See
[Image projects and project-derived bases](../../../tooling/cli/doc/05-configuration.md#image-projects-and-project-derived-bases).

## Pipeline

```text
generate ──→ transpile ──→ types ──→ npm
         └──→ compile ──→ docker
```

The package command runs the full build pipeline first, then packages the output. The npm channel requires transpile + types output. The docker channel requires compile output.

When present, a project's `AI.md` and `doc/` directory are included in its npm package. `AI.md` is the sole supported root guidance file.

## npm Channel

### What It Does

1. Locates transpiled lib output and type declarations from build output
2. Copies lib files and merges type declarations
3. Generates an output `package.json` with:
   - The version of the project's line, derived from git
   - Resolved `workspace:*` dependencies at the same per-package versions
   - Inherited workspace metadata (author, license, repository, bugs, homepage)
   - Stripped dev-only fields (devDependencies, scripts, private)
   - Source paths rewritten from `.ts`/`.tsx` to `.js` (see below)
4. Copies README.md and LICENSE files if present
5. Writes `npm/channel.json` with the version and the channel packaged, inside the directory this step owns

### Source Path Rewriting

The source `package.json` points at TypeScript; the tarball contains only the
transpiled `.js`. Every field that names a source file is rewritten:

| Field | Rewritten |
|-------|-----------|
| `main` | value |
| `exports` | all values, at every condition depth |
| `bin` | all values |
| `browser` | both **keys** and values (a `false` value — module stubbed out for browsers — is preserved verbatim) |

The `browser` map's keys name the module being replaced, so leaving them on
`.ts` would publish a map that redirects nothing: the paths it names were never
shipped. The map is a per-module redirect for consuming bundlers and is **not**
the browser/server publication boundary — that is enforced upstream by the
transpile phase, which builds browser-condition entrypoints in their own
`bun build` graph (see [Build → Build graph partition](./02-build.md#build-graph-partition)).

Types entries are then injected after rewriting, so `exports` conditions carry
a leading `types` key pointing at a `.d.ts` that was actually emitted.

### Workspace Dependency Resolution

Dependencies declared as `workspace:*` are resolved to concrete versions:

| Commit | Resolution |
|------|-----------|
| Untagged | Next version + the ordered suffix `<commitTime>-<sha>[-<dirtyHash>]` (e.g., `0.0.3-20260902173000-abc1234-def5678`) |
| Tagged with the line's tag | The tag's version only (e.g., `0.0.3`) |

Both come from git: the line's last tag advanced by the conventional commits
that touch it. Nothing in a configuration file declares a version, and no flag
selects a "stable" shape.

A direct extension invocation that the CLI did not stamp derives the suffix
itself, with the CLI's rules: `PUTNAMI_SOURCE_REVISION` binds `<sha>` to the
first 12 characters of that commit, and `PUTNAMI_SOURCE_COMMIT_TIME` supplies
its time when the repository lacks it. See the CLI's
[pre-release version shape](../../../tooling/cli/doc/14-version-management.md#pre-release-version-shape).

### Release-set packaging

When core supplies a validated `distribution/release-set/v2` plan for
`publish --impacted --channel <c>` or `publish --all --channel <c>`, the plan
replaces the line's git-derived version for the selected npm packages:

- the selected package receives its candidate version;
- a selected internal dependency receives its candidate version;
- an unchanged internal dependency keeps the exact version from the resolved
  channel head, so it is neither packaged nor uploaded again;
- authored `workspace:*`, `workspace:~`, and `workspace:^` operator semantics
  are preserved around that per-package version;
- internal `catalog:` aliases use the same release-set version map, while
  concrete external catalog entries remain unchanged.

The plan selects a member when its **selection fingerprint** — the execution
cache key of its package task minus the embedded version — differs from the
baseline head's record, or when one of its internal dependencies is selected;
`--all` selects every member. That key already covers the root `package.json`
catalog, so a catalog change republishes the packages it feeds without any
declared workspace input. The plan carries the head resolved for every listed
channel, so the coordinator releases each by compare-and-swap instead of
asserting an empty channel.

Packaging fails closed if the selected npm member is missing, the plan is
malformed, or a published dependency map would retain an unresolved
`workspace:` or `catalog:` selector. `devDependencies` remain excluded from the
published package.

After packaging, managed release-set publication uploads an
immutable `.tgz`, hashes the exact bytes with SHA-256, downloads the registry
copy with the same target-bound credential, and emits a `kind=published` proof
only when both digests match. It never requests public access or an anonymous
read. Repository `access` parameters cannot control managed visibility: the
registry's server-owned policy keeps the candidate private. A future public
release requires a separate explicit server attestation; neither the
selection nor release-set metadata is visibility authority. An authenticated
404 or a byte mismatch fails publication without emitting proof. The npm upload
uses the registry's native PUT with an empty `dist-tags` map: no dist-tag is
written on any path, by design, because a channel is moved by the release and
never by a publisher. The
release-set provider alone coordinates the verified private set. Dry runs never
emit publication proof.

`putnami publish --npm --dry-run` uploads nothing and asks the registry one
question: does it already hold this package at this version. It emits one
`member-probe` event:

| State | When |
|-------|------|
| `absent` | The registry does not hold the version. |
| `identical` | The registry copy has the digest of the tarball the real publish would upload. The real publish reuses it. |
| `conflict` | The registry copy differs, or the staged package cannot be packed to compare. |
| `unverified` | The registry cannot be reached, refuses the request, or answers outside the protocol. |

How it asks depends on the publication:

- **A release-set member** sends one GET of the version's tarball through the
  HTTP client of the managed publication, and runs no `npm` process. The
  request carries a bearer only when the credential seam yields one; otherwise
  it is anonymous and the event says so. It requests no publish lease. It packs
  the staged package with `bun pm pack` only when the registry holds the
  version, and compares SHA-256 digests.
- **Any other publication** asks through `npm view <name>@<version> dist --json`
  with the environment the real publish runs `npm` with, because npm's
  configuration owns the registry and the credential. It compares the
  `dist.integrity` npm reports with the tarball `npm pack --ignore-scripts`
  writes. It cannot observe whether npm sent a credential, so it never marks an
  answer anonymous. A host without `npm` reports `unverified`. The real publish
  of such a package reuses any version the registry already holds without
  comparing content, so the dry run is stricter than the publish.

The task succeeds whatever the answer; the CLI fails the run on `conflict` and
`unverified`
(see [Publish and `--dry-run`](../../../tooling/cli/doc/03-commands.md#registry-checks)).

Under a native publication run on Putnami's CI runner there is no signed-in
session. The runner holds the publication capability itself and exports a
numeric-loopback broker as `PUTNAMI_REGISTRY_NPM_URL`
(`http://127.0.0.1:<port>/npm`). A managed publication then uploads to and
verifies through that broker and asks the credential seam about the broker
host, which the cloud answers with the run's capability. The declared registry
stays the logical coordinate and the wire payload carries no registry URL.
Without the variable the path above is unchanged; a remote HTTPS value is
ignored as Cloud compatibility input; a malformed loopback value fails the job
before any credential or network call. Unmanaged publication never reads it.

The staged npm tree is mode-canonical before packing: directories and ordinary
files use `0755` and `0644`, while `package.json` `bin` targets and build
outputs that start with a `#!` line use `0755`. Which files are executable
comes from those two sources only; an executable bit the build left on disk
does not count. The resulting archive digest therefore does not depend on the
publishing process umask. On Windows the disk keeps no executable bit, so the
staged tree cannot record these modes there, and the archive modes come from
`bun pm pack` or `npm`; no test checks them on Windows yet. Registry selection is also frozen before upload:
an explicit `--registry` wins, otherwise `registries.npm.publish` from
`putnami.workspace.json` (or the project's override of that entry) decides.
`registries.npm.scopes` writes one `@<scope>:registry=` line per entry into the
workspace `.npmrc`; no registry URL is hard-coded and none is authored by hand. Unsafe or credential-bearing
registry URLs are rejected without rendering URL userinfo. Managed staged
`publishConfig.registry`, `publishConfig.access`, and `publishConfig.tag` are
rejected before a lease or npm invocation; any remaining non-authority
`publishConfig` metadata is stripped from the native PUT manifest. The only
managed package-manager subprocess is the local `bun pm pack` (bun is the
workspace's package manager; the CI runner image ships node and bun, not npm):
it runs inside the staged package directory with `--ignore-scripts`, gets no
registry bearer, and removes ambient npm auth/config, Putnami capability
tokens, Node/Bun preload hooks, process-loader hooks, agent sockets, and proxy
variables. Registry existence checks, upload, and exact-byte verification use
bounded direct HTTP against the constructed same-origin tarball URL. Those
requests authenticate explicitly, disable ambient proxy routing and redirects,
and never render registry error bodies.

For a linked developer checkout, the npm publisher waits until the staged
package name is known and then asks `@putnami/cloud` for a short-lived lease
bound to the linked owner workspace, the package's canonical final component,
and the `publish` action. The complete scoped coordinate remains unchanged for
npm publication and evidence. Managed publication fails before invoking npm if
that target-bound credential is unavailable. A signed-in laptop can therefore
run `publish --all --npm` without a host-wide registry key. Third-party and
cloudless registries use native npm credentials only on the unmanaged path. A
managed third-party target without an exact lease fails closed and never falls
back to repository or ambient npm credentials.

Without a release-set plan, the existing full/cloudless behavior and its
workspace/git-derived versions remain unchanged.

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--npm` | `boolean` | `false` | Create npm package |
| `--access` | `string` | `public` | Package access level for unmanaged/cloudless publication; managed release sets ignore it and remain server-private |
| `--registry` | `string` | — | Custom npm registry URL |
| `--dry-run` | `boolean` | `false` | Show what would be packaged without creating artifacts |

## Docker Channel

### What It Does

1. Locates compiled binary from build output matching the target platform
2. Copies `.gen/public/` assets next to the binary (if present)
3. Computes the image **content hash** (assembly spec + binary + staged assets)
4. Stages a content stamp (`putnami-buildinfo.json` with `name` + `contentHash`)
5. Assembles the image **in-process** (no docker daemon, no Dockerfile) on a
   **digest-pinned** `gcr.io/distroless/cc-debian12:nonroot` base, tagged
   `name:c-<contentHash>`
6. Writes the image as a self-contained OCI layout under `docker/oci`
7. Writes `docker/manifest.json` with the content hash, the **reproducible
   candidate digest** (known before any push), target platform, local layout
   path, and an advisory version. It contains no remote or publication claim.

### Assembled Image

The image is equivalent to this recipe — but assembled deterministically
(fixed timestamps, explicit file modes, pinned gzip level), so identical
inputs produce an identical digest on any machine:

```dockerfile
FROM gcr.io/distroless/cc-debian12@sha256:<pinned>
COPY <binary> /app/<binary>         # mode 0755
COPY .gen /app/.gen                 # Only if .gen/public/ exists
WORKDIR /app
ENV NODE_ENV=production
ENV PORT=<port>
ENV PWD=/app
EXPOSE <port>
ENTRYPOINT ["/app/<binary>"]
COPY putnami-buildinfo.json /app/.gen/version.json
```

The base is pinned by digest because a moving tag would make the digest
depend on when the base was resolved; base CVE updates arrive as explicit,
reviewable pin bumps (or per-workspace via `--docker-base-image`, which must
also be digest-pinned). The cc-debian12 variant provides the glibc and
libstdc++ that bun-compiled binaries need.

The image is **content-pure**: it carries no git-derived bytes — no version,
revision, or branch, and no OCI version/revision labels — so identical content
always produces an identical image and the digest can travel across commits.
Identity is assigned at publish time: `putnami publish --docker` pushes the
content once per unique content, verifies the remote digest, and writes the
publish-owned `publish/docker/published-image.json` record without changing
package bytes. It tags the image with its version and nothing
else — channel tags are written by the release projection, not by the
publisher — and the deployer injects the release id as
`PUTNAMI_VERSION` / `PUTNAMI_REVISION` environment.

The content stamp lands at `/app/.gen/version.json`, where `getBuildInfo()`
from `@putnami/utils` reads it at runtime and overlays the deploy-injected
identity environment — deployed containers report both their content hash and
the published release id.

### Caching

The `package-docker` cache key depends only on source inputs, so unchanged
content is a cache hit even at a new release id — no rebuild, and at publish
time no layer upload. The OCI layout lives inside the task output, so a cache
restore brings the pushable image itself, not just a manifest pointing at a
daemon image that may not exist locally. The pinned base is cached once per
digest under `.putnami/cache/oci-base` (packaging is offline after first
use).

### Platform Mapping

The Docker channel maps platform specifiers to compiled binary suffixes:

| Platform | Compile target | Binary Suffix |
|----------|----------------|---------------|
| `linux/amd64` | `bun-linux-x64` | `-linux-x64` |
| `linux/arm64` | `bun-linux-arm64` | `-linux-arm64` |

The mapping is used **twice, from one place**: `package --docker` binds its
compile step to the image's target, so that single bun target is the only one
compiled, and the packager then looks the binary back up by the same suffix.
Compiling the full target matrix to copy one executable into a
single-platform image meant three whole bun compiles were discarded per
packaged service.

`--platform` therefore decides what gets compiled, not just what gets
assembled. It is a plan-time parameter and a cache-key input: changing it
changes the compile step's key, so a four-target entry can never answer a
one-target request. `--compile-target` still overrides it explicitly.

### Image Naming

The image name is derived from the project name:
- Strips `@` scope prefix
- Replaces `/` with `-`
- Example: `go.putnami.dev/tooling/cli` → `go.putnami.dev-tooling-cli`

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--docker` | `boolean` | `false` | Assemble Docker image |
| `--docker-registry` | `string` | — | **Override** for one push. The declared target is `registries.oci.publish` in `putnami.workspace.json`; this flag ranks above it, and neither affects local package candidate bytes |
| `--docker-tag` | `string` | — | Extra alias tag next to the content-addressed tag |
| `--docker-base-image` | `string` | pinned distroless | Override the digest-pinned base image (must include `@sha256:...`) |
| `--docker-base-project` | `string` | — | Use a first-class image dependency's typed local OCI candidate as the base |
| `--docker-load` | `boolean` | `false` | Also load the assembled image into the local docker daemon |
| `--platform` | `string` | `linux/amd64` | Docker image target platform — and the only bun target the compile step builds |
| `--port` | `number` | `3000` | Container port to expose |
| `--workspace-builder-image` | `string` | — | Accepted for compatibility; multi-stage builds are a Go-extension feature |

## Metadata Output

Each channel writes a `channel.json` record INSIDE the output directory it owns
(`npm/channel.json`, `docker/channel.json`), which downstream CI/CD steps can
consume:

```json
{
  "version": "0.0.3-20260902173000-abc1234-def5678",
  "channels": ["npm"]
}
```

There is no shared file to merge into: the channel index is derived over every
record (`pkgmeta.ReadChannelIndex`), so a cache restore of one channel's output
restores its record without the other channel running.

## Boundaries

- **Scope**: Creating npm tarballs and Docker images from build output
- **Out of scope**: Registry publishing (see the publish docs). A `type: "image"` project packages a local OCI candidate for downstream composition; its publish task alone derives a remote target, pushes/verifies the digest, and never assigns release/channel tags.
- **Dependencies**: Requires build output. npm channel requires transpile + types output. Docker channel needs network on first use to fetch the pinned base (cached afterwards); the `docker` CLI is only needed for `--docker-load`.
- **Not supported**: Custom Dockerfiles — the image recipe is fixed so digests stay reproducible.

## Measured

`bench/single-binary-packaging/` holds the measured comparison behind this
packaging shape: `bun build --compile` (± `--asset`, `--bytecode`,
`--sourcemap=inline`) against the `oven/bun` + `bun run` image baseline —
image size, cold start, memory, build reproducibility, and a runtime
compatibility checklist.

## OCI release-set coordinates

An image member names its full native repository path without the registry host.
For `oci.example.com/team/app`, the coordinate is `team/app`. The workspace's
`registries.oci.publish` supplies the namespace; a project's OCI registry entry
replaces that workspace entry. The probe and publisher use the same coordinate
so registry authorization and channel tags address the uploaded repository.
