# Getting Started

`@putnami/go` provides Go build, test, lint, serve, and package jobs for Putnami workspaces, enabling Go services alongside TypeScript and Python projects.

## Prerequisites

- A Putnami workspace

Go itself is **not** required — the extension auto-manages the Go toolchain.

## Installation

`@putnami/go` is included by default in every Putnami workspace. No separate installation is needed.

To add it to an existing workspace that doesn't have it:

```bash
putnami deps add @putnami/go
```

The extension's own repository project explicitly activates its local
`/go/extension` provider. This self-hosts `putnami lint,test,build --projects
@putnami/go` through the same declared prepared runtime used by consumers,
instead of leaving the provider project without a validation plan.

## Project Structure

Go modules live alongside TypeScript or Python packages. Two common layouts are supported:

### Simple layout (root main.go)

```
my-go-service/
├── main.go             # Entry point
├── main_test.go        # Tests
├── go.mod
└── putnami.json
```

### Standard cmd/internal layout

```
my-go-service/
├── cmd/
│   ├── serve/          # Server binary (auto-detected by `putnami serve`)
│   │   └── main.go
│   └── migrate/        # Migration tool binary
│       └── main.go
├── internal/           # Private packages (not importable outside this module)
│   └── handler/
│       └── handler.go
├── go.mod
├── go.sum
└── putnami.json
```

Both layouts work automatically — the extension activates on any project containing `go.mod` or `*.go` files. The build job compiles all `cmd/*/main.go` targets, and the serve job auto-detects the right entrypoint.

### In a polyglot workspace

```
my-workspace/
├── packages/
│   ├── my-typescript-lib/
│   │   ├── src/
│   │   └── package.json
│   └── my-go-service/
│       ├── cmd/
│       │   └── serve/
│       │       └── main.go
│       ├── internal/
│       │   └── handler/
│       │       └── handler.go
│       ├── go.mod
│       ├── go.sum
│       └── putnami.json
└── go.work              # workspace-level Go module graph
```

## Quick Start (recommended: template)

### 1. Scaffold a server

```bash
putnami projects create api-gateway --template go-server
```

### 2. Build and run

```bash
putnami build api-gateway
putnami serve api-gateway
```

The `go-server` template includes a typed `go.putnami.dev/config` block for the server port and a `schema/` directory. `putnami build` runs the Go generate phase and writes the config schema manifest to `schema/config.json` by default, with `.gen/config-schema.json` used when schema commits are disabled.

## Libraries

Scaffold a library:

```bash
putnami projects create go-lib --template go-library
```

### Consuming a workspace library

Add the library to your consumer's `putnami.json` dependencies:

```json
{
  "dependencies": [
    "go-lib"
  ]
}
```

The build job automatically syncs workspace dependencies into `go.mod` as `replace` directives — no manual `go.mod` editing required.

## Manual setup (no template)

### 1. Create the project directory and register it

Create `packages/api-gateway/putnami.json`:

```json
{
  "name": "@myworkspace/api-gateway"
}
```

Add the project to `putnami.workspace.json`:

```json
{
  "includes": [
    "packages/api-gateway"
  ]
}
```

### 2. Initialize the Go module

```bash
cd packages/api-gateway
go mod init github.com/myorg/api-gateway
```

### 3. Add an entry point

Create `packages/api-gateway/cmd/serve/main.go` (or just `packages/api-gateway/main.go` for simple projects):

```go
package main

import (
    "fmt"
    "log"
    "net/http"
    "os"
)

func main() {
    port := os.Getenv("PORT")
    if port == "" {
        port = "8080"
    }

    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintf(w, "Hello from Go!")
    })

    log.Printf("Starting server on :%s", port)
    log.Fatal(http.ListenAndServe(":"+port, nil))
}
```

### 4. Build and run

```bash
putnami build api-gateway
putnami serve api-gateway
```

## Cross-Compilation

Cross-compile using `--target os/arch`:

```bash
# Linux x86-64
putnami build api-gateway --target linux/amd64

# macOS Apple Silicon
putnami build api-gateway --target darwin/arm64

# Windows x86-64
putnami build api-gateway --target windows/amd64
```

See [build.md](./build.md) for all build options.

## Toolchain Management

`putnami install` installs the Go release the workspace pins when the machine
does not already provide it. The install lives in the Putnami home, once for
the machine, and every workspace that pins the release runs it.

Before a task starts, the CLI selects the Go the task runs with. It takes the
first of these that reports the pinned release:

