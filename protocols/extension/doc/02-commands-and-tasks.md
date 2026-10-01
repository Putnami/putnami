# Commands and Tasks

Extensions expose functionality through **commands** (the public API) and implement it with **tasks** (atomic executable units).

## Commands

A command is a user-facing operation invoked via `putnami <command>`:

```json
{
  "commands": {
    "build": {
      "description": "Build the project",
      "activationFiles": ["tsconfig.json"],
      "flags": {
        "target": {
          "type": "string",
          "default": "production",
          "choices": ["development", "production"]
        }
      },
      "run": [
        { "id": "transpile", "task": "build-transpile" }
      ]
    }
  }
}
```

### Command Fields

| Field | Required | Description |
|-------|----------|-------------|
| `run` | **yes** | Pipeline step DAG |
| `description` | no | Human-readable description |
| `activationFiles` | no | File patterns that must exist for the command to activate |
| `flags` | no | CLI flags the command accepts |
| `defaults` | no | Default parameter values |
| `dependsOn` | no | Command prerequisites. Bare names are same-project prerequisites; `!command` is a session barrier across the current project selection |
| `sessionPrerequisites` | no | Dependent-owned same-session commands with optional condition, selection, project policy, gates, and local parameter bindings |
| `priority` | no | Higher priority sorts earlier when multiple extensions provide the same command |
| `quiet` | no | Suppress from job recap |
| `visibility` | no | `public` (default) or `internal` |
| `outputs` | no | Named outputs projected from step results |
| `channel` | no | Execution channel for concurrency control |

### Activation

Commands only activate for projects that match `activationFiles` patterns. If no patterns are specified, the command activates for all projects that use the extension.

### Command Dependencies

Command-level `dependsOn` auto-plans prerequisite commands before the command
runs:

```json
{
  "commands": {
    "publish": {
      "dependsOn": ["!lint", "!test", "!build", "!package"],
      "run": [{ "id": "push", "task": "publish-push" }]
    },
    "deploy": {
      "dependsOn": ["!publish"],
      "run": [{ "id": "apply", "task": "deploy-apply" }]
    }
  }
}
```

Bare command names are same-project prerequisites: `dependsOn: ["build"]`
plans `build` for each project that has the dependent command, then root steps
of the dependent command wait for that project's `build` leaf steps.

Names prefixed with `!` are session barriers. `!build` plans `build` across the
current project selection, then every root step of the dependent command waits
for every `build` leaf job in the final plan. These are functional dependencies:
if a barrier job fails, downstream release/deploy jobs are skipped.

Pipeline-step references prefixed with `^`, `/`, or `*` are invalid at command
level; put them on the relevant entry in `run` instead.

### Conditional Session Prerequisites

Use `sessionPrerequisites` when the dependent command owns more policy than a
plain `!command` barrier can express. This field requires `cliContract: 4`;
contract-3 readers cannot represent its execution gates and must reject it:

```json
{
  "commands": {
    "deploy": {
      "sessionPrerequisites": [{
        "command": "publish",
        "if": "!params.preview",
        "projectsFromParam": "apps",
        "projectIf": "params.deploy.enabled != false && params.deployPrerequisite != false",
        "dependsOn": ["lint", "test", "build", "validate", "validate-workspace"],
        "params": {
          "docker": { "value": true },
          "publishConfig": { "fromProjectParam": "deploy.publishConfig" }
        }
      }],
      "run": [{ "id": "apply", "task": "deploy-apply" }]
    }
  }
}
```

`if` reads the owning command's resolved parameters. `projectsFromParam` names
a string parameter containing a workspace target expression; an absent or
empty value keeps the current selection. `projectIf` is then evaluated for
each candidate using that project's resolved owning-command parameters.

Every root from every extension contributing the prerequisite command waits
for the session leaves of every command in `dependsOn`. The dependent command's
roots wait for all prerequisite leaves, so an ordinary failure blocks the
dependent side effect. `params` applies only to jobs introduced by this
relation: `value` supplies a constant, while `fromProjectParam` copies a dotted
path from the owning parameters resolved for that project. A separately invoked
prerequisite command does not inherit these overrides. A missing dotted path is
an error. One command identity is never first-wins: an explicit invocation, a
`dependsOn` expansion, or another relation with incompatible local parameters
fails planning instead of silently reusing a differently configured job.

