# ADR 0055 — Run credentials stay out of repository processes

- **Status**: accepted. On a run that publishes through `publication-v1`,
  locally or hosted, the publish credential stays out of repository processes
  too ([ADR 0057](0057-publication-authority-stays-in-the-engine.md)).
- **Scope**: `@putnami/cli` (`internal/runcredential`, `internal/launch`,
  `internal/credentialprovider`, `internal/cacheprovider`, `internal/jobs`,
  `internal/commands/lifecycle`, `internal/extension`, `internal/engine`),
  `go.putnami.dev/sdk/extension`
  (`procguard`, `registrycred`), `go.putnami.dev/protocol/{registry,cache,extension,runner}`,
  `@putnami/typescript` and `@putnami/go` extensions (`workspace-fetch`)

## Context

A hosted run executes a repository's code on a machine that also holds the
run's credentials: the registry bearer that installs private packages, the
cache token, and the token a remote execution hands to its executing engine.

Every repository-controlled process runs as the same user in the same
environment: `./putnamiw`, a `postinstall` script, a test, a hook, and any
process one of them leaves running. Such a process reads its own environment,
the user's home, and, on Linux, the environment and memory of any other process
of the same user through `/proc` or `ptrace`. A credential in any of those
places is a credential the repository holds.

## Decision

On a hosted run, no repository-controlled process can read a credential. A
hosted run is a run whose engine received `--credential-fd <n>`. Without the
flag, nothing changes.

### 1. The credential arrives on a descriptor and stays in memory

- The runner starts the engine with `--credential-fd <n>`: an inherited
  descriptor, 3 or more, holding the bearer on one line.
- The engine calls `procguard.DenyInspection` first, reads the descriptor and
  closes it before it starts any process, keeps the bearer in memory only, and
  removes `PUTNAMI_CACHE_TOKEN` and `PUTNAMI_CLOUD_TOKEN` from its environment.
  No job, hook or child receives either, even when a manifest declares it.
- On Linux the engine is non-dumpable (`PR_SET_DUMPABLE` 0) whenever it holds a
  credential, through `--credential-fd` or a credential purpose enabled by
  `--providers`. A same-user process then cannot read its `/proc/<pid>/environ`
  or `/proc/<pid>/mem` or attach to it.
- A pin relaunch writes the bearer to a new descriptor, passes its number as
  `--credential-fd`, and `exec`s the pinned CLI with otherwise the same
  arguments. No other process inherits it.
- Providers receive the credential over RPC, never in their environment: a
  credential-provider in the `initialize` member `runCredential`; a cache
  provider through `authenticate`, after both sides negotiate the
  `run-credential` capability. A provider that receives it calls
  `procguard.DenyInspection`.
- The executing engine of a `--where remote` run receives it the same way: the
  runner provider starts the pinned entrypoint with `--credential-fd <n>` as its
  only argument.

### 2. Every credentialed download ends before repository code starts

- A hosted run runs the extensions installed from the artifact store, which the
  CLI downloaded and verified against the lock, and the workspace's own path
  extensions: a workspace project with a `putnami.extension.json`, or an
  `extensions` entry that names a path inside the workspace. Discovery looks in
  no `node_modules` and skips an extension loaded from an absolute path, or
  whose files resolve outside the workspace or inside its `node_modules`
  directory, naming the extension and the reason. `putnami extensions install
  <path>` accepts only a path extension of the workspace.
- A path extension is repository code and serves no provider. Discovery removes
  its `credential-provider`, `cache-provider`, `runner-provider`,
  `session-reporter`, `log-reporter` and `cloud-release-set` commands, each with
  a skip record that names the extension and the command, so every provider
  comes from the store. Its runtime, jobs and hooks start only after custody
  ends (§4).
- A hosted run reads no registry the workspace declares
  (`registries.put.registry`). The runner's `PUTNAMI_REGISTRY_PUT_URL`, then
  `PUTNAMI_REGISTRY_URL`, or the default registry decides where the pinned CLI
  and the extensions come from. The repository chooses versions, never the
  source of the code that holds the credential.
- A hosted run resolves no toolchain inside the workspace. The toolchain probe
  runs before the fetch jobs and serves every later job, so a candidate inside
  the workspace is rejected without being started.
- `putnami install` runs every extension's `workspace-fetch` to completion,
  then `workspace-install`. A hosted run ignores the install state under
  `.putnami`: a repository could commit one that skips the fetch.
- `workspace-fetch` downloads dependencies and runs none of their code. A
  store extension's `workspace-fetch` is the only job that receives the read
  credential, on an inherited descriptor named by `PUTNAMI_JOB_CREDENTIAL_FD`,
  and only in that first install step, the dependency fetch. A path
  extension's `workspace-fetch` runs after custody ends, offline and without a
  credential. A `workspace-fetch` selected by a plan or an alias is an ordinary
  job.
- The dependency fetch of a hosted install sees only the store extensions: it
  prepares no path extension's runtime and plans none of its jobs. Any job
  other than a store-installed extension's `workspace-fetch` run by the
  extension's own runtime fails before it starts, and no job runs an
  extension's `preBuild` hook.
