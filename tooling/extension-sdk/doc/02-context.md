# Job Context

The `context` package parses the Putnami job execution context — a JSON file passed by the orchestrator via `--putnamiContext`. It provides typed access to workspace, project, extension, job metadata, parameters, and version information.

## Overview

When the orchestrator runs an extension job, it writes a temporary JSON file containing everything the job needs: workspace root paths, project metadata, extension identity, job parameters, and optional version info. The `context` package deserializes this file into a strongly-typed `Context` struct.

Jobs typically don't call `Parse` directly — the [CLI package](./03-cli.md) handles context loading automatically via `Run` and `RunSubcommand`.

## Usage

### Accessing Context Fields

```go
func myJob(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
    // Workspace
    fmt.Println(ctx.WorkspaceRoot)     // "/home/user/my-project"
    fmt.Println(ctx.Workspace.Version) // "1.2.3"

    // Project
    fmt.Println(ctx.Project.Name)     // "@putnami/my-lib"
    fmt.Println(ctx.Project.Path)     // "typescript/framework/my-lib"
    fmt.Println(ctx.Project.FullPath) // "/home/user/my-project/typescript/framework/my-lib"

    // Extension & Job
    fmt.Println(ctx.Extension.Name) // "@putnami/go"
    fmt.Println(ctx.Extension.Root) // "/home/user/my-project/node_modules/@putnami/go"
    fmt.Println(ctx.Job.Name)       // "build"

    // Output paths
    fmt.Println(ctx.OutputPath) // ".putnami/projects/.../build/latest/output"
    fmt.Println(ctx.CacheRoot)  // ".putnami/projects/.../build/latest"

    return "OK", nil, nil
}
```

### Reading Parameters

Parameters are passed from `putnami.extension.json` task definitions and CLI flags. Use `Params.String`, `Params.Bool`, and `Params.Int` with optional fallback keys:

```go
// Simple parameter access
target := ctx.Params.String("target")           // "" if not set
verbose := ctx.Params.Bool("verbose", false)     // false if not set
timeout := ctx.Params.Int("timeout", 30000)      // 30000 if not set

// Fallback keys — tries "coverage-threshold" first, then "coverageThreshold"
threshold := ctx.Params.Int("coverage-threshold", 0, "coverageThreshold")

// Fallback keys for booleans
dryRun := ctx.Params.Bool("dry-run", false, "dryRun")
```

Fallback keys are useful when the same parameter may appear under different names (CLI flag style `"dry-run"` vs JSON style `"dryRun"`).

### Release-set plans

Use `releaseset.FromContext` or `releaseset.ParseParams` to read the orchestrator's
`releaseSetPlan` parameter. An absent plan supports full publication. At the
supported `distribution.ProtocolVersion`, unknown fields in the plan and its
members are ignored so additive orchestrator changes can reach older SDKs.
Unsupported versions are rejected with both the received and supported versions.
The decoder still requires one JSON document within `distribution.MaxJSONBytes`,
and validates the known membership, provenance, dependency and channel fields.

`Plan.Baseline()` is the head unchanged members inherit from: the head of the
first channel the plan advances, or — when that channel is still empty and the
publication named one — the head of `Plan.BaselineChannel`, a channel the plan
READS and never releases. `Plan.BaselineChannelName()` reports which of the two
it was. The baseline channel's head rides in `Heads` beside the advanced ones,
because the publication resolved all of them in one exchange
(`protocols/distribution` ADR 0006).

This rule applies to the versioned job-context plan. The provider's `releaseSet`
project metadata remains a closed declaration, and authored `infra/runtime.json`
remains strict so unsupported policy fields produce diagnostics.

### Version Information

Version is populated when the orchestrator detects git metadata:

```go
if ctx.Version != nil {
    fmt.Println(ctx.Version.SHA)     // "abc123"
    fmt.Println(ctx.Version.Branch)  // "main"
    fmt.Println(ctx.Version.Tag)     // "v1.2.3"
    fmt.Println(ctx.Version.IsDirty) // false
    fmt.Println(ctx.Version.Suffix)  // "pre.1"
}
```