1. `GOROOT/bin/go`, when the caller sets `GOROOT`
2. `go` on `PATH`
3. The install in the Putnami home,
   `~/.putnami/toolchains/go/go-{version}/go/bin/go`
4. A copy inside the workspace,
   `.putnami/extensions/@putnami-go/libs/go-{version}/go/bin/go`

The CLI then sets `GOROOT` to the selected release and puts its `bin`
directory first on the task's `PATH`.

`putnami install` writes no copy at the location of item 4. A workspace that
holds one from an earlier Putnami release keeps running it, without a download,
while the Putnami home holds no install of that release. This candidate is
deprecated and stays for one release.

Native extension jobs then consider compiler candidates in this order:

1. An executable `GOROOT/bin/go`, which the CLI sets as described above
2. An executable `go` on `PATH`
3. A Go release inside the workspace, through the link
   `.putnami/extensions/@putnami-go/bin/go`; on Windows, which has no such
   link, each release under `.putnami/extensions/@putnami-go/libs/`, newest
   first
4. `bin/go` under `PUTNAMI_EXTENSION_ROOT`
5. The stable extension path `.putnami/bin/extensions/putnami-go/bin/go`

The runtime selects the first candidate compatible with the governing
`go.work`, falling back to the current project's `go.mod` when no workspace
requirement exists. The effective minimum is the newer of the file's `go`
directive and a non-`default` `toolchain` directive; `toolchain default` does
not raise the `go` requirement. Each candidate reports its own installed
version under `GOTOOLCHAIN=local`, so an older `GOROOT` or `PATH` compiler is
skipped instead of masking the mismatch through an implicit toolchain download.

This selection happens inside the prepared extension runtime; build, test,
lint, serve, run, code generation, and publish smoke-test subprocesses all use
the same selected compiler. When the compiler is a release inside the
workspace, its real `GOROOT/bin` is also placed first on the child `PATH`.

Jobs do not download a missing compiler on demand. If none of the locations
above contains a compatible executable Go binary, the error lists every
incompatible candidate and its reported local version. Run `putnami install`
to install the pinned release (or install a sufficiently new Go system-wide)
and retry.

**Install-time version resolution:** the workspace asks for a minimum Go
version, read from the `go.work` directive at the workspace root, else from the
`go.mod` in the project directory. When `putnami.lock.json` pins a release under
`toolchains.go` that satisfies that minimum, the pin is exact: the tasks the CLI
runs accept only that release, so `putnami install` does too. A `go` on `PATH`
that reports another release, newer or older, is passed over for the managed
install of the pinned one. Without a pin, or with a pin older than the minimum,
any Go that satisfies the minimum qualifies. When no qualifying Go is found,
`putnami install` installs the pinned release. It downloads the archive for the host platform
(a `.zip` on Windows, a `.tar.gz` elsewhere) and refuses it unless its SHA-256
equals the digest the lock records for that platform. A lock with no Go pin, or
with no digest for the platform, installs nothing. A source URL that carries
credentials, such as `https://user:token@mirror.example/go.zip`, appears in the
job output without them, and the credentials go only to that URL's scheme and
host, after a redirect too.

Go is installed to `~/.putnami/toolchains/go/go-{version}/`, where the Go
distribution is the `go` directory. `PUTNAMI_HOME` relocates `~/.putnami`; when
neither `PUTNAMI_HOME` nor a home directory is set, the Putnami home is
`.putnami` under the workspace root. The layout under the Putnami home is
`toolchains/<name>/<name>-<version>/`.

The install is shared by every workspace of the machine:

- A second workspace that pins the same release downloads nothing and adds no
  copy.
- Installs that start at the same time, from one workspace or from several,
  leave one complete install. They exclude each other on the lock file
  `toolchains/go/go-{version}.lock`, beside the install directory: one
  downloads, the others wait and use its install.
- `putnami install` writes no Go release and no `bin/go` link into the
  workspace.

If Go is already in PATH at the pinned release, the system installation is
used and nothing is downloaded.

On a hosted run, `workspace-fetch` runs no program inside the workspace. When
the Putnami home is `.putnami` under the workspace root, it does not run the Go
installed there: the runner provides the pinned Go on `PATH`, in `GOROOT`, or
in a Putnami home outside the workspace.

A workspace that declares no Go yet (no Go member, no `go.work`, no root
`go.mod` and no Go pin) on a host with no `go` skips the Go phase of
`workspace-install`: its first Go project installs Go. A workspace that
declares Go and cannot install a verified release still fails.