- A fetch job reads the descriptor once, closes it before it starts any process,
  and makes itself non-dumpable. A job handed a descriptor, even an empty one,
  starts no credential child such as `putnami cloud registry-token`.
- The engine sets `PUTNAMI_HOSTED_FETCH=1` in that job and no other, and hands
  the job credential only to a job whose `PUTNAMI_CLI_EXECUTABLE` is the
  engine's own executable. A CLI started with `PUTNAMI_HOSTED_FETCH` set exits 2
  before it reads the workspace, so a fetch built on an older SDK that calls back
  into the CLI runs no workspace extension beside the credential.
- Every other job and hook runs with `PUTNAMI_OFFLINE_DEPENDENCIES=1` and no
  credential. No package manager reaches the network for dependencies, and no
  installer writes a credential into the user's home.
- The engine gives `workspace-fetch` a private 0700 `TMPDIR` and removes it
  however the job ends. Only a killed engine leaves it, readable by its user
  alone, until the runner discards the machine.
- TypeScript: `workspace-fetch` copies the manifests, `bun.lock` and `bunfig`
  into a private directory and runs `bun install --ignore-scripts
  --frozen-lockfile` there, with the bearer in a 0600 `.npmrc` under a private
  `HOME` it deletes afterwards. `workspace-install` then runs
  `bun install --frozen-lockfile` in the workspace from the shared bun cache,
  which runs the lifecycle scripts and leaves `bun.lock` unchanged. The fetch
  must not touch the workspace tree: with bun 1.4.0, an in-place
  `--ignore-scripts` install makes the second install run no script, and
  `--offline` is accepted and ignored.
- Go: `workspace-fetch` downloads modules and tools with an ephemeral 0600
  `NETRC` it deletes afterwards, and starts no program inside the workspace.
  Every later Go command runs with `GOPROXY=off` and `-mod=readonly`.

### 3. The bootstrap reaches the locked CLI without a repository process

The CLI that starts a hosted run may differ from the locked CLI, and the store
may be empty. Both downloads happen before the workspace's credential provider
exists, so a bootstrap provider serves them.

- The engine opens the bootstrap provider when it holds the run credential, or
  when `--providers` or `PUTNAMI_PROVIDERS` enables the `install` provider, and
  never for a launcher-exempt invocation such as `putnami pin`.
- The bootstrap provider is the credential-provider a user-scope extension
  (`~/.putnami/user`) declares; a workspace extension is never considered. This
  is an exception to [ADR 0051](0051-user-scope-extensions-run-without-a-workspace.md),
  limited to these runs. With no user-scope declarer, a run without the flag
  keeps the host-keyed credential; with several, the first download that asks
  fails.
- It serves the `read` purpose for two downloads only: the locked CLI the
  relaunch fetches, and the lock-pinned extensions the engine materializes before
  discovery, for every invocation in a workspace. It is their first credential
  source and a refusal fails the download. Without the flag, a host its
  credential does not name keeps the host-keyed credential
  ([registry ADR 0002](../../../../protocols/registry/doc/adr/0002-one-credential-call-per-purpose.md)).
- A hosted run never asks the host-keyed credential: its child,
  `putnami cloud registry-token`, loads the workspace's extensions. Neither the
  CLI nor the SDK's `registrycred` helpers start it when
  `PUTNAMI_OFFLINE_DEPENDENCIES=1` is set or a credential descriptor was handed
  over. A download no credential-provider serves goes out without a credential,
  and a registry refusal fails the run with an error that says to install a
  user-scope credential-provider.
- It starts on the first download that asks, receives the credential in
  `initialize` only, and inherits neither the descriptor, `PUTNAMI_PROVIDERS`,
  nor the cloud capability transport. The engine shuts it down before it
  `exec`s the locked CLI and before it installs the workspace's own provider (or
  a bound request's provider).
- **Custody level.** A CLI's custody level numbers what it guarantees for the
  credential; it is 2 (`runcredential.CustodyLevel`): a native credential
  holder, no credential child from a fetch, and no command inside a credentialed
  fetch. A CLI advertises it at the end of its `--help` line for
  `--credential-fd`, as `custody level 2`. Before a hosted relaunch, the engine
  runs `<locked CLI> --help` in an empty temporary directory with only `PATH`,
  `HOME`, `PUTNAMI_NO_RELAUNCH=1` and `PUTNAMI_NO_AUTO_INSTALL=1`, no descriptor
  beyond the standard streams, and a 30-second limit. It `exec`s the locked CLI
  only when the command exits 0 and advertises a level at or above its own; a
  line with no level is below. A relaunch without the run credential runs no
  check. Extensions advertise no level.
- The check trusts the printed level because the binary was verified against the
  lock when it entered the store; it stops an older genuine build, not a forged
  one. A stored binary is not hashed again, so a hosted run refuses a store the
  workspace could hold: with neither `HOME` nor `PUTNAMI_ARTIFACT_DIR` set, the
  store falls back to `<workspace>/.putnami/artifacts`, and the run stops before
  it reads the workspace.