For a workspace-once contributor, constant bindings are shared and selections
from compatible relations are merged deterministically. A `fromProjectParam`
binding is accepted only with one selected project; choosing a project-derived
value across multiple projects is ambiguous and fails closed. Relation-local
bindings also cannot overwrite a literal `run[].with` value.

### Shared Root Commands

Multiple extensions may declare the same command name. Putnami composes all active contributors for the selected project into one execution plan, so extension authors should attach common workflows to canonical root commands such as `publish` and `deploy` instead of requiring users to remember extension-specific commands.

Step IDs only need to be unique inside one extension command. When two active contributors use the same step ID, the CLI namespaces scheduler keys by extension identity internally while keeping displayed names as `{command}~{step}`.

Flags with the same name across active contributors must have identical definitions. Identical shared flags are accepted; conflicting definitions fail validation before execution. Unique extension-specific flags remain available in `params`.

Command groups can act as aliases by pointing a subcommand at the same flat command:

```json
{
  "commandGroups": {
    "cloud": {
      "subcommands": {
        "deploy": { "command": "deploy" }
      }
    }
  }
}
```

This keeps `putnami cloud deploy` as a compatibility surface while `putnami deploy` remains the canonical composed command.

A command group can declare shared `flags` once, inherited by every subcommand instead of redeclared on each one:

```json
{
  "commandGroups": {
    "cloud": {
      "flags": {
        "env": { "type": "string", "default": "prod" }
      },
      "subcommands": {
        "status": { "command": "cloud-status" },
        "deploy": {
          "command": "cloud-deploy",
          "flags": { "env": { "type": "string", "default": "staging" } }
        }
      }
    }
  }
}
```

A subcommand's effective flags merge three layers in increasing precedence — the flat command target's flags, the group's shared flags, then the subcommand's own flags — so the more specific declaration wins (here `deploy` overrides the inherited `env` default). That merged set is what `putnami cloud <subcommand> --help` renders, what shell completion offers, and what supplies flag defaults during param resolution. Names that collide with a Putnami global flag (such as `--output` or `--dry-run`) are consumed by the CLI before the extension sees them, so declare only extension-owned names here.

Command groups can include nested subcommands for extension-owned verbs. The
top-level subcommand keeps the flat `command` binding used by the dispatcher;
nested subcommands inherit that target and contribute static completion
metadata:

```json
{
  "commandGroups": {
    "cloud": {
      "subcommands": {
        "config": {
          "command": "cloud-config",
          "subcommands": {
            "list": { "positionals": [{ "name": "project" }] },
            "show": {
              "positionals": [{ "name": "project" }],
              "flags": { "with-secrets": { "type": "boolean" } }
            }
          }
        }
      }
    }
  }
}
```

The CLI uses this tree for shell completion only; extension binaries still
receive and parse the nested verb arguments themselves.

A command group can name a `default` subcommand. `putnami <group>` with no
subcommand word runs it, with any flags that follow, so `putnami audit
--depth=2` runs `putnami audit run --depth=2`. `putnami <group> --help` and
`putnami <group> help` still print the group help. The value must name one of
the group's own subcommands; the strict parser rejects any other value with
`unresolved-default-subcommand`. A group without `default` prints its help
when no subcommand is named.

A subcommand declares whether it needs a workspace with `workspace`: `required`
(the meaning of an absent value) or `optional`. An `optional` subcommand must
also be `interactive: true`; the strict parser rejects any other combination
with `invalid-workspace-requirement`, and an unknown value with
`invalid-enum`:

```json
{
  "commandGroups": {
    "audit": {
      "default": "run",
      "subcommands": {
        "run": { "command": "audit-run", "interactive": true, "workspace": "optional" },
        "report": { "command": "audit-report" }
      }
    }
  }
}
```

