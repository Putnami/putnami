# Extension SDK

Shared Go infrastructure for Putnami extension binaries. Provides the JSONL protocol emitter, job context parser, CLI runner, flag parsing, and subprocess execution — everything an extension needs to communicate with the orchestrator.

The workspace publishes `go.putnami.dev/sdk/extension` as a Go member of its
release set. Downstream extension workspaces can upgrade this module and the
framework protocols from the same immutable set; there is no native CLI archive
for the SDK library.

## Features

- JSONL event emitter (phases, progress, diagnostics, metrics, artifacts)
- Job context parser with typed parameter access
- Standard job lifecycle (context loading, meta/result emission)
- Multi-job binary support via subcommand dispatch
- Lightweight flag parser (`--key value`, `--key=value`, `--no-flag`)
- Subprocess execution with timeouts, context cancellation, and captured output
- Structured log forwarding from child processes
- Manifest authoring with v3 task declarations and command groups, validated at build time
- The distribution matrix (`pkgmeta.ArchivePlatforms`): the registry platform keys every archive packager publishes under
- Agent-content packaging (`agentartifact`): one builder for extension content, `PackageExtension` for a content-only extension, and `StageExtensionContent`/`VerifyStagedExtensionContent` for a packager that ships an executable beside the content ([ADR 0003](doc/adr/0003-every-packager-ships-agent-content-through-one-step.md))
- ARC/DARC domain-manifest authoring, with the permission half left declarative and a pin that holds a committed manifest to its Go authoring
- Result envelopes byte-identical to the CLI's for interactive subcommands
- Runtime executable handshake helper for `__putnami runtime-info`
- Workspace-probe serving for an extension that owns project discovery
- Neutral eviction policy for an extension-owned machine cache
- Versioned OCI image plans, deterministic one-pass layer assembly, and a verified cache shared across image extensions and worktrees
- Invocation-scoped sensitive artifacts, finalizers and SIGKILL lease recovery
- Host platform identity scrub for test subprocesses (`K_SERVICE` and friends)
- Temporary directories that outlive a killed creator by at most one run (`scratch`): the next creator removes what a dead one left
- Digest-pinned archive installs (`pinnedarchive`): download, SHA-256 verification against the pin, link-free `.tar.gz`/`.zip` extraction, and an atomic publish under a `filelock` lock
- The Putnami home and its toolchain directories (`putnamihome`), resolved as the CLI resolves them
- Reproducible, link-free `.tar.gz` packaging without a `cp` or `tar` process (`treearchive`)
- Publication outbox (`publicationoutbox`): a job writes its packed artifacts and `outbox.json` under `PUTNAMI_PUBLICATION_OUTBOX`; the engine reads each artifact once and refuses a size or digest the descriptor does not state ([ADR 0004](doc/adr/0004-a-publication-job-packs-and-the-engine-uploads.md))
- Registry uploaders that take the bearer as an argument and send it only to the registry they were given: the managed npm PUT (`npmpublish`), the gomod-write upload (`gomodpublish`), the put-write upload of archive, config, migration and doc members (`putpublish`), and `oci.PushLayout`; each reuses a version already published at the same digest
- Spec-verification fragment merge (`specreport`): the shared adapter half of the executable-spec gate — bounded fragment reads, project attribution, strict-wire validation, and the reserved `putnami-feature-verification` report artifact

## Lifecycle primitives

