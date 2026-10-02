# Extension System

Extensions provide the actual job implementations (build, test, lint, serve, etc.) for projects. The Go CLI reads extension manifests declaratively and executes jobs as subprocesses — it never imports or evaluates extension code directly.

## Extension Discovery

Extensions are discovered from these sources, in order:

1. **Workspace projects** — Any project containing a `putnami.extension.json` file at its root is an extension, unless the workspace pins its name (see below).
2. **Explicit references** — Entries listed in `putnami.workspace.json` → `extensions` are resolved as a workspace path, then in `node_modules/`, then as the installed build the lock file pins.
3. **Set-aside workspace projects** — A project manifest that step 1 set aside for a pin loads here when the pinned build did not load, with a warning to run `putnami install`.
4. **Node modules** — Extensions referenced in the root `package.json` `devDependencies` are looked up in `node_modules/`.

Each name is registered once, by the first source that loads it. Use `putnami extensions list` to see all discovered extensions with their provided jobs.

A hosted run ([`--credential-fd`](03-commands.md#run-credential---credential-fd)) loads only the extensions installed from the artifact store and the workspace's path extensions: a workspace project, or an `extensions` entry that names a path inside the workspace. A path extension serves no provider there, and its jobs and hooks start after the run credential's last handoff.

### Consuming the published build of an extension you develop

A workspace can hold the source of an extension and still run its published build. List the extension by name with a version in `extensions`:

```json
"extensions": { "@acme/tool": "1.4.0" }
```

When a key equals a project manifest's `name` and is not that project's path, it pins the published build, and every project runs that build. The project still builds, tests, packages and publishes itself. To run the source instead, list the project's path (`"/tools/acme": ""`): an explicit path keeps the project manifest.

A project can name the extension either way in its own `extensions`, and both run the pinned build. A path entry (`"/tools/acme"`) also keeps the project an extension consumer for `--impacted`: a change to the extension's source re-runs that consumer's jobs of the extension. A name entry adds no such edge. Name the path only in the projects that must gate a change to the source, such as an image that bakes the source build.

## Extension Manifest

Each extension is defined by a `putnami.extension.json` file:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@putnami/typescript",
  "version": "2.0.0",
  "autoServe": true,
  "extensionDependencies": {
    "@putnami/go": "^1.0.0"
  },
  "commands": { ... },
  "tasks": { ... },
  "tools": { ... },
  "contracts": { ... },
  "hooks": { ... }
}
```

### Top-Level Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | `string` | Extension identifier (e.g., `@putnami/typescript`) |
| `version` | `string` | Semver version |
| `autoServe` | `bool` | Whether to auto-serve projects using this extension |
| `extensionDependencies` | `map[string]string` | Other extensions this depends on (semver constraints) |
| `commands` | `map[string]Command` | User-facing job definitions |
| `tasks` | `map[string]Task` | Atomic executable units |
| `tools` | `map[string]Tool` | Namespaced MCP tools served by an extension-owned subprocess |
| `contracts` | `map[string]Contract` | JSON schema definitions for I/O validation |
| `hooks` | `HooksConfig` | Lifecycle hooks |
| `runtime` | `RuntimeDefinition` | The extension's own executable and how to prepare it — see [Lifecycle Primitives](#lifecycle-primitives) |
| `workspace` | `WorkspaceAdapter` | Project discovery and metadata this extension owns — see [Lifecycle Primitives](#lifecycle-primitives) |
| `agentContent` | `AgentContentContribution` | Agent instructions (skills, workers, references, helpers) shipped under the extension's own version; materialized only in a workspace that opts in with `extension:<name>` — see [Agent Workflows](18-agent-workflows.md#how-a-workspace-opts-in) |

A manifest contributes at least one command, MCP tool or agent-content
contribution; a content-only extension needs no runtime.

## Commands

Commands are the public API of an extension — they map to CLI job commands like `build`, `test`, `lint`, `publish`, and `deploy`. Root commands are composable: more than one active extension can declare the same `commands.<name>`, and Putnami merges their matching `run` steps into one plan for `putnami <name> <project>`.

```json
{
  "commands": {
    "build": {
      "description": "Build the project",
      "activationFiles": ["tsconfig.json"],
      "channel": "npm",
      "priority": 10,
      "quiet": false,
      "flags": {
        "target": {
          "type": "string",
          "default": "es2022",
          "description": "JavaScript target"
        },
        "minify": {
          "type": "boolean",
          "default": false
        }
      },
      "defaults": {
        "sourcemap": "true"
      },
      "run": [
        { "id": "generate", "task": "build-generate" },
        {
          "id": "transpile",
          "task": "build-transpile",
          "dependsOn": ["generate"],
          "with": {
            "generatedDir": { "fromStep": "generate", "output": "generated" }
          }
        }
      ],
      "outputs": {
        "dist": { "fromStep": "transpile", "output": "dist" }
      }
    }
  }
}
```

### Command Fields

| Field | Type | Description |
|-------|------|-------------|
| `description` | `string` | Human-readable description |
| `activationFiles` | `string[]` | Glob patterns that must exist in the project for this command to activate (e.g., `["tsconfig.json"]`, `["go.mod"]`, `["**/*.go"]`) |
| `channel` | `string` | Publish channel filter — command only activates for projects whose `publish` includes this channel |
| `priority` | `int` | Higher priority runs earlier in the deterministic contributor order when multiple extensions provide the same command |
| `quiet` | `bool` | Suppress from job recap output |
| `flags` | `map[string]FlagDef` | Additional CLI flags this command accepts |
| `defaults` | `map[string]string` | Default parameter values |
| `run` | `PipelineStep[]` | Pipeline steps forming a DAG |
| `outputs` | `map[string]Binding` | Output bindings extracted from step results |

### Composable Root Commands

Extensions should contribute to canonical root commands instead of inventing extension-specific workflows for common operations. For example, a language extension can contribute package publishing steps to `publish`, a platform extension can contribute release steps to `deploy`, and an infrastructure extension can contribute infrastructure apply steps to the same `deploy` command. Users still run:

```bash
putnami publish my-app
putnami deploy my-app
```

Each contributor keeps its usual activation rules. A command only joins the composed plan for projects where its extension dependency, `activationFiles`, channel, and disable filters match. If a single extension matches, job keys stay as before, such as `deploy~apply`. If multiple extensions match the same root command for a project, Putnami namespaces internal scheduler keys by extension identity to avoid collisions while keeping displayed names readable, such as `deploy~apply (@putnami/pulumi)` in plan output.

Flag merging is deterministic. If two active contributors declare the same flag name, their full flag definitions must be identical. Identical shared flags, such as a common `dry-run` boolean, are merged once. Different definitions for the same flag fail validation before execution with a message naming the command, flag, and conflicting extensions. Contributor-specific flags can use unique names and remain available through the normal `params` map without changing the common command shape.

Command groups can point back at the same flat command for compatibility aliases:

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

`putnami cloud deploy my-app` then routes through the normal project selection and planner for that extension's `deploy` contribution, while `putnami deploy my-app` remains the canonical composed workflow.

A command group can declare shared `flags` once instead of repeating them on every subcommand:

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

Each subcommand's effective flags merge three layers in increasing precedence: the flat command target's flags, then the group's shared flags, then the subcommand's own flags. The more specific declaration wins on a name collision, so `deploy` above overrides the inherited `env` default while `status` keeps it. The merged set drives `--help`, shell completion, and the flag defaults folded into `params`. A flag whose name collides with a Putnami global (such as `--output` or `--dry-run`) is consumed by the CLI before the extension runs, so use extension-owned names for group flags.

Command groups may also describe nested verbs that are parsed inside the
extension binary. The top-level group subcommand still names the flat command
that runs; nested entries inherit that command target and are used for static
shell completion:

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

This lets completions offer `putnami cloud config list` and nested flags such
as `putnami cloud config show --with-secrets` without running the extension
binary during completion.

A group can name a `default` subcommand, and a subcommand can declare that it
runs without a workspace:

```json
{
  "commandGroups": {
    "audit": {
      "default": "run",
      "subcommands": {
        "run": {
          "command": "audit-run",
          "interactive": true,
          "workspace": "optional"
        },
        "report": { "command": "audit-report" }
      }
    }
  }
}
```

- `default` names the subcommand that `putnami audit` runs when no subcommand
  word follows. Flags after the group name reach that subcommand, so `putnami
  audit --strict` is `putnami audit run --strict`. `putnami audit --help`
  still prints the group help. Without a `default`, a bare `putnami audit`
  prints the group help. The value must be one of the group's own
  subcommands; the strict parser rejects any other value
  (`unresolved-default-subcommand`).
- `workspace` is `required`, the meaning of an absent value, or `optional`.
  An `optional` subcommand must also be `interactive: true`; the strict parser
  rejects it otherwise (`invalid-workspace-requirement`). It runs outside any
  workspace when its extension is pinned in the [user scope](#the-user-scope).
  Inside a workspace the member changes nothing.

Neither member raises the manifest's `cliContract`: a CLI that predates them
reads the group as it read it before, which is safe. See the extension
protocol's [ADR 0008](../../../protocols/extension/doc/adr/0008-workspace-requirement-and-group-default-need-no-contract.md).

## MCP Tools

An extension can add namespaced tools to the local `putnami mcp` server. Tool
names must include a namespace such as `putnami.search`; bare names are
reserved for the core workspace tools. The descriptor is read from the manifest
when the server starts, but the extension command is not launched until an
agent calls the tool.

```json
{
  "tools": {
    "putnami.search": {
      "description": "Search the hosted workspace intelligence index.",
      "inputSchema": {
        "type": "object",
        "properties": {"query": {"type": "string"}},
        "required": ["query"],
        "additionalProperties": false
      },
      "annotations": {
        "readOnlyHint": true,
        "destructiveHint": false,
        "idempotentHint": true,
        "openWorldHint": true
      },
      "_meta": {
        "putnami.dev/contract": {
          "access": "read",
          "readOnly": true,
          "supportsDryRun": false
        }
      },
      "command": "{extensionRoot}/bin/acme-tool",
      "args": ["mcp-tool"],
      "cwd": "{workspaceRoot}",
      "timeoutMs": 30000
    }
  }
}
```

`command`, `args`, `cwd`, `env`, and `timeoutMs` follow task subprocess
conventions. The CLI writes a single JSON request to stdin with the tool name,
arguments, workspace root, and extension root; the command writes a single MCP
tool result (`{"content":[{"type":"text","text":"…"}]}`) to stdout.
When MCP agent provenance is enabled, the request also contains an `agent`
object with `clientName`, `clientVersion`, `harness`, `model`, and `userAgent`.
The same safe header values are exposed as `PUTNAMI_AGENT_HARNESS`,
`PUTNAMI_AGENT_MODEL`, and `PUTNAMI_AGENT_USER_AGENT`; the structured request
is the source of truth. The Go extension SDK's `mcp.Serve` helper implements
that boundary, and `request.Agent.ApplyHTTPHeaders(outboundRequest)` applies
the values without overwriting extension-selected headers.

All four MCP safety hints and `putnami.dev/contract` metadata are required so
agents can distinguish read-only tools from mutating tools and know whether a
dry run exists. Core names win on collisions; if two extensions claim the same
name, neither tool is advertised. A malformed manifest or missing executable
is isolated to that extension, leaving the core MCP session available.

### Collaboration provider tools

A tool whose `_meta` also carries `putnami.dev/provider`
(`{"contract": "tasks", "version": 1, "operation": "find"}`) implements one
operation of a collaboration contract — tasks, change proposals or memory. It
is never advertised under its own name. A workspace binds a provider in
`options.collaboration` of `putnami.workspace.json`, and Putnami then exposes
the operation as the MCP tool `tasks.find` and the command `putnami tasks find`,
routes every call to the bound provider through this same subprocess boundary,
and adds a `provider` member (contract, version, operation, binding settings) to
the request. See [Collaboration providers](25-collaboration-providers.md).

### Pipeline Steps

Each step in the `run` array references a task and declares dependencies:

```json
{
  "id": "transpile",
  "task": "build-transpile",
  "dependsOn": ["generate"],
  "if": "params.transpile != false",
  "with": {
    "generatedDir": { "fromStep": "generate", "output": "generated" }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | `string` | Step identifier (unique within the pipeline) |
| `task` | `string` | Task name to execute |
| `dependsOn` | `string[]` | Step IDs or external refs (`^stepId`, `/stepId`, `*stepId`) that must complete first |
| `if` | `string` | Expression evaluated at plan time — step is skipped if false |
| `runOn` | `string` | Lifecycle condition: `success` (default), or `finally` for an invocation-resource finalizer |
| `finalizes` | `object` | Required with `runOn: finally`: `producer` names the invocation-output step whose start arms cleanup; `consumers` is the complete downstream terminal frontier. Finalizers have no ordinary `dependsOn` or result exports |
| `with` | `map[string]Binding` | Input bindings (values passed to the task) |

For an invocation-resource relation, producer start arms the finalizer exactly
once and cleanup waits until every listed consumer is terminal. The producer
task must declare an invocation-scoped output. Only those related tasks receive
job-context `invocation.artifactRoot` and the matching
`{invocationArtifactRoot}` task-argument token; unrelated steps receive neither.

### External Dependency References

| Prefix | Meaning | Example |
|--------|---------|---------|
| `^` | Same step in upstream dependency projects | `^generate` → all deps' `build~generate` steps |
| `/` | Same step at workspace root level | `/install` → workspace-level install step |
| `*` | Same step in all other projects | `*generate` → every project's `build~generate` |

### Command Barriers

Command-level `dependsOn` auto-plans prerequisite commands. Bare command names
are scoped to the same project; `!command` is a session barrier across the
current project selection:

```json
{
  "publish": { "dependsOn": ["!lint", "!test", "!build", "!package"] },
  "deploy": { "dependsOn": ["!publish"] }
}
```

`!build` plans `build` across the selected projects, then every root step of
the dependent command waits for every `build` leaf job in the final plan. These
are functional dependencies, so failures skip downstream side-effecting jobs
unless `--continue-on-error` is set.

Pipeline-step references prefixed with `^`, `/`, or `*` are invalid at command
level; put them on the relevant entry in `run` instead.

When prerequisite policy belongs to the dependent command rather than to every
provider of the prerequisite, declare `sessionPrerequisites`. It can condition
the subtree on dependent params, replace the current project selection from a
string param, filter projects with an expression, gate all prerequisite
contributors behind other session commands, and bind prerequisite-only params
from constants or dependent params resolved per project. All resulting edges
are functional: a gate failure blocks the prerequisite, and a prerequisite
failure blocks the dependent command. Those local bindings never change a
separate explicit invocation of the prerequisite command; if both invocations
would share an identity with different parameters, planning fails rather than
keeping whichever job was emitted first. The same rule applies to prerequisite
jobs already introduced through `dependsOn`, and local bindings cannot replace
literal `run[].with` values. Missing project-parameter paths fail closed.
Workspace-once contributors merge compatible selections and shared constant
bindings, but a project-derived binding across multiple selected projects is
ambiguous and rejected.

### Conditional Steps (`if` Expressions)

Plan-time expressions control whether a step runs:

```json
{ "if": "params.minify == true" }
{ "if": "params.target != 'es5'" }
{ "if": "params.generate && params.strict" }
{ "if": "!params.skipGenerate" }
{ "if": "commands.test && project.type == 'library'" }
{ "if": "params.race != commandParams.test.race" }
```

Supported syntax:
- **Variable access**: `params.name`, `steps.stepId.status`, `commands.name`, `commandParams.command.name`, `project.type`
- **Comparison**: `==`, `!=`
- **Logical operators**: `&&`, `||`
- **Negation**: `!expr`
- **Parentheses**: `(expr)`
- **Literals**: strings (`'value'`, `"value"`), booleans (`true`, `false`), numbers

`commands.<name>` tests membership in the order-independent effective command
set for the current provider and selected project; disabled, unmatched, and
dependency-synthesized commands are not members. `commandParams.<command>.<name>`
reads that effective command's fully resolved parameter, so replacement work
can require equivalent compile settings. `project.type` is the target's
resolved classification after provider probing and authored overrides. Callers
without this planner context retain a step that references these roots, so an
unavailable fact cannot authorize pruning; params-only expressions remain
backward compatible.

### Dead-Code Elimination

After evaluating `if` conditions, the pipeline is pruned:

1. Remove steps whose `if` evaluated to false.
2. Walk backward from surviving leaf steps.
3. Keep only steps reachable from leaves.
4. Unreachable intermediate steps are eliminated.

This prevents running unnecessary setup steps when their consumers are disabled.

## Tasks

Tasks are atomic executable units. Commands reference tasks in their pipeline steps.

```json
{
  "tasks": {
    "build-transpile": {
      "kind": "command",
      "command": "{extensionRoot}/bin/putnami-bun",
      "args": ["run", "{extensionRoot}/bin/build-transpile"],
      "cwd": "{projectRoot}",
      "timeoutMs": 300000,
      "env": {
        "NODE_ENV": "production"
      },
      "inputs": {
        "sources": {
          "from": "project",
          "files": ["src/**/*.ts", "src/**/*.tsx"]
        },
        "config": {
          "from": "project",
          "files": ["tsconfig.json", "package.json"]
        },
        "workspaceConfig": {
          "from": "workspace",
          "files": ["bun.lock", "tsconfig.base.json"]
        }
      },
      "outputs": {
        "dist": {
          "kind": "directory",
          "path": "dist",
          "description": "Compiled output"
        },
        "data": {
          "kind": "data",
          "description": "Build metadata"
        }
      },
      "cache": {
        "enabled": true,
        "deterministic": true,
        "key": {
          "files": ["src/**/*.ts", "tsconfig.json", "package.json"],
          "env": ["NODE_ENV"]
        }
      }
    }
  }
}
```

### Task Fields

| Field | Type | Description |
|-------|------|-------------|
| `kind` | `string` | Always `"command"` — the task spawns a subprocess |
| `command` | `string` | Executable path (supports template variables) |
| `args` | `string[]` | Arguments (support template variables) |
| `cwd` | `string` | Working directory (supports template variables) |
| `env` | `map[string]string` | Environment variables |
| `timeoutMs` | `int` | Execution timeout in milliseconds (default: 300000 = 5 minutes) |
| `inputs` | `map[string]InputPort` | Declared inputs for the task |
| `outputs` | `map[string]OutputPort` | Declared outputs from the task |
| `writes` | `ResourceRef[]` | Resources this task writes (see [Write Resources](#write-resources)) |
| `reads` | `ResourceRef[]` | Resources this task reads |
| `resources` | `map[string]int` | Units of a named scarce resource one execution holds (see [Resource Budgets](#resource-budgets)) |
| `cache` | `CachePolicy` | Cache behavior and key specification |

### Input Ports

Input ports declare what a task needs:

| `from` | Description | Additional Fields |
|--------|-------------|-------------------|
| `"project"` | Files from the project directory | `files`: glob patterns |
| `"workspace"` | Shared files from the workspace root | `files`: glob patterns |
| `"task"` | Output from another task | `task`, `output` |
| `"params"` | Job parameters | `name` |
| `"env"` | Environment variables | `name` |
| `"runtime"` | Runtime information | `name` |

### Output Ports

| `kind` | Description | Additional Fields |
|--------|-------------|-------------------|
| `"data"` | Arbitrary data returned in the result | `description` |
| `"file"` | A single output file | `path`, `description` |
| `"directory"` | An output directory | `path`, `description` |

### Write Resources

`writes` and `reads` let a task declare the shared resources it touches — a
generated tree, a shared output directory — so the planner can keep two jobs from
writing the same place at once **without** turning that into a functional
dependency. This is distinct from `dependsOn` (which means "I need this step's
output") and from `inputs`/`outputs` (which feed cache keys).

```jsonc
"build-generate": { "kind": "command", "command": "...", "writes": ["gen"] },
"build-compile":  { "kind": "command", "command": "...", "reads":  ["gen"] }
```

The planner serializes only conflicting accesses to the same resource: two
writers never overlap, a reader never overlaps a writer, and two readers stay
parallel. A resource is a bare string (project-scoped) or an object with an
explicit scope:

| Form | Meaning |
|------|---------|
| `"gen"` | Project-scoped resource — conflicts only within the same project |
| `{ "id": "registry", "scope": "workspace" }` | Workspace-scoped — conflicts across all projects |

Serialize edges never enter cache keys, and a serialized predecessor failing does
not skip its successor. A task with `reads` but no `writes` stays parallel with
other readers, but it will not overlap a planned writer of the same resource. Use
separate tasks when behavior changes by mode: a fix-mode lint task that mutates
project files should declare `writes: ["sources"]`, while the no-fix lint task
should declare `reads: ["sources"]`.

### Resource Budgets

`writes`/`reads` say two tasks must not touch a resource at the same time.
`resources` says something different: how much of a **divisible** scarce thing
one execution of this task holds while it runs — database connections, ports,
remote-cache bandwidth.

```jsonc
"test-exec": { "kind": "command", "command": "...", "resources": { "db-connections": 230 } }
```

The caller declares what exists, once per resource, with a repeatable global
flag:

```bash
putnami lint,test,build --impacted --max-parallel 8 --resource db-connections=400
```

A task starts when the worker count allows it **and** every resource it claims
still has budget. Three rules follow from that, and they are what keep the
budget from ever making a run slower than it already is:

- A task that claims nothing is never gated by anyone else's claim, so `lint`
  and `build` run at the substrate's width while a database ceiling holds back
  only the tasks that open connections.
- A resource with no budget on the run is **unlimited**. A run that declares
  none — every run that does not pass `--resource` — admits exactly as before.
- A single task claiming more than the whole budget is refused at **plan time**,
  naming the resource and both amounts, rather than waiting forever for a
  release that can never be large enough.

Claims **add up** across a batch, because a batched tool runs its members' work
concurrently inside one subprocess, so their units are held at the same time.
Group formation stops at the budget: a peer that would push the group past it
stays in the queue and runs on its own turn rather than being clamped into a
dispatch that would over-run the real ceiling.

Names are opaque: the CLI knows nothing about databases, and nothing is
hard-coded. A claim never enters a cache key — it is an admission quantity, not
an input.

### Cache Policy

```json
{
  "cache": {
    "enabled": true,
    "deterministic": true,
    "key": {
      "files": ["src/**/*.ts", "tsconfig.json"],
      "env": ["NODE_ENV", "BUILD_ID"]
    }
  }
}
```

- `enabled` — Whether caching is allowed (default: `true`).
- `deterministic` — Whether the task produces identical output for identical input.
- `key.files` — Glob patterns for files whose content contributes to the cache key.
- `key.env` — Environment variable names whose values contribute to the cache key.

When using v2 task input ports, cache keys are automatically derived from declared inputs (no explicit `cache.key.files` needed). Use `from: "workspace"` for lockfiles and root configuration shared by nested projects; those files are hashed once per session and a matching watch-mode change invalidates consumers across the workspace.

## Lifecycle Primitives

Contract v3 gives an extension **three** generic lifecycle surfaces the CLI
consumes, and no fourth. They are what replaced the CLI's hardcoded knowledge of
languages, toolchains, project markers and test databases: core stopped
recognizing ecosystems and started asking the extension that owns one.

All three are additive. A manifest that declares none of them is a v2 manifest
and behaves exactly as it did.

| Primitive | Manifest surface | What the extension takes ownership of |
|-----------|------------------|----------------------------------------|
| Runtime preparation | `runtime` | Building and addressing its own executable, instead of the CLI knowing which wrapper binary it ships |
| Workspace adapter | `workspace` | Which directories are its projects, what their metadata is, and how its manifests are kept in sync |
| Invocation-scoped outputs | `declares.outputs` + `runOn: "finally"` | Secrets and disposable resources that exist only for one CLI invocation, with guaranteed cleanup |

The normative reference — every field, every merge rule, every failure code, and
the lifecycle states each primitive moves through — is
[`protocols/extension/doc/07-lifecycles.md`](../../../protocols/extension/doc/07-lifecycles.md).
What follows is the author's-eye summary and the CLI-side behavior that goes
with each.

### Runtime preparation

```json
{
  "runtime": {
    "executable": "bin/putnami-ts",
    "prepare": {
      "command": "{extensionRoot}/bin/prepare",
      "args": ["--output", "{runtimeOutput}"],
      "inputs": ["cmd/**", "internal/**", "go.mod", "go.sum"]
    }
  }
}
```

Declare `runtime` and reference the resolved binary from tasks as
`{extensionRuntime}`. `prepare` is optional: omit it when each platform archive
ships the binary, and the same `executable` path is used either way.

CLI-side behavior an author should count on:

- Preparation runs **once per run**, in the engine's runtime-synchronization
  phase before any job is scheduled — never lazily inside a task, and never
  under a task timeout.
- Preparation is **workspace-independent** (`GOWORK=off` for Go): a prepare is a
  function of the extension's own module, which is what makes a locally prepared
  runtime and an archive-shipped one the same artifact.
- The prepared artifact's **digest is the extension's cache identity**. Change a
  declared `prepare.inputs` file and every task that runs on that runtime
  re-keys.
- Preparation failure is **never silently recovered**. There is no fallback to
  `go run`, a shell wrapper, or name-based classification — the failure codes in
  the protocol doc (`runtime.*`) are reported and the run stops.
- On Windows the CLI runs the declared `executable` with an `.exe` suffix:
  `bin/putnami-ts` in the manifest is `bin/putnami-ts.exe` on disk. Ship that
  binary, not a POSIX launcher script, which Windows cannot run.

The provider commands accept `{extensionRuntime}` too: the remote build-cache
provider (`cache-provider`), the runner provider (`runner-provider`) and the
session reporter (`session-reporter`). Before the CLI starts one, it prepares
the runtime the command references and verifies it with the `runtime-info`
handshake, exactly as for a task. When that runtime cannot be prepared:

| Provider | What the CLI does |
|----------|-------------------|
| `cache-provider` | Builds with the local cache only and prints one notice that names the failure |
| `runner-provider` | Stops the remote run with the failure, before it submits anything |
| `session-reporter` | Prints that the reporter is unavailable and keeps the session for `putnami sessions replay` |

### Workspace adapter

```json
{
  "workspace": {
    "markers": ["package.json"],
    "inputs": ["package.json", "bun.lock", "tsconfig*.json"],
    "excludes": ["node_modules", "dist"],
    "syncTask": "workspace-sync"
  }
}
```

An extension that declares `workspace` becomes the answer to "is this directory
a project, and what is it?" for its ecosystem. One that does not is **never
probed**, which is what keeps probe cost proportional to the number of
extensions that actually own project metadata.

CLI-side behavior an author should count on:

- The probe is invoked as `<runtime> __putnami workspace-probe`, one **batched**
  request per extension, from the workspace root. Stdout carries the result
  document and nothing else — log to stderr.
- Answers are cached in `.putnami/workspace-index.json` per provider and
  replayed. An unchanged workspace starts **zero** probe processes; only the
  providers whose declared `inputs` moved are re-asked.
- The validity oracle is the **content digest** of the declared inputs, never a
  size or an mtime. Every `marker` must also appear in `inputs`, or the
  manifest is rejected.
- Core owns the canonical project path and ID, and always excludes `.git`,
  `.putnami` and gitignored directories. Provider-specific data lands in
  `project.metadata["<extension name>"]`, namespaced — a task reads its own
  block and never another provider's.
- `syncTask` is where the extension performs its own manifest mutations
  (`putnami projects sync` fans out to it). Core writes no language manifest.

### Extension-owned machine caches

An extension that keeps a machine-global cache owns it end to end. Core hands it
one stable directory and never looks inside.

| Surface | Meaning |
|---------|---------|
| `extension.cacheRoot` (job context) | The absolute, per-extension machine cache root. Layout is the extension's business |
| `cache-clean` command | Reserved, hidden command name. `putnami cache clean` fans out to every extension that declares it — a cold reset |
| `cache-gc` command | Reserved, hidden command name. `putnami cache gc` fans out to it, and it is also what core's throttled opportunistic collection invokes |

Both are ordinary internal commands running ordinary typed tasks, so cache
policy is declared, digested and validated like the rest of the manifest. The
neutral eviction arithmetic — recency sort, low watermark, grace window,
cross-process lock, JSONL summary — is in the SDK's
[`cachepolicy`](../../extension-sdk/) package; the extension supplies only what
one of its cache entries *is*.

### Invocation-scoped sensitive outputs and finalizers

Use this when a task provisions something that exists **only for one CLI
invocation** and must be cleaned up: a throwaway database, a temporary
credential, a socket.

```json
{
  "tasks": {
    "test-database-setup": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["database", "up", "--dsn-file", "{invocationArtifactRoot}/database/dsn.env"],
      "cache": false,
      "declares": {
        "outputs": {
          "dsn": {
            "kind": "runtime-file",
            "scope": "invocation",
            "sensitive": true,
            "path": "database/dsn.env"
          }
        },
        "effects": ["process"]
      }
    }
  },
  "commands": {
    "test": {
      "run": [
        { "id": "setup", "task": "test-database-setup" },
        { "id": "run", "task": "test-run", "dependsOn": ["setup"] },
        {
          "id": "teardown",
          "task": "test-database-teardown",
          "runOn": "finally",
          "finalizes": { "producer": "setup", "consumers": ["run"] }
        }
      ]
    }
  }
}
```

| Declaration | Effect |
|-------------|--------|
| `scope: "invocation"` | The output lives in the invocation's private scratch, is never captured and never restored, and is discarded when the invocation ends |
| `kind: "runtime-file"` | The content only means anything inside the invocation that wrote it |
| `sensitive: true` | Its path and bytes may never reach an event, a result, a session record, telemetry or cache traffic. Requires `scope: "invocation"` and a literal `path` |
| `runOn: "finally"` + `finalizes` | Marks the step as the finalizer. `producer` start arms it; cleanup waits for every named `consumer` to reach a terminal state |

Rules worth internalizing before writing one:

- A task with an invocation-scoped output must set `cache: false`. A cache hit
  would report success without ever creating the artifact.
- `finalizes.consumers` must be the **complete** downstream frontier, and a
  finalizer has no ordinary `dependsOn`. Trigger and frontier come only from
  `finalizes`, so they cannot disagree.
- Nothing may bind an input from a finalizer, and neither a finalizer task nor
  its command may export a result: cleanup reports lifecycle status, not data.
- Cache negotiation happens **before** materialization. An all-hit frontier
  prunes the producer and its finalizer together; any consumer miss keeps the
  producer and arms exactly one finalizer.
- The producer, its listed consumers and the finalizer — and nobody else —
  receive `invocation: {id, artifactRoot}` in their job context and the
  `{invocationArtifactRoot}` template token in their manifest arguments.

#### Crash recovery

A `SIGKILL` cannot run a finalizer, so recovery is contractual rather than
best-effort, and an extension that provisions external resources must
participate:

- Core holds a **non-secret** invocation lease — invocation ID, PID, provider,
  action digest, creation time.
- Stamp the corresponding **non-secret** lease identity onto the external
  resource (a container label, a row, a directory name). Credentials never
  belong in a lease, label, name, filter, log or error.
- On setup and on maintenance, reap resources whose lease has **no live owner**
  *before* provisioning or reusing anything. That is what makes the next
  invocation after a kill the one that recovers the orphan, instead of waiting
  for a GC interval.
- Finalizers and provider cleanup must be **idempotent**: the same teardown may
  be attempted by the finalizer and by a later reaper.

A successful reap is reported as `sensitive.lease_reaped` — a recovery signal,
not a failure. The SDK's `dbtestenv` package implements this shape for database
test environments and is the reference to copy.

## Contract Validation

Contracts define JSON schemas for task I/O, validated at plan time before any job runs.

The CLI validates:
- **Input bindings** — Each `with` binding in a step must match a declared input port on the referenced task.
- **Output references** — `fromStep` + `output` references must point to valid steps and output ports.
- **Command outputs** — Top-level `outputs` bindings must reference valid step outputs.

Validation errors are reported with clear messages and cause exit code 4 (`ExitValidation`) unless `--continue-on-error` is set.

## Bindings

Bindings wire data between pipeline steps.

### Value Binding

```json
{ "value": "production" }
```

The value is materialized as a parameter for that pipeline step only. It
overrides command defaults, project options and CLI parameters of the same
name, and participates in the task's cache identity (including an explicit
`null` value). When both kebab-case and camelCase spellings are bound, each
exact spelling keeps its value; a generated camelCase alias never overwrites an
explicit camelCase binding.

### Context Binding

```json
{ "from": "command", "path": "params.target" }
{ "from": "context", "path": "workspace.name" }
```

### Step Binding

```json
{ "fromStep": "generate", "output": "generated" }
```

Dot-notation paths are supported for JSON traversal. Optional `default` and `required` fields control behavior when the value is missing.

## Extension Installation

Extensions can be installed from a remote resolver:

```bash
putnami extensions install
```

By default, `install` is **lock-first**: it installs the exact version recorded in the lock file from a version-pinned channel, so `latest` moving ahead of the lock never changes what a bare `install` fetches. Advancing the pin is opt-in — use `putnami extensions update`, or `--latest` to ignore the lock file and resolve the latest versions matching configured constraints:

```bash
putnami install --latest
putnami extensions install --latest
```

This is useful for CI/e2e environments where you want to always test against the latest compatible versions rather than pinned ones. The lock file is updated after installation with the newly resolved versions.

When `putnami extensions update` or `putnami upgrade` moves an extension that `putnami.workspace.json` pins to one exact release, it writes the new release to that pin as well as to the lock. Ranges, channels, `latest`, unpinned entries and local paths are left as they are.

### Materializing for Another Platform

`install` normally materializes artifacts for the invoking machine. To produce artifacts for a different target — pre-warming an image layer, for instance — name the platform and a destination:

```bash
putnami extensions install --platform linux/amd64 --dest ./.gen/warm-artifacts
```

`--platform <os>/<arch>` keeps the lock-first resolution but downloads the archive for the requested platform and verifies it against that platform's digest in `putnami.lock.json` → `integrities`. `--dest <dir>` writes a drop-in artifact-store root at `<dir>/sha256/<xx>/<digest>/`, which a packaging step can copy into `~/.putnami/artifacts` on the target machine.

Either flag makes the run a **materialization**: it does not run install hooks, does not repoint the stable `.putnami/bin/extensions/<name>` links, and does not rewrite the lock file. Its output is a tree for another machine to run, not an install for this one.

Two consequences worth knowing:

- **It fails closed.** If the lock has no integrity for the requested platform and the registry advertises none, the command errors rather than materializing unverified bytes. `PUTNAMI_UNSAFE_INSTALL=1` is not honored on this path — it exists to unblock one machine's own install, not to sign off on bytes an image bakes in. Run `putnami extensions update` (which records the foreign platforms a shared lock already lists) to fill the missing digest.
- **It is reproducible.** Modes are canonicalized and timestamps are stamped to a fixed value, so the same lock and platform produce byte-identical output on any machine. That is what makes the tree safe to hash into an image content key.

### The User Scope

The user scope holds extensions you pin for yourself, outside any workspace. It
lets a command such as `putnami audit` run in a directory that has no
`putnami.workspace.json`:

```bash
putnami extensions install --user @acme/audit        # the constraint defaults to latest
putnami extensions install --user @acme/audit@1.4.0
putnami extensions list --user
putnami extensions remove --user @acme/audit
```

The user scope lives in `~/.putnami/user/`. It has its own `putnami.lock.json`
in the workspace lock format and its own stable links under
`~/.putnami/user/.putnami/bin/extensions/`. The extension trees stay in the
machine-wide artifact store, so a tree a workspace already installed is linked,
not downloaded again.

`install --user` follows the workspace install path:

- The registry comes from `PUTNAMI_REGISTRY_URL`, or the default registry. No
  workspace manifest is read.
- The archive is verified against the SHA-256 digest the lock records or the
  registry advertises. An archive with no digest is refused unless
  `PUTNAMI_UNSAFE_INSTALL=1` is set, exactly as for a workspace.
- A re-run keeps the recorded pin and downloads nothing. `--latest` moves the
  pin to the newest release, and an explicit version moves it to that version.
- It runs no extension install hook, and it reads and writes nothing in the
  current directory, inside a workspace or not.
- It takes one registry extension (`@scope/name[@version]`). A local path,
  `--platform` and `--dest` are usage errors.

Outside a workspace, `putnami <group> <subcommand>` resolves against the user
scope only. A pinned extension whose link is missing is repaired the way a
workspace repairs one: from the artifact store, or by a verified download. When
the repair fails, or the pinned version cannot load, the CLI prints a warning
that names `putnami extensions install --user <name>` and, to move the pin,
`putnami extensions install --user --latest <name>`.

Only a subcommand declared `interactive: true` and `workspace: "optional"` runs
there. Every other command, including the extension's flat commands and
`putnami build`, fails with `putnami: no workspace found (looking for
putnami.workspace.json)` and exit code 1. Project selection flags (`--projects`,
`--all`, `--impacted` and the related filters) are usage errors, because there
are no projects to select.

The job runs in the directory you invoked it from. Its scratch space, job
context file, output directory and caches live under `~/.putnami/user/` or in
the machine-wide caches, so the invoking directory is left untouched. The job
context carries `userScope.callerDir`, and the environment carries
`PUTNAMI_CALLER_DIR`. Either tells the extension that no workspace exists; see
[Process Setup](04-job-execution.md#process-setup).

Inside a workspace the workspace's own pins are the only ones, so a workspace
that pins version A of an extension runs A even when the user scope pins B.

A JavaScript monorepo whose root `package.json` declares `workspaces` and has
no `putnami.workspace.json` in the current directory or above it is the one
exception. A command group runs there from the user scope, and nothing is
installed or written in the monorepo, when all of these hold:

- The user scope pins an extension that provides the group. A missing link is
  repaired first, as outside a workspace, and aliases from
  `~/.putnami/config.json` apply.
- The monorepo's own extensions do not provide the command.
- The root `package.json` does not list the providing extension in
  `devDependencies`, installed or not.

Otherwise the monorepo is the workspace. When a user-scope pin cannot be
loaded and the monorepo does not provide the command either, the CLI prints
the pin's warning, with both repair commands, before it runs the command in
the monorepo.

When the user scope takes the command group, the CLI version that the
monorepo's `putnami.lock.json` pins does not apply: the CLI you invoked runs
the command, as outside a workspace. A pin with a missing link is repaired
only after the switch to the pinned CLI, so that pinned CLI decides the run.
A pinned CLI that predates the user scope then runs the command in the
monorepo; run `putnami extensions install --user <name>` to repair the link.

### Lock File

`putnami.lock.json` records exact resolved versions and integrity hashes for both extensions and templates:

```json
{
  "version": 3,
  "extensions": {
    "@putnami/typescript": {
      "version": "2.1.0",
      "manifestHash": "sha256-manifest...",
      "integrities": {
        "darwin/arm64": "sha256-abc123...",
        "linux/amd64": "sha256-def456..."
      },
      "source": "https://put.putnami.dev/putnami/typescript/download?channel=2.1.0"
    }
  },
  "templates": {}
}
```

Download archives are per-`os`/`arch`, so a single archive digest can only verify on the platform that produced it. The lock therefore records:

- `integrities` — a map of `os/arch` → archive SHA-256. A lock generated on one platform starts with only that platform's digest. Another platform verifies its archive against the registry's advertised digest instead, and records its own digest the next time the lock is written for a change you asked for: a new extension, a moved version, or `putnami extensions update`. `putnami install` never rewrites the lock for this platform's digest alone, or for the registry URL it downloaded through, so a CI checkout on another platform stays clean and `--impacted` plans from the commit.
- `manifestHash` — the SHA-256 of the (platform-independent) extension manifest. It binds a cross-platform install to the locked artifact when this platform has no recorded archive digest yet.

The legacy single `integrity` field is still accepted for backward compatibility and is migrated into `integrities` on the next install. On an integrity mismatch, the installer prints a remediation hint pointing at `putnami <kind> update`, which refreshes the lock.

#### Lock format versions

The top-level `version` field is the lock **format** version, and it is load-bearing: a CLI reads exactly the window it supports and refuses anything outside it with a versioned error rather than acting on fields it cannot interpret — a NEWER file because it does not know the vocabulary, an OLDER one because the vocabulary it requires is missing. Vocabulary a version does not define is dropped on both read and write, so a file never claims one format while carrying another's fields.

| Version | Adds |
|---------|------|
| `1` | Resolved versions, `integrity`/`integrities`, `manifestHash`, `source`. **No longer read** since 0.3.0. |
| `2` | `taskContract` per entry — the task-contract protocol version the pinned extension's manifest declares (`2`, or `3` when any of its tasks carries a `declares` block). The current read floor. |
| `3` | Top-level `toolchains` pins, plus `cli.protocolVersion` for machine-output preflight. Current writers pin Go and Bun only; historical Node entries remain readable. |
| `4` | Platform-independent `agentArtifacts` pins, which only `putnami migrate agent-content` reads and removes today. The current write default. |

The read window is `[2, 4]`. Existing v2 and v3 locks remain readable; ordinary
reads and writes preserve the recorded version. `putnami migrate vnext --apply`
is the explicit projection to v4. A v1 lock — including a file that records no
`version` at all, which is how v1 spells itself — is refused with:

```
putnami.lock.json is lock format version 1, but this putnami requires version 2: run `putnami migrate vnext --apply` to convert it
```

It is refused rather than silently upgraded: an unrelated command must not convert a committed lock as a side effect, and reading a v1 file as v2 would mean inventing the task-contract records v2 exists to carry. `putnami migrate vnext --apply` (see [03-commands.md](03-commands.md#migrate)) is the ONLY reader allowed below the floor, and `migrate vnext` is exempt from the workspace-pin launcher for the same reason — relaunching reads the lock. It records what the installed manifests already declare, then projects to the current format:

```json
{
  "version": 4,
  "extensions": {
    "@putnami/typescript": {
      "version": "2.1.0",
      "manifestHash": "sha256-manifest...",
      "taskContract": 3
    }
  },
  "templates": {}
}
```

Recording the contract level in the lock is what lets a command know a pinned extension's contract without resolving and parsing its manifest. An entry with no `taskContract` is simply "not recorded"; nothing infers a level from its absence.

#### Toolchain and CLI protocol preflight

Lock format v3 adds a `toolchains` map beside `cli`, `extensions`, and
`templates`. `putnami install` derives Go from `go.work` and Bun from
`package.json#packageManager`, then records exact versions, vendor release
sources, and vendor-published SHA-256 archive digests for supported `os/arch`
pairs. Other runtime declarations, including `package.json#engines.node`, are
outside the current writer contract and are not provisioned by Putnami. A Node
pin already present in a v3/v4 lock remains readable for compatibility, but a
metadata refresh removes it.

A pin that neither file derives stays in the lock, unchanged and without a
metadata request, while a declared extension's runtime resolves its identity:
through a task, `runtime.runToolchains`, or, for an extension declared by path,
`runtime.prepare.toolchains`. A TypeScript-only workspace that declares
`@putnami/typescript` by path keeps its `go` pin this way, because that
extension compiles its runtime with the pinned Go. The refresh drops the pin
once no declared extension resolves it, and never adds one the lock does not
already carry.

The pins derive from files that `workspace-install` writes, such as `go.work`,
so that command runs before the lock pins anything it installs. A toolchain
alias that only `workspace-install` needs resolves as if it were optional: when
the lock does not pin it, has no digest for the host, or no candidate matches,
the job runs under the unavailable identity. The prepare toolchains of an
extension declared by path resolve the same way, but only in a run whose every
command is `workspace-install`. Every other command still stops on a missing
pin, even in the same run, once it plans a job that needs the toolchain. A
workspace that declares `@putnami/go` and has no Go project yet builds without
a `go` pin, because the build plans no Go job. `putnami install` and
`putnami init` end with the refresh that records the pins. An explicit `putnami install` also pins, before
its installers run, each toolchain that `go.work` or `packageManager` declares
at a release the lock does not pin, so the installers install the declared
release in the same run. The implicit install that a command runs in a
fresh checkout does not refresh them: it only adds a pin that `go.work` or
`packageManager` declares and the lock does not carry yet, so the first project
of a language gets its pin on the next command. After you change a declared
version, run `putnami install`.

The Go entry comes from the go.dev release index. The first pin of a release on
a machine fetches the index and writes the entry to a pin record under the
Putnami home, `toolchains/go/go-<version>.pin.json`, beside the directory the
release installs in. Every later pin of that release, in any workspace of the
machine, reads the record and makes no request to go.dev, because a published
Go release never changes its archives. The lock holds the same bytes from the
record and from the index.

A record is used only when it names the requested version and the go.dev
source, and maps only supported `os/arch` pairs to SHA-256 digests. Any other
record is ignored, and the entry of the next fetch replaces it. A Putnami home
that cannot hold the record does not fail the pin: each pin then asks the
index. To make the next pin ask the index again, delete the record.

```json
{
  "version": 3,
  "cli": {
    "version": "1.4.2",
    "protocolVersion": 2
  },
  "toolchains": {
    "go": {
      "version": "1.26.1",
      "integrities": {
        "linux/amd64": "…",
        "darwin/arm64": "…"
      },
      "source": "https://go.dev/dl/"
    }
  },
  "extensions": {},
  "templates": {}
}
```

`cli.protocolVersion` describes the machine result/session protocol emitted by
that exact pin. It is refreshed without downloading the CLI when install is
already running as the pinned version. It is omitted rather than guessed when
the running binary and requested pin differ. Consumers can always query a
downloaded binary directly with `putnami --version --output=json` before paying
for a gate run.

A digest is bound to `(name, version, os/arch)`, so a version bump can never reuse the previous version's digests. Every command that can advance a locked version — `putnami extensions update` / `putnami templates update`, `putnami upgrade`, `install --latest`, and `install <name>@<version>` — therefore **re-resolves** each platform the lock already listed at the newly resolved version, reading the digest the registry advertises for that `os`/`arch`. A committed lock keeps all of its platforms across a bump instead of shrinking to whichever machine ran it. A lock-pinned `putnami install` does not move the version, so it carries the existing map over unchanged and makes no extra requests. Only the host's digest is verified against bytes the CLI actually downloaded and hashed; a foreign platform's digest is registry-asserted, which is the strongest guarantee available without that platform's bytes. A platform that cannot be resolved at the new version (no archive for that `os`/`arch`, no advertised integrity, network failure) is reported on stderr and simply omitted — the upgrade still succeeds, and that platform falls back to the `manifestHash`-bound install until it runs its own `install`.

### Version Resolution

Semver constraints are supported:

| Constraint | Matches |
|------------|---------|
| `^1.2.0` | `>=1.2.0 <2.0.0` |
| `~1.2.0` | `>=1.2.0 <1.3.0` |
| `>=1.0.0` | `>=1.0.0` |
| `<2.0.0` | `<2.0.0` |
| `1.2.3` | Exact version only |
| `latest` | Latest available |

Pre-release versions sort lower than releases.

## Templates

Templates are standalone project scaffolds, decoupled from extensions. Each template lives in its own directory with a `putnami.template.json` manifest:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-template.json",
  "name": "typescript-library",
  "description": "TypeScript library with exports",
  "extension": "@putnami/typescript"
}
```

### Template Discovery

Templates are discovered from three sources, in priority order:

1. **Workspace projects** — Any project containing a `putnami.template.json` file.
2. **Convention directories** — `<domain>/templates/*/` directories at the workspace root (e.g., `typescript/templates/typescript-library/`).
3. **Installed templates** — `.putnami/templates/<name>/<version>/` for downloaded templates.

When a lock file (`putnami.lock.json`) exists, installed templates are resolved using the exact version from the lock entry rather than scanning all version directories.

### Template Manifest Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | `string` | Template identifier (e.g., `typescript-library`) |
| `description` | `string` | Human-readable description |
| `version` | `string` | Semver version (optional) |
| `extension` | `string` | Extension to activate for projects created from this template |
| `workspaceDevDependencies` | `map[string]string` | Dev dependencies to add to the workspace root |

### Using Templates

```bash
putnami projects create my-app --template typescript-web
putnami projects create my-lib --template go-library --path libs/my-lib
```

A template that renders a `go.mod` is added to `go.work`, and `go mod tidy`
runs in it, with the go command the template extension's tasks run with: the
release the lock pins, or else a `go` on PATH. On a host with neither, create
first pins the Go that `go.work` declares and runs `workspace-install`, which
installs it. See ADR 0019. When `go mod tidy` fails, create keeps the project
and exits non-zero. The error quotes the end of the go output, with URL
credentials redacted, and names the create command to run again with `--force`.

### Template Installation

Templates can be installed from a remote resolver, mirroring the extension installation lifecycle:

```bash
putnami templates install              # Install all configured templates (default)
putnami templates install my-template  # Install a specific template
putnami templates update               # Update to latest compatible versions (lock and exact pins)
putnami templates list                 # List configured and discovered templates
putnami templates remove <name>        # Remove an installed template
```

The combined `putnami install` command also installs templates when the workspace config declares them.

`templates update` writes a moved release to a template entry that pins one exact release (`"@scope/tpl:1.2.3"`) as well as to the lock. When a template is declared more than once across `putnami.workspace.json` and the global putnami config, only the entry the config reader keeps is rewritten. If that rewrite would leave another entry in effect, the update stops before writing the lock and names the template.

#### Workspace Configuration

Declare installable templates in `putnami.workspace.json`:

```json
{
  "templates": [
    "typescript-library",
    "go-server:^1.0.0"
  ]
}
```

Format: `"template-name"` (resolves to latest) or `"template-name:constraint"` (semver constraint).

#### Template Lock File

Templates share the unified `putnami.lock.json` lock file with extensions. Template entries are stored in the `templates` section:

```json
{
  "version": 1,
  "extensions": {},
  "templates": {
    "typescript-library": {
      "version": "2.1.0",
      "integrities": {
        "linux/x64": "sha256-abc123..."
      },
      "manifestHash": "sha256-def456...",
      "source": "https://put.putnami.dev/putnami/typescript-library/download?channel=2.1.0"
    }
  }
}
```

The lock file is written after each install/update and should be committed to version control for reproducible builds. Template lock entries use the same `integrities`, `manifestHash`, and version-pinned `source` fields as extension entries; the legacy single `integrity` field is still read for older locks.

By default, `install` respects the lock file. Use `--latest` to ignore the lock file and resolve the latest versions:

```bash
putnami templates install --latest
```

### Publishing Templates

Templates are packaged as archives using the `template-archives` publish channel. To make a template publishable:

1. **Create the template project** with a `putnami.template.json` manifest and all scaffold files.

1. **Configure the publish channel** in the template project's `putnami.json`:

```json
{
  "name": "typescript-library",
  "publish": ["template-archives"]
}
```

1. **Package the template** using the `package` job. The Go extension's package job detects the `template-archives` channel and creates platform archives containing the template manifest and all scaffold files:

```bash
putnami package --projects typescript-library
```

This creates archives in `.putnami/out/<project>/package/archives/` — one per platform (for resolver compatibility), all containing identical content since templates are platform-independent.

1. **Publish the archives** using the standard publish pipeline:

```bash
putnami publish --projects typescript-library
```

The archives are uploaded to the Putnami registry via the CI publish job. The resolver serves them at `/dl/<template-name>` — the same endpoint used for extensions.

## Shipping Agent Content

An extension can ship the agent instructions that belong to it — skills, worker
profiles, references and helper scripts — under the same version as its
commands and tools. A workspace receives them only when it opts in with
`agentArtifacts: ["extension:<name>"]` ([Agent Workflows](18-agent-workflows.md#how-a-workspace-opts-in)).

### Author the content

1. Write the content in the closed authoring layout under a source directory of
   the extension: `skills/<name>/SKILL.md` (with optional `agents/`,
   `references/` and `scripts/`) and `agents/<name>/{AGENT.md,claude.yaml,codex.toml}`.
1. Declare the content policy in the extension project's `putnami.json`, under
   `options.agent-artifact` (`forbiddenContent`, `requiredSkills`).
1. Declare the contribution in `putnami.extension.json`, in its authored form,
   with `cliContract` 5, the contract a manifest with agent content needs:

```json
{
  "name": "@acme/tooling",
  "cliContract": 5,
  "agentContent": { "path": "agent-content", "source": "agent-src" }
}
```

`source` is what you write; `path` is where the package step writes the built
tree. Never commit `manifestSha256`: the package step writes it.

### Package it

The package step builds the content with the same builder as every agent
artifact, applies the content policy, writes the built tree under `path`,
replaces `source` with `manifestSha256` (the digest of what it built), and
stamps `cliContract` 5. A manifest without `agentContent` keeps contract 4.

| Extension kind | Package job | What ships |
|---|---|---|
| Content only (no runtime, commands, tools, tasks or hooks) | `putnami package`, with `@putnami/scaffold` in the project's `extensions` | One archive, written with identical bytes under every registry platform key |
| Go, with a runtime | `putnami package` from `@putnami/go` | One archive per platform; each carries its own executable and the same content bytes |
| npm | `putnami package --npm` from `@putnami/typescript` | The npm package directory, with the content under `path` |

Packaging fails, and writes no archive, when:

- the content breaks the declared policy or the authoring layout;
- the contribution has no `source`;
- the manifest's `name` differs from the name the package is published under;
- the stage already holds `path` (for example `bin` or `lib`);
- an npm `package.json` declares a `files` list that leaves `path` out;
- the staged content does not match the digest the manifest binds.

### Install and upgrade

A consumer declares the extension and opts into its content. The content always
comes from the directory the extension's commands and tools load from:

- **Registry extension** (declared in `extensions`): `putnami install` installs
  the pinned release, and the content comes from that same release. The lock
  pins the extension manifest, the manifest binds the content manifest, and the
  content manifest binds every file. `putnami upgrade` moves the extension and
  its content together.
- **npm extension** (declared in the root `package.json` `devDependencies`):
  the package manager installs it into `node_modules/<name>`, and its lock pins
  it. `putnami install`, and `putnami upgrade` when it runs the dependency
  phase, materialize the content after the package manager ran. The package's
  `package.json` must carry the extension's name and the version its manifest
  declares.
- **Local extension** (declared by path): the content is built from its
  authored source.

Declare an extension once. A registry entry and a devDependency of the same
name, or a registry entry that `node_modules` also provides, are refused with
nothing written: the two can be different releases
([ADR 0047](adr/0047-extension-owned-agent-content.md)).

## Hooks

Extensions can define lifecycle hooks:

```json
{
  "hooks": {
    "onInstall": {
      "kind": "command",
      "command": "putnami",
      "args": ["cloud", "setup"],
      "cwd": "{workspaceRoot}",
      "timeoutMs": 120000
    },
    "preBuild": {
      "kind": "command",
      "command": "{extensionRoot}/bin/putnami-bun",
      "args": ["run", "{extensionRoot}/bin/pre-build-hook"],
      "cwd": "{projectRoot}",
      "timeoutMs": 120000
    }
  }
}
```

### onInstall Hook

Runs once per extension after `putnami extensions install` materializes the extension. Because `putnami install` runs `extensions install` first, it also runs `onInstall` hooks. Use this for idempotent workspace setup owned by the extension, such as enabling a remote cache:

```json
{
  "hooks": {
    "onInstall": {
      "kind": "command",
      "command": "putnami",
      "args": ["cloud", "setup"],
      "cwd": "{workspaceRoot}"
    }
  }
}
```

Install hooks are workspace-scoped. They inherit stdin/stdout/stderr, receive `PUTNAMI_WORKSPACE_ROOT`, `PUTNAMI_EXTENSION_ROOT`, `PUTNAMI_HOOK=onInstall`, and `PUTNAMI_HOOK_CONTEXT`, and can use template variables such as `{workspaceRoot}`, `{extensionRoot}`, `{outputRoot}`, and `{cacheRoot}`.

### preBuild Hook

Runs once per `(extension, project)` pair before the first job execution. The hook receives a context JSON file with workspace, project, and extension paths. It returns exports and assets via a JSONL `summary` event:

```json
{"v": 1, "type": "summary", "data": {"exports": {"types": "src/__generated__/types.ts"}, "assets": {"schema": "schema.json"}}}
```

The `"v": 1` here is not a stale example: the hook summary wire is version-pinned
at v1 and is **not** part of the runtime-events ladder that
[04-job-execution.md](04-job-execution.md#protocol-version-negotiation)
describes, so a hook binary must not negotiate `PUTNAMI_RUNTIME_EVENTS` for this
channel.

Hook timeouts default to 120 seconds.

#### Hook invocation order

Several extensions can declare `preBuild` for one project, and they share that
project's generated tree — so the order is part of the build contract, not an
implementation detail. Hooks run in ascending `order`, ties broken by extension
name in byte order:

```json
{
  "hooks": {
    "preBuild": {
      "kind": "command",
      "command": "bun",
      "args": ["run", "{extensionRoot}/bin/generate"],
      "order": 100
    }
  }
}
```

- `order` `0` (the default, and what a manifest without the key means) is the
  producer rank: the hook only writes generated sources.
- a higher `order` is the consumer/finalizer rank: the hook must observe what the
  producers generated — for example `@putnami/application`, whose hook imports
  the workload entry point and inventories the finished `.gen` tree.

Declare the rank on the consumer rather than naming producers, and do not rely on
`extensionDependencies`: that is a library relation and can point the opposite
way (`@putnami/web` depends on `@putnami/application`'s API but must generate its
server loaders before `@putnami/application`'s hook loads them).

### CI archive transport

A hosted invocation may provide `PUTNAMI_REGISTRY_PUT_URL` as an HTTP numeric
loopback origin with an explicit port and `/put` path. The same private-broker
contract used by native publishers then routes CLI, extension and template
downloads ahead of the authored Put registry. The broker authenticates upstream;
the installer does not bootstrap a credential provider for this local endpoint.
Committed archive/binary integrity checks still apply. Invalid local endpoints
fail before network access; ordinary remote HTTPS settings retain the existing
workspace registry selection.