Outside any workspace, the CLI loads only the extensions pinned in the user
scope (`putnami extensions install --user <name>`) and runs only their
`optional` subcommands. Every other command, including `putnami audit report`
here, fails with `putnami: no workspace found (looking for
putnami.workspace.json)` and exit code 1. The subcommand's process starts in
the caller's directory, and its job context carries a `userScope` member that
names that directory; see the job context protocol
(`protocols/job/README.md`). Inside a workspace, `workspace` changes nothing:
the workspace's own pin of the extension runs.

The CLI reads `workspace`, `interactive` and `default` on the group and the
top-level subcommand it dispatches. A nested subcommand is validated with the
same rules and carries no dispatch meaning.

Neither member moves the manifest's `cliContract`. A CLI that predates them
ignores both: it prints the group help where a newer CLI runs the default, and
it never runs a subcommand outside a workspace. Both readings fail safe, so no
contract increment is needed (see
[ADR 0008](adr/0008-workspace-requirement-and-group-default-need-no-contract.md)).

## Tasks

A task is an atomic executable unit — a subprocess with defined inputs, outputs, and caching:

```json
{
  "tasks": {
    "build-transpile": {
      "kind": "command",
      "command": "bun",
      "args": ["run", "{extensionRoot}/bin/build"],
      "cwd": "{projectRoot}",
      "timeoutMs": 300000,
      "inputs": {
        "source": { "from": "project", "files": ["src/**/*.ts"] },
        "config": { "from": "project", "files": ["tsconfig.json"] },
        "lock": { "from": "workspace", "files": ["bun.lock"] }
      },
      "outputs": {
        "dist": { "kind": "directory", "path": "dist/" }
      },
      "cache": {
        "enabled": true,
        "deterministic": true
      }
    }
  }
}
```

### Task Fields

| Field | Required | Description |
|-------|----------|-------------|
| `kind` | **yes** | Execution kind (currently only `command`) |
| `command` | **yes** | Command to execute |
| `args` | no | Command arguments |
| `cwd` | no | Working directory (supports `{projectRoot}`, `{extensionRoot}`) |
| `env` | no | Environment variables |
| `timeoutMs` | no | Execution timeout (default: 300000ms) |
| `inputs` | no | Input port declarations |
| `outputs` | no | Output port declarations |
| `cache` | no | Cache policy |
| `batchable` | no | Opportunistic same-key batching policy for cacheable per-project tasks |
| `declares` | no | Task contract v3: exact outputs, effects, and source mutation |
| `inputSchemaRef` | no | Reference to a contract schema for input validation |
| `outputSchemaRef` | no | Reference to a contract schema for output validation |

### The Task Contract (`declares`)

Without `declares`, a task's filesystem footprint is **inferred**: the runner
captures a directory and subtracts what was in it beforehand to guess which
files the task produced. Inference is why identical sources can store different
bytes under the same cache key, and why every new artifact shape needs another
special case.

`declares` replaces the guess with a statement. It is **additive** — a task
without it keeps the inferred behavior exactly — so a manifest migrates one task
at a time:

```json
{
  "tasks": {
    "build-generate": {
      "kind": "command",
      "command": "bun",
      "args": ["run", "{extensionRoot}/bin/generate"],
      "writes": ["gen"],
      "declares": {
        "outputs": {
          "gen": { "kind": "directory", "root": "project", "path": ".gen" },
          "coverage": {
            "kind": "file",
            "root": "command-output",
            "path": "lcov.info",
            "optionalEmpty": true
          }
        },
        "effects": ["toolchain-cache"]
      }
    }
  }
}
```

`declares.outputs` are **filesystem** artifacts, keyed by output id. They are
distinct from the sibling `outputs` map, which declares data ports for pipeline
wiring — though a declared output may take its path from such a port (see
`pathFrom`).

#### Declared Output Fields