### Publish Channels

Check which publishing channels are configured for the project:

```go
channels := ctx.PublishChannels() // ["npm", "docker"]

if ctx.HasPublishChannel("npm") {
    // publish to npm
}
if ctx.HasPublishChannel("docker") {
    // build and push Docker image
}
```

### Registry endpoints

`Params["registries"]` carries the project's **effective** registry endpoints,
one raw entry per ecosystem: the workspace `registries` section, with each entry
the project overrides replacing the workspace one whole. It is the single source
for every endpoint an extension publishes to, so read your own ecosystem's entry
instead of hard-coding a URL or parsing a native dotfile.

The shape of one entry belongs to the ecosystem profile the owning extension
declares — the workspace document validates only the outer map.

```go
var registries map[string]json.RawMessage
if raw, ok := ctx.Params["registries"]; ok {
    _ = json.Unmarshal(raw, &registries)
}

var npm struct {
    Publish string            `json:"publish"`
    Scopes  map[string]string `json:"scopes"`
}
if entry, ok := registries["npm"]; ok {
    _ = json.Unmarshal(entry, &npm)
}
```

An absent member, or an absent entry for your ecosystem, is not an error: a
workspace may publish with a command-line argument or an environment variable
alone. Fall through rather than failing.

The endpoints reach a task through the context and never through a task's bound
parameters, so they are **not** cache-key material: pointing the workspace at a
mirror does not invalidate a single cached task.

### Provider-owned project metadata

`Project.Metadata` carries what each extension's **workspace probe** derived about
the project, namespaced by the extension that produced it. Read only your own
namespace: the whole map travels on the wire, so scanning every key means
depending on facts another extension owns and is free to change.

```go
var meta struct {
    Main string          `json:"main"`
    Bin  json.RawMessage `json:"bin"`
}
if raw, ok := ctx.Project.Metadata["@putnami/typescript"]; ok {
    _ = json.Unmarshal(raw, &meta)
}
```

The distinction from `Options` is who wrote it: `Options` is **authored** in the
project's `putnami.json`, `Metadata` is **derived** by the owning extension's
probe.

### Resolved project selection

`Selection` is how the invocation chose the projects in scope. Read it to know
what your verdict may claim: a task that reports on the workspace may only say
so when the run was not narrowed.

```go
if ctx.Selection == nil {
    // An orchestrator that resolved nothing. Do NOT assume the run was
    // unscoped — say what you checked, not what you covered.
} else if ctx.Selection.EmptyImpact {
    // `--impacted` resolved cleanly and nothing changed. A success, not a
    // failure to find work.
} else if ctx.Selection.Scoped {
    // Narrowed: report on ctx.Selection.ProjectIDs only.
    fmt.Println(ctx.Selection.Mode)           // "projects" or "impacted"
    fmt.Println(ctx.Selection.Baseline)       // "origin/main" (impacted only)
    fmt.Println(ctx.Selection.BaselineSource) // the resolution tier
    fmt.Println(ctx.Selection.ProjectIDs)     // sorted project ids
}
```

`Mode` is one of `SelectionModeAll`, `SelectionModeProjects` and
`SelectionModeImpacted`. `Scoped` is false exactly at `SelectionModeAll`.
`ProjectIDs` is sorted and free of duplicates, so two runs over one tree agree
byte for byte.

`SelectedProjects` and `Selection` answer different questions. `SelectedProjects`
lists the projects and reaches only the jobs that need cross-project context;
`Selection` states what was asked for and how it resolved, and reaches every job.

Each `SelectedProjects` entry is a `ProjectRef` — the same shape
`Project.DependencyClosure` uses — and it is what you open a file with:

```go
for _, ref := range ctx.SelectedProjects {
    fmt.Println(ref.ID)         // canonical project id, joins to Selection.ProjectIDs
    fmt.Println(ref.Name)       // resolved name
    fmt.Println(ref.SourceName) // the name the project DECLARED, when it declared one
    fmt.Println(ref.Version)    // resolved version: project > scope > workspace
    fmt.Println(ref.Path)       // workspace-relative
    fmt.Println(ref.FullPath)   // absolute
}
```

