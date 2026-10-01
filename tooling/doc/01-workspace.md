# Workspace

A workspace is the root of a Putnami project. It is a single directory identified by a `putnami.workspace.json` file that defines how projects are discovered, how dependencies are resolved, and how jobs run across the entire codebase.

## Projects

Every project has a physical **Path** relative to the workspace root and a canonical logical **ID** used in CLI output, dependency declarations, and target expressions. Normally the ID is the slash-prefixed path. A complete, non-empty parenthesized path segment is a transparent group folder: it stays in `Path` but is omitted from `ID`.

| Physical `Path` | Logical `ID` |
| --- | --- |
| `typescript/frameworks/web` | `/typescript/frameworks/web` |
| `identity/(workloads)/auth-server` | `/identity/auth-server` |
| `identity/(libs)/identity-client` | `/identity/identity-client` |

Only complete segments are transparent. Names such as `work(loads)`, `(workloads)-legacy`, and `()` remain part of the ID. Group folders can use any non-empty name; `libs` and `workloads` have no special treatment beyond the parentheses convention.

Filesystem operations—including discovery, includes, file ownership, package-manager workspaces, and `projects sync`—continue to use physical paths. ID selectors, aliases, groups, graph keys, and generated artifacts use logical IDs. This is a clean break: Putnami does not register former or physical path-derived forms (for example `/identity/workloads/auth-server` or `/identity/(workloads)/auth-server`) as compatibility ID aliases.

Projects come in two shapes:

- **App** — a runnable or deployable project (web app, API, worker)
- **Library** — shared code consumed by other projects

Use `putnami projects list` to see all projects and their IDs.

## Workspace structure

You can organize projects however you like. Common patterns:

```bash
# Classic apps + packages
apps/<project>
packages/<project>

# Domain-based
domains/<domain>/services/<project>
domains/<domain>/libs/<project>

# Domain-based with transparent grouping folders
domains/<domain>/(workloads)/<project>
domains/<domain>/(libs)/<project>

# Language-first with scopes
go/framework/<project>
typescript/framework/<project>
```

A minimal workspace:

```
my-workspace/
  apps/
    web/
      package.json
  packages/
    shared/
      package.json
  putnami.workspace.json
  package.json
```

## Project discovery

Putnami discovers projects from two sources, in priority order:

1. **Workspace includes** — root `putnami.workspace.json` entries that resolve to direct projects or autonomous scopes
2. **Package manager workspaces** — the root `package.json` `workspaces` field

If a project appears in multiple sources, the first source wins.

## Scopes

Scopes are an organizational layer between the workspace and projects. A scope is a directory with a `putnami.json` that declares its own includes, tags, extensions, and groups. This decentralizes ownership — each subtree manages its own config instead of everything living in the root.

Register scope directories in `putnami.workspace.json`:

```json
{
  "includes": ["go/framework", "typescript/framework", "typescript/samples", "tooling/cli", "platform/ci"]
}
```

Each scope directory has its own `putnami.json`:

```json
{
  "includes": ["app", "http", "sql", "inject"],
  "line": { "tag": "go/v{version}" },
  "tags": ["go"],
  "extensions": ["@putnami/go"],
  "publishConfig": {
    "go": { "namePattern": "go.putnami.dev/{name}" }
  },
  "groups": {
    "go-core": "/go/framework/app,/go/framework/inject"
  }
}
```

### A scope is an import boundary

A project is importable only by the projects of its own scope, unless its
`putnami.json` declares `"visibility": "public"`. The boundary applies to real
imports (a `go.mod` require, a `package.json` dependency), never to a declared
`dependencies` entry. A generated client is importable from every scope when
its `client.putnami.json` names a provider in the workspace and its
`putnami.json` declares no `visibility` (see
[Generated Clients](07-generated-clients.md)).

`options.workspace.visibility` in `putnami.workspace.json` decides what an
import across a boundary does:

| Value | Effect |
|---|---|
| `enforce` | The default. Every command that plans over the graph fails; `install`, `deps` and `projects sync` still run. |
| `report` | Each import is a warning, and the exit code does not change. |
| `off` | The check does not run. |

To fix a refused import, mark the imported project `public`, move it into the
importer's scope, or drop the import.

### Version lines