### 4. No process starts with the credential after repository code

A repository process runs as the owner of the artifact store, the user scope and
the Putnami home, so it can rewrite any program the engine starts next. Custody
ends when repository code starts.

- The engine marks the run when it starts a repository process: a hook, a job
  other than a credentialed `workspace-fetch`, a toolchain inside the workspace,
  an extension runtime outside the store, such as a path extension's, or a
  nested CLI that loads the workspace's extensions, such as
  `putnami cloud release-set`. It marks a runtime before it prepares it. From
  then on,
  every handoff fails with an error naming the process that asked and the one
  that ran repository code: the job credential descriptor, a cache provider's
  `authenticate`, and `initialize.runCredential`. A holder keeps what it has.
- A test lists every place under `internal/` that starts a process, directly,
  through a variable that holds a helper, or through an SDK helper
  (`registrycred`, `releaseset`, `dbtestenv`), and fails on an unclassified one.
  A second test reads every package outside the CLI that the CLI imports and
  fails on an unlisted helper that starts a process.
- On a hosted run the order is fixed: every store extension's
  `workspace-fetch`; the remote cache provider starts and authenticates; every
  path extension's `workspace-fetch`, offline; the extensions' `onInstall`
  hooks and the installers; the `before` hooks and the tasks. The install that
  `putnami build` runs first follows it, and the build reuses that provider.
  Every executable that receives the credential is bytes the store held before
  any repository process of the invocation ran, and no path extension's runtime
  starts before the last handoff.
- A cache provider or credential-provider must be one native executable that
  loads nothing from the store after it starts. On a hosted run the CLI starts
  either only as its extension's native runtime (`{extensionRuntime}`) and
  refuses, naming the provider, a command that is not the runtime or a runtime
  that is a script (`#!`), a link, or not an executable in the host's format
  (ELF, Mach-O, PE). The run fails before its first hook. A hosted run with a
  remote cache therefore needs an `@putnami/cloud` release whose
  `cache-provider` task runs `{extensionRuntime}`.
- A hosted runner passes `--credential-fd` to one invocation per fresh sandbox:
  a fresh checkout, `HOME`, artifact store and `TMPDIR`, in a container or VM no
  earlier process outlives. An earlier invocation can leave `.git/config`
  entries git runs, state under `.putnami`, a rewritten store, home or user
  scope, and detached daemons. `putnami build --credential-fd <n>` installs in
  the same invocation. The runner holds this rule; the CLI cannot check it.
- `putnami upgrade` refuses `--credential-fd` before any phase: it rewrites the
  lock a hosted run executes as committed, and restarts after the
  `deps-upgrade` hooks.
- An extension command group refuses `--credential-fd` with a usage error: it
  resolves its remote cache after the install ran repository code. A hosted run
  supports job commands, such as `build`, and built-in commands, such as
  `install`.

## Consequences

- A repository process on a hosted run finds no credential in its environment,
  the user's home, the workspace, or the memory of the engine and its providers.
- A hosted runner starts the CLI binary directly. `./putnamiw` is a
  repository-controlled script that would hold the descriptor first, so a
  workspace that builds its own CLI cannot run hosted; it must pin the CLI from
  a registry. A workspace's path extensions run hosted, after custody ends, and
  serve no provider: a workspace whose cache provider or credential-provider is
  a local source must pin that extension from a registry to run hosted with it.
- A hosted install runs the fetch command twice: once for the store
  extensions, once for the path extensions after the cache provider starts.
- A pinned CLI below the custody level is never `exec`ed by a hosted relaunch:
  the run fails with exit 2, naming the pinned version and `putnami pin
  <version>`. A check that cannot run fails with exit 1. Each hosted relaunch
  costs one extra process start. A change that closes a custody gap raises the
  level.
- An extension that downloads dependencies outside `workspace-fetch` fails on a
  hosted run. That failure is the contract.
- A hosted install writes no committed file: not `putnami.lock.json`,
  `bun.lock`, `.npmrc`, `go.work`, a `go.mod`, a manifest or assistant content.
  A configured artifact the committed lock does not pin fails it. A workspace
  runs hosted only from files a local install already wrote and committed.
- A credential-provider without the `runCredential` member fails at the first
  credential of a hosted run. A cache provider that does not echo
  `run-credential` receives no credential. A run without the flag sends neither.
- A runner that reuses any part of the sandbox across credentialed invocations
  breaks custody: the store is verified at download, not at each run.
- A hosted run with a remote cache starts the cache provider even when every
  task hits the local cache.
- A hosted run takes toolchains from the runner's `PATH`, its environment or the
  Putnami home, never from the workspace.
- A hosted run records no failure for replay: it runs offline, which a task key
  that does not declare `PUTNAMI_OFFLINE_DEPENDENCIES` does not describe.
- A hosted run holds no cloud token, so a job that publishes with
  `PUTNAMI_CLOUD_TOKEN` fails there.

## Out of scope

- Base-image pulls in container package jobs.
- The producers: the cloud's credential-provider, its cache `authenticate`
  handler, and the hosted runner that passes `--credential-fd`.