`SourceName` and `Version` are optional: an orchestrator that predates either
member emits none. Read an empty value as **"nobody said"**, never as an answer
— match a package by name alone rather than against a version you invented.

### The whole workspace membership

`WorkspaceProjects` is every project the orchestrator resolved, in canonical id
order, whatever this run selected. It reaches every job.

```go
for _, ref := range ctx.WorkspaceProjects {
	fmt.Println(ref.ID)           // canonical project id
	fmt.Println(ref.Path)         // workspace-relative
	fmt.Println(ref.Dependencies) // resolved DIRECT dependency ids
	fmt.Println(string(ref.Config)) // raw authored putnami.json, when supplied
}
```

It is not a longer `SelectedProjects`, and substituting one for the other gives
both possible wrong answers. `SelectedProjects` is what the run **acts on**;
this is what the workspace **contains**. Under `--impacted` the selection is a
subset, so a project outside it reads as absent and an edge into it is never
walked — a false violation and a missed one.

Three rules follow, and each is a measured failure rather than advice:

- **See the workspace, claim the selection.** This member decides what a task can
  READ. What it may CLAIM is `Selection`: a task that reports on the workspace
  may say so only when the run was not narrowed.
- **A per-project task may need it too.** A rule can be project-scoped in what it
  reports and workspace-wide in what it resolves against — an identity declared
  in a sibling, say. A project-scoped validation that saw only its own project
  reported a real cross-project reference as dangling.
- **Follow the read with the key.** A task that reads this member must declare
  `from: "workspace"` inputs covering what it opens. A key that stops at the
  project stores a verdict a sibling's file can silently invalidate.

`Dependencies` is read from the containing member: inside `WorkspaceProjects`,
whose producer has resolved the graph, an entry without it declares no
dependency. Anywhere else its absence says nothing, and a verdict about edges
must decline rather than read "I was told nothing" as "there is nothing".

`Config` is the orchestrator's parsed-and-reencoded authored `putnami.json`.
It stays raw because `protocols/workspace` owns that document's shape. Decode
only the workspace-protocol fields your extension needs; absence means an older
or non-Putnami producer supplied no authored config, so fail closed rather than
scanning manifests to reconstruct it.

### Project Bin Field

`Main`, `Bin` and `Exports` are **superseded by `Metadata`**. The orchestrator
stopped producing them in an earlier migration — they were `package.json` fields
the CLI parsed and stamped into every task's context regardless of language. They
remain in the contract as optional members for producers outside this repository.

`GetBinString` still extracts a string-shaped `bin`, wherever the value came
from:

```go
if bin := ctx.Project.GetBinString(); bin != "" {
    fmt.Println("Binary:", bin)
}
```

### File Patterns

The orchestrator may pass file patterns for targeted operations (e.g., lint only changed files):

```go
if len(ctx.FilePatterns) > 0 {
    // Only process matching files
    for _, pattern := range ctx.FilePatterns {
        fmt.Println("Pattern:", pattern)
    }
}
```

## API Reference

### `Parse(path string) (*Context, error)`

Reads and parses a Putnami context JSON file.

**Returns:** The parsed context, or an error if the file cannot be read or parsed.

### `Context`

| Field | Type | Description |
|-------|------|-------------|
| `WorkspaceRoot` | `string` | Absolute path to the workspace root |
| `OutputPath` | `string` | Path for job output files |
| `CacheRoot` | `string` | Path for job cache |
| `Workspace` | `Workspace` | Workspace metadata |
| `Project` | `Project` | Project metadata |
| `Selection` | `*Selection` | The invocation's resolved project selection (nil if the orchestrator resolved none) |
| `SelectedProjects` | `[]ProjectRef` | The projects the run acts on; present for jobs that need cross-project context |
| `WorkspaceProjects` | `[]ProjectRef` | The complete workspace membership with resolved direct dependency edges and raw authored project config |
| `Extension` | `Extension` | Extension identity |
| `Job` | `Job` | Current job identity |
| `Params` | `Params` | Job parameters |
| `FilePatterns` | `[]string` | Optional file patterns for targeted operations |
| `Version` | `*Version` | Git version info (nil if unavailable) |

