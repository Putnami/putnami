# Configuration

The CLI uses JSON configuration files for configuration, with a multi-scope merging system that allows global, workspace, and local overrides.

## Config File Location

Configuration is loaded from three scopes, merged in order (later overrides earlier):

| Scope | Path | Purpose |
|-------|------|---------|
| Global | `~/.putnami/config.json` | User-wide defaults |
| Workspace | `<workspaceRoot>/putnami.workspace.json` | Shared workspace settings |
| Local | `<cwd>/putnami.json` | Directory-specific overrides (if cwd differs from workspace root) |

The workspace root is detected by walking upward from the current directory, looking for `putnami.workspace.json` or `package.json` with a `workspaces` field.

## Workspace Configuration

The workspace-level `putnami.workspace.json` schema:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-workspace.json",
  "name": "my-workspace",
  "output": "jsonl",
  "quiet": false,
  "verbose": false,
  "includes": ["go/framework", "typescript/framework", "tooling/cli", "platform/ci"],
  "extensions": ["@putnami/typescript", "@putnami/go"],
  "templates": ["typescript-library", "go-server:^1.0.0"],
  "registries": {
    "npm": { "publish": "https://npm.example.com", "scopes": { "@acme": "https://npm.example.com" } },
    "go":  { "origin": "https://go.example.com", "proxy": ["https://proxy.golang.org", "direct"] },
    "oci": { "publish": "oci.example.com/acme" },
    "put": { "registry": "https://put.example.com" }
  },
  "aliases": {
    "dev": "serve",
    "ci": "lint,test,build"
  },
  "options": {
    "*": { "minify": false },
    "build": { "target": "es2022" },
    "@putnami/typescript": { "strict": true },
    "@putnami/typescript:build": { "sourcemap": true }
  },
  "hooks": {
    "cli": {
      "before": ["echo starting"],
      "after": ["echo done"]
    },
    "commands": {
      "build": {
        "before": ["echo pre-build"],
        "after": ["echo post-build"]
      }
    }
  },
  "disable": {
    "extensions": ["@putnami/python"],
    "jobs": ["publish"],
    "tags": ["experimental"]
  }
}
```

### Field Reference

| Field | Type | Description |
|-------|------|-------------|
| `name` | `string` | Workspace name |
| `output` | `string` | Default output format (`"jsonl"`, `"cloud-logging"`) |
| `baseline` | `string` | Default git ref `--impacted` diffs against when `--baseline` is not provided |
| `epicBranches` | `string[]` | Long-lived integration branch names or globs (e.g. `"epic/*"`); `--impacted` measures a branch against the nearest of these or the trunk — never the branch itself (see [Impact Analysis](06-workspace-and-projects.md#impact-analysis)). A branch on an epic branch also skips the mixed-intent warning (see [Change size and mixed intent](06-workspace-and-projects.md#change-size-and-mixed-intent)) |
| `quiet` | `bool` | Suppress non-essential output by default |
| `verbose` | `bool` | Enable verbose output by default |
| `includes` | `string[]` | Workspace membership paths. Each entry may be an autonomous scope or a direct project |
| `scopes` | `string[]` | Deprecated compatibility alias for autonomous scope entries |
| `projects` | `string[]` | Deprecated compatibility alias for direct project entries |
| `extensions` | `string[]` | Explicitly referenced extension names (use `/path` IDs) |
| `templates` | `string[]` | Installable template names (`"name"` or `"name:constraint"`) |
| `registries` | `map[string]object` | One entry per ecosystem id (see [Registries](#registries)) |
| `aliases` | `map[string]string` | Custom command aliases |
| `projectAliases` | `map[string]string` | Map short names to project IDs for target expressions (see [Target Expressions](06-workspace-and-projects.md#target-expressions)) |
| `groups` | `map[string]string` | Named project sets as target patterns (see [Target Expressions](06-workspace-and-projects.md#target-expressions)) |
| `options` | `map[string]map[string]any` | Default job parameters (see Options Merging) |
| `hooks` | `HooksConfig` | CLI and command lifecycle hooks (see [Workspace Hooks](#workspace-hooks)) |
| `disable` | `DisableConfig` | Disable extensions, jobs, or tags globally |

### Options Merging

The `options` map provides default parameter values for jobs. Keys follow a specificity hierarchy:

| Key Pattern | Applies To |
|-------------|------------|
| `"*"` | All commands across all extensions |
| `"build"` | The `build` command from any extension |
| `"@putnami/typescript"` | Any command from `@putnami/typescript` |
| `"@putnami/typescript:build"` | Only the `build` command from `@putnami/typescript` |

Values merge in order of specificity: `*` → command → extension → extension:command. More specific keys override less specific ones.

Remote-cache authority can be set per command with the reserved
`cache-trust` option. The CLI flag and environment variable still have higher
precedence:

```json
{
  "options": {
    "build": { "cache-trust": "ci" },
    "test": { "cache-trust": "none" }
  }
}
```

For a multi-command invocation, Putnami applies the strictest configured value
to the whole run because the provider session is shared. Resolution order is
`--cache-trust` → `PUTNAMI_CACHE_TRUST` → per-command option → `ci` in CI or
`any` locally.

How each task's exported CPU ceiling is derived is set the same way, with the
reserved `cpu-policy` option:

```json
{
  "options": {
    "*": { "cpu-policy": "critical-path" }
  }
}
```

`critical-path` (the default) sizes a task's ceiling by its share of the plan's
makespan, floored by the parallelism its history proves it used — pick it when
one long chain bounds the run. `measured` uses that measured occupancy alone —
pick it for a plan of many comparable tasks, or on a shared machine where
per-task accounting matters more than one chain's makespan. Resolution order is
`--cpu-policy` → `PUTNAMI_CPU_POLICY` → per-command option → `critical-path`.
Unlike `cache-trust`, the ceiling policy is run-scoped rather than merged: two
commands in one invocation configuring different policies is an error. Both
policies are execution hints and never affect cache keys; see
[job execution](04-job-execution.md) for why the default is `critical-path`.

### Generated Schema Commit Regime

Whether a project TRACKS its generated schema artifacts (`schema/*.json`) or
keeps them in the gitignored `.gen/` tree is a per-project decision, declared in
that project's `putnami.json`:

```json
{
  "options": {
    "generate": {
      "schema": false
    }
  }
}
```

`false` keeps config and framework schema artifacts in `.gen/` and suppresses the
committed `schema/` copy; `true` opts into tracking them. Declare it in the
project's own `putnami.json` — it describes generated-artifact policy for that
project, not a user or CI preference.

**Recommended default: `false` for applications.** A tracked generated file is a
build output that a build can dirty, so any job that regenerates it — including a
job for a different project that happens to run the same generator — can leave
the worktree modified and fail a gate that never touched that project. An
application's manifest has no external reader, so tracking it buys nothing. Track
schemas when a CONSUMER reads them from the repository: a library whose
dependents aggregate its capability manifest, or a spec reviewed as an API
contract in pull requests.

Declare the regime explicitly either way. The option currently defaults to
`true` when absent, so an undeclared project tracks artifacts by accident rather
than by decision; `putnami doctor` reports that as
`doctor.undeclared_schema_commit` for any project that commits generated schemas
without declaring it.

#### What a committed generated artifact may contain

A tracked generated file must be a function of its OWN project's declared inputs
and of nothing else in the workspace. Content derived from workspace-wide state
makes an unrelated edit re-stamp the file, and the resulting failure is reported
against whichever job noticed the dirty worktree rather than against the change
that caused it. `putnami doctor` reports three such classes as
`doctor.committed_manifest_stability`:

| Class | Example | Instead |
|---|---|---|
| Workspace version string | `info.version` inheriting the workspace version | Declare `version` (or `options.openapi.version`) in the project, or accept the constant default |
| Dependency-closure enumeration | `packages[]` listing every reachable module | The manifest names only packages that contributed to it; the closure stays in `.gen/version.json` |
| Source binding | a `source-v1:sha256:…` digest of the project tree | Nothing — freshness belongs to the indexer, not to a committed file |

Both codes are advisory: they never block a profile, and either can be waived per
project (and per field) in `doctor.waivers.json`.

### Workspace Hooks

`hooks` runs shell commands around a whole `putnami` invocation. `hooks.cli`
brackets every run; `hooks.commands.<name>` brackets the runs that include that
command, and `hooks.commands."*"` brackets all of them. Within a phase, the CLI
hook runs outermost:

```
hooks.cli.before → hooks.commands.<name>.before → jobs → hooks.commands.<name>.after → hooks.cli.after
```

A failing `before` hook aborts the run before any workspace state is touched. An
`after` hook failure can only turn a success into a failure — it never masks a
code the run already produced.

#### Execution contract

Workspace hooks run under the same contract as extension hooks
([Extensions](07-extensions.md#hooks)):

| | Behavior |
|---|---|
| Shell | `sh -c` — **not** a login shell. `/etc/profile` and `~/.profile` are not sourced, so the same `putnami.workspace.json` behaves the same on every machine and in CI. On Windows, `sh` comes from Git for Windows; when it is not on `PATH`, the hook fails with an error that names Git for Windows. |
| Working directory | The **workspace root**, always. A relative hook path such as `./scripts/pre.sh` means the same thing whichever subdirectory you invoked `putnami` from. |
| Timeout | `timeoutMs` for the group, defaulting to **120 000 ms**. A hook that outruns it is killed and fails the run. |
| Environment | The CLI's environment plus `PUTNAMI_WORKSPACE_ROOT` and `PUTNAMI_HOOK`. |
| Streams | stdin, stdout and stderr are inherited. |

`PUTNAMI_HOOK` is the config path that declared the command —
`hooks.cli.before`, `hooks.commands.build.after` — so one script can serve
several phases. Workspace hooks get no `PUTNAMI_PROJECT_ROOT` or
`PUTNAMI_EXTENSION_ROOT`: they bracket an invocation, not a project's job, and
have neither to name.

#### Contract fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `before` | `string[]` | — | Commands to run before the phase, in order |
| `after` | `string[]` | — | Commands to run after the phase, in order |
| `timeoutMs` | `int` | `120000` | Timeout applied to **each** command of this group. Omitted or `<= 0` uses the default |
| `loginShell` | `bool` | `false` | Run this group's commands through `sh -lc`, sourcing the user's profile first |

```json
{
  "hooks": {
    "cli": {
      "before": ["./scripts/preflight.sh"],
      "after": ["./scripts/report.sh"],
      "timeoutMs": 30000
    },
    "commands": {
      "build": {
        "before": ["nvm use && npm run codegen"],
        "loginShell": true
      }
    }
  }
}
```

Set `loginShell` only when a hook needs a binary that a version manager (nvm,
asdf, mise) installs on `PATH` from your profile. It is off by default because a
login shell makes the same repo behave differently per developer dotfile and
makes local diverge from CI. Prefer an absolute path or a workspace-managed
toolchain over turning it on.

Config scopes merge differently for the two halves: `before` and `after` lists
**accumulate** across `~/.putnami/config.json` and `putnami.workspace.json`,
while `timeoutMs` and `loginShell` are **replaced** by the later scope. A
workspace file that only adds commands leaves a global contract intact, and one
that states `"loginShell": false` turns a global opt-in back off.

#### After-hooks on Ctrl-C

`after` hooks are cleanup, so they still run when the run is interrupted with
Ctrl-C or SIGTERM: they are detached from the run's cancellation and given a
shared 5-second budget instead. That budget is a hard bound — a cleanup hook
that ignores cancellation is killed rather than allowed to make Ctrl-C feel
unresponsive, and a second Ctrl-C force-quits immediately.

### Disable Config

Selectively disable parts of the workspace:

```json
{
  "disable": {
    "extensions": ["@putnami/python"],
    "jobs": ["publish", "@putnami/typescript:generate"],
    "tags": ["experimental", "deprecated"]
  }
}
```

- `extensions` — Completely skip these extensions during planning.
- `jobs` — Skip specific jobs. Can be a bare command name (`"publish"`) or qualified (`"@putnami/typescript:generate"`).
- `tags` — Default exclude tags. Projects with these tags are excluded unless explicitly included with `--tag`.

### Registries

`registries` declares **one entry per ecosystem**, keyed by the ecosystem id
(`^[a-z][a-z0-9-]{0,31}$`). It is the single source of truth for every registry
endpoint the workspace uses: no URL is hard-coded in an extension and none is
hand-written into a dotfile. The native files bun and Go read (`.npmrc`,
`GOPROXY`) are generated from these entries.

```json
{
  "registries": {
    "npm": { "publish": "https://npm.example.com", "scopes": { "@acme": "https://npm.example.com" } },
    "go":  { "origin": "https://go.example.com", "proxy": ["https://proxy.golang.org", "direct"] },
    "oci": { "publish": "oci.example.com/acme" },
    "put": { "registry": "https://put.example.com" }
  }
}
```

The **shape of each entry is owned by the ecosystem's profile**, declared by the
extension that owns that ecosystem in its `putnami.extension.json`: the CLI
validates an entry against the JSON Schema the profile carries and knows nothing
else about it. `npm` is owned by `@putnami/typescript`, `go` by `@putnami/go`,
`oci` by the extension SDK's shared publisher, and `put` by `@putnami/cloud`. An
ecosystem no installed extension declares is an error, which is what keeps a
typo from silently publishing nowhere.

| Ecosystem | Fields | Read by |
|---|---|---|
| `npm` | `publish`, `scopes` (scope → registry URL) | the publish registry and the `@<scope>:registry=` lines written into `.npmrc`; `upgrade` resolves each package's dist-tag on its scope's registry |
| `go` | `origin`, `proxy` (ordered list) | the module origin a publish uploads to and `upgrade` resolves `@v/<channel>.info` on, per module; `proxy` drives `GOPROXY` (default `https://proxy.golang.org,direct`) |
| `oci` | `publish` | the image repository a Docker publish pushes to by digest |
| `put` | `registry` | the endpoint `upgrade --cli`, `upgrade --extensions`, `extensions install`, and template downloads resolve archives from, with the user's credential |

`putnami init --extension ts` declares the entry the TypeScript templates'
framework dependencies install from:
`"npm": { "scopes": { "@putnami": "https://npm.putnami.dev" } }`. Without it,
`.npmrc` names no registry for `@putnami`, and bun resolves the framework
packages from npmjs.org, which does not serve them. `putnami init --force` keeps
every entry the existing workspace config declares, whole. A workspace created
before this entry existed adds it by hand.

A project overrides an entry in its own `putnami.json` under the same key. The
override **replaces the workspace entry whole** for that ecosystem: a project
that names a different npm registry does not inherit the workspace's `scopes`
map. Environment overrides still exist for one-off use — `DOCKER_REGISTRY`,
`GO_REGISTRY_URL`, `PUTNAMI_REGISTRY_URL` — and rank below the command-line flag
and above nothing else.

An external registry (npmjs.org, docker.io) is outside the visibility chain.
Direct native publication uses that registry's credentials and access control.
For a managed mirror, declare
`distribution.registries.<ecosystem>.mirror.to` in `putnami.ci.json`, for example
`"npm": {"mirror": {"to": "https://registry.npmjs.org"}}`. The release-set
provider authorizes this destination and copies eligible public members using
its configured native registry credential. The CLI carries the destination in
the release request; it never copies a private artifact just because a mirror
was declared. Mirror destinations are bounded registry hosts/paths or HTTPS
URLs without credentials, queries or fragments.

## Breadcrumb Configuration

Intermediate `putnami.json` files placed at directory boundaries define inheritable config for all projects below them. This is useful for applying shared tags, extensions, or publish config to a subtree of projects without repeating it in each project.

### Breadcrumb Schema

```json
{
  "line": { "tag": "ts/v{version}" },
  "includes": ["application", "web"],
  "tags": ["typescript", "framework"],
  "extensions": ["/typescript/extension"],
  "publishConfig": {
    "npm": { "namePattern": "@putnami/{name}" }
  },
  "projectAliases": {
    "web": "/typescript/frameworks/web",
    "cli": "/tooling/cli"
  },
  "groups": {
    "frontend": "/typescript/frameworks/web,/typescript/frameworks/ui",
    "all-ts": "/typescript/..."
  }
}
```

### Breadcrumb Fields

| Field | Type | Merge | Description |
| ----- | ---- | ----- | ----------- |
| `line` | `object` | Local only | Declares this scope a **version line**: its projects are versioned and tagged together under `tag`, a pattern carrying exactly one `{version}` (default `<scope path>/v{version}`). Not inherited, and refused on an `activate: true` scope or inside another line. See [Version Management](14-version-management.md#version-lines) |
| `activate` | `boolean` | Local only | When `true`, the scope directory is also registered as an activation target — extensions run against the scope itself in addition to its `includes`. See [Activated Scopes](#activated-scopes). |
| `includes` | `string[]` | Local only | Project paths relative to this scope directory |
| `projects` | `string[]` | Local only | Deprecated compatibility alias for `includes` |
| `tags` | `string[]` | Union | Tags applied to all projects below this directory |
| `extensions` | `string[]` | Deepest wins | Extensions activated for projects below |
| `publishConfig` | `map[string]map` | Deepest wins | Publish configuration per channel |
| `projectAliases` | `map[string]string` | Aggregated | Short names mapping to project IDs |
| `groups` | `map[string]string` | Aggregated | Named sets of project patterns |
| `distribution` | `{ "visibility": ... }` | Deepest wins | Distribution level of the projects below that declare none. See [Distribution visibility](#distribution-visibility) |

### Inheritance Rules

Breadcrumb `putnami.json` files are scanned from the workspace root down to each project's parent directory. The merge strategy differs by field:

- **Tags** — merged as a union through the chain. A project inherits tags from all ancestors.
- **Line** — not merged. A project belongs to the **nearest ancestor** scope that declares a `line` block, exactly one; a workspace where no scope declares one is a single line.
- **Extensions** — replaced by the deepest (closest to project) breadcrumb that declares them.
- **PublishConfig** — replaced per channel by the deepest breadcrumb.
- **Distribution** — replaced by the deepest breadcrumb that declares it. A project's own `distribution` wins over every breadcrumb.
- **ProjectAliases and Groups** — not inherited per project. Instead, they are aggregated across the entire workspace into a global index. Duplicate alias or group names across different breadcrumbs produce an error.

### Example Directory Layout

```text
workspace/
├── putnami.workspace.json              # workspace config (includes, aliases, etc.)
├── typescript/
│   ├── putnami.json                    # breadcrumb: tags=["typescript"], extensions=["/typescript/extension"]
│   ├── frameworks/
│   │   ├── putnami.json                # breadcrumb: tags=["framework"], publishConfig for npm
│   │   ├── application/putnami.json    # project config
│   │   └── web/putnami.json            # project config
│   └── samples/
│       └── putnami.json                # breadcrumb: tags=["sample"]
└── go/
    └── putnami.json                    # breadcrumb: tags=["go"], extensions=["/go/extension"]
```

In this layout, `typescript/frameworks/web` inherits:
- Tags: `["typescript", "framework"]` (union of both ancestors)
- Extensions: `["/typescript/extension"]` (from `typescript/` breadcrumb)

### Activated Scopes

By default a scope `putnami.json` (one with `includes`) is *not* an activation target — only its declared sub-projects run jobs. Setting `activate: true` promotes the scope directory itself to a project, so extensions run against scope-shaped artifacts (e.g. workspace-level infra) without inventing a leaf project to host them.

```json
// cloud/putnami.json
{
  "name": "cloud",
  "activate": true,
  "includes": ["workloads/api", "workloads/worker"],
  "extensions": ["/cloud/extension"]
}
```

When `activate: true`:

- The scope directory is registered as a project with ID equal to its path (e.g. `/cloud`). Job matching applies the standard activation-files check against the scope dir, so jobs only fire when their expected artifacts (e.g. `infra/pulumi/Pulumi.yaml`) exist there.
- The scope's `name`, `tags`, `extensions`, `options` apply to the scope-self project in addition to being inherited by children.
- An implicit dependency edge is added from each direct include back to the scope-self. It orders the schedule: workspace-level artifacts run before the workloads that consume them, and `dependsOn: ["^<job>"]` from a child resolves naturally to the scope's job. Cycles introduced by these edges are reported as a workspace load error. The edge does not propagate `--impacted`: a change to the scope-self's own files (`doc/`, `infra/`) selects the scope-self only. A workload that consumes scope-self outputs declares `"dependencies": ["/cloud"]`, which turns the edge into a full one. The scope's `putnami.json` is the exception, and not through the edge: every include inherits its `tags`, `extensions` and name pattern from it, so a change to it selects the scope-self and every include, exactly as it does for a scope without `activate`.

Targeting:

- `/cloud` — scope-self only.
- `/cloud/...` — scope-self plus all descendants.

`activate: true` is opt-in; existing scopes that omit it behave exactly as before.

## Project Configuration

Each project can have its own `putnami.json`:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "name": "@myorg/my-app",
  "version": "1.2.1",
  "description": "Main web application",
  "tags": ["frontend", "deploy"],
  "dependencies": ["/packages/shared-lib", "/packages/ui"],
  "extensions": ["/typescript/extension"],
  "runsWith": ["@myorg/api-server"],
  "publish": ["npm", "docker"],
  "build": {
    "assets": [{ "from": "public" }],
    "compile": { "entrypoint": "src/index.ts" }
  },
  "options": {
    "build": { "minify": true },
    "@putnami/typescript": { "strict": true }
  },
  "tasks": {
    "lint": { "cpuWeight": 4 },
    "lint~golangci-lint": { "timeoutMs": 900000 },
    "build~transpile": { "cpuWeight": 2 }
  },
  "jobs": {
    "custom-check": {
      "kind": "command",
      "command": "bash",
      "args": ["-c", "echo custom check"],
      "cwd": "{projectRoot}"
    }
  },
  "disable": {
    "jobs": ["generate"]
  }
}
```

### Project Field Reference

| Field | Type | Description |
|-------|------|-------------|
| `name` | `string` | Project name (used in dependency references and CLI output) |
| `type` | `"application" \| "library" \| "image"` | Resolved project kind; `image` is a daemonless, spec-packaged OCI artifact |
| `description` | `string` | Human-readable description |
| `tags` | `string[]` | Tags for filtering (`--tag`, `--exclude-tag`) |
| `dependencies` | `string[]` | Other workspace projects this project depends on (use `/path` IDs, e.g. `"/typescript/frameworks/utils"`) |
| `extensions` | `string[]` | Extensions this project uses (use `/path` IDs, e.g. `"/typescript/extension"`) |

| `runsWith` | `string[]` | Projects that should serve alongside this one |
| `publish` | `string[]` | Publish channels (e.g., `"npm"`, `"docker"`) |
| `main` | `string` | Main entry point |
| `bin` | `any` | Binary entry points |
| `exports` | `any` | Package exports map |
| `build` | `BuildConfig` | Build configuration (assets, compile settings) |
| `options` | `map[string]map[string]any` | Per-command/extension option overrides |
| `tasks` | `map[string]ProjectTaskTuning` | Per-task execution tuning |
| `jobs` | `object` | Accepted but ignored — see [Project `jobs` is not implemented](#project-jobs-is-not-implemented) |
| `disable` | `ProjectDisableConfig` | Disable specific jobs for this project |
| `distribution` | `{ "visibility": "internal" \| "private" \| "public" }` | Who may pull what this project publishes. See [Distribution visibility](#distribution-visibility) |

### Distribution visibility

`distribution.visibility` states who may pull the artifacts a project publishes:
`internal`, `private`, or `public`. It applies to every release-set member the
project produces, for example both its npm package and its image.

```json
// typescript/framework/web/putnami.json
{
  "name": "@putnami/web",
  "distribution": { "visibility": "public" }
}
```

A scope `putnami.json` declares the same block for every project below it:

```json
// typescript/framework/putnami.json
{
  "includes": ["web", "ui"],
  "distribution": { "visibility": "public" }
}
```

This is not the top-level `visibility` field. `visibility` is an import boundary
inside the workspace (see [Workspace and Projects](06-workspace-and-projects.md#visibility-who-may-import-a-project));
`distribution.visibility` decides who may pull a published artifact.

A project's level resolves in this order:

1. The project's own `distribution.visibility`.
2. The `distribution.visibility` of the deepest scope above it.
3. The last `distribution.members[]` rule in `putnami.ci.json` that selects it.

The level fills the member link of the release-set visibility chain, the finest
one: the release-set provider still resolves the chain and never narrows a level
it already published. When a `putnami.json` level and a `members[]` rule both
answer the same project and disagree, `publish` fails with an error that names
both fields. Remove the rule, or make the two agree. An unknown level, or a
`distribution` block without a level, fails validation and fails `publish`.

### Image projects and project-derived bases

An image project packages an OCI filesystem spec directly, without compiling
application source, invoking Docker, or interpreting a Dockerfile:

```json
{
  "name": "images/toolchain",
  "type": "image",
  "extensions": ["@putnami/go"],
  "publish": ["docker"],
  "options": {
    "package": {
      "image": {
        "base": "registry.example.com/team/base@sha256:<64-hex-digest>",
        "platform": "linux/amd64",
        "layers": [
          { "tarball": "rootfs/toolchain.tar" },
          {
            "files": [
              { "source": "bin/entrypoint", "path": "/usr/local/bin/entrypoint", "mode": 493 }
            ]
          }
        ],
        "env": ["PATH=/usr/local/bin:/usr/bin"],
        "entrypoint": ["/usr/local/bin/entrypoint"]
      }
    }
  }
}
```

The base must be digest-pinned. The complete base reference, platform, ordered
runtime configuration, layer bytes, destination paths, and modes form one full
SHA-256 content version. Packaging always writes the local candidate under
`.putnami/out/<project>/package/docker/oci`; its manifest records the candidate
digest, platform, content identity, and relative layout only. It never resolves
an output registry, requests write credentials, pushes, or claims publication.
Publishing resolves its target from `registries.oci.publish`, requests the exact
workspace/package/`publish` lease, pushes or reuses the candidate by digest,
verifies the remote digest, and writes the separate publish-owned record. An
image is tagged with its version and with its content tag, and with nothing
else: channel tags are written by the release projection, not by the publisher.

The resolution order is `--docker-registry` on the command line, then
`registries.oci.publish`, then `DOCKER_REGISTRY`, then the managed target
`oci.putnami.dev/<workspace>/<flattened-project>`. `--docker-registry` is an
override for a one-off push, not the place a workspace declares its registry.
Older projects may keep a generic target at `options.package.image.registry`;
package still does not resolve, authenticate to, contact, or serialize an
output registry.

A workload can consume that local candidate artifact as its base:

```json
{
  "options": {
    "package": {
      "dockerBaseProject": "/images/toolchain"
    }
  }
}
```

`dockerBaseProject` is a real workspace dependency edge. It must resolve inside
the workload's dependency closure, its image package runs first, and a base
input change impacts and repackages the workload. The workload reads the base
project's typed local OCI layout and verifies its candidate digest before
composition. A cold transitive package therefore needs no output registry; no
config file or Dockerfile is rewritten.

### Project Task Tuning

Projects can tune scheduler hints for expensive tasks without changing cache keys:

```json
{
  "tasks": {
    "lint": { "cpuWeight": 4 },
    "lint~golangci-lint": { "timeoutMs": 900000 },
    "build~transpile": { "cpuWeight": 2 }
  }
}
```

Task keys are either a full pipeline step name, such as `build~transpile`, or a command name, such as `lint`, which applies to every step of that command unless a more specific step entry exists. Each tuning field resolves independently, so a step can set `cpuWeight` while inheriting `timeoutMs` from its command entry. `cpuWeight` is a relative multiplier on deterministic, weight-independent history demand: for otherwise-identical task history, `4` receives four times the CPU budget of the default `1`, capped by machine capacity. Changing or removing the multiplier takes effect on the next comparable run; a prior weighted grant is never persisted as unweighted demand.

`timeoutMs` is a positive integer scheduler deadline in milliseconds for that
project's copy of the task. A full step entry wins over a command entry. `0` and
negative values are rejected: they are extension-manifest sentinels for the CLI
default and no deadline, and a project config may not silently disable hang
protection. Task tuning is execution-only, so changing either field does not
change a cache key. The exception is a task that reads the project config as
text: a task that rewrites its sources, such as a formatter, and a task whose
input pattern selects the config with a glob in its last segment, such as a
linter's `**/*.json`. Every byte of the config, `tasks` included, is part of
that task's key. See
[A project config is hashed through the reading task's scope](10-caching.md#a-project-config-is-hashed-through-the-reading-tasks-scope).

Putnami gives a task subprocess its resolved deadline in
`PUTNAMI_TASK_DEADLINE_MS` (after batch-group scaling). A runtime that owns a
second timeout must derive it below the scheduler deadline, leaving room for
process startup and result reporting; otherwise the scheduler can kill the task
first and hide the tool's useful timeout diagnostic. The Go extension does this
for golangci-lint when the user has not supplied `--timeout` explicitly.

### Project `jobs` is not implemented

A project cannot define or override jobs. The `jobs` key is still accepted by the parser, so older configs keep loading, but it never reaches planning: the CLI prints a warning naming every project that declares one and then ignores it.

```
putnami: warning: 1 project(s) (packages/app) declare `jobs` in putnami.json, but project-level jobs are NOT implemented — the key is parsed and ignored; use `disable.jobs` to opt out of an extension-provided job, or `tasks` for per-project execution tuning
```

Jobs are provided by extensions. What a project controls is which of them run and how they are scheduled:

- `disable.jobs` — opt out of an extension-provided job for this project (`"lint"` or `"@putnami/typescript:lint"`).
- `tasks` — per-project execution tuning for a job that already exists (see [Project Task Tuning](#project-task-tuning)).
- `options` — per-command and per-extension parameter defaults.

## Build store settings

The machine-global build store's garbage collection is tuned via a `store` section, valid in both `~/.putnami/config.json` (global) and `putnami.workspace.json` (per-repo):

```json
{
  "store": {
    "maxBytes": 21474836480,
    "gcGrace": "2h",
    "maxIdleBuilds": 200
  }
}
```

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `maxBytes` | `int` | Global byte budget across all per-repo stores; GC evicts least-recently-used entries above it | 10 GiB |
| `gcGrace` | `string` | Go duration protecting recently-used / in-flight entries from eviction; must exceed your longest build | `1h` |
| `maxIdleBuilds` | `int` | Evict entries not hit in this many builds, regardless of budget; `0` disables | `100` |

Resolution precedence is **env var > workspace config > global config > default** (e.g. `PUTNAMI_STORE_MAX_BYTES` overrides `store.maxBytes`). The store **location** is not configurable here — set it with the `PUTNAMI_STORE_DIR` env var (machine-specific). See [Caching](10-caching.md) for the full GC model.

## Session retention

How many session records `<workspace>/.putnami/sessions` keeps is set by a `sessions` section, valid in both `~/.putnami/config.json` (global) and `putnami.workspace.json` (per-repo):

```json
{
  "sessions": {
    "keep": 200
  }
}
```

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `keep` | `int` | Session directories retained when the store prunes; minimum 1 | `20` |

Resolution precedence is **workspace config > global config > default**. There is deliberately **no environment variable**, unlike the build store's GC settings: session records are per-worktree, and a committed workspace config already reaches every worktree that writes them. `0` is not an accepted value — it could be read as either "unlimited" or "delete everything", so the schema's minimum is 1.

Raising `keep` bounds a worktree's disk, not a record's lifetime: an agent worktree is deleted with its records whatever the value. Collect what must outlive it with `putnami sessions export` — see [Session Recording](11-session-recording.md#export-sessions).

## Environment Variables

Environment variables provide process-level defaults and CI overrides. Explicit
CLI flags still win over environment variables. Public CLI environment variables
use the `PUTNAMI_` prefix:

| Variable | Description | Equivalent Flag |
|----------|-------------|-----------------|
| `PUTNAMI_OUTPUT` | Default output format | `--output` |
| `PUTNAMI_DEBUG` | Enable debug mode (`true`/`false`) | `--debug` |
| `PUTNAMI_VERBOSE` | Enable verbose mode | `--verbose` |
| `PUTNAMI_QUIET` | Enable quiet mode | `--quiet` |
| `PUTNAMI_NO_COLOR` | Disable colored output | `--no-color` |
| `PUTNAMI_COLOR` | Force colored output | `--color` |
| `NO_COLOR` | Standard no-color convention (any value) | `--no-color` |
| `K_SERVICE` | Google Cloud Run service name (auto-selects cloud-logging output). Read by the CLI process itself; **removed** from test subprocesses — see below | `--output=cloud-logging` |
| `PUTNAMI_NO_AUTO_INSTALL` | Disable first-use auto-install (any value); the first command on a fresh checkout/worktree will not run `putnami install` for you. See [`install`](03-commands.md#first-use-auto-install). | — |
| `PUTNAMI_REGISTRY_URL` | Override the CLI update registry URL (default: `https://put.putnami.dev`) | — |
| `PUTNAMI_STORE_DIR` | Override the machine-global build store location (default: `~/.putnami/store/<repo-id>`) | — |
| `PUTNAMI_STORE_MAX_BYTES` | Global build-store byte budget for GC (default: 10 GiB) | `store.maxBytes` |
| `PUTNAMI_STORE_GC_GRACE` | Grace window protecting recently-used / in-flight cache entries from GC, as a Go duration (default: `1h`) | `store.gcGrace` |
| `PUTNAMI_STORE_MAX_IDLE_BUILDS` | Evict cache entries not hit in this many builds, regardless of budget; `0` disables (default: `100`) | `store.maxIdleBuilds` |
| `PUTNAMI_SOURCE_REVISION` | The full lowercase hex commit a publication is bound to when it is not `HEAD` (a runner building a synthetic merge of that commit); the built tree stays the checkout's. See [Pre-release version shape](14-version-management.md#pre-release-version-shape) | — |
| `PUTNAMI_SOURCE_COMMIT_TIME` | Committer time of `PUTNAMI_SOURCE_REVISION` in unix seconds; required only when the repository does not have that commit | — |