| Field | Required | Description |
|-------|----------|-------------|
| `kind` | **yes** | `file` (one regular file), `directory` (the whole subtree, recursively), or `runtime-file` (a file that only means anything inside the invocation that wrote it) |
| `root` | no | `project` (default), `workspace`, or `command-output`. Left unset for an invocation-scoped output |
| `scope` | no | `durable` (default) or `invocation` — see [lifecycles](07-lifecycles.md) |
| `sensitive` | no | The output's path and bytes never reach events, results, sessions, telemetry, or cache traffic. Requires `scope: "invocation"` and a literal `path` |
| `path` | one of | Literal root-relative path |
| `pathFrom` | one of | Name of a port in the task's `outputs` map whose runtime value is the path |
| `optionalEmpty` | no | The output may legitimately be absent, or empty, after a successful run |
| `excludes` | no | Root-relative subpaths this **directory** output cedes: neither captured nor restored with it, and ownable by another task |
| `preserves` | no | Output-relative subpaths of this **directory** output the workspace owns (a `putnami.json`, a `go.mod`): never captured, never replaced or deleted by a restore, never judged for drift. Allowed on a `pathFrom` output |
| `drift` | no | `warn` or `fail`: the engine compares the bytes the task produces (or a cache hit restores) with the bytes present at the path immediately before, and reports a difference as `generated-output-drift` |
| `description` | no | Human-readable description |

Exactly one of `path` and `pathFrom` is set. `pathFrom` exists for outputs whose
location is chosen per project — a configured generated-client directory —
so they stay declarable instead of hard-coded in the runner.

A `path` is canonical and concrete: slash-separated, cleaned, relative to its
root, never the root itself, and never a glob or a template variable. The root
is what makes the same declaration resolve per project:

| Root | Resolves to | Ownership |
|------|-------------|-----------|
| `project` | The project directory | Per project |
| `workspace` | The workspace root | Workspace-global: two tasks that declare the same path collide even across projects |
| `command-output` | `.putnami/out/<project>/<command>` | Per command: that directory is **shared by every step of one command**, which is exactly why steps must name their own files |

An **invocation-scoped** output (`"scope": "invocation"`) resolves against the
invocation's private scratch instead, which is not a staging root and is never
captured — so it leaves `root` unset. See
[lifecycles](07-lifecycles.md) for the sensitive-artifact rules and the
`runOn: "finally"` finalizer that tears one down.

`optionalEmpty` is the difference between "this run produced nothing, which is
fine" (coverage off, no client generated) and "the task and its declaration
disagree". Without the flag, a missing declared output is a defect.

#### Ceding a Subpath (`excludes`)

A task that declares a subtree **whole** and runs **first** makes everything a
later task writes into that tree uncapturable: the first task's snapshot
predates those bytes, and its restore replaces the tree with that snapshot, so
a run that serves both tasks from cache loses them. `excludes` resolves that
without splitting the tree — the owner cedes exactly the subpath the later task
produces, and the later task declares it:

```json
{
  "tasks": {
    "build-generate": {
      "declares": {
        "outputs": {
          "gen": {
            "kind": "directory",
            "root": "project",
            "path": ".gen",
            "excludes": [".gen/migration-bundle"]
          }
        }
      }
    },
    "build-describe": {
      "declares": {
        "outputs": {
          "migrationBundle": {
            "kind": "directory",
            "root": "project",
            "path": ".gen/migration-bundle",
            "optionalEmpty": true
          }
        }
      }
    }
  }
}
```

Each entry is a literal cleaned path, **strictly inside** `path` and never equal
to it, and duplicates are a validation error. Only a `directory` output with a
literal `path` may cede: a file has no subpath, and a `pathFrom` value is
unknown until the task runs, so "inside `path`" would not be decidable from the
manifest.

Two consequences are worth stating:

- The ceded region has **one** owner, not none: a third task claiming it still
  collides with the task that took it. A ceded subpath that nobody claims is
  captured by nobody — which is what the declaration says.
- The ceding task's restore leaves the ceded subtree exactly as it finds it,
  the way its execution would: the directory swap carries whatever sits there
  over into the restored tree (ADR 0003, 2026-09-14 amendment). Restore order
  still matters for a different reason: the owning task must run **after** the
  ceding task in every command that schedules both, because its restore (or
  execution) is what makes the ceded path reflect the current sources.

#### Leaving Workspace Files Alone (`preserves`)

