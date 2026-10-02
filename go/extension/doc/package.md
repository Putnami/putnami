# Package

**Command:** `putnami package [project]`

Creates distribution-ready artifacts for a Go project. Supports three channels: **archives** (platform-specific tarballs), **docker** (local Docker image), and **go** (Go module source for publishing).

This step **does not upload** anything. Workloads and projects with
`type: "image"` both produce a typed local OCI candidate; publishing and remote
verification are separate. Dependent workloads compose directly from that
candidate during the same graph run, so no intermediate registry is required.

An authored `type: "image"` project with `publish: ["docker"]` participates in
native release sets even when it has no `go.mod`. The Go extension declares its
OCI member with the `image` package step and `docker` publish step. The member
uses the same resolved project name and registry namespace as the immutable
image publisher: an explicit project name takes precedence over a scope name
pattern. A declared `registries.oci.publish` namespace is retained in the native
member; the managed registry defaults to the workspace name only when no
namespace is declared. Native publication must match that selected member and
its typed project identity. The release-set distribution namespace is separate
from the OCI repository namespace. Packaging alone does not opt a project into
publication.

## Overview

- **archives**: cross-compiles the release matrix — by default the 5 platforms (linux-x64, linux-arm64, darwin-x64, darwin-arm64, windows-x64) — and packages each as a `.tar.gz` archive. The windows-x64 archive carries each program as `compiled/<name>.exe`. It packages the platforms at the same time, within the task's CPU grant.
- **docker**: builds a Docker image locally using `gcr.io/distroless/static:nonroot` as the base; consumes the binary the `package` pipeline's own cross-compile step produced — **one** binary, for the image's `--platform`
- **go**: prepares Go module source for publishing by stripping `replace` directives and stamping the version

Channels are activated by passing the corresponding flag to `putnami package`.

`package` is the **distribution intent**: it schedules its own cross-compile
step, so it needs no prior `putnami build`. Ordinary [`build`](./build.md)
compiles for the host alone — reach for `package` (or `build --target` /
`platforms`) when you need other platforms.

## Platform matrix

The cross-compile step builds exactly what the **active channels consume** — not
a fixed matrix:

| Active channel | Platforms compiled |
|----------------|--------------------|
| `docker` | exactly one: the image's `--platform` (default `linux/amd64`) |
| `archives` | the declared release matrix: `platforms` if declared, otherwise the 4 archive platforms |
| `go` (module source) | none — the channel carries no binary |
| several at once | the union of what each consumes |
| `--target <os/arch>` | exactly that platform, whatever the channels are |

**Active** means the union of the project's `publish` declaration and the
channel flags this invocation passed — the same union the pipeline uses to
decide which packagers run. `putnami package --docker` on a project that
declares `publish: ["archives"]` runs *both* packagers, so it compiles both
sets: a flag adds a channel, it never removes a declared one.

An image carries one executable for one `os/arch`; compiling four to copy one in
was three whole cross-compiles discarded per packaged service. A Go-module-only
project compiles nothing at all, because no channel reads the binaries.

The release matrix is **declared, never widened**. `platforms` (the
`--platforms` flag, or `options["@putnami/go"].platforms` in `putnami.json`)
names it; a project that declares nothing gets the whole archive matrix. The cross-compile step and the archive packager read that one
parameter, and resolve the active channels the same way, so what gets compiled
and what gets staged cannot disagree:

```jsonc
// putnami.json — publish linux only
{
  "publish": ["archives"],
  "options": { "@putnami/go": { "platforms": ["linux/amd64", "linux/arm64"] } }
}
```

```bash
# one image platform, one compile
putnami package . --docker --platform linux/arm64
```

Every input to this decision — `--target`, `platforms`, `--platform`, the
channel flags, and the project's `publish` declaration — is resolved before the
task runs and participates in its **cache key**. Changing the image platform
changes the key, so a five-platform cache entry can never be served to a
one-platform request.

## Usage

### Create platform archives

```bash
putnami package . --archives
```

Dry run (show what would be created):

```bash
putnami package . --archives --dry-run
```

### Docker image

```bash
# Assemble the image (no prior `putnami build` needed — package compiles its own binary)
putnami package . --docker
```

Override image tag:

```bash
putnami package . --docker --docker-tag v1.2.3
```

Multi-stage build from CI workspace image:

```bash
putnami package . --docker \
  --workspace-builder-image ghcr.io/myorg/workspace-builder:latest
```