A scope that declares a `line` block is a **version line**: its projects are
versioned and tagged together, under the tag pattern the block names. The
pattern carries exactly one `{version}`; `"line": {}` takes the default
`<scope path>/v{version}`, and a workspace where no scope declares a line is one
line whose pattern is `v{version}`.

A project belongs to the **nearest ancestor** line, exactly one. A line inside
another line is refused when the workspace loads, because a project cannot be
versioned by two tags. `putnami scopes list` prints each scope's pattern in a
`LINE` column.

Versions are derived from git — the line's last tag advanced by the conventional
commits that touch the line — and are never declared in a configuration file.
See [Version Management](../cli/doc/14-version-management.md).

### Registries

`registries` in `putnami.workspace.json` declares one entry per ecosystem, and
is the single source of truth for every registry endpoint the workspace uses:

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

The shape of each entry is owned by the extension that owns that ecosystem, and
the native files bun and Go read are generated from these entries. A project
overrides one entry in its own `putnami.json`, replacing it whole for that
ecosystem. See [Configuration](../cli/doc/05-configuration.md#registries).

### Scope inheritance

Scopes form an inheritance chain from the workspace root down to each project:

- **Tags**: merged (union) — a project inherits all tags from its ancestor scopes
- **Extensions**: deepest scope wins (replaces parent)
- **PublishConfig**: deepest scope wins per channel
- **Line**: not inherited — a project takes its nearest ancestor line
- **Registries**: a project entry replaces the workspace entry for that ecosystem
- **Groups and aliases**: aggregated across all scopes (duplicates are errors)

Scopes are most useful when your workspace has many projects organized by language or domain. Instead of listing 50+ projects in the root config, you declare a few scopes that each manage their own subtree.

Use `putnami projects list` to inspect the projects resolved from workspace and scope includes.

## Tags and filtering

Projects can declare tags in `putnami.json` or in `package.json` under the `putnami` key:

```json
{
  "name": "@myorg/my-app",
  "tags": ["frontend", "deployable"]
}
```

Use tags to narrow which projects a job targets:

```bash
putnami build --all --tag frontend
putnami test --all --exclude-tag e2e
putnami build --all --tag backend,deployable
```

Tags listed in `putnami.workspace.json` under `disable.tags` are excluded from all selections by default. Use `--tag` to explicitly include them:

```json
{
  "disable": {
    "tags": ["e2e"]
  }
}
```

## Dependency graph and `--impacted`

Putnami maintains a dependency graph across all projects in the workspace. The `--impacted` flag uses this graph to run jobs only on the tasks affected by git changes compared to a resolved baseline.

Selection is task-level. A changed file selects the TASKS whose declared inputs read it — the same declarations the build cache keys on — and a file no declaration selects runs nothing at all, so a `README.md` no action reads selects no project.

Impact then propagates over task edges: a project that depends on a selected one runs the tasks whose `^` step reference names a task the other runs, plus the tasks that depend on those inside it. `^generate` in a step of `build` names the `build~generate` of every dependency, so a changed source reaches every importer's `generate` and what sits behind it, while a changed `_test.go` reaches no importer — nothing references a `test` step. A claim Putnami cannot attribute to a task keeps its whole project, and every task of it runs.

`tooling/cli/doc/06-workspace-and-projects.md` states the full rule, and `tooling/cli/doc/adr/0044-selection-is-task-level.md` the decision.

```bash
# Build only what changed
putnami build --impacted

# Preview what would run
putnami build --impacted --plan
```

When `--baseline` is omitted, Putnami resolves the baseline from workspace `baseline`, the branch upstream, `origin/HEAD`, then local `main` or `master`. If none resolve, `--impacted` falls back to `--all`; use `--impacted-strict` to fail instead.

## Aliases and groups

Define shortcuts in `putnami.workspace.json` for frequently used targets:

```json
{
  "projectAliases": {
    "web": "/typescript/frameworks/web",
    "cli": "/tooling/cli"
  },
  "groups": {
    "frontend": "/typescript/frameworks/web,/typescript/frameworks/ui",
    "all-go": "/go/..."
  }
}
```

Aliases and groups can also be defined in scope-level `putnami.json` files.

## Introspection commands

```bash
putnami workspace describe          # Workspace name, root, project and extension counts
putnami projects list               # All projects with paths and tags
putnami projects describe <project> # Dependencies, exports, config for one project
```

All introspection commands support `--output=jsonl` for machine-readable output.