A generated directory can be a workspace project of its own: an author commits
a `putnami.json` beside the generated client, and a Go client module keeps its
`go.mod` and `go.sum`. Those bytes belong to the workspace, not to the task.
The task may scaffold one when it is absent, but a capture that records it and
a restore that swaps the recorded tree in would delete or overwrite the
workspace's copy on every cache hit. `preserves` names them:

```json
"declares": {
  "outputs": {
    "client": {
      "kind": "directory",
      "pathFrom": "clientOutput",
      "optionalEmpty": true,
      "preserves": ["go.mod", "go.sum", "putnami.json"],
      "drift": "fail"
    }
  }
}
```

Each entry is relative to the **output**, not to the root, so it is decidable
on a `pathFrom` output too, and it may name a directory: the TypeScript client
tasks preserve `node_modules`, the install a consumer materializes inside a
client that is a package of its own. A preserved path is not captured, a restore leaves
whatever the destination holds there (or its absence) exactly as it is, and a
`drift` comparison judges it on neither side. Nothing owns a preserved path,
so it takes no part in ownership. Only a durable `directory` output may
preserve anything. See
[ADR 0005](adr/0005-a-declared-directory-output-may-preserve-workspace-files.md).

#### Policing Committed Generated Bytes (`drift`)

A generator that writes into the worktree — a client, a schema sidecar, a
migration, a rendered document — produces bytes the repository **commits**.
Whether the committed bytes still equal what the current inputs generate is a
question only the writer can answer, and only at the moment it writes: a
verifier that runs later in the session sees a tree the generator, or a cache
restore, has already rewritten. The policy therefore rides on the declared
output:

```json
{
  "tasks": {
    "clientgen-go": {
      "outputs": { "goClientOutput": {} },
      "declares": {
        "outputs": {
          "client": {
            "kind": "directory",
            "pathFrom": "goClientOutput",
            "optionalEmpty": true,
            "drift": "fail"
          }
        }
      }
    }
  }
}
```

| `drift` | Meaning |
|---------|---------|
| absent | No comparison |
| `warn` | The engine emits `generated-output-drift` at warning severity, naming the added, removed and changed paths; the task keeps its status |
| `fail` | The same diagnostic at error severity, and the task fails with that code. Its outputs are still written or restored, so the worktree holds the regenerated bytes and the operator commits them |

The engine takes the reference **immediately before the task writes**: for an
executed task, before its process starts; for a cache hit, before the recorded
tree is swapped in. A warm run and a cold run therefore reach the same
verdict. For a literal `path` the reference is that path; for a `pathFrom`
output the path is unknown before the run, so the reference is a digest of the
project tree (dot-directories, `node_modules` and `vendor` excluded), which is
why a `pathFrom` output may carry the policy under the `project` root only.
Neither the reference nor the verdict is a cache-key input, and a drift failure
is never recorded as a replayable failure: it is a fact about the worktree the
task ran in, not about the task's inputs. The `drift` field itself is part of
the task contract, so declaring it moves the task's cache key once.

The policy requires a durable, non-sensitive `file` or `directory` output under
the `project` or `workspace` root. The `command-output` root is never
committed, and a runtime file or an invocation-scoped output is never in the
tree after the run, so none of them has a committed state to drift from. See
[ADR 0004](adr/0004-a-declared-output-may-police-its-own-drift.md).

#### Effects

`declares.effects` names what a task does **beyond** writing its declared
outputs. The vocabulary is closed:

| Effect | Meaning |
|--------|---------|
| `workspace-files` | Rewrites shared workspace-root files that are not its declared outputs (a lockfile, workspace config) |
| `toolchain-cache` | Writes a machine-global toolchain cache outside the workspace (Go module cache, bundler cache) |
| `network` | Performs network I/O that contributes to the result |
| `registry` | Publishes to an external package registry |
| `cloud` | Mutates remote cloud state |
| `process` | Starts a long-lived process or binds a port |

`registry`, `cloud`, and `process` are **external**: replaying a stored result
would skip them entirely, so a task declaring one must set `"cache": false`.
`network` and `toolchain-cache` are not — a cache hit legitimately skips them,
and the declaration documents the dependency.

#### Source Mutation