### `Workspace`

| Field | Type | Description |
|-------|------|-------------|
| `Name` | `string` | Workspace name |
| `Version` | `string` | Workspace version (from `putnami.workspace.json`) |

### `Project`

| Field | Type | Description |
|-------|------|-------------|
| `Name` | `string` | Project name (e.g. `"@putnami/sdk"`) |
| `Path` | `string` | Relative path from workspace root |
| `FullPath` | `string` | Absolute path to the project |
| `Main` | `string` | Main entry point — superseded by `Metadata` |
| `Bin` | `json.RawMessage` | Binary definition (string or object) — superseded by `Metadata` |
| `Exports` | `json.RawMessage` | Package exports map — superseded by `Metadata` |
| `Compile` | `json.RawMessage` | Compilation config |
| `Publish` | `json.RawMessage` | Publish channels array |
| `Options` | `map[string]json.RawMessage` | Extension-specific options, **authored** in `putnami.json` |
| `Metadata` | `map[string]json.RawMessage` | Provider-owned metadata, **derived** by each extension's workspace probe (v2) |

### `Selection`

| Field | Type | Description |
|-------|------|-------------|
| `Mode` | `string` | `"all"`, `"projects"` or `"impacted"` |
| `Scoped` | `bool` | Whether the run was narrowed; false exactly at mode `"all"` |
| `Baseline` | `string` | The ref `--impacted` resolved to (impacted only) |
| `BaselineSource` | `string` | The tier that produced `Baseline`, so a fallback ref can be distrusted |
| `ProjectIDs` | `[]string` | The selected projects' canonical ids, sorted |
| `EmptyImpact` | `bool` | The legitimate `--impacted` no-op: nothing changed, so nothing is in scope |

### `Extension`

| Field | Type | Description |
|-------|------|-------------|
| `Name` | `string` | Extension package name |
| `Root` | `string` | Absolute path to the extension |

### `Job`

| Field | Type | Description |
|-------|------|-------------|
| `Name` | `string` | Job name (e.g. `"build"`, `"test"`) |

### `Version`

| Field | Type | Description |
|-------|------|-------------|
| `SHA` | `string` | Git commit SHA |
| `Branch` | `string` | Current branch name |
| `IsDirty` | `bool` | Whether the working tree has uncommitted changes |
| `Tag` | `string` | Git tag (if any) |
| `Suffix` | `string` | Pre-release suffix (e.g. `"pre.1"`) |

### `Params`

`Params` is a `map[string]json.RawMessage` with typed accessor methods.

#### `(Params) String(key string, fallbacks ...string) string`

Returns the string value of a parameter. Tries `key` first, then each fallback key in order. Returns `""` if no key is found.

#### `(Params) Bool(key string, defaultVal bool, fallbacks ...string) bool`

Returns the boolean value of a parameter. Recognizes JSON booleans and string values: `"true"`, `"1"`, `"yes"`, `"on"` (true) and `"false"`, `"0"`, `"no"`, `"off"` (false). Returns `defaultVal` if no key is found.

#### `(Params) Int(key string, defaultVal int, fallbacks ...string) int`

Returns the integer value of a parameter. Parses JSON numbers (truncates to int). Returns `defaultVal` if no key is found or the value is not a number.

### `(*Context) PublishChannels() []string`

Parses `Project.Publish` as a string array. Returns nil if not set or not an array.

### `(*Context) HasPublishChannel(channel string) bool`

Returns true if the project publishes to the given channel.

### `(*Project) GetBinString() string`

Returns the `Bin` field as a string if it's a simple string value. Returns `""` if nil or an object.

## Boundaries

- **Scope**: Parsing the orchestrator-provided context JSON into typed Go structs
- **Out of scope**: Context creation, modification, or serialization — the orchestrator is the sole producer
- **Dependencies**: Standard library only (`encoding/json`, `os`, `fmt`)