To use a first-class image project as the base, set
`options.package.dockerBaseProject` to its project ID. Putnami adds that as a
graph edge, packages the base first, and composes directly from its typed local
OCI layout and candidate digest. No registry is needed between the two package
tasks. This local-base composition uses the native assembler; combining
`dockerBaseProject` with the legacy `--workspace-builder-image` path fails
closed because that builder cannot import the typed local layout without
silently discarding the requested base. See
[Image projects and project-derived bases](../../../tooling/cli/doc/05-configuration.md#image-projects-and-project-derived-bases).

### Go module

```bash
putnami package . --go
```

## Execution Flow

### archives channel

1. Stage common files: `putnami.extension.json`, `package.json`, `bin/`, `templates/`, `tools/`, `README.md`, `AI.md`, `LICENSE.md`, `config/`
   — each one only when the project has it, byte for byte, with no substitute for an absent file; `AI.md` is the only guidance file staged
2. Stage each `go/framework/{module}/AI.md` from the workspace as `framework-docs/{module}/AI.md`
3. Stamp version into `putnami.extension.json` (if present)
4. Cross-compile the Go binary for each platform of the declared release matrix (if a Go entrypoint is found), plus every executable declared in `options["@putnami/go"].executables`. Each declared executable is staged in `compiled/` beside the runtime and must be an executable regular file there, or packaging fails
5. Build the pinned development tools for each platform into `compiled/tools/` — only for the project that owns `tools/versions.json` (the Go extension itself). Each binary's embedded build information must report the pinned version or packaging fails
6. Create a `.tar.gz` archive per platform in `.putnami/out/{project}/package/archives/`.
   A windows-x64 archive is refused when its stage holds a symbolic link, or when the project builds no Go binary and ships no
   `<runtime.executable>.exe`: a Windows CLI creates no link from an archive and runs only the `.exe` runtime
7. Write `archives/channel.json` (the channel record, inside the directory this task owns) and `metadata.json` (the archive publication manifest: version, artifact name, channels packaged)

### template-archives channel

1. Stage `putnami.template.json`, `README.md`, `LICENSE.md` and every other top-level entry except dotfiles and `node_modules/`
2. Stamp version into `putnami.template.json`
3. Refuse a stage that holds a symbolic link, and name the entry: the CLI installs the same template archive on every OS, and a Windows CLI creates no link from an archive
4. Write one identical `.tar.gz` archive per platform in `.putnami/out/{project}/package/archives/`

### docker channel

1. Verify the binary exists (from this command's own `build-cross-compile` step)
2. Assemble the image **in-process** (no docker daemon, no Dockerfile) on a
   **digest-pinned** `gcr.io/distroless/static:nonroot` base, with the binary
   at `/app`, fixed timestamps, and explicit file modes
3. Write the image as a self-contained OCI layout under
   `.putnami/out/{project}/package/docker/oci`, tagged with a
   content-addressed immutable tag `c-<hash>`
4. Write `manifest.json` with the content hash, the **reproducible candidate
   digest** (known before any push), target platform, local layout path, and an
   advisory version. It contains no remote or publication claim.

Assembly is **deterministic**: identical inputs produce a byte-identical
image — and therefore an identical digest — on any machine, not just within a
docker layer-cache lineage. The base is pinned by digest because a moving tag
would make the digest depend on when the base was resolved; base CVE updates
arrive as explicit, reviewable pin bumps (or per-workspace via
`--docker-base-image`, which must also be digest-pinned).

The image is **content-pure**: it carries no git-derived bytes — no version
stamp, no `org.opencontainers.image.version`/`revision` labels. Identity is
assigned at publish time: `putnami publish --docker` resolves the registry
target from `registries.oci.publish` (with `--docker-registry` as an override
above it), obtains its target-bound publish credential, pushes the content once
per unique content by digest, and tags the version only — channel tags are
written by the release projection, not by the publisher. Only after reading the remote digest back does publish write
`publish/docker/published-image.json`; the package manifest stays byte-identical.
When the engine names a publication outbox in `PUTNAMI_PUBLICATION_OUTBOX` and
the target is `oci.putnami.dev`, publish packs instead: it copies the OCI
layout into the outbox and writes `outbox.json` with the repository, the
expected manifest digest and the version tag. It asks for no credential, sends
no registry request and writes neither `published-image.json` nor
`.gen/version.json`; the engine pushes the layout and writes
`published-image.json`. Any other registry ignores the outbox.
The binary itself reports its version via `version-var`
ldflags injection or `runtime/debug.ReadBuildInfo` (a `version-var` build
embeds the release id in the binary, so such projects trade digest travel for
self-reporting binaries).

An explicit `--docker-tag` is recorded as an extra alias; `--docker-load`
additionally loads the assembled image into the local docker daemon for
immediate `docker run`.

**Requirements:** none for assembly (network on first use to fetch the pinned
base, cached under `.putnami/cache/oci-base` afterwards). The `docker` CLI is
only needed for `--docker-load` and for the `--workspace-builder-image`
escape hatch, which still uses `docker build` (multi-stage builds cannot be
assembled in-process and do not get reproducible digests).

### go channel

A project with a custom OCI packager can name its producer in
`options["@putnami/go:publish"]`: set `docker-package-publisher` to the extension
name and `docker-package-step` to its exact `package` step (for example a CGO
packager's `cgo` step). The workspace probe carries that route into release-set
selection fingerprints. Go still owns the Docker upload, and Go-module members
keep their own package route. Omitted values retain `@putnami/go` / `docker`.

1. Copy Go module source to a staging directory
2. Strip all `replace` directives from `go.mod` (workspace-local paths are not valid for published modules)
3. Stamp the version into `go.mod` and any internal `putnami.extension.json` `run` entries
4. Write the prepared module to `.putnami/out/{project}/package/go/`

For a provider-backed `publish --impacted` or `publish --all` with a
`--channel`, the CLI supplies an immutable release-set plan to the selected
package tasks. The Go packager uses the exact candidate version for the
current module and rewrites each internal `go.putnami.dev/*` requirement to
the version recorded for that member in the full snapshot. This intentionally
permits mixed versions: an unchanged upstream keeps its inherited version and
is neither packaged nor uploaded, while a selected downstream is repackaged
against that exact inherited version. `--all` selects every member, so
nothing is inherited; either way the plan carries the resolved channel head
when one exists so the coordinator releases by compare-and-swap rather than
asserting an empty channel. Packaging fails if the release set and `go.mod`
do not describe the same closed internal dependency graph.

The publisher hashes the exact module zip bytes, uploads them, then verifies the
authenticated immutable zip and `go.mod`. It runs the final `go mod download`
smoke with an authenticated `GOPROXY=<origin>,off`, an empty `GONOPROXY`, and
`GOSUMDB=off`. The smoke environment removes inherited values before installing
those exact overrides, so neither duplicate-variable precedence nor a fallback
can contact `proxy.golang.org` or `sum.golang.org`. Registry URLs carrying
userinfo are rejected before diagnostics or dry-run output. HTTPS is mandatory
outside `localhost`, `.localhost`, and IP loopback addresses; download failures
redact the short-lived token embedded in the subprocess-only GOPROXY. Publisher
HTTP requests also disable ambient proxy routing and redirects, keeping the
target-bound bearer on the validated registry origin. The smoke subprocess
likewise removes ambient proxy variables, Putnami capability tokens,
Node/Bun/process preload hooks, and agent sockets before starting `go mod
download`.

Go publication is currently private under every release-set selection. A
plan coordinates immutable versions and digests; it is not
an authority for visibility. The publisher never asks for `promote`, never
calls the `/release` operation, never performs an anonymous read when it
publishes, and the closed
`gomod-write/v1` PUT has no repository-controlled visibility field. A future
public release requires a separate explicit server-owned attestation contract;
a command, channel, manifest or release-set plan cannot forge it. The command
coordinator releases the private channel only after every selected artifact
has verified successfully.

`putnami publish --go --dry-run` uploads nothing and asks the registry one
question: does it already serve this module at this version. The publisher
sends one GET of the proxy zip (`/<module>/@v/<version>.zip`) through the same
HTTP client as a real publication, and emits one `member-probe` event:

| State | When |
|-------|------|
| `absent` | The registry answers 404 or 410. |
| `identical` | Under a release-set plan, the served zip has the SHA-256 of the zip an earlier `package` staged for the planned module and version. The real publish reuses it. |
| `conflict` | Without a plan, the registry serves the version: a Go publish outside a release set fails when the registry has publicly released it, and a read cannot tell a released version from a private one. Under a plan, the served zip differs from the staged one, or no zip is staged for that version. |
| `unverified` | The staged zip cannot be read, or the registry cannot be reached, refuses the request, or answers outside the protocol. |

A dry-run `package` stages no zip. Under a plan the module and version come from
the plan, and the dry run compares with the zip a real `package` staged
earlier for that module and version. An `identical` verdict says so: its
reason reads "compared with the module zip the last `package` staged; re-run
`package` if the source changed since". With none, the conflict says so: run
`putnami package` for the project without `--dry-run`, then the dry run again.
Without a plan the module and version come from the staged module, or from the
`go` channel record when the dry-run package staged none.

A served zip with another digest is a `conflict` with or without a plan. The
registry answers a publish of a version it has publicly released with 409, and
a publish of a private version with 201 while it keeps the bytes it holds. The
real publish verifies the zip the registry serves after either answer, so it
fails on other bytes.

The request carries the credential the real publish asks for: an explicit token
first, then the host-only credential seam. The seam carries the registry host
and nothing else, so the cloud decides what the credential grants; the probe
sends reads only with it. A public module needs no credential: when none
resolves, the request is anonymous and the event says so. A private registry
that refuses an anonymous read fails the dry run as `unverified`. The task
succeeds whatever the answer; the CLI fails the run on `conflict` and
`unverified`
(see [Publish and `--dry-run`](../../../tooling/cli/doc/03-commands.md#registry-checks)).

On a linked developer checkout, the publisher obtains its write credential only
after reading the staged module coordinate. It asks `@putnami/cloud` for a
short-lived lease bound to the linked owner workspace and the module path's
canonical binding-relative suffix. Every Go publication requests only the
`publish` action, including provider-backed release-set coordination. The
complete module path remains unchanged in registry URLs, publication, and
evidence. This keeps
`putnami publish --all --go` usable from a
signed-in laptop without restoring the retired host-wide registry key. Explicit
`--go-registry-token` and cloudless registry credentials retain priority.

Under a native publication run on Putnami's CI runner there is no signed-in
session. The runner holds the publication capability itself and exports a
numeric-loopback broker as `PUTNAMI_REGISTRY_GOMOD_URL`
(`http://127.0.0.1:<port>/go`). The publisher then sends the blob upload, the
version PUT, and both immutable verification reads to that broker, and asks the
credential seam about the broker host, which the cloud answers with the run's
capability. The `go mod download` smoke is skipped on this route: the go
toolchain refuses to send credentials to an `http://` proxy, so the two
authenticated reads are the consumer proof there. The declared origin stays the
logical coordinate in every message and event. Without the variable the direct
path above is unchanged; a remote HTTPS value is ignored as Cloud compatibility
input; a loopback value that is malformed, or present without a release-set
plan, fails the job before any credential or network call.

When the engine names a publication outbox in `PUTNAMI_PUBLICATION_OUTBOX`,
a module publication without an explicit token packs instead of uploading. It
copies the packaged zip, `go.mod` and `.info` into the outbox and writes
`outbox.json`, which names the module path, the version, the owning project
and the digest and size of each file. The job asks for no credential, consults
no broker, sends no registry request, runs no `go mod download` smoke and
reports no published member: the engine uploads the bytes it verifies and
reports the member. A publication with `--go-registry-token`,
`PUTNAMI_REGISTRY_TOKEN` or `goRegistryToken` ignores the outbox and uploads as
above. A dry run packs nothing.

## Output Location

Every step of the `package` command shares this directory and owns an exact
subpath of it (see [Declared Outputs](build.md#declared-outputs)):

```
.putnami/out/{project}/package/
├── bin/                    # cross-compiled binaries — owned by build-cross-compile
├── archives/               # owned by package-archives
│   ├── {artifact}-linux-x64.tar.gz
│   ├── {artifact}-linux-arm64.tar.gz
│   ├── {artifact}-darwin-x64.tar.gz
│   └── {artifact}-darwin-arm64.tar.gz
│   └── channel.json
├── docker/                 # owned by package-docker
│   ├── manifest.json
│   └── channel.json
├── go/                     # owned by package-go
│   ├── (Go module source)
│   └── channel.json
└── metadata.json           # archive publication manifest — owned by package-archives
```

Each packager records the channel it produced INSIDE the directory it owns, so a
cache restore of that directory restores the record with the artifact it
describes. The channel index `publish` reads is derived over those records
(`pkgmeta.ReadChannelIndex`); it is not a file. `<channel>/channel.json` format:

```json
{
  "version": "1.2.3-20260902173000-abc1234",
  "artifact": "my-artifact",
  "channels": ["archives"]
}
```

`metadata.json` is the archive publication manifest, written by
`package-archives` alone and read by an archive uploader
(`@putnami/cloud publish-archives`):

```json
{
  "version": "1.2.3-20260902173000-abc1234",
  "artifact": "my-artifact",
  "stable": false,
  "channels": ["archives"]
}
```

## Versioning

The version is derived from git, for the version line the project belongs to.
There is no flag and no configuration field that sets it:

| Commit | Version output |
|------|---------------|
| untagged | `0.1.0-20260902173000-a1b2c3d` — the line's next version plus the ordered suffix |
| tagged with the line's tag | `0.1.0` — the tag's version, exactly |
| dirty tree | the untagged shape plus the diff hash; refused for a tagged publish |

`<next>` is the line's last tag advanced by the conventional commits that touch
the line. Release a line with `putnami version tag --scope <line>`; see
[Version Management](../../../tooling/cli/doc/14-version-management.md).

## Archive Contents

Each `.tar.gz` archive includes:

| Path | Description |
|------|-------------|
| `putnami.extension.json` | Manifest with stamped version (extensions only) |
| `package.json` | Package metadata |
| `bin/` | Shell entry-point scripts |
| `templates/` | Project scaffold templates |
| `config/` | Bundled configuration files (e.g. `.golangci.yml`) |
| `README.md` | Documentation |
| `AI.md` | Root agent reference used before extension-specific tools are available |
| `compiled/` | Cross-compiled Go binary for this platform, and each declared executable (see below) |
| `compiled/tools/` | Prebuilt pinned development tools for this platform (`golangci-lint`, `staticcheck`). Present only in the `@putnami/go` archive. `putnami install` copies them into the machine tool home instead of compiling them |

An extension that runs more than one program declares the extra ones in its
`putnami.json`. Each entry names the file under `compiled/` and the Go package
to build it from:

```json
{
  "options": {
    "@putnami/go": {
      "entrypoint": "./cmd/putnami-clientgen",
      "executables": [
        { "name": "putnami-client-generate-go", "package": "go.putnami.dev/api/cmd/clientgen" }
      ]
    }
  }
}
```

To obtain a tool binary for a platform other than the machine you are on — for
example when assembling a `linux/amd64` image from a Mac — materialize the
archive for that platform and read it out of `compiled/tools/`:

```bash
putnami extensions install @putnami/go --platform linux/amd64 --dest ./stage
ls ./stage/*/compiled/tools/        # golangci-lint  staticcheck
```

## Docker Image Details

The assembled image is equivalent to this recipe on a digest-pinned
`gcr.io/distroless/static:nonroot` base — but built in-process with fixed
timestamps and explicit modes, so the digest is reproducible:

```dockerfile
FROM gcr.io/distroless/static@sha256:<pinned>
ENV PORT=3000
COPY {binary} /app          # mode 0755, mtime epoch
ENTRYPOINT ["/app"]
# user nonroot inherited from the base; no labels, no version stamp
```

With `--workspace-builder-image` (multi-stage CI build), the legacy
`docker build` path runs instead. It cannot be combined with
`options.package.dockerBaseProject`:

```dockerfile
ARG WORKSPACE_BUILDER_IMAGE=...
FROM ${WORKSPACE_BUILDER_IMAGE} AS builder
FROM gcr.io/distroless/static:nonroot
ENV PORT=3000
COPY --from=builder /app/.putnami/.../bin/{binary} /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
```

## API Reference

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--archives` | `false` | Activate the archives channel (platform tarballs) |
| `--docker` | `false` | Activate the docker channel (local Docker image) |
| `--go` | `false` | Activate the go channel (Go module source) |
| `--dry-run` | `false` | Show what would be created without producing any artifacts |
| `--docker-registry <url>` | — | **Override** of the publish target; the declared one is `registries.oci.publish`. Ignored by local packaging either way |
| `--docker-tag <tag>` | — | Extra alias tag next to the content-addressed tag |
| `--docker-base-image <ref>` | pinned distroless | Override the digest-pinned base image (must include `@sha256:...`) |
| `--docker-base-project <id>` | — | Use a first-class image dependency's typed local OCI candidate as the base |
| `--docker-load` | `false` | Also load the assembled image into the local docker daemon |
| `--platform <os/arch>` | `linux/amd64` | Docker image target platform — and the only platform the docker channel compiles |
| `--platforms <list>` | 4 archive platforms | Release matrix for the archive channels (`all`, or `linux/amd64,darwin/arm64`) |
| `--workspace-builder-image <img>` | — | Multi-stage `docker build` from a CI workspace image (legacy path, non-reproducible digests) |
| `--port <n>` | `3000` | `PORT` env variable baked into the Docker image |

## Boundaries

- **Scope**: project-only — workspace-level runs are not supported
- **Docker**: assembles locally (in-process, daemon-free), does not push; `docker` CLI only needed for `--docker-load` or `--workspace-builder-image`
- **Archives**: produces one archive per platform of the declared release matrix — the 4 archive platforms (linux-x64, linux-arm64, darwin-x64, darwin-arm64) unless `platforms` narrows it
- **Go channel**: strips `replace` directives — published Go modules cannot reference workspace-local paths
- **Pre-requisite**: none — `package` schedules its own cross-compile step, so no prior `putnami build` is needed
- **Publish**: workload packaging creates local artifacts; first-class image projects publish content only when its full content key is missing, and never apply channel tags