`declares.mutatesSources` states that a task rewrites the sources it reads — a
formatter, a `lint --fix`, a codemod. It makes the *possibility* of mutation
known before the task runs; the scheduler can then compare the keyed
project-source digest before and after execution and mark only results that
actually changed those sources as non-restorable. A task that sets it must also
declare the project-scoped `sources` write resource, which is what serializes it
against conflicting jobs:

```json
{
  "tasks": {
    "lint-fix": {
      "kind": "command",
      "command": "biome",
      "args": ["check", "--write"],
      "writes": ["sources"],
      "cache": { "enabled": true, "noOutput": true },
      "declares": { "mutatesSources": true }
    }
  }
}
```

#### One Owner Per Output

No two tasks may declare the same output path, and nesting counts as the same
path: a file declared inside a declared subtree overlaps that subtree, because
capturing or restoring the subtree also captures or restores the file.
Containment is tested at a path **segment** boundary, so `dist` and `dist2` are
independent. Ownership is what makes declared capture safe to run concurrently —
if two tasks could claim `.gen`, restoring one would silently undo the other.
Root and path comparison is case-insensitive on every host, so `Dist/API` and
`dist/api` cannot become separate owners on Linux and aliases on Windows or a
default macOS filesystem.

### Batchable Tasks

A cacheable per-project task can opt into opportunistic batching:

```json
{
  "batchable": {
    "tool": "biome",
    "maxProjects": 8,
    "maxWorkers": 4,
    "configFiles": [
      "{projectRoot}/biome.json",
      "{workspaceRoot}/biome.json",
      "{extensionRoot}/config/biome.json"
    ]
  }
}
```

`tool` is the stable logical tool identity. `configFiles` is an ordered list;
the first existing regular file supplies a source-qualified configuration
digest. `maxProjects` optionally caps projects in one shared invocation (minimum
2). `maxWorkers` optionally limits batching to scheduler runs at or below that
resolved worker count (minimum 1), which lets extensions retain cross-project
parallelism when a tool's batching is only economic under constrained
concurrency. Omitting either field leaves that dimension unbounded.

`maxProjectsParam` optionally names a parameter that lets a workspace or a
project choose the cap through its options, for example
`options["@putnami/go:test"]["batch-max-projects"]`. The scheduler reads the
parameter from the job's resolved parameters, under its exact spelling first
and then its camelCase alias:

- Absent: `maxProjects` applies unchanged, and the batch key is the key the
  policy produced before it named the parameter.
- A positive integer: it replaces `maxProjects`, and the batch key folds the
  resolved value, so jobs with different caps never share a batch.
- `1`: every project runs alone.
- Any other value: planning fails before any task runs.

The parameter only groups jobs, so it must never key a task. Manifest
validation refuses a task that declares it as an input or a cache key param, a
command that runs the task and declares it as a flag, and a step that binds it,
in either spelling.

Omitting `configFiles` opts into the **extension-loop** form: the shared
invocation runs each selected project's own tool pass (its own config discovery)
in a loop, so there is no shared configuration to compatibility-gate on. This is
how the TypeScript build producers (`generate`, `transpile`, `types`, `compile`)
batch. Do not list a per-project file that always exists (e.g.
`{projectRoot}/tsconfig.json`) as the sole candidate: it resolves to a distinct
path per project, giving each a distinct digest and silently disabling batching.

The scheduler groups only jobs that are already ready and share the tool,
resolved configuration file and content, toolchain version, task identity,
effective parameters, synthesized execution environment, and policy bounds. It
never waits for more jobs to join a batch.

`configFiles` should list only **shared** candidates (workspace- or
extension-scoped). Listing a per-project file that always exists at a distinct
path (e.g. `{projectRoot}/package.json` or `{projectRoot}/pyproject.toml`)
gives every project a distinct digest and its own batch key, so batching
silently never happens. Prefer the shared workspace lockfile/config that all
grouped projects resolve identically.

Batching amortizes only the extension-process overhead, not per-project tool
startup. A **single-tool** task (e.g. `lint`) runs its tool once over every
grouped path. An **extension-loop** task (e.g. `test-run`) runs each selected
project's suite in turn inside the shared process, so it still pays each
project's tool boot but recovers the extension fork/boot cost. Extension-loop
tasks that are heavy should set a conservative `maxWorkers`/`maxProjects`: since
grouping serializes the loop onto one worker, unbounded grouping of heavy suites
can regress wall time when workers are idle.