The GC-tuning settings are also configurable in the config file (see [Build store settings](#build-store-settings) below). The store **location** (`PUTNAMI_STORE_DIR`) is environment-only — it is machine-specific and so does not belong in a committed config file.

### Host platform identity in test subprocesses

When `putnami test` runs on a managed runtime — a CI worker that is itself a
Cloud Run service, an App Engine job, a Lambda — that runtime's identity
variables (`K_SERVICE`, `K_REVISION`, `K_CONFIGURATION`, `CLOUD_RUN_*`, `GAE_*`,
`AWS_LAMBDA_*`, …) describe **the machine running the harness**, not the code
under test. Every language extension removes them from the test subprocess
environment, so application code that treats them as a production signal behaves
in a test the same way it does on a laptop.

The CLI's own process still reads them (that is what auto-selects the
`cloud-logging` renderer), and `putnami run` / `putnami serve` still pass them
through — a locally served app is a deployment. Credentials, endpoints and
project/region selectors (`GOOGLE_APPLICATION_CREDENTIALS`, `AWS_ACCESS_KEY_ID`,
`GOOGLE_CLOUD_PROJECT`, …) are never scrubbed, so integration tests keep
working. The full list and the criteria a variable is admitted by live in
[`tooling/extension-sdk/doc/06-hostenv.md`](../../extension-sdk/doc/06-hostenv.md).

Generated config schemas use project-level policy only:
`options.generate.schema=false` selects the gitignored
`.gen/config-schema.json` location; otherwise the committed
`schema/config.json` path is used.

### Precedence Order

From lowest to highest priority:

1. Workspace config defaults (`options` field)
2. Environment variables (`PUTNAMI_*`)
3. CLI flags (`--verbose`, `--output`, etc.)

For color resolution specifically:

1. CLI flag (`--color` / `--no-color`)
2. `PUTNAMI_COLOR` env var
3. `NO_COLOR` / `PUTNAMI_NO_COLOR` env var
4. Auto-detect (TTY → color, pipe → no color)

## Config Inspection

View the fully merged configuration:

```bash
putnami config show
putnami config show --output=jsonl
```

Set individual configuration values:

```bash
putnami config set verbose true
putnami config set output jsonl
```