The orchestrator consumes exactly **three** generic lifecycle surfaces from an
extension, and no fourth. They are what replaced the CLI's built-in knowledge of
languages, toolchains, project markers and test databases. The normative
contract is [`protocols/extension/doc/07-lifecycles.md`](../../protocols/extension/doc/07-lifecycles.md);
the author's-eye version, with the CLI-side behavior, is
[`tooling/cli/doc/07-extensions.md`](../cli/doc/07-extensions.md#lifecycle-primitives).

| Primitive | Manifest | SDK support |
|-----------|----------|-------------|
| Runtime preparation | `runtime.executable`, `runtime.prepare` | `runtimeinfo.Handle` answers the `__putnami runtime-info` handshake; `manifest` validates the declaration at build time |
| Workspace adapter | `workspace.markers`, `.inputs`, `.excludes`, `.syncTask` | `ServeProbe` in `go.putnami.dev/protocol/workspace` serves `__putnami workspace-probe`; project-level `watchedFiles` may include provider-owned workspace-root invalidation inputs without changing the v1 wire shape |
| Invocation-scoped sensitive outputs | `declares.outputs` (`scope: "invocation"`, `sensitive`), `runOn: "finally"` + `finalizes` | `dbtestenv` is the reference implementation — provisioning, the non-secret lease, orphan reaping after `SIGKILL`, and the `sensitive` binding artifact a consumer reads with `BindingFrom` |

Beside them, an extension that keeps a machine-global cache owns it end to end:
core hands it `extension.cacheRoot` and dispatches the reserved hidden commands
`cache-clean` and `cache-gc` to it. `cachepolicy` supplies the neutral half —
recency sort, low watermark, grace window, cross-process collector lock, and the
freed-bytes summary — while the extension supplies only what one of its cache
entries *is*.

## OCI image identity

Published release-set coordinates retain the full native repository path,
excluding only the registry host. For `oci.example.com/team/app`, the coordinate
is `team/app`, including when `team` is part of `registries.oci.publish`. The
namespace is required for native authorization and channel-tag projection.

Native image publication reports a `published-member` only after the pushed
or reused digest is verified. The member uses the exact version from its native
release-set plan. The same selected member binds the managed repository
namespace to the task's typed project identity, even when the workspace and
release-set namespaces differ. A configured registry must agree with that
member. The version identifies publication evidence and does not authorize a
tag write. Through the native private broker, image publication
writes only the immutable digest, leaving channel projection to the release.
A dry run pushes nothing and reports a `member-probe` event per image instead.
See [OCI publication evidence](doc/09-oci-publication.md) for the producer contract.

`oci.NewImagePlan` computes the v2 content identity and reusable layer keys from
explicit SHA-256 source fingerprints plus a pinned compression identity. Pass
the same plan to `oci.AssemblePlanToLayout`; this avoids rediscovering every
layer fingerprint after the content tag is known. A trusted producer may hand
raw-byte fingerprints through `PlanOptions.TrustedFingerprints`, while cache
misses still verify those bytes during layer construction.

V2 intentionally rolls content hashes once after upgrade; tag shape, image
bytes, self-contained layouts, and Docker fallback behavior are unchanged. No
workspace configuration migration is required, and v1 cache entries safely
become misses.

## Installation

Add to your extension's `go.mod`:

```
require go.putnami.dev/sdk/extension v0.0.0
replace go.putnami.dev/sdk/extension => ../../tooling/extension-sdk
```

## Quick Start

```go
package main

import (
    "go.putnami.dev/sdk/extension/cli"
    pctx "go.putnami.dev/sdk/extension/context"
    "go.putnami.dev/sdk/extension/exec"
    "go.putnami.dev/sdk/extension/jsonl"
)

func main() {
    cli.RunSubcommand(map[string]cli.JobFunc{
        "build": buildJob,
        "test":  testJob,
    })
}

func buildJob(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
    flags := cli.ParseFlags(args)
    target := cli.FlagString(flags, "target", "linux/amd64")

    emit.PhaseStart("compile")
    result, err := exec.Run("go", []string{"build", "-o", "app", "."}, exec.Dir(ctx.Project.FullPath))
    if err != nil {
        emit.PhaseEnd("compile", "failed")
        return "FAILED", nil, err
    }
    if !result.Success {
        emit.Diagnostic("error", result.Stderr, "", 0)
        emit.PhaseEnd("compile", "failed")
        return "FAILED", nil, nil
    }
    emit.Metric("target", target, "")
    emit.PhaseEnd("compile", "success")
    return "OK", map[string]any{"target": target}, nil
}

func testJob(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
    emit.PhaseStart("test")
    result, err := exec.Run("go", []string{"test", "./..."}, exec.Dir(ctx.Project.FullPath))
    if err != nil {
        emit.PhaseEnd("test", "failed")
        return "FAILED", nil, err
    }
    if result.Success {
        emit.PhaseEnd("test", "success")
        return "OK", nil, nil
    }
    emit.PhaseEnd("test", "failed")
    return "FAILED", nil, nil
}
```

## Documentation

- **[JSONL Emitter](doc/01-jsonl-emitter.md)** — Structured event emission for the orchestrator protocol
- **[Job Context](doc/02-context.md)** — Typed access to workspace, project, and job metadata
- **[CLI Runner & Flags](doc/03-cli.md)** — Job lifecycle management and flag parsing
- **[Subprocess Execution](doc/04-exec.md)** — Run external commands with options
- **[Manifest Authoring](doc/05-manifest.md)** — Build a validated `putnami.extension.json`, including v3 task declarations and command groups
- **[Host Platform Identity](doc/06-hostenv.md)** — The variables a test subprocess must not inherit from its harness host, and why
- **[Result Envelopes](doc/07-result-envelope.md)** — Emit the CLI's own result document from an interactive extension subcommand
- **[Domain Manifest Authoring](doc/08-architecture-manifest.md)** — Build a validated `putnami.architecture.json` (ARC/DARC) and pin a committed one to its authoring
- **[OCI Publication Evidence](doc/09-oci-publication.md)** — Report verified image members using the native plan's identity, with digest-only private publication
- **[Lifecycle Primitives](../../protocols/extension/doc/07-lifecycles.md)** — Runtime preparation, workspace adapters, and invocation-scoped sensitive outputs with finalizers and crash recovery (normative contract)

## API Overview

| Package | Key Exports | Description |
|---------|-------------|-------------|
| `jsonl` | `New`, `Emitter`, `ForwardPipe`, `ForwardLine`, `MapSeverity` | JSONL protocol v1 emitter |
| `context` | `Parse`, `Context`, `Params` | Job context deserialization |
| `cli` | `Run`, `RunSubcommand`, `JobFunc`, `ParseFlags`, `FlagString`, `FlagBool`, `FlagInt` | Job entry point and flags |
| `exec` | `Run`, `Result`, `Dir`, `Env`, `UnsetEnv`, `Timeout`, `WithContext`, `Stdin` | Subprocess execution |
| `hostenv` | `PlatformIdentityVars`, `IsPlatformIdentity`, `ScrubPlatformIdentity` | The single list of environment variables a managed runtime injects to describe its own deployment (`K_SERVICE`, `GAE_*`, `AWS_LAMBDA_*`, …), and the scrub a test harness applies so a test process cannot mistake the identity of the machine that launched it for its own. Deployment identity only — credentials, endpoints and project/region selectors are deliberately preserved |
| `mcp` | `Serve`, `Handler` | One-request MCP-tool bridge for extension binaries |
| `manifest` | `New`, `Builder`, `Build`, `WriteFile`, `Validate`, `Declares`, `File`, `Directory`, `Effects`, `CommandGroup`, `Subcommand`, `Runs`, `Interactive`, `GroupFlag` | Manifest authoring with protocol validation at build time |
| `architecture` | `NewDomain`, `Builder`, `Build`, `CanonicalBytes`, `Validate`, `Pin`, `Reference`, `Query`, `Snapshot`, `Command`, `Projection`, `Bind` | ARC/DARC domain-manifest authoring with protocol validation at build time (experimental) |
| `resultv2` | `Emit`, `Envelope`, `Path` | The CLI's result envelope, written by an interactive extension subcommand. The subprocess inherits stdout and nothing wraps it, so the bytes must be the ones a built-in structured command writes — including the rule that `--output=json` stays indented and only the *handler* is switched to jsonl |
| `runtimeinfo` | `Handle`, `Write` | Strict extension identity, platform, CLI-contract, runtime-protocol, and ABI handshake |
| `registrycred` | `ResolveToken`, `ResolveTokenWithCLI`, `EnsureNativeCredential`, `Outcome` | Cloud-neutral registry credential seam, keyed by HOST and nothing else. `ResolveToken` returns a bare bearer; `ResolveTokenWithCLI(ctx, host, executable)` uses the calling CLI's explicit absolute executable for private-pin bootstrap without changing the parent's launcher policy or consulting PATH. `EnsureNativeCredential` asks the cloud to write the host's native credential file before an install runs a package manager. CLI-spawned extensions call back through the advertised absolute CLI executable |
| `dockerpublish` / `oci` | `Publish`, `PushContent`, `PushContentWithDigest`, `PushOptions` | Content-addressed image publication, one manifest existence check per image. Managed private runs may supply an invocation-owned numeric-loopback OCI broker; transport changes only for that call and published references remain canonical. |
| `recorded` | `HTTP`, `Response`, `NewServer`, `Server`, `Command`, `Exchange`, `Executable`, `Branch` | Test support for production boundaries: serves an HTTP response recorded with `curl -si --http1.1`, and replays a command's recorded stdout, stderr and exit status as an executable. A test that guards a registry, a credential command or another external boundary answers with what production sent instead of an authored stub; see the CLI's [testing at production boundaries](../cli/doc/23-testing-at-production-boundaries.md) |
| `memberprobe` | `Subject`, `Emit`, `Decode`, `ProbeArchive`, `NewHTTPClient`, `Redact`, `Endpoint`, `StagedReason`, `ReasonNoArtifact`, `ReasonOtherDigest` | The dry-run half of a publication. A publisher asks its registry, with GET or HEAD only, whether it already holds a member at the version the real publish would write, and reports one `member-probe` event: `absent`, `identical`, `tag-move`, `conflict` or `unverified`. `Subject` owns the verdict rules and the wording of a reason, `Subject.HeldWith` takes the reason that says what the real publish does with another digest, `Subject.HeldTag` is the verdict of a version tag an OCI publish moves, `Subject.Staged` makes an `identical` verdict name the artifact an earlier package staged (`StagedReason`), `Emit` writes the event and `Decode` reads it through the protocol's strict parser. `ProbeArchive` asks the put registry about one archive with a single HEAD. The job still succeeds; the orchestrator fails the dry run. |
| `privatebroker` | `FromEnv`, `Parse`, `Endpoint` | Reads the invocation-owned numeric-loopback publication broker a native publication run exports per registry kind (`PUTNAMI_REGISTRY_<KIND>_URL`). Absent means the direct path, a remote HTTPS value is Cloud compatibility input and is ignored, a malformed loopback value fails closed. `dockerpublish`, the Go module publisher, and the npm publisher share it so every managed publication asks the credential seam about the broker host. |
| `scratch` | `New`, `Dir`, `Sweep`, `Namespace`, `OwnerLockName`, `UnownedGrace` | Temporary directories that outlive their creator by at most one run. `New` holds an exclusive `filelock` lock on an owner lock file inside the directory until `Dir.Remove`; the operating system releases it when the creator dies, however it dies. `New` also starts, at most every ten minutes per process and prefix, a background sweep that removes every `putnami-` directory under the temporary directory whose owner lock is free, whatever its creator, and the directories without a valid lock whose name starts with its prefix once they are older than `UnownedGrace`. The lock file records its directory's name, so a copied directory is never mistaken for a dead one. The directory holds the hidden owner lock file, and the `Dir` must stay reachable until `Remove`. Use it wherever cleanup is a `defer` or runs after `m.Run()`: a `go test -timeout` panic, a canceled session or a SIGKILL skips both |
| `filelock` | `Acquire`, `Lock`, `OpenFile`, `LockFile`, `UnlockFile`, `RemoveDir`, `RemoveDirLocks`, `ErrBusy`, `ErrDirectory`, `Inheritable`, `NoFollow`, `SyncDir` | The cross-process advisory file lock the CLI, the SDK and the extensions share: shared or exclusive, released by the operating system when its holder dies, and never covering the file's content. `flock(2)` on Unix; on Windows, `LockFileEx` on one byte at offset 2^62 through a handle that lets its holder rename the lock file. Only Unix passes a held lock to a child process (`Inheritable`); a platform without an implementation does not compile. `SyncDir` makes a holder's replacing rename durable; on Windows it has nothing to flush. `RemoveDir` removes the directory that holds a lock file its caller holds: every other entry under the lock, then the release, then the lock file, because Windows 10 before version 1809 cannot remove a directory that holds an open file. `RemoveDirLocks` does the same for a directory that holds several lock files |
| `dirlink` | `Create`, `Replace`, `IsLink`, `Resolve` | The directory links Putnami writes: a symbolic link on Unix, a directory junction on Windows, which a standard user creates without Developer Mode. A junction stores an absolute path, so a relative target resolves against the link's directory there. `Replace` renames a staged link over the old one on Unix; on Windows it removes and recreates the link under an exclusive `filelock` lock on `<link>.lock`, and replaces an empty directory, which is what a junction create stopped between its two steps leaves. Readers use `IsLink` and `Resolve` where they would test `fs.ModeSymlink` or call `filepath.EvalSymlinks`: since Go 1.23 a junction reports `fs.ModeIrregular`, and `EvalSymlinks` does not follow it |
| `envkeys` | `Keys`, `Host` | Matches process environment entries (`NAME=value`) by variable name, the one rule the CLI and the extensions share: exactly on Unix, and by upper case on Windows, where the system block spells PATH as `Path`. `Value`, `Has`, `Last`, `Remove` and `Set` read and edit an entry list; `Canonical` is the key a map-backed environment stores a name under |
| `sourcebinding` | `Record`, `FileMode`, `HostStatsExecBit`, `RunGit` | The source-v1 record one git-enumerated path contributes to a project's source binding (`go.putnami.dev/protocol/capabilities`): its project-relative slash path, its mode and the digest of its working bytes, link target or visible gitlink commit. The CLI and the SDD extension both read it here, because a binding one computes is compared with a binding the other recorded, possibly on another host. Where the host stores no executable bit (Windows), a tracked file takes it from the mode git records |
| `ownerperm` | `Restrict`, `OwnerOnly` | Restricts a private file or directory to its owner. On Unix the permission bits do it; on Windows, where `os.Chmod` only toggles read-only, `Restrict` also writes a protected DACL (access control list) that grants the current user full control and nobody else, and that a directory passes on to what is created in it |
| `robustio` | `Rename`, `Remove` | `os.Rename` and `os.Remove` that retry for up to two seconds on Windows while the call fails with `ERROR_ACCESS_DENIED` or `ERROR_SHARING_VIOLATION`: a reader, an antivirus scanner or the indexer holds the file without sharing delete access. Unix renames and removes once. It follows `cmd/go`'s internal `robustio` |
| `pinnedarchive` | `Install`, `Download`, `Extract`, `WithoutCredentials`, `Pin`, `Format`, `TarGz`, `Zip`, `Options`, `Limits`, `DefaultLimits`, `ErrNoDigest`, `ErrDigestMismatch`, `ErrUnsafeEntry`, `ErrTooLarge`, `ErrIncomplete`, `ReadAdvertisedIntegrity`, `NormalizeIntegrity` | Installs a directory from an archive pinned by its SHA-256, such as a toolchain a workspace lock names. A pin without a valid digest is refused before any network access, and an archive whose digest differs is refused before extraction. Extraction writes regular files and directories only, through an `os.Root`: a link, a device, an absolute name, a `..` climb, a backslash or a colon refuses the whole archive, identically on every platform. Installers of one destination exclude each other with a `filelock` lock on `<dest>.lock`, and the destination appears only through the final rename of a complete staging directory, which `robustio` retries while an antivirus scanner holds a new file on Windows. `Install` removes only a destination that is not a complete install: one that a writer outside the lock publishes during the download is kept. `Options.Prepare` shapes the staging directory after the extraction and before `Complete` judges it, for an archive that nests its content under a directory of its own; its error refuses the install. `WithoutCredentials` removes the userinfo of a pin's URL and returns a client that sends it as Basic authentication to that URL's scheme and host only, so no log line and no error names the credentials. `ReadAdvertisedIntegrity` reads the SHA-256 a registry advertises for an archive it serves, and `NormalizeIntegrity` turns it into plain lowercase hex |
| `putnamihome` | `Resolve`, `ToolchainRoot`, `IsToolchainInstall`, `UserHomeEnv`, `Env`, `DirName`, `ToolchainsDirName` | The Putnami home of an extension job, as the CLI resolves it for the `putnami-home` candidates of a runtime toolchain: `PUTNAMI_HOME`, else `.putnami` under the user's home directory, else `.putnami` under the workspace root. `ToolchainRoot` is the directory that holds the releases of one toolchain, `toolchains/<name>`, one `<name>-<version>` directory each, shared by every workspace of the machine. `IsToolchainInstall` recognizes such a release directory by its place and name. The Go and TypeScript extensions install Go and Bun there |
| `treearchive` | `CopyTree`, `WriteTarGz`, `LinkError` | Stages a directory tree and packs it into a `.tar.gz` without starting a process, so packaging works on Windows, where neither `cp` nor a POSIX `tar` is guaranteed. A tree holds directories and regular files only: a symbolic link or a Windows junction fails with a `*LinkError` naming it and its target, and any other special file fails by name. The archive depends only on the tree's paths, contents and execute bits: `filepath.WalkDir` order, relative slash names, epoch timestamps, uid and gid 0 without names, 0755 for a directory or an executable file and 0644 otherwise, and a gzip header without name or time. The CLI's `putnami dev template package` and the scaffold extension's template packager share it |
| `cachepolicy` | `Collect`, `SelectVictims`, `Entry`, `Options`, `Result`, `RemoveTree`, `TreeSize`, `PruneEmptyDirs`, `WriteSummary`, `Acquire` | Neutral eviction/GC helpers for an extension-owned machine cache: budget arithmetic, grace windows, the cross-process collector lock, and the freed-bytes summary the reserved `cache-clean` / `cache-gc` commands report through |
| `infraagg` | `Job`, `Aggregate`, `Options`, `Result`, `Outcome`, `IsWorkload`, `RemoveAggregatedManifest` | Neutral half of extension-owned infra aggregation: dependency-closure merge, overrides, authored-over-default runtime precedence, and the atomic write of `<workload>/.gen/requirements.json`. A language extension registers `Job(...)` as its `build~infra` task and supplies only its own runtime compatibility |
| `docslinks` | `Job`, `Param`, `Code`, `CheckProject`, `Check`, `WorkspaceDocuments`, `ProjectDocuments`, `Emit`, `Anchors`, `Slug`, `Finding` | The documentation link check: every relative link and anchor in a project's README.md files and doc/ trees must resolve, with the case on disk and GitHub's anchor rule. A language extension registers `Job()` as its `lint-docs` task; the SDD extension's `validate-workspace` runs `Check` over what `WorkspaceDocuments` returns, every document of the workspace in one walk. See [`docslinks/README.md`](docslinks/README.md) |
| `genresult` | `Relativize`, `RelativizeList`, `RelativizePath`, `Resolve`, `ResolvePath` | The one rule both language runtimes apply to `.gen/generate-result.json`: every path it serializes is project-relative and slash-separated, so the manifest — a cached artifact restored into other worktrees, machines and CI runners — never depends on where the checkout lives. Producers keep absolute paths in memory and relativize at the boundary; consumers resolve against the project root, and a value that is already absolute (a manifest predating this rule) resolves to itself |
| `dbtestenv` | `UpJob`, `DownJob`, `Up`, `Down`, `ResolveMode`, `ClosureDatabases`, `SynthesizeBinding`, `TestPolicy`, `BindingFrom`, `ReadLease`, `Lease`, `Provider`, `SelectProvider`, `Provisioner`, `NewProvisioner`, `DockerAvailable`, `ProvidedServer`, `NewProvidedServer` | Extension-owned database test environments: the `auto`/`require`/`skip` policy, the dependency-closure datasource merge, and the invocation-scoped `sensitive` binding artifact a test task reads through `BindingFrom`. Two providers sit behind the same task — a digest-keyed Postgres container this run owns (reuse, stale reaping, `--infra-down` teardown), and a server the pipeline started and named in `PUTNAMI_TEST_PG_URL`, which is what gives a CI run per-project environments instead of one injected workspace union. A language extension registers `UpJob()`/`DownJob()` as a `finalizes` producer/finalizer pair on its `test` pipeline; see [`dbtestenv/README.md`](dbtestenv/README.md) |
| `proctree` | `New`, `Tree`, `Stop`, `Relay`, `StartDetached`, `TerminateGroup`, `KillGroup`, `GroupAlive`, `ProcessAlive` | Starts a child as the root of its own process tree and stops the tree as a whole, the same way on every OS the CLI and the extensions ship for: a process group (`Setpgid`, SIGTERM then SIGKILL) on Unix; on Windows a console process group (CTRL_BREAK_EVENT) inside a Job Object with `KILL_ON_JOB_CLOSE` (`TerminateJobObject`), which the child joins before its first instruction. Windows ends a job's processes asynchronously, so `Tree.Close` there returns once every process it ended has exited, within five seconds. `Stop` bounds a stop request by a grace period and kills at once when the request could not be sent. `StartDetached` starts a process that outlives every tree its caller runs in |
| `procguard` | `DenyInspection` | Keeps a process that holds a credential out of reach of the other processes its user runs. On Linux it marks the process non-dumpable (`prctl(PR_SET_DUMPABLE, 0)`): a process of the same user without `CAP_SYS_PTRACE` can no longer read its `/proc/<pid>/environ` or `/proc/<pid>/mem`, list its descriptors or attach with `ptrace`. The flag resets when a process executes a program, so a child is inspectable again: pass it no credential. Elsewhere it does nothing: macOS lets a process of the same user read another one's arguments and environment whatever the target does, so keep a credential out of both on every OS |

## Support and contract

`putnami-extension-sdk` is a public, documented, maintained package classified
`stable` in the workspace [support catalog](../../putnami.support.json). The
[extension-authoring specification](specs/extension-authoring.json) states the
observable promise, and two accepted decisions explain the durable choices
behind it:

- [the SDK owns the neutral half of every shared lifecycle](doc/adr/0001-the-sdk-owns-the-neutral-half-of-a-lifecycle.md);
- [a provisioned secret lives in exactly one artifact](doc/adr/0002-a-secret-lives-in-one-artifact.md).

The contract covers wire conformance of the emitter and its log forwarding,
build-time manifest validation with a non-aliasing builder, subprocess control,
the host-identity variable list, checkout-independent generation paths, the
machine-cache eviction policy, credential confinement and test-environment mode
selection, neutral infra aggregation, and the single writer of the
package→publish metadata index. The wire contracts themselves belong to
[`protocols/extension`](../../protocols/extension), [`protocols/job`](../../protocols/job)
and [`protocols/runtime`](../../protocols/runtime); this module is the Go
authoring library over them, and there is no authoring SDK for other languages.
Before v1.0.0, minor `0.x` releases may contain documented breaking changes —
see [RELEASE.md](../../RELEASE.md).

## MCP tool bridge

Extensions can contribute namespaced tools to `putnami mcp` through their
`putnami.extension.json` `tools` map. The CLI keeps the MCP JSON-RPC connection
and starts the declared extension command only when a tool is called. The
command receives one `extension.ToolCallRequest` JSON value on stdin and must
write one `extension.ToolCallResult` to stdout.

For Go extensions, route a dedicated binary subcommand to `mcp.Serve`:

```go
if len(os.Args) > 1 && os.Args[1] == "mcp-tool" {
    _ = mcp.Serve(context.Background(), os.Stdin, os.Stdout, map[string]mcp.Handler{
        "putnami.search": func(ctx context.Context, req extension.ToolCallRequest) (any, error) {
            return search(ctx, req.Arguments)
        },
    })
    return
}
```

The extension retains outbound authentication and network calls; the core CLI
only forwards the request and result. If agent provenance was enabled for the
MCP server, `req.Agent` is non-nil and is the structured source of truth for
the bounded harness/model identity. Apply its documented request headers with
`req.Agent.ApplyHTTPHeaders(outboundRequest)`.

Return an error for a normal tool error; the helper encodes it as
`isError: true`. A handler may return a value AND an error, which is the failure
that still produced a report — a validation that ran and found the document
invalid. `Serve` then writes two content blocks, the report first and the
message second, exactly as the CLI's own tools do; a handler that failed before
it had anything to say returns a nil value and gets the message alone.

### Answering about the workspace

A tool that reports on the workspace declares `"workspaceSelection": true` in
its manifest entry. The CLI then resolves the view and puts it on the request:

```go
"sdd.list_specs": func(ctx context.Context, req extension.ToolCallRequest) (any, error) {
    if req.Selection == nil {
        return nil, errors.New("this CLI does not publish `selection` on a tool request")
    }
    // req.WorkspaceProjects is the complete resolved membership; req.Selection
    // is what this call's projects/impacted/baseline arguments resolved to.
    return catalog(req.WorkspaceProjects, req.Selection)
}
```

Do not re-derive either. Resolving `impacted` means diffing a baseline, mapping
changed paths to owners and walking the dependent graph; an extension that did
it would be a second definition of what the workspace contains. Refuse a request
that carries no `selection` rather than defaulting to the whole workspace: the
default answer is well-formed, plausible, and not the one that was asked for.

## Private OCI destination policy

When an invocation supplies a numeric-loopback `PUTNAMI_REGISTRY_OCI_URL`,
`dockerpublish` accepts only the managed OCI destination. External targets such
as `ghcr.io` are refused before network access; there is no direct-registry
fallback. The override applies to the invocation, so publish external images in
a separate invocation without this private override. This preserves the selected
transport and credential boundary for mixed image selections.

The policy covers every image publication, not only `image` projects: a Go or
TypeScript workload publishing `--docker` to the managed registry takes the same
loopback route. Under the broker the publisher asks the credential seam about
the broker host (the cloud answers it with the run's capability) and writes the
content by digest. The workload Docker path applies the version as its only
tag. A first-class `image` project writes only the digest; its planned member
version creates no tag. A daemon-built candidate without an OCI layout is
refused rather than pushed around the broker.

The same rule applies per registry kind through `privatebroker`: the Go module
publisher honours `PUTNAMI_REGISTRY_GOMOD_URL` (`/go`) and the npm publisher
honours `PUTNAMI_REGISTRY_NPM_URL` (`/npm`), each sending every registry request
to the broker and asking the credential seam about the broker host.
