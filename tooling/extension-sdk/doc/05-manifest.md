# Manifest Authoring

The `manifest` package builds an extension's `putnami.extension.json` in Go and refuses to emit one that violates the extension protocol. It is the authoring-time counterpart of `putnami dev extension validate`: both run `extension.FullValidateManifest`, so a manifest that builds here is a manifest the CLI accepts.

## Overview

A manifest is normally hand-written JSON. Under the v2 task contract a mistake in it was cheap — the CLI inferred a task's filesystem footprint anyway. The **v3 task contract** makes the manifest load-bearing: a declared output is captured and restored verbatim, and two tasks claiming one path is a correctness bug that shows up in a *consumer's* workspace at plan time, far from the extension that caused it.

Building the manifest through this package moves that verdict to the extension's own build. Three invariants are enforced:

| Invariant | Meaning | Diagnostic code |
|-----------|---------|-----------------|
| EXACTNESS | A declared path is a concrete file or subtree under a named root — no globs, no template variables, no escapes, never the root itself | `invalid-output-path`, `unresolved-output-port` |
| ONE OWNER PER OUTPUT | No two tasks declare the same path; a file nested in another task's declared subtree *is* the same path, unless the enclosing output **cedes** it (`excludes`) | `output-overlap`, `invalid-output-exclude`, `duplicate-output-exclude` |
| HONEST EFFECTS | A task whose consequences a cache hit cannot reproduce (registry push, cloud mutation, long-lived process) declares the effect and is not cacheable | `effect-conflict`, `invalid-enum`, `duplicate-effect` |
| COMMITTED DRIFT | A drift policy rides only on a durable, non-sensitive file or directory under the project or workspace root; a `pathFrom` output carries it under the project root only | `invalid-output-drift`, `invalid-enum` |

The rules themselves live in `protocols/extension` and are the same ones the planner applies, so the SDK and the CLI cannot drift apart.

## Usage

```go
import (
    "go.putnami.dev/sdk/extension/manifest"
    proto "go.putnami.dev/protocol/extension"
)

builder := manifest.New("acme/tooling", "1.0.0").
    Command("build", proto.CommandDefinition{
        Description: "Build the project",
        Run: []proto.PipelineStep{
            {ID: "generate", Task: "build-generate"},
            {ID: "compile", Task: "build-compile", DependsOn: []string{"generate"}},
        },
    }).
    Task("build-generate", proto.TaskDefinition{
        Kind:    "command",
        Command: "acme-tooling",
        Args:    []string{"generate"},
        Declares: manifest.Declares(
            manifest.Output("gen", manifest.Directory(".gen",
                manifest.Describe("Generated specs, loaders, and schema sidecars."))),
        ),
    }).
    Task("build-compile", proto.TaskDefinition{
        Kind:    "command",
        Command: "acme-tooling",
        Args:    []string{"compile"},
        Declares: manifest.Declares(
            manifest.Output("binary", manifest.File("bin/app", manifest.InCommandOutput())),
            manifest.Output("coverage", manifest.File("lcov.info",
                manifest.InCommandOutput(), manifest.OptionalEmpty())),
            manifest.Effects(proto.EffectToolchainCache),
        ),
    })

if err := builder.WriteFile("putnami.extension.json"); err != nil {
    log.Fatal(err) // every violation, one per line, each naming its field
}
```

### Declaring outputs

Four constructors cover the shapes a declared output can take, so `kind` can never be forgotten and `path`/`pathFrom` can never both be set:

| Constructor | Declares |
|-------------|----------|
| `File(path, opts...)` | a single regular file at a literal path |
| `Directory(path, opts...)` | a whole subtree at a literal path |
| `FileFromPort(port, opts...)` | a file whose path the task reports at runtime |
| `DirectoryFromPort(port, opts...)` | a subtree whose path the task reports at runtime |

Options: `InProject()` (default), `InWorkspace()`, `InCommandOutput()`, `OptionalEmpty()`, `FailOnDrift()`, `WarnOnDrift()`, `Describe(text)`.

`InCommandOutput()` resolves against `.putnami/out/<project>/<command>`, which every step of one command **shares** — which is why two steps of the same command must name different files.

`OptionalEmpty()` marks an output that may legitimately be absent, or an empty directory, after a *successful* run (a coverage file when coverage is off). Without it, a missing declared output means the declaration and the task disagree.

A directory output can also **cede** a subpath to the task that produces it, which is how a subtree written after the enclosing tree was captured survives a cache hit. There is no option for it yet: set `Excludes` on the value `Directory(...)` returns, and `Build` validates it like every other rule (`excludes` must be literal paths strictly inside `path`, on a directory output with a literal path, without duplicates). See [protocols/extension ADR 0003](../../../protocols/extension/doc/adr/0003-a-declared-directory-output-may-cede-one-subpath.md).

`FailOnDrift()` (or `WarnOnDrift()`) is for an output the repository **commits** — a generated client, a schema sidecar. The engine compares the bytes the task produces, or a cache hit restores, with the bytes present at the path immediately before, and reports a difference as the diagnostic `generated-output-drift` naming the added, removed and changed paths; under `FailOnDrift` the task fails with that code while the worktree keeps the regenerated bytes to commit. Never put it on build-time generation nobody commits (`.gen`), and never on a `command-output` or invocation-scoped output — `Build` rejects those (`invalid-output-drift`). See [protocols/extension ADR 0004](../../../protocols/extension/doc/adr/0004-a-declared-output-may-police-its-own-drift.md).

