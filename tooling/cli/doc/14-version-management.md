# Version Management

Putnami derives release versions from git and separates them from publishing. A version is never declared in a configuration file: `version get` computes it, `version tag` records it, and `publish` stamps every member with the version of the line its project belongs to.

## Version lines

A **line** is a set of projects versioned and tagged together. A scope declares one with a `line` block in its `putnami.json`:

```jsonc
// typescript/putnami.json
{ "line": { "tag": "ts/v{version}" } }
```

The tag pattern carries exactly one `{version}` placeholder; `"line": {}` takes the default `<scope path>/v{version}`, and a workspace where no scope declares a line is one line whose pattern is `v{version}`. A project belongs to its **nearest ancestor** line, exactly one; a line block inside another line is refused by the loader. `putnami scopes list` prints a `LINE` column with each scope's tag pattern, or `-`.

## How a version is computed

For each line, in order:

| Situation | Version |
|---|---|
| HEAD carries the line's tag | the tag's version, exactly |
| HEAD carries the tags of several lines | `--scope <line>` names the one being released; without it a publish is refused |
| any other commit | `<next>-<yyyymmddHHMMSS>-<sha>`, an ordered pre-release |
| a dirty tree | the same, with the diff hash appended |
| a shallow clone, or a checkout with no tags | refused for `version` and `publish`: `git fetch --unshallow --tags` |
| a full clone with no tag on the line | `0.0.0`, with no bump |

`<next>` starts from the line's last reachable tag and advances by the **conventional commits** that touch a project of the line, attributed by the files each commit changed:

| Commit | Before 1.0 | From 1.0 |
|---|---|---|
| `!` or a `BREAKING CHANGE:` footer | minor | major |
| `feat` | minor | minor |
| `fix`, `perf` | patch | patch |
| anything else | no advance | no advance |

A pre-release is always at least a patch above the last tag, so a docs-only commit still produces a new, ordered version. Versions of one line are totally ordered by their timestamp segment, which is what lets a registry answer "the newest build of this channel" without any Putnami-specific metadata.

```bash
putnami version get                   # one line per version line: "<line> <version>"
```

## Releasing a line: `version tag`

```bash
putnami version tag --scope typescript --dry-run   # print the proposed tag and changelog
putnami version tag --scope typescript --yes       # release commit, then the annotated tag
putnami version tag --scope typescript --yes --push
```

`version tag` refuses a dirty tree and a HEAD that is already tagged for that line. It computes the next version from the same commits, renders the line's changelog into `<line scope dir>/CHANGELOG.md` (the root line writes `CHANGELOG.md`), commits that as `chore(release): <line> <version>`, then puts the annotated tag **on that commit** — so the changelog is inside the release, not one commit behind it. The rendered notes become the tag message. An explicit tag name overrides the computed one; `--scope` is required when the workspace declares more than one line.

Pushing the tag is what triggers the cohort publication: a publish on a tagged commit republishes every member of that line at the tag's version and creates an immutable channel named after the tag (see [Commands](03-commands.md#publish-to-a-release-set-channel)).

### Seeding a line

Until a line has a reachable tag, every commit on it computes `0.0.0-<suffix>`, and `version tag` proposes `0.0.1`. To start a line at another version, tag the commit by hand once, then push that tag by name. `git push --tags` would also push every other tag of the local clone.

```bash
git tag -a ts/v0.2.0 -m 'ts 0.2.0' <commit>
git push origin ts/v0.2.0
```

