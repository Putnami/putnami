# ADR 0056 — `init` resolves on one channel

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli/internal/commands/lifecycle`:
  `init_channel.go`, `workspace_init.go`, `deps.go`, `projects_create.go`,
  `projects_create_goframework.go`), `@putnami/typescript`
  (`typescript/extension/cmd/putnami-ts/workspace_install.go`),
  `tooling/cli/scripts/smoke-check-release.sh` and `smoke-check-release.ps1`

## Context

`putnami init` resolves three kinds of things: put artifacts (the language
extension, the agent-content extension, the template), the Go framework
version a Go starter requires, and the `@putnami/*` packages a TypeScript
starter seeds in the workspace catalog. Each one read `latest`, whatever
channel the CLI was installed from.

A tagged publish releases every version line into one immutable channel named
after the tag, for example `tooling-v0.4.0`. A release candidate is
smoke-tested on that channel before `latest` moves to it. A CLI installed from
the candidate channel then initialized a workspace from `latest`: it mixed the
candidate CLI with an older release set, or stopped with
`download <template> at latest … HTTP 404` while `latest` was empty. The smoke
could not prove the candidate.

Every registry already publishes a channel natively
([ADR 0020](0020-upgrade-resolves-channels-natively.md)): the put registry
answers `download?channel=<c>`, npm answers `dist-tags[<c>]`, and a Go module
origin answers `@v/<c>.info`.

## Decision

**One channel choice drives every resolution of an `init` run, and the
workspace `init` creates does not remember it.**

### 1. The choice

`init` resolves on the first of:

1. `--channel <name>`;
2. `PUTNAMI_CHANNEL`, when it is not empty;
3. the install record of the running CLI (point 2);
4. `latest`.

`stable` reads `latest`, as in `putnami upgrade`. The choice is a channel, not
an exact version: the version lines do not share a version, so one version
cannot name a release set.

A channel name is in the portable channel alphabet, the one every ecosystem
accepts (`PortableChannelPattern` of the extension protocol,
`^[a-z0-9][a-z0-9._-]{0,63}$`): it starts with a lowercase letter or a digit
and holds only lowercase letters, digits, `.`, `_` and `-`, 64 characters at
most. `init` refuses an empty name, a name outside that alphabet, and a
semantic version (with or without a leading `v`) with a usage error. It refuses
before it writes a file or sends a request, and before it reports that the
directory is already a workspace. The name goes unchanged into a registry
query, a module proxy path and an `<artifact>@<channel>` reference. The
alphabet holds no uppercase letter, so the Go version query carries the name
without case encoding, byte for byte the path the Go extension asks.

### 2. The install record

The install record is the name of the running executable once its links
resolve. The installers place the CLI as `putnami-<variant>-<tag>` and point
`putnami` at it, where `<tag>` is the channel or the version the install asked
for. No file is added: a separate record would go stale the first time
`putnami upgrade` replaces the binary, and the name is rewritten by the same
step that replaces it.

`<tag>` is a channel when it is in the portable channel alphabet, unless it
is:

| Tag | Written by | Record |
|---|---|---|
| a semantic version, with or without `v` | a version install, `putnami upgrade` | none |
| `source-<revision>` | `putnami upgrade --from-source` | none |
| `dev` | a local build of this repository | none |

A name of any other shape records nothing. On Windows the active
`putnami.exe` is a copy of the installed `putnami-<variant>-<tag>.exe`, not a
link to it, so its name carries no tag and no record applies: a Windows user
chooses a channel with `--channel` or `PUTNAMI_CHANNEL`.

No record means `latest`.

### 3. Put artifacts

On a channel other than `latest`, `init` installs the language extension, the
agent-content extension and the template as `<name>@<channel>`, the form
`putnami extensions install <name>@<channel>` takes. The install the project
creation falls back to when the template is not in the workspace uses the same
form. The steps that follow read the lock: materializing the agent content,
and the implicit install before a job. None of them resolves a channel.

The channel is a target of the run only:

- `putnami.workspace.json` keeps bare artifact names, with no constraint;
- `putnami.lock.json` records the exact version, the digests and a source that
  names the exact version;
- a later `putnami install` reads the lock.

A channel that names no release for an artifact fails the run. It never falls
back to `latest`.

### 4. Starter dependencies

`init` hands the channel to the workspace installers as the job option
`putnami-channel`, the way `upgrade` hands its target to the `deps-upgrade`
job. The option is not ambient state: it travels in the job context, and it is
absent on `latest`, so an install that chose no channel sends the request it
sent before.

- **TypeScript.** For a channel other than `latest`, `@putnami/typescript`
  seeds a missing workspace catalog entry with the exact version the channel's
  dist-tag names for that package, read with the dist-tag reader `deps-upgrade`
  uses. It never writes the channel name: a catalog entry that named an
  immutable channel would pin the workspace to it. A package the channel does
  not name fails the install before the package manager runs. For `latest` the
  seed stays the `latest` dist-tag. Core adds no npm reader.
- **Go.** For a channel other than `latest`, the framework version comes from
  the Go version query `@v/<channel>.info` instead of `@latest`, on the same
  proxies and origin, with the same credentials. `projects create` run on its
  own still asks `@latest`.
- **Python.** The starter depends on no Putnami framework package. Nothing
  beyond the extension and the template is resolved.

### 5. The release smokes

`smoke-check-release.sh` and `smoke-check-release.ps1` export the channel they
are started on as `PUTNAMI_CHANNEL`, after they clear every `PUTNAMI_`
variable. The command line stays `putnami init --project webapp --extension
ts`, the one a user runs, and a `latest` smoke sends the requests it sent
before. The smokes do not rely on the install record: it does not exist on
Windows, and an explicit hand-off does not depend on how the installer names
the binary.

## Consequences

- A candidate smoke uses only the candidate's release set, and passes while
  `latest` is empty.
- A CLI installed from a moving channel such as `canary` on macOS or Linux
  initializes on that channel without a flag. `init` prints the channel and
  what chose it. `--channel latest` restores the previous behavior.
- `putnami upgrade` is unchanged. It reads neither `PUTNAMI_CHANNEL` nor the
  install record, and follows `latest` by default, so a workspace created on a
  candidate channel moves to `latest` at its next upgrade.
- **The Go starter renders one framework version for its four modules**
  (`go.putnami.dev/app`, `config`, `http`, `logger`): the version the channel
  names for `go.putnami.dev/app`. A tagged publish releases every module of a
  line at one version, so an immutable release channel is consistent. On a
  moving channel an impacted publication can leave the four modules at
  different versions; `go mod tidy` then fails for a module that was not
  published at the version of `go.putnami.dev/app`. The same limit holds on
  `latest`.
- Reading mutable channels is not atomic across registries: a moving channel
  that changes between two reads of one run can mix two publications. An
  immutable release channel cannot.
- A template that renders `putnamiVersion` still gets `latest`. No template
  in this repository renders it.
- The exact version in the lock is the one the registry states in
  `X-Resolved-Version`. A registry that answers a channel download without
  that header leaves the lock with the version `0.0.0` and a source that names
  the channel, as any `<name>@<channel>` install does. `init` does not add a
  second check.

## Rejected alternatives

- **An exact version as the choice.** The CLI, the extensions, the templates
  and the frameworks are released on separate version lines. One version names
  at most one of them.
- **A file that records the install channel.** `putnami upgrade` replaces the
  binary without rewriting such a file, so it would name the channel of an
  install that is no longer the running one.
- **Resolving the release set through the provider.** It needs the publisher's
  namespace, which a consuming workspace does not know
  ([ADR 0020](0020-upgrade-resolves-channels-natively.md)).
- **Writing the channel as a constraint in `putnami.workspace.json`.** An
  immutable candidate channel never moves, so the workspace could not be
  upgraded without editing its config.
- **Reading the channel from the environment inside the installers.** The job
  runner decides which variables a task receives. A job option reaches the
  task on every placement and is part of what the task's inputs record.
- **A npm dist-tag reader in core for the TypeScript seed.** ADR 0020 keeps
  each registry protocol in the extension that owns the ecosystem.