### Declaring effects and source mutation

```go
Task("publish-registry", proto.TaskDefinition{
    Kind:     "command",
    Command:  "acme-tooling",
    Cache:    manifest.NoCache(), // required: a cache hit would skip the push
    Declares: manifest.Declares(manifest.Effects(proto.EffectRegistry, proto.EffectNetwork)),
})

Task("lint-fix", proto.TaskDefinition{
    Kind:     "command",
    Command:  "acme-tooling",
    Writes:   []proto.ResourceRef{manifest.SourcesWrite()}, // what the planner serializes on
    Cache:    &proto.TaskCachePolicy{NoOutput: true},
    Declares: manifest.Declares(manifest.MutatesSources()),
})
```

The effect vocabulary is closed: `proto.EffectWorkspaceFiles`, `proto.EffectToolchainCache`, `proto.EffectNetwork`, `proto.EffectRegistry`, `proto.EffectCloud`, `proto.EffectProcess`. `registry`, `cloud`, and `process` are **external** — declaring one while caching is enabled is rejected.

`MutatesSources()` must be paired with `SourcesWrite()`, and the pairing is checked both ways: the write resource without the flag is also an error, because a v3 task states source mutation explicitly.

### Declaring a command group

A command group turns flat commands into `putnami <group> <subcommand>`. The group is a naming surface: each subcommand `Runs` a flat command of the same manifest, which is where the pipeline, the flags, and the caching live.

```go
builder.
    CommandGroup("features", "Inspect the workspace feature registry",
        GroupFlag("registry", proto.FlagDefinition{
            Type:        "string",
            Description: "Feature registry to read.",
        }),
        Subcommand("list", "List the features of the selected projects.",
            Runs("features-list"),
            Interactive(),
            Positional("query"),
            Example("putnami features list", "Every feature in scope."),
            Example("putnami features list --output=json", "The same set, as one document."),
        ),
        Subcommand("inspect", "Show one feature and its evidence.",
            Runs("features-inspect"),
            Interactive(),
            RequiredPositional("feature-id"),
            SubcommandFlag("evidence", proto.FlagDefinition{Type: "boolean"}),
        ),
    ).
    Command("features-list", proto.CommandDefinition{ /* … */ })
```

| Constructor | Declares |
|-------------|----------|
| `CommandGroup(name, description, opts...)` | the group; a repeated name replaces the earlier group, like `Command` |
| `GroupFlag(name, flag)` | a flag shared by every subcommand of the group |
| `Subcommand(name, description, opts...)` | one subcommand; an empty description falls back to the flat command's |
| `Runs(command)` | the flat command in this manifest the subcommand routes to |
| `Interactive()` | command UX instead of job UX: no renderer, inherited stdio, `PUTNAMI_INTERACTIVE=1` |
| `SubcommandFlag(name, flag)` | a flag owned by this subcommand; it overrides the group's and the command's |
| `Positional(name)` / `RequiredPositional(name)` | positional arguments, in declaration order |
| `Example(command, description)` | one runnable line under `Examples:` in `--help` |

Three rules are enforced by `Build`, not by this package:

- a group must have at least one subcommand;
- a subcommand must resolve to a command the same manifest defines;
- no flag — group or subcommand — may shadow a reserved global (`--output`, `--json`, `--help`, `--version`, `--verbose`, `--debug`, `--quiet`, `--color`, and their short forms). The CLI consumes those before the extension is reached, so declaring one would describe a flag the binary can never receive. An interactive subcommand reads the resolved output mode from `params["output"]` instead — see [Result envelopes](./07-result-envelope.md).

A boolean flag your manifest declares can also be negated by a token the CLI owns. `--no-cache` is a CLI-wide global, so the host consumes it before your binary sees argv; when the invoked subcommand declares the positive flag as a boolean, the host forwards the negation as a param instead — `params["cache"] = false` for `--no-cache`. Read it with the flag's own name and your declared default (`ctx.Params.Bool("cache", true)`); do not read `no-cache`, which is the framework's own spelling of the global and is delivered to every task regardless of what it declared.

Nested subcommands are part of the protocol but have no constructor yet; assign `proto.SubcommandDefinition.Subcommands` directly if you need one.

## Errors

`Build` and `Validate` return a `*ValidationError` carrying every protocol diagnostic, so one run reports all of an author's mistakes:

```go
built, err := builder.Build()
var verr *manifest.ValidationError
if errors.As(err, &verr) {
    for _, code := range verr.Codes() {
        // branch on the rule that was broken
    }
}
```

`WriteFile` validates before writing and leaves no file behind when validation fails, so a rejected manifest can never be picked up by a packager.

## The `cliContract` stamp

A manifest that passes `Build` is stamped `cliContract` at the SDK's current CLI contract, provided it declares a contract surface at all (commands, command groups, or tools). The stamp stays **earned rather than claimed**: it is written only after the same verdict the package-time gate applies, run by the same code, so passing `Build` is exactly what the field asserts. A hook-only manifest — no commands, no groups, no tools — is deliberately left unstamped, because there is nothing for the contract to govern.

Stamping here is what makes an SDK-authored manifest loadable at all: since CLI contract 3 a manifest that declares a contract surface without a matching stamp is rejected by the loader rather than adapted. It costs nothing at package time, where the gate re-validates and re-stamps the staged copy anyway.

## v2 manifests

The task contract is additive. A task with no `Declares` member is a v2 task and is accepted unchanged — `Build` neither attaches a declaration nor changes the verdict for a manifest that has none. Migrate one task at a time.