`workspace-install` also adds to an existing `go.work`, with `go work use`, each
Go member of the workspace membership the CLI resolves that `go.work` does not
use yet, including members that `includes` reaches. It does not create a
`go.work`.

The Go projects that `workspace-install` and `deps-upgrade` act on are the ones
the legacy `projects` member of the workspace configuration names, when it names
a Go project, and otherwise the Go members of that membership. A standalone
member (one that publishes Go or sets `options["@putnami/go"].standalone`) that
only `includes` reaches therefore has its `GOWORK=off` build list warmed, and
its `go.putnami.dev` requirements upgraded, like a member `projects` names.

This extension resolves the shared Go cache root itself — `PUTNAMI_GO_CACHE_DIR`,
then `$PUTNAMI_HOME/cache/go`, then `~/.putnami/cache/go`, then the generic
per-extension machine root the CLI provides as `PUTNAMI_EXTENSION_CACHE_ROOT` —
and maps it to `GOCACHE=<root>/build` and `GOMODCACHE=<root>/mod` for every
native Go child. A run served by a cache provider adds a third directory,
`<root>/prog`, where the `GOCACHEPROG` helper keeps its lookup records; the
compiled objects it shares through the remote cache stay in `<root>/build` — see
[remote build cache](./build.md#remote-build-cache). It also owns collecting the
whole root, through the hidden `cache-clean` and `cache-gc` commands
`putnami cache clean` / `cache gc` fan out to: both build directories are
collected, the module cache is accounted but never evicted. The root lives
outside the CLI's build store (`~/.putnami/store`), so the store's byte budget
never counts or evicts it. The budget that bounds it is this extension's own:
`PUTNAMI_GO_CACHE_MAX_BYTES`, 10 GiB by default across `build`, `prog` and
`mod`. `cache-gc` enforces it on demand or after a run, at most once an hour,
and never during a run. The runtime also
disables
implicit toolchain downloads with `GOTOOLCHAIN=local`, leaves `NETRC` alone so
`go` reads the standard `~/.netrc`, preserves third-party `GOPROXY` settings, and
applies the module-origin proxy/checksum defaults when callers do not provide
them.

The module origin is the vanity module server the workspace **declares**, not a
host built into the tooling: `registries.go.origin` in
`putnami.workspace.json`, overridable per project, with `GO_REGISTRY_URL` as a
one-off override. `registries.go.proxy` is the ordered `GOPROXY` list, which
defaults to `https://proxy.golang.org,direct` and never leads with the origin.
An inherited `GOPROXY` led by that origin is normalized, and the
workspace's own modules resolve through the origin's vanity import and `NETRC`,
never through a public proxy fallback. Run your own vanity server and you get the
same protection by declaring its URL. Declare nothing and you get stock Go
behavior: no `GONOPROXY` or `GONOSUMDB` entry is injected.

### Private registries: credentials at install time

The workspace's own modules are fetched from the declared origin, which
authenticates them. Before any job that runs a `go` command able to reach that
origin — `build`, `test`, `lint`, `run`, `serve`, `package`, `publish`,
`workspace-sync`, and the generators — the extension asks `@putnami/cloud` to
write your credential for it:

```
putnami cloud registry-token --host <declared origin host> --materialize
```

The cloud writes the `machine <host>` entry in the file `go` reads: the one
`NETRC` names, or your own `~/.netrc` when `NETRC` is unset. The extension never
sees the token, and the host is the whole request. The extension never sets
`NETRC`, and it refreshes the entry before each job that fetches modules, so an
expired credential is replaced before `go` sends it. `putnami projects create`
refreshes it the same way before it resolves the framework version.

`putnami upgrade` asks the origin for each module's channel itself
(`@v/<channel>.info`, or `@latest`), and sends the credential `go` would send:
the `machine` entry of `NETRC`, or of `~/.netrc` when `NETRC` is unset, over
https only, unless `GOAUTH` leaves out `netrc`. It never follows a redirect off
https. An anonymous 401, 403 or 404 names the missing credential and the file
it looked in.

The refresh never blocks a build. A workspace that declares no origin, a machine
with no `@putnami/cloud`, and a user who is not signed in (`run putnami cloud
login`) each log one line and let the job run.

### Hosted runs: only workspace-fetch sees the credential

On a hosted run, no process the repository controls may read a registry
credential. That includes `./putnamiw`, a test, a hook, and a tool the workspace
pins. The Go extension splits the install into two jobs for this:

1. `workspace-fetch` runs first, before any process the repository controls.
   It downloads the build list of every Go project and builds the pinned dev
   tools. It is the only job that gets the read credential. It installs no Go:
   the runner provides the pinned Go on its `PATH`, in `GOROOT`, or in the
   Putnami home. It keeps `go.work` and every `go.mod` as committed.
2. `workspace-install` and every later job run with module downloads off, and
   use what `workspace-fetch` downloaded. `workspace-install` also keeps
   `go.work` as committed.

`workspace-fetch` reads the credential from the descriptor that
`PUTNAMI_JOB_CREDENTIAL_FD` names, and closes the descriptor before it starts
any process. For each `go` command that downloads (`go mod download`,
`go list -m`, `go install`), it writes the credential to a temporary `NETRC`
file and points only that command at it. The file has one entry for each host
the credential serves, and a host on port 443 also gets an entry without the
port. Only you can read the file (mode 0600, in a 0700 directory). The job
removes the file when the command ends, whether the command succeeds or fails,
and when a signal stops the job. The token is never in an environment variable.
A tool the job built runs its version check after the file is gone. Without a
credential, `workspace-fetch` downloads with your own `~/.netrc`, as
`workspace-install` does. A descriptor the job cannot read fails the job before
it runs anything.

The engine sets `PUTNAMI_OFFLINE_DEPENDENCIES=1` in every job of a hosted run.
With it:

- Every `go` command runs with `GOPROXY=off`, `GONOPROXY=none`, and
  `-mod=readonly` added to `GOFLAGS`. `GONOPROXY=none` closes the direct route
  to a module's origin that `GONOPROXY` or `GOPRIVATE` would otherwise open. A
  `-mod` flag you already set in `GOFLAGS` is kept.
- `workspace-install` downloads nothing. It uses an installed Go instead of
  installing one, keeps `go.work` as committed, skips the module warm-up, uses the dev tools
  already on the machine, and ignores `--force`. A missing Go fails the job; a
  missing tool is a warning. Both messages name `workspace-fetch`.
- No job asks `@putnami/cloud` to write a credential into `~/.netrc`.
- `build-tidy` runs no `go mod tidy`, as with `GOPROXY=off`. The task keys its
  cache on `PUTNAMI_OFFLINE_DEPENDENCIES`, so this no-op never answers a run that
  can download modules.
- The `go test` process and the tests it runs keep those `go` settings, and see
  neither `PUTNAMI_OFFLINE_DEPENDENCIES` nor `PUTNAMI_JOB_CREDENTIAL_FD`. Your
  tests are not jobs of the run.

Without `PUTNAMI_OFFLINE_DEPENDENCIES`, every `go` command runs with the same
environment and arguments as before.

### Lifecycle jobs: differences from the former scripts

`workspace-install` and `deps-upgrade` run as `putnami-go` subcommands. Their
messages, events, path spellings, and exit codes match the Bash scripts they
replace, except for these deliberate differences:

- Each event carries the runtime event protocol version the CLI advertises in
  `PUTNAMI_RUNTIME_EVENTS`, or v1 when nothing is advertised. The scripts always
  wrote v2.
- A diagnostic without a line number omits `line`. The scripts wrote
  `"line":0`.
- Event text writes `<`, `>`, and `&` as `\u003c`, `\u003e`, and `\u0026`. Both
  spellings decode to the same string.
- The job reads the reserved `--json` and `--output=<mode>` flags itself
  instead of passing them on.
- Lists sort by byte order, not by the locale.
- The job asks the module origin each question once per run, so it sends
  fewer requests.
- The lookup of `go` on `PATH` skips a relative entry, as `os/exec` refuses to
  run a program found through the current directory.
- On Windows, only an `.exe` found on `PATH` runs: a `.bat` or `.cmd` file would
  start `cmd.exe`, and the lifecycle jobs start no shell.
- An unreadable or malformed context file ends the job with exit code 2 and a
  message on stderr, before any event. `deps-upgrade` wrote its meta event
  first and stopped at its first read of the file; `workspace-install` ignored
  the file.
- `deps-upgrade` refuses a `registries.go.proxy` that is neither an array nor
  an object, with an error diagnostic on the context file and exit code 1. The
  script stopped with the exit code 5 of `jq` and no diagnostic.
- When `go env GOMODCACHE` fails, `deps-upgrade` emits an error diagnostic with
  the `go` command's message, rolls back, and exits 1. The script stopped with
  the `go` command's exit code and no result event.
- When `deps-upgrade` cannot write a missing `go.work`, it logs an error and
  exits 1. The script logged `Created go.work` and went on without one.
- A rollback or checksum restore that cannot put a file back emits an error
  diagnostic naming the file, and the job fails. `deps-upgrade` ignored those
  errors, and `workspace-install` stopped without a result event.

## Pinned Lint Tools

`golangci-lint` and `staticcheck` are pinned in
[`../tools/versions.json`](../tools/versions.json). That file is the
machine-readable contract for anyone baking a CI image. `schemaVersion` and
`tools` are read by the extension itself; `goVersion` is informational — it
records the Go minor the release built the tools with, and nothing resolves
against it, because the binary that matters is the one the LOCAL toolchain can
run (see the tool home key below).

`putnami install` warms those tools, and `putnami lint` reads exactly what it
warmed. One copy per machine lives at:

```
$PUTNAMI_HOME/tools/go/<tool>/<version>/go<major.minor>/<goos>-<goarch>/<tool>
```

`PUTNAMI_HOME` defaults to `~/.putnami`. The key is the whole identity of the
binary: the tool, its pinned version, the local Go major.minor it serves, and
the platform. A tool built with an older Go minor than your workspace uses
panics while type-checking newer sources, so two workspaces on two Go minors
keep two copies instead of evicting each other. A tool built with the same Go
minor or a newer one serves the workspace: it checks your code against your
local standard library and reports what a tool built with your Go minor
reports. `putnami install --force` deletes the copy for the current key only.

`PUTNAMI_GO_CACHE_DIR` does NOT move the tool home. It relocates the Go build
and module caches, which `putnami cache clean` empties on purpose, and a pinned
tool binary is managed state rather than a cache entry.

Resolution order, identical in `putnami install` and in every other job of the
extension:

1. a PATH binary reporting the pinned version;
2. the machine tool home (then the workspace-local directories earlier releases
   used, so an already-warm checkout is not forced to recompile);
3. the prebuilt binary shipped in the extension archive at
   `compiled/tools/<tool>`, copied into the tool home after its embedded build
   information is checked against the pin and its Go minor is your local one
   or a newer one;
4. `go install` of the pinned coordinate, as a last resort.

Step 3 is why a fresh CI machine does not spend 40 s of wall time and 113 s of
CPU compiling linters on every run, and a 4-vCPU Windows host about 4.8
minutes. Your local Go is the one the workspace pins, often an older minor
than the release built the tools with; only a local Go newer than that build
compiles the tools.

In the workspace that SHIPS the pins — the one whose project carries
`tools/versions.json` — `putnami install` also fetches the tool SOURCES into
the module cache. `package~archives` cross-compiles the tools for every release
platform inside a task graph that runs with `GOPROXY=off`, and step 3 hands it
binaries, not sources. The warm-up runs `go install -n <pin>`: the same module
query and the same `<module>@latest` deprecation query the real command makes,
without the compile. The packaging build then reads the module cache as a
`file://` proxy and needs no network. Every other workspace fetches nothing.

### Getting a tool binary for another platform

An image producer that needs the pinned linter for, say, `linux/amd64` should
materialize the extension archive for that platform instead of compiling:

```bash
putnami extensions install @putnami/go --platform linux/amd64 --dest ./stage
```

The archive expands with the tool binaries at
`compiled/tools/golangci-lint` and `compiled/tools/staticcheck`, built for the
requested platform. Copy them into the image; nothing else is required.

## Zero-npm Workflow

The extension is fully self-contained — no npm, Node.js, or Bun required at runtime. Combined with the compiled Putnami CLI, the only things you need are:

```bash
# Every team member needs only the putnami binary
putnami build --impacted   # TS team builds TS; Go team auto-installs Go and builds
putnami test --impacted
putnami lint --impacted
```

Team members who never touch Go pay zero cost — the extension only activates on projects with `go.mod` or `*.go` files.

## Next Steps

- [Build](./build.md) — compile Go binaries with cross-compilation
- [Test](./test.md) — run tests with coverage
- [Lint](./lint.md) — lint with golangci-lint and staticcheck
- [Serve](./serve.md) — run with hot-reload in development
- [Package](./package.md) — create release archives, Docker images, and Go module distributions