Repository-wide candidate platforms, pre-1.0 compatibility, and license
transition intent live in [`RELEASE.md`](../../../RELEASE.md). They are release
planning, not part of the SDD protocols or the current support catalog. What the
release promises a user is the
[First Public-Release Contract](../../../README.md#first-public-release-contract):
pre-1.0 compatibility is migration-based rather than a promise that minor
releases are backward compatible.
[Compatibility and Migration](21-compatibility-and-migration.md) states what
that means per artifact format — including the lock file's supported version
window, what `putnami migrate vnext --apply` converts, and what to do when a
lock, an extension, or a machine consumer is outside the window.

## CLI Binary Versions

The CLI also supports multiple binary versions installed side by side. This lets developers switch between local development builds and installed releases.

## Layout

All binaries live in a flat `.putnami/bin/` directory with versioned names:

```
.putnami/bin/
├── putnami              → putnami-go-dev   (symlink to active)
├── putnami-go-dev       # Go CLI (local build)
└── putnami-go-1.2.0     # Go CLI (installed release)
```

The `putnami` symlink points to whichever variant is active.

On Windows the files carry `.exe`, and `putnami.exe` is a copy of the active
binary rather than a link, because a symbolic link needs Developer Mode there.
Windows refuses to delete or overwrite a running executable but allows a
rename, so `putnami upgrade` and `putnami version use` switch binaries under a
lock on `.putnami-switch.lock`: the current `putnami.exe` is renamed aside
to a `.old-` name that starts with a dot, the new copy moves in, and a later
switch deletes the file that was moved aside once it no longer runs. When the
new copy cannot move in, the old one moves back; when that fails too, the old
one stays at its `.old-` path and the error names that path. The PowerShell
installer follows the same lock and names.

## Binary Upgrading

Use `putnami upgrade` to update the CLI, extensions, and dependencies in one command:

```bash
putnami upgrade                     # Upgrade everything from the stable channel
putnami upgrade --cli               # Only upgrade the CLI binary
putnami upgrade --extensions        # Only upgrade extensions and templates
putnami upgrade --deps              # Only upgrade framework dependencies
putnami upgrade --channel canary    # Follow the canary channel on every registry
putnami upgrade --release rs_<64-hex> --namespace putnami
                                    # Use one exact immutable release set
putnami upgrade --version 1.2.3     # Use an exact release version
putnami upgrade --from-source       # Build the CLI from this workspace's source
putnami upgrade --dry-run           # Resolve and print without writing files
```

`--channel` resolves on each ecosystem's own registry — an npm dist-tag, a Go
`@v/<channel>.info` version query resolved **per module**, and the put
registry's channel projection — so it needs no release set, no namespace, and
no Cloud provider. Every endpoint comes from the workspace `registries` entry
for that ecosystem. `--release` is the one selector that still uses a release
set, and it takes the publishing `--namespace` explicitly. There is no
`--branch`: a branch is not a distribution channel. See
[How a channel resolves](03-commands.md#how-a-channel-resolves) and
[ADR 0020](adr/0020-upgrade-resolves-channels-natively.md).

The `--cli` flag checks the put registry (`registries.put.registry`, else `PUTNAMI_REGISTRY_URL`, else `https://put.putnami.dev`) for the selected release, sends the user's credential for that host when one exists, downloads the platform-specific binary, verifies SHA-256 integrity, and installs it as `putnami-go-<version>`. A `401` or `403` names `putnami cloud login` instead of failing as a missing release; the next paragraphs describe a request that went out without a credential.

`pin` and a cold workspace-pin launch use that same native registry and host
credential. Before the pinned binary is available, the selected CLI obtains
the credential through its own absolute executable; only that credential child
bypasses workspace relaunch. The parent still verifies the committed binary
digest and refuses to run a different CLI. No credential is recorded in the
lock or forwarded to another origin after a download redirect.

When the credential command provides no credential, the download goes out
anonymous. If the registry then answers `401`, `403` or `404`, the error names
the registry, the command that provided no credential
(`putnami cloud registry-token --host <host>`) and that command's own reason.
A `401` or `403` also suggests `putnami cloud login`. The registry answers
`404` to an anonymous reader of a private archive, the same answer it gives
for a missing version, so a `404` names both causes and suggests nothing.
`upgrade --cli` reports the same way. An extension archive download adds that
reason to its own error.

With `--providers install`, `upgrade --cli` and the extension archive downloads
first ask the workspace's credential provider for its `read` credential, and
send it only to a host that credential names. Another host, or a provider that
holds no credential, keeps the host-keyed credential described above; a refusal
fails every download that needs the credential, whatever its host, with its
code. When `upgrade` restarts into the CLI it just installed, the remaining
phases keep the same providers: the restart sets `PUTNAMI_PROVIDERS`, which an
older target CLI ignores where it would refuse `--providers`. A cold
workspace-pin launch runs before the flags are parsed and keeps the host-keyed
credential. See
[`--providers`](03-commands.md#credential-provider---providers). A hosted run
([`--credential-fd`](03-commands.md#run-credential---credential-fd)) asks no
host-keyed credential for any download.

Existing `PUTNAMI_REGISTRY_URL` values ending in `/dl` keep the `putnamiw`
mirror contract (`/dl/putnami?version=&target=&platform=`) for pins and cold
launches. Declare `registries.put.registry` to use the native Put endpoint
(`/putnami/cli/download?channel=&os=&arch=`); the authored workspace entry
takes precedence over that legacy mirror fallback.

After downloading, `upgrade` runs the binary's `--version` and refuses to install it if it does not match the channel-resolved version — surfacing a channel that is serving a stale (mis-stamped) build rather than silently installing it.

When a full upgrade installs a new CLI, it replaces itself with that verified
binary before it upgrades extensions or framework dependencies. This prevents
the CLI that started the command from loading extensions which require a newer
task-runtime contract. A workspace CLI pin is not changed by this handoff: a
pin is an explicit, committed trust decision. To use the upgraded CLI for
ordinary commands in a pinned workspace, update the pin deliberately with
`putnami pin <version>` (and add the digest from each platform your team uses).
When the installed CLI cannot be made active, the upgrade stops before it
changes extensions or dependencies, so it cannot leave a newer extension set
behind an older active engine.

## How the CLI binary reaches the download channel

`upgrade`, `pin`, and the install script all fetch from one endpoint:

```
GET {registry}/putnami/cli/download?channel=<channel>&os=<os>&arch=<arch>
```

It streams the platform binary and advertises `X-Resolved-Version` plus a SHA-256 integrity header. The binary is put there by `putnami publish`:

1. `package` cross-compiles the CLI and builds platform archives under `.putnami/out/<project>/package/archives/` (the CLI's version string is stamped into the binary at build time via the `@putnami/go.version-var` ldflag).
2. `@putnami/cloud`'s `cloud-publish-archives` job uploads those archives to the registry. The CLI project opts in by declaring the `archives` publish channel in `tooling/cli/putnami.json` (`"publish": ["archives"]`); `putnami publish` selects and uploads it with no extra flag.

The CLI and the extensions are ordinary members of the release set, in the `archive` and `put` ecosystems: `publish --channel <c>` carries them into the snapshot with every other member, and the release writes the put registry's channel projection in the same transaction that advances the channel. There is no separate recipe to run afterwards.

If a project builds release archives but **no** publish step uploads them (e.g. `@putnami/cloud` is not installed or failed to load), `publish` fails instead of silently shipping nothing — the gap that once stranded merged CLI fixes off the `publish → upgrade` path.

### Pre-release version shape

A pre-release is `<base>-<commitTime>-<sha>`, for example
`0.1.0-20260902173000-8e6fb533`: the UTC committer time of `HEAD` in the
fixed-width `yyyymmddHHMMSS` layout, then the short commit SHA. A dirty tree
appends a hash of the uncommitted diff (`…-8e6fb533-a1b2c3d`). The suffix is
one semver identifier, so npm and Go compare it as a string and a newer commit
always sorts after an older one: a native `@latest` follows the newest
publication. Versions published before this shape (`<base>-<sha>`) stay valid
and readable everywhere; they only lack that ordering.

A runner that builds a synthetic merge or squash commit of the commit it was
asked to check publishes under that commit, not under `HEAD`: it sets
`PUTNAMI_SOURCE_REVISION` to the full lowercase hex commit id (40 or 64
characters). The suffix's `<sha>` and the release set's `sourceRevision` then
name that commit; the tree that is built, hashed and cached is still the
checkout's, so the branch and the dirty hash stay `HEAD`'s. The suffix's
`<sha>` is always the first 12 characters of `PUTNAMI_SOURCE_REVISION`, so one
commit has one version on every runner, whether or not its repository has the
commit. When the repository has the commit, `<commitTime>` is its committer
time. When it does not, `PUTNAMI_SOURCE_COMMIT_TIME` (unix seconds) is
required; the run fails without it rather than stamping `HEAD`'s time. Setting
`PUTNAMI_SOURCE_REVISION` to `HEAD`'s own id keeps `HEAD`'s time but still
carries the 12-character `<sha>`, where an unset variable keeps git's
abbreviation. A malformed value of either variable fails the run, and so does
a workspace git cannot read; an empty `PUTNAMI_SOURCE_REVISION` is the same as
an unset one.

The CLI reads both variables once and hands every job the resulting version
through its job context. Job processes do not inherit them, so a test suite or
a nested `putnami` run that a job starts describes its own checkout.
`putnami version tag` refuses to run while `PUTNAMI_SOURCE_REVISION` names a
commit other than `HEAD`: the tag it creates names the checked-out commit.

### Channel semantics

| Channel | Resolves to |
|---|---|
| `latest` | The set a user promoted with `putnami channel set latest --from <channel or rs_id>`; the repository declares it `protected`, so no publish advances it |
| `stable` | Alias for the latest stable release (`upgrade`'s default) |
| `canary` | The newest publication of the `canary` channel |
| `<tag>` | The immutable channel a tagged publish created, in the portable encoding (`ts/v0.3.0` becomes `ts-v0.3.0`); it never moves again |
| `<version>` | An exact release, e.g. `0.1.0-20260902173000-8e6fb533` |

After a candidate is published, any configured external release execution plane
runs `tooling/cli/scripts/smoke-check-release.sh <channel>` on the four supported
macOS/Linux × amd64/arm64 targets, and its PowerShell port
`smoke-check-release.ps1` on a `windows/amd64` host, before promotion: five
candidate jobs. It then runs `latest` nightly through the default public
installer URL on the four macOS/Linux targets; the Windows host exists only for
a run, so `latest` has no nightly Windows job. The repository intentionally does not
duplicate that post-publication matrix in GitHub Actions; the release owner runs
the same five candidate jobs manually if no plane is configured or it is
unavailable, plus the same four-target `latest` smoke nightly until automation
is configured or restored.

The smoke runs the exact `curl` install, TypeScript `init`, and `serve` commands
against isolated home/config/cache/store/workspace state with credentials
cleared. It verifies the selected executable and stamp, generated workspace,
locks, extension and MCP registration, typed readiness, a real successful HTTP
response, and graceful shutdown. Any failing leg prints bounded diagnostics;
an automated run sets `SMOKE_DIAGNOSTICS_DIR` to retain the artifact bundle.
The legs themselves are covered by `tooling/cli/internal/installscript`, which
points both served URLs at local test servers — see
[Installing the CLI](22-installing-the-cli.md#the-release-smoke).

## Self-hosting a local CLI build

When the download channel is unavailable, or a merged fix has not been published yet, build the CLI from the current workspace and adopt it as the active binary — no manual `go build` + symlink swap required:

```bash
putnami upgrade --from-source            # build from source, install into .putnami/bin
putnami upgrade --from-source --global   # ...and replace the global ~/.putnami/bin CLI
```

This builds `tooling/cli/cmd/putnami`, stamps a `0.0.0-source-<sha>` version (so `--version` plainly shows a local build, with `-dirty` when the tree has uncommitted changes), installs it as `putnami-go-source-<sha>` through a new inode + atomic symlink swap (never rewriting the running binary), and points `putnami` at it. It requires the framework workspace (the CLI source) and is mutually exclusive with `--channel`/`--version`/`--extensions`/`--deps`.

Because these are recovery paths, they are **exempt from the workspace pin**: `upgrade --from-source`, `version list`, and `version use` run as the binary you invoked rather than relaunching into the pinned engine — so they still work when the pinned binary is broken or predates the flag. (`./putnamiw` also sets `PUTNAMI_NO_RELAUNCH=1` for its own from-source builds.)

List the installed binaries and switch the active one:

```bash
putnami version list                     # installed CLI binaries (* = active)
putnami version list --global            # ...in ~/.putnami/bin
putnami version use go-source-1a2b3c4    # switch the active binary
putnami version use go-1.2.0 --global    # switch the global active binary
```

## Pinning the CLI per workspace

A workspace can pin the exact CLI version everyone uses, so the global `putnami`, `./putnamiw`, and the `putnami mcp` server all run **one engine** — no drift between a contributor's installed CLI and what CI runs. A workspace that *produces* the CLI declares a source sentinel instead; see [Source workspaces](#source-workspaces-building-the-cli-instead-of-pinning-one).

```bash
putnami pin 1.2.3     # Pin the CLI to 1.2.3 for this workspace
putnami pin           # Show the current pin
putnami pin --remove  # Remove the pin
```

`putnami pin <version>` downloads that version's binary for the current platform, records its SHA-256 in `putnami.lock.json`, and warms the machine-global store. It also downloads and verifies the binary for every platform Putnami publishes by default — `darwin/arm64`, `darwin/amd64`, `linux/amd64`, `linux/arm64` — plus any platform an earlier pin already carried, so one invocation from one machine keeps the lock verifiable everywhere your team and CI run it. See [Per-platform digests](#per-platform-digests).

```json
{
  "version": 3,
  "cli": {
    "version": "1.2.3",
    "protocolVersion": 2,
    "integrities": { "darwin/arm64": "9323ac7d…", "linux/amd64": "…" }
  }
}
```

Commit `putnami.lock.json` to share the pin with your team.

The nested `protocolVersion` is the machine-output protocol emitted by that
exact CLI, so CI can reject an unsupported stream before starting the gate.
When inspecting an uncommitted or newly downloaded binary, the same declaration
is available without a workspace lock:

```bash
putnami --version --output=json
```

That command returns the ordinary versioned result envelope with the CLI
version in `data.version`.

### How it resolves

When a pin is present, the global `putnami` launcher — on every invocation in that workspace — resolves the pinned version from the machine-global store (downloading and **verifying it SHA-256 fail-closed** if it isn't already cached), then re-execs into it. So `putnami <anything>` runs the pinned engine, including `putnami mcp` (an agent's MCP server runs the same version as the CLI). A binary that is already the pinned one is detected and run directly, with no re-exec.

Resolution is **fail-closed**. A pin says "run exactly this engine", so quietly running a different one is the stale-binary drift the pin exists to prevent — and it is invisible, because the wrong engine produces wrong results rather than an error. When a pin is present and this process cannot become it, the command stops with a message naming what broke and the exact command that recovers:

| Failure | Exit | Recovery it names |
| --- | --- | --- |
| `putnami.lock.json` unreadable | 2 | `PUTNAMI_NO_RELAUNCH=1 putnami <your command>` |
| Lock written by a newer CLI | 2 | `PUTNAMI_NO_RELAUNCH=1 putnami upgrade` |
| No digest for this `os/arch` | 2 | `putnami pin <version>` (appends this platform's digest, if Putnami publishes it) |
| Download failed (offline, registry down) | 4 | `PUTNAMI_NO_RELAUNCH=1 putnami <your command>` |
| Bytes do not match the pinned digest | 1 | `putnami pin <version>` (re-anchors trust) |
| Artifact store would not take the binary | 1 | `PUTNAMI_NO_RELAUNCH=1 putnami <your command>` |
| Resolved binary would not exec | 1 | `PUTNAMI_NO_RELAUNCH=1 putnami <your command>` |

The launcher stays out of the way only where there is no pin to betray: no workspace, no lock file, a lock that pins no CLI, or a binary that already **is** the pin. Every recovery command above works because it is decided *before* the pin is: exempt invocations never enter the launcher, and `PUTNAMI_NO_RELAUNCH` is read before the lock file is opened (see [Escape hatches](#escape-hatches)).

### Per-platform digests

The pin records a digest per `os/arch`. A single `putnami pin <version>` records the current platform, every platform in the default matrix (`darwin/arm64`, `darwin/amd64`, `linux/amd64`, `linux/arm64`, `windows/amd64`), and every platform an earlier pin already carried — each verified against *that platform's own* archive, not copied from the local one. Re-pinning the *same* version fills in any gap without re-downloading a platform already verified for it; re-pinning a *different* version re-verifies every one of those platforms against the new version's own archives.

If the new version does not publish an archive for a platform the previous pin carried, that platform's digest is dropped — carrying it forward unverified would let the lock claim a binary Putnami never built — and `pin` says so on stderr, naming the platform and the version. Everything else survives.

A platform outside the default five (for example `windows/arm64`, or another architecture Putnami does not build by default) is not attempted automatically; add it with one `putnami pin <version>` run from that machine, which appends its digest to the same entry alongside the platforms already recorded.

A platform with no digest has nothing to verify the download against, so the pin cannot be honored there and `putnami` fails closed until someone runs `putnami pin <version>` (from that platform, if it is outside the default five) to record it.

### Security

The pin is the trust anchor for every later launch, so the download enforces the same registry rules as the rest of the CLI: it refuses a plaintext `http://` registry (set `PUTNAMI_ALLOW_INSECURE_REGISTRY=1` to override) and verifies the downloaded binary's SHA-256 against the pin fail-closed.

### Escape hatches

Because resolution is fail-closed, the way out is never optional — these hatches are decided **before** the pin is read, so they work no matter how badly the pinned engine, the lock, or the network is broken:

| Hatch | What it does |
| --- | --- |
| `putnami pin <version>` / `putnami pin --remove` | Never relaunched, so you can always complete, change, or drop a pin — even when the pinned engine is broken or predates the `pin` command. |
| `putnami version list` / `putnami version use <name>` | Never relaunched, so you can inspect and switch the installed binaries. |
| `putnami upgrade --from-source` | Never relaunched, so you can build and adopt a local CLI when the channel is unavailable. |
| `PUTNAMI_NO_RELAUNCH=1 <any command>` | Bypasses the launcher for one command and everything it spawns, no matter how badly the pinned engine, the lock, or the network is broken. `./putnamiw` sets it automatically for from-source builds, so local CLI development is never hijacked by a workspace pin. |

Note that plain `putnami upgrade` is **not** exempt — it relaunches into the pinned engine like any other command, so use the `PUTNAMI_NO_RELAUNCH=1` form when the pin is what you are trying to escape.

`PUTNAMI_NO_RELAUNCH` covers a **published pin** only. It does not apply in a source workspace (below): there is no pinned binary there to be broken, so the opt-out would only let a foreign engine run. Use `./putnamiw`, or `putnami pin <version>` to leave self-hosting altogether.

## Source workspaces: building the CLI instead of pinning one

A workspace that **produces** the CLI cannot pin one. Pinning would mean testing
a `tooling/cli` change with a binary built from a different commit, and adopting
each change would take a second "activation" commit that bumps the pin after
publishing. Such a workspace declares itself the source of its own engine:

```json
{
  "version": 3,
  "cli": {
    "source": "workspace"
  }
}
```

`source: "workspace"` is a reserved literal. `version` and `integrities` are
absent by construction — there is no published artifact to name. The lock format
does not change.

This is the shape the `putnami` repository itself uses. It is **not** the same as
having no `cli` entry:

| `cli` entry | Launcher behavior |
| --- | --- |
| absent | Fail-open. Whatever binary is running continues; nothing is claimed. |
| a published pin | Fail-closed on the pin. The launcher resolves, verifies, and re-execs into that exact binary. |
| `source: "workspace"` | Fail-closed on provenance. Only a binary built from this tree runs. |

### How a binary proves it came from the tree

`./putnamiw` exports two variables right before running the CLI it just built (or
reused from its build cache):

| Variable | Carries | What it proves |
| --- | --- | --- |
| `PUTNAMI_FROM_SOURCE` | the absolute workspace root | this tree, not another checkout |
| `PUTNAMI_FROM_SOURCE_KEY` | the build's content key | this binary, not another engine |

The launcher accepts a binary when the root names the same workspace it
discovered — compared as resolved paths, so git worktrees and symlinked checkouts
still match — **and** the running binary is the store blob the key names,
`$PUTNAMI_HOME/artifacts/cli-source/<key>/putnami`.

The key pair is the workspace's **ephemeral engine pin**: it lives as long as the
process tree that inherits it, and no longer. The blob it names is
content-addressed and published first-writer-wins, so nothing rewrites it while a
process is running from it. That is why the wrapper execs the blob directly rather
than `.putnami/bin/putnami`: the link stays published for humans and for tools
that put `.putnami/bin` on `PATH`, but a link is a name whose meaning another run
can change, and `./putnamiw` re-points it on every run whose key differs. A
concurrent invocation anywhere in the same checkout used to retroactively refuse
the re-entrant children of a CLI that was still running.

A checkout with no git work tree or no SHA-256 tool gets no content key, so there
is no blob to pin; there the launcher falls back to checking that the running
binary IS `.putnami/bin/putnami`.

Anything else is refused. The message names each proof separately, because they
fail for unrelated reasons and the fix differs:

```
$ putnami build --all
putnami: putnami.lock.json declares this workspace the source of its own CLI
(cli.source = "workspace"), so only a putnami built from this tree may run here;
/usr/local/bin/putnami is not it. A from-source run carries two things:
PUTNAMI_FROM_SOURCE naming this workspace root, and the binary being the engine
that run selected — the immutable store blob PUTNAMI_FROM_SOURCE_KEY pins, or
.putnami/bin/putnami on a checkout with no content key. Here
PUTNAMI_FROM_SOURCE is unset, no from-source build is pinned
(PUTNAMI_FROM_SOURCE_KEY is unset), and .putnami/bin/putnami points at
~/.putnami/artifacts/cli-source/b59626bb…/putnami, not this binary, so running
this would gate the commit with an engine the commit does not contain
  Try: ./putnamiw build --all
```

Two things still run as-is: a **Go test binary** (`go test` produced it from this
very tree) and the launcher-exempt recovery commands in the table above.
`putnami pin <version>` replaces the sentinel with a real pin, and
`putnami pin --remove` clears it.

### The from-source build cache

In a source workspace that is a git work tree, `./putnamiw` keys its build on the
**content** of the sources it compiles:

- `git ls-tree HEAD` over `go.work go.work.sum tooling protocols go`;
- the dirty overlay — `git status --porcelain=v1 --untracked-files=all -z` over
  the same pathspec, plus `git hash-object` of every working-tree file it names,
  so uncommitted and untracked edits move the key. `git status` honors
  `.gitignore`, so `node_modules`, `.gen`, and `.putnami` are excluded;
- the resolved Go toolchain identity and the exact ldflags.

The result lives at `$PUTNAMI_HOME/artifacts/cli-source/<key>/putnami`, shared by
every worktree on the machine: two worktrees at the same tree state share one blob
and one build. `.putnami/bin/putnami` is an absolute symlink into it, and
`.putnami/bin/.from-source` records the key it was built from.

The pathspec deliberately over-approximates. Over-invalidating costs one warm
relink; under-invalidating ships a stale engine while claiming it is the commit's
own — which is what the older mtime gate did when it watched `tooling/cli` alone.
You no longer need `--bootstrap` after editing `protocols/` or the extension SDK.

| Situation | What happens |
| --- | --- |
| Same tree state as the last run | The cached blob runs. `go build` is not invoked. |
| Any keyed file changed | Rebuild, then republish under the new key. |
| A sibling worktree already built this key | Its blob is adopted; nothing is compiled. |
| `--bootstrap` | Rebuild and republish, ignoring the cached key. |
| `go build` fails | Hard failure. No fallback to a cached blob or a downloaded binary. |
| Not a git work tree (a tarball checkout) | No key; the historical mtime gate runs unchanged. |

## Build Integration

The CLI auto-installs to `.putnami/bin/` after building:

| Project | Install path | Config |
|---------|-------------|--------|
| `tooling/cli` | `.putnami/bin/putnami-go-dev` | `putnami.json` → `@putnami/go.install` |

After a successful build, if no `putnami` symlink exists (or the current one is broken), a symlink is automatically created pointing to the freshly built binary.

## Install Script

[Installing the CLI](22-installing-the-cli.md) is the authority for the install
script — every flag, every refusal, and the same-shell `PATH` behavior. The short
version, and how it relates to the layouts above:

```bash
# Install the newest stable Go variant into the versioned ~/.putnami/bin layout
curl -fsSL https://putnami.dev/install.sh | bash

# Install a specific variant and version (a bare semver is v-prefixed)
curl -fsSL https://putnami.dev/install.sh | bash -s -- --variant go --version 1.2.0

# Install under the plain name in a directory you manage yourself
curl -fsSL https://putnami.dev/install.sh | bash -s -- --install-dir ~/.local/bin
```

On Windows, the PowerShell installer takes the same options through environment
variables. Type these in a PowerShell window:

```powershell
# Install the newest stable Go variant into %USERPROFILE%\.putnami\bin
irm https://putnami.dev/install.ps1 | iex

# Install a specific version
$env:PUTNAMI_VERSION = '1.2.0'; irm https://putnami.dev/install.ps1 | iex
```

Three properties matter for the layouts and the upgrade path in this document:

- **The default layout is the one `version use` and `upgrade --global` manage.**
  `~/.putnami/bin/putnami-<variant>-<tag>` plus a relative `putnami` symlink, so
  an install and an upgrade land in the same shape. On Windows it is
  `putnami-<variant>-<tag>.exe` plus a `putnami.exe` copy, switched as
  described in [Layout](#layout).
- **The installer is fail-closed on integrity, exactly like `upgrade`.** It reads
  the registry's `X-Integrity` (or RFC 9530 `Digest`) header, compares it to the
  download's SHA-256, and refuses when there is nothing to compare —
  `PUTNAMI_UNSAFE_INSTALL=1` is the same override used elsewhere in the
  installer. It also refuses a binary whose `--version` disagrees with
  `X-Resolved-Version` — the same `verifyDownloadedBinaryStamp` guard applies
  on the upgrade path.
- **It never escalates privileges.** `--install-dir` pointed at a directory you do
  not own fails with the directory named; it does not call `sudo`.

## Binary Naming Convention

Binaries follow the pattern `putnami-{variant}-{version}`:

- `putnami-go-dev` — local Go build
- `putnami-go-1.2.0` — installed Go release v1.2.0