The shared invocation receives the group through `selectedProjects`, each entry
carrying its own `outputPath` so an extension-loop task writes every project's
captured outputs (coverage, JUnit) to that project's own directory. It must
return one result for every selected project in the final result data:

```json
{
  "batchResults": [
    {
      "projectId": "/packages/api",
      "status": "OK",
      "diagnostics": [],
      "artifacts": [
        {
          "id": "coverage",
          "name": "Coverage Profile",
          "kind": "coverage",
          "path": ".putnami/out/packages/api/test/coverage.out"
        }
      ],
      "summary": { "errors": 0, "warnings": 0, "infos": 0 }
    }
  ]
}
```

Each item may include `data`, artifacts (`id`, `name`, `kind`, and `path`),
diagnostics (`category`, `severity`, `description`, optional `file`, `line`, and
`column`), summary counts, and `metrics` (each `name`, `value`, and `unit`). The
`metrics` array lets a producer whose per-project `data` is otherwise empty (e.g.
a transpile that returns nil data) still replay its counters —
`transpiled-files`, `type-declarations`, `generate-hash`,
`compiled-executables` — into the reconstructed terminal row so it matches a
singleton run. Putnami splits these back into ordinary per-project results. Cache
keys, entries, terminal session rows, hit/miss outcomes, and DAG dependencies
remain per project. Tasks without `batchable` continue through the singleton
execution path.

### Template Variables

Task fields support template variables:

| Variable | Expands to |
|----------|------------|
| `{extensionRoot}` | Absolute path to the extension directory |
| `{projectRoot}` | Absolute path to the project directory |
| `{workspaceRoot}` | Absolute path to the workspace root |
| `{selectedProjects}` | Comma-separated selected project names for workspace-once and batched jobs |
| `{selectedProjectIDs}` | Comma-separated selected project IDs for workspace-once and batched jobs |
| `{selectedProjectPaths}` | Comma-separated selected project paths for workspace-once and batched jobs |
| `{selectedProjectRoots}` | Comma-separated selected project roots for workspace-once and batched jobs |

The [runtime primitive](07-lifecycles.md) declares two further tokens against
the same syntax: `{extensionRuntime}`, the resolved runtime executable a task
invokes instead of a language-specific wrapper; `{runtimeOutput}`, the
destination a `runtime.prepare` writes its build into; and
`{invocationArtifactRoot}`, the non-secret private artifact root delivered only
to the producer, consumers, and finalizer of an explicit `finalizes` relation.
A prepare may use **only** `{extensionRoot}` and `{runtimeOutput}`, because
preparation is workspace-independent.

### Input Ports

Tasks declare their inputs explicitly via `inputs`. Each input has a source:

| Source | Description |
|--------|-------------|
| `project` | Files from the project directory (use `files` for globs) |
| `workspace` | Shared files from the workspace root, such as lockfiles and root compiler config |
| `task` | Output from another task in the pipeline |
| `params` | Command parameters |
| `env` | Environment variables |
| `runtime` | Runtime context (platform, arch, versions) |

A `task` input is the typed producer/consumer edge, and `optional` is what
makes it a requirement or a courtesy:

```json
"inputs": {
  "schema": { "from": "task" },
  "coverage": { "from": "task", "optional": true }
}
```

A `task` port that is **not** `optional` must be bound by the consuming
pipeline step (`with`) to a producing step that survives into the plan.
When it is not, planning fails naming both identities — the consumer job,
its extension and task, the port, and the producer that is missing — rather
than letting the task discover at runtime that the file it needs was never
written. An `optional` port means the consuming task handles absence itself.

`optional` has no meaning on the other sources; a missing `project` file is
simply an input that contributes nothing to the cache key.

### Output Ports

Tasks declare their outputs via `outputs`:

| Kind | Description |
|------|-------------|
| `file` | Single output file |
| `directory` | Output directory |
| *(omit)* | JSON data output |
