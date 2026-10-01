# Workspace and Projects

The CLI operates on a **workspace** — a directory containing multiple **projects** with shared tooling. This document covers workspace detection, project discovery, dependency graphs, filtering, and impact analysis.

## Workspace Detection

The CLI finds the workspace root by walking upward from the current directory, checking each directory for:

1. `putnami.workspace.json` — Putnami's native workspace config file.
2. `package.json` with a `workspaces` field — Node.js workspace convention.

The first match becomes the workspace root. If neither is found, structured commands that don't require a workspace (e.g., `workspace init`, `telemetry`, `completion`) still work; job commands fail with "no workspace found".

A `package.json` root with no `putnami.workspace.json` at or above the current directory gives way to the user scope for a command group that only the user scope provides. See [The User Scope](07-extensions.md#the-user-scope).

## First use after checkout

A fresh checkout or worktree has no local installation marker. The first job
command checks the workspace locks and restores the workspace when necessary.
`putnami install` runs this lifecycle explicitly:

1. Restore extensions from their locked versions and run installation hooks.
2. Check generated agent context and install declared templates and workflows.
3. Prepare the provider index used to resolve projects and dependencies.
4. Pin each toolchain that `go.work` or `package.json#packageManager` declares
   at a release the lock does not pin, so the installers install that release.
5. Run the workspace installers for dependencies, tools and extension setup.
6. Finalize lock metadata and record the successful installation state.

Automatic installation follows the same preparation and setup steps, but keeps
the committed lock metadata unchanged before recording installation success,
except that it adds a pin that `go.work` or `package.json#packageManager`
declares and the lock does not carry yet. Only an explicit `putnami install`
refreshes that metadata or changes a committed pin.

Independent artifact restorations and provider probes run concurrently with a
bounded worker count. Their results are merged in stable order; hooks and shared
metadata writes keep their sequencing. Workspace installers already use the job
scheduler and overlap when their dependencies and resource declarations permit.

MCP uses a smaller preparation path: restore exact read surfaces and prepare the
provider index if absent, without running dependency installers, Cloud setup or
context generation. Concurrent index preparation for one worktree reconciles
under a shared lock and rechecks the index before probing. A running MCP process
reloads an index created or replaced by another CLI process.
Lock waiting reports contention after one second and stops after 60 seconds,
or earlier if the caller cancels or its deadline expires. If filesystem access
prevents opening the lock, CLI synchronization warns and keeps the resolved
view in memory without publishing an index.

Normal installation output shows compact progress and a final list of completed
actions. Use `--verbose` for the detailed transcript. See
[Output and Rendering](08-output-and-rendering.md#installation-progress).
`PUTNAMI_NO_AUTO_INSTALL=1` disables the full automatic installation; `--plan`
and preview-only `--dry-run` do not run it.

## Project Discovery

Membership is **Putnami's**: a project is a project because the workspace config
or a scope config says so. Core reads no language manifest to decide it, and
resolves no language identity itself — that is the workspace probe's answer (see
[Workspace Probe](#workspace-probe) below).

### Strategy 1: Workspace Includes

If `putnami.workspace.json` has an `includes` array, each entry is resolved as either an autonomous scope or a direct project. An autonomous scope is a directory with a `putnami.json` containing an `includes` field. Project paths in a scope are relative to the scope directory:

```json
// root putnami.workspace.json
{
  "includes": ["go/framework", "typescript/framework", "tooling/cli", "platform/ci"]
}

// go/framework/putnami.json — autonomous scope
{
  "includes": ["app", "http", "sql"],
  "tags": ["go"],
  "extensions": ["/go/extension"]
}
```

The scope `go/framework` with project `app` resolves to `go/framework/app`. Scopes can also declare tags, extensions, groups, and aliases that apply to their sub-projects via the scope inheritance chain.

By default a scope is not itself a project — only its `includes` are. To make the scope directory an activation target as well (useful for scope-shaped artifacts like workspace-level infra), set `activate: true`. The scope is then registered as a project with an implicit dependency edge from each direct include, so workspace-level work runs before the workloads that consume it. See [Activated Scopes](05-configuration.md#activated-scopes).

The older workspace fields `scopes` and `projects`, and the older scope-level `projects` field, are still read as compatibility aliases.

The root `package.json` `workspaces` array is **not** a discovery source. It is
npm membership, it is written by the TypeScript extension's own `workspace-sync`
task, and reading it back would put npm's model inside a language-neutral
orchestrator. A directory that is not yet a Putnami member becomes one through
`putnami projects sync`.

### Auto-Sync Discovery

The `putnami projects sync` command walks the workspace tree looking for
directories that carry `putnami.json` — core's own marker — plus every marker the
installed extensions declare in their `workspace.markers` (`package.json`,
`go.mod`, `pyproject.toml`, or anything an out-of-tree extension claims). This is
how `includes` is populated when adopting Putnami in an existing monorepo.
Autonomous scope directories are kept as scope entries rather than direct
projects.

Three exclusions are core's and cannot be overridden: `.git`, `.putnami`, and any
directory git ignores entirely. Everything else an extension lists in
`workspace.excludes` is honored as a walk exclusion.

Adapter excludes are applied as a **union**: a directory any installed adapter
refuses to descend into is skipped for every provider, and for core's own
`putnami.json` marker. Installing the Python extension therefore makes `build/`
undiscoverable workspace-wide; installing the Go extension does the same for
`testdata/` and `vendor/`. This is the conservative direction — the scan can miss
a project, never invent one from a fixture or an install tree — and its one
destructive consequence is closed at the other end: `projects sync --prune` never
removes a member whose directory an adapter excluded, because the scan's silence
about a directory it was told not to enter is not evidence that the project is
gone. Name a project directory something other than another language's build or
fixture directory if you want the scan to find it.

What `projects sync` writes is deliberately narrow:

- **Core** writes Putnami membership (the workspace config's `includes` and each
  scope's `includes`), honors `--prune` and `--skip-install`, aggregates the
  dry-run report, and prints the canonical diff. `--prune` is **refused** (with a
  warning; the rest of the sync still runs) when a provider recorded in
  `.putnami/workspace-index.json` did not resolve for this run, or when extension
  discovery failed outright: the scan would then be blind to every project that
  has no `putnami.json`, and pruning on that answer empties the workspace config.
- **Each extension** writes its own native manifests from its declared
  `workspace.syncTask` — package names and the npm `workspaces` array
  (TypeScript), the `go.mod` workspace-replace closure (Go), the
  `pyproject.toml` `[project] name` (Python). Those tasks run on every sync, not
  only when core found a membership change: core cannot know what is out of sync
  inside a language manifest, so deciding "nothing to do" on a provider's behalf
  would be guessing.
- **One residue** stays in core: the `go.mod` replace closure of the LOCAL
  EXTENSION modules whose runtime must be prepared before any extension can run.
  A local extension is compiled in module mode, so a missing workspace replace in
  its own `go.mod` makes it unbuildable — and therefore makes the sync task that
  would repair it unable to run.

### Workspace Probe

Language identity and dependency edges come from the **workspace probe**: core
asks each extension that declares a `workspace` adapter about the candidate
directories it already knows, and merges the answers.

- The merged answer is stored in `.putnami/workspace-index.json` together with
  the content digest of every metadata input it was resolved from.
- A recorded provider answer also carries the identity of the implementation
  that produced it (`providers[].implementation`: a workspace-local
  extension's runtime input digest, a registry-installed one's version). A
  rebuilt or upgraded provider is asked again on the next command even when
  none of its input files changed; a record without the field is asked once.
- An ordinary load re-hashes those inputs. When they still match, **no extension
  process starts** and the recorded answer is replayed.
- When an input moved, exactly the providers that declare it are re-probed;
  everyone else's recorded answer is carried forward.
- A changed input no provider claims — `putnami.json`, the workspace config, a
  scope config — re-probes all of them, because those files decide membership.
- `--plan` and `--dry-run` may probe in memory but never persist.

A project resolves the same way whether this run probed or replayed the recorded
answer, which is what lets the merged view key the build cache.

### Project Metadata

Each discovered project has:

| Field | Source | Description |
| ----- | ------ | ----------- |
| `ID` | Derived from `Path`, omitting transparent group folders | Canonical logical identity (e.g. `/identity/auth-server`) |
| `Name` | `putnami.json` → `name`, else the probe's source identity, else the scope `namePattern`, else the directory name | Human-readable name |
| `Path` | Relative to workspace root | Physical directory path, including group folders |
| `Tags` | Config → `tags`, the probe, or scope inheritance | Filtering labels |
| `Dependencies` | Config → `dependencies` unioned with the probe's derived edges | Project **names** for probe-derived edges (providers report repo-relative paths, core resolves each to the depended-on project's name), plus the authored entries verbatim — an ID or a name. The graph translates both to IDs. |
| `Extensions` | Config → `extensions`, the probe, or scope inheritance | Extension IDs |
| `RunsWith` | Config → `runsWith` or the probe | Projects that serve alongside |
| `Publish` | Config → `publish` or the probe | Publish channels |
| `Metadata` | The probe, namespaced per extension | Provider-owned data core carries and never interprets |

#### Project ID

Every project has a canonical logical `ID`. For ordinary paths it is the forward-slash-prefixed workspace-relative `Path`: a project at `typescript/frameworks/web` has ID `/typescript/frameworks/web`.

A complete, non-empty path segment wrapped in parentheses is a transparent group folder. It remains part of the physical `Path` but is omitted from the logical `ID`:

| Physical `Path` | Logical `ID` |
| --- | --- |
| `identity/(workloads)/auth-server` | `/identity/auth-server` |
| `identity/(libs)/identity-client` | `/identity/identity-client` |
| `commerce/(internal-tools)/catalog-importer` | `/commerce/catalog-importer` |

The convention applies to any parenthesized segment, not a fixed list of folder names. Parentheses embedded in a normal segment do not make it transparent, and the empty segment `()` is not transparent.

The ID is:

- The primary key for the dependency graph
- Used in CLI output (`putnami projects list`)
- The preferred format for dependency and extension references in `putnami.json`
- The basis for target expressions (see [Target Expressions](#target-expressions))

Physical paths remain authoritative for project discovery, scope and workspace includes, file ownership, filesystem access, package-manager workspaces, and `projects sync`. Current-directory and `./` or `../` selectors resolve through physical paths and return the matching logical IDs. All absolute ID selectors, aliases, groups, graph keys, CLI output, and generated protocol artifacts use logical IDs.

Transparent group folders are a clean break for identity. Former and physical path-derived forms are not registered as aliases: select `identity/(workloads)/auth-server` as `/identity/auth-server`, not `/identity/workloads/auth-server` or `/identity/(workloads)/auth-server`. Workspace loading fails if two physical paths collapse to the same logical ID, and the diagnostic lists every colliding path.

Legacy name-based references (e.g. `@putnami/utils`) are still accepted in `dependencies` and are automatically translated to IDs via a Name→ID lookup during graph construction.

## Dependency Graph

The CLI builds a directed dependency graph from project declarations. This graph powers impact analysis and topological job ordering.

### Graph Construction

The graph uses project IDs as keys (e.g. `/typescript/frameworks/runtime`). Dependencies declared as legacy Names (e.g. `@putnami/runtime`) are automatically translated to IDs during construction.

```go
type DependencyGraph struct {
    deps       map[string][]string  // project ID → IDs of direct dependencies
    dependents map[string][]string  // project ID → IDs of direct dependents
    nodes      map[string]bool      // all project IDs
}
```

Dependencies are declared in each project's `putnami.json` using `/path` IDs, and
unioned with the edges each language provider derives from its own manifest
(`go.mod` requires and replaces, `workspace:` package dependencies):

```json
{
  "dependencies": ["/packages/shared-lib", "/packages/utils"]
}
```

Providers report their derived edges as **repo-relative paths** — that is the
probe protocol's contract, and it is what lets a provider resolve its own module
graph without learning core's naming rules. Core resolves each path to the
depended-on project's **name** before the graph is built, so `Project.Dependencies`
holds names for provider-derived edges and whatever the author wrote (an ID or a
name) for declared ones. Graph construction translates both to IDs.

### Key Operations

| Operation | Algorithm | Description |
| --------- | --------- | ----------- |
| `DependenciesOf(id)` | Direct lookup | Projects that `id` depends on |
| `DependentsOf(id)` | Direct lookup | Projects that depend on `id` — the ordering family, implicit include→scope-self edges included |
| `ImpactDependentsOf(id)` | Direct lookup | `DependentsOf(id)` minus the implicit include→scope-self edges of activated scopes; what a change to `id` reaches through one declared or provider-derived edge. `--impacted` and `why_impacted` walk this family |
| `TransitiveDependentsOf(ids)` | BFS | All projects transitively downstream of `ids` in the ordering family |
| `TopologicalSort()` | Kahn's algorithm | Deterministic ordered list respecting dependencies; detects cycles |

### Deterministic Ordering

`TopologicalSort()` uses Kahn's algorithm with sorted queues: when multiple projects are freed in the same iteration (i.e. all their dependencies have been processed), they are sorted alphabetically by ID before processing. This ensures consistent, reproducible ordering across runs — important for CI/CD reliability and debugging.

### Transitive Propagation

When project A changes, all projects transitively depending on A are considered affected:

```text
A (changed)
├── B depends on A → affected
│   └── D depends on B → affected
└── C depends on A → affected
```

`TransitiveDependentsOf(["A"])` returns `["B", "C", "D"]`.

## Project Filtering

Project selection combines multiple filter dimensions. The filtering pipeline:

```text
All projects
    │
    ▼
  Project selector (--all, --impacted, target expression, .)
    │
    ▼
  Tag filter (--tag, --exclude-tag, workspace excludeTags)
    │
    ▼
  Name/ID exclusion (--exclude)
    │
    ▼
  Selected projects
```

### Selection Modes

| Mode | Flag | Behavior |
| ---- | ---- | -------- |
| All | `--all` | Every project in the workspace |
| Impacted | `--impacted` | Projects affected by git changes (see Impact Analysis) |
| Specific | `--projects a,b,c` | Comma-separated project names or patterns |
| Target | `/path`, `alias`, `group` | Target expression (see below) |
| Current | `.` (positional) | Projects scoped to the current directory (see below) |
| Default | *(none)* | Smart branch-aware selection (see below) |

### Default Selection (No Target)

When no target is provided and no `--all` or `--impacted` flag is set, the CLI picks a branch-aware default:

| Context | Selection |
| ------- | --------- |
| Feature branch (not `main`/`master`) | Projects impacted vs trunk (`origin/HEAD`, then `origin/main`, then local `main`/`master`) |
| `main`/`master`, successful same-command/params run recorded in this worktree | Projects impacted vs that run's HEAD SHA |
| `main`/`master`, no local marker, remote cache active, and a same-command/params remote run marker exists | Projects impacted vs the remote marker SHA, if that SHA resolves locally |
| `main`/`master`, no marker or stale/unavailable remote marker | All projects |

The default is intentionally not scoped to the current working directory. Use `.` when you want cwd-based scope:

```bash
putnami build                               # smart default
putnami build .                             # cwd scope
cd /workspace/typescript && putnami build . # only TS projects
cd /workspace/typescript/frameworks/web && putnami build . # just web
```

Automatic runs print a selection note, for example `· impacted vs origin/main (trunk) — 3 projects` or `· all projects (first build on main) — 80 projects`. The saved local marker is keyed by worktree, branch, command list, and job parameters, so a successful `lint` run does not satisfy a later `build` run and `build --target linux/amd64` does not satisfy `build --target darwin/arm64`. In CI, a fresh checkout can use the same key against the remote cache's successful-run marker when remote caching is enabled; missing, stale, invalid, or unavailable remote markers fall back to all projects.

### Target Expressions

Target expressions provide path-based and alias-based project selection. They replace or complement the older name-based `--projects` flag.

| Pattern | Matches |
| ------- | ------- |
| `/typescript/frameworks/web` | Exact project by ID |
| `/typescript/frameworks/...` | All projects under `/typescript/frameworks/` recursively |
| `/typescript/...` | All projects under `/typescript/` recursively |
| `/identity/auth-server` | Grouped project whose physical path may be `identity/(workloads)/auth-server` |
| `./application` | Relative path from current directory |
| `../sibling` | Parent-relative path |
| `.` | Current project (or all projects under cwd) |
| `web-app` | Alias from `projectAliases` |
| `frontend` | Group from `groups` |
| `/path1,/path2` | Union of multiple targets |
| `/path1,-/path2` | Union minus subtraction |

Target expressions are used as positional arguments:

```bash
putnami build /typescript/frameworks/web          # exact ID
putnami build /typescript/...                     # recursive wildcard
putnami build /identity/auth-server               # logical ID; group folder omitted
putnami test web                                  # alias
putnami lint frontend                             # group
putnami build ./relative-path                     # relative to cwd
putnami build /a,/b,-/c                           # union and subtraction
```

#### Aliases and Groups

Aliases and groups are defined in `putnami.workspace.json` (at the workspace root) or in breadcrumb `putnami.json` configs:

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

- **Alias** — maps a single short name to a project ID.
- **Group** — maps a name to a target pattern (which can include wildcards, unions, etc.).

Resolution order for a bare word: alias → group → legacy name match.

See [Configuration > Breadcrumb Configuration](05-configuration.md#breadcrumb-configuration) for details on where aliases and groups can be defined.

### Tag Filtering

Tags interact with three sources:

1. **Workspace exclude tags** — `disable.tags` in workspace config. Applied by default.
2. **`--tag <tags>`** — Include only projects with at least one matching tag. If a `--tag` value matches a workspace exclude tag, the exclude is overridden for that tag.
3. **`--exclude-tag <tags>`** — Exclude projects with any matching tag. Combined with workspace exclude tags.

Example:

```json
// putnami.workspace.json
{
  "disable": { "tags": ["experimental"] }
}
```

```bash
# Excludes "experimental" projects by default
putnami build --all

# Includes "experimental" projects (overrides workspace exclude)
putnami build --all --tag experimental

# Excludes both "experimental" and "slow" projects
putnami build --all --exclude-tag slow
```

### Name/ID Exclusion

`--exclude` removes specific projects by name or ID:

```bash
putnami build --all --exclude /typescript/frameworks/web
putnami build --all --exclude my-legacy-app
```

## Impact Analysis

The `--impacted` flag runs jobs only on projects affected by uncommitted or unpushed changes.

### Algorithm

1. **Resolve the baseline** — Use `--baseline` when provided, then `baseline` from `putnami.workspace.json`, then the nearest configured epic branch (`epicBranches`), then the trunk (`origin/HEAD`, `origin/main`), then local `main`/`master`, and only as a last resort the branch's upstream tracking ref. A branch is never measured against itself: the upstream tier is skipped when it is the branch's own published counterpart — detected by name, by push destination (`@{push}`), or when the upstream already contains HEAD — so `--impacted` selects the branch's work even after a push, including from a locally renamed branch.
2. **Compute changed files** — Run `git merge-base <baseline> HEAD` to find the fork point, then `git diff --name-only` against it. If a bare local branch name is missing but `origin/<name>` exists, Putnami retries with the remote ref. Also include `git diff --cached` (staged) and `git ls-files --others --exclude-standard` (untracked).
3. **Map files to the tasks that read them** — Each changed file is first associated with the nearest project whose directory contains it (the deepest project directory that holds the path, and only that one), with every project that claims it as a cross-project asset, and with every project that declared it as a file input under `options.<layer>.filePatterns`. For files directly at the workspace root, core also exact-matches root entries from providers' per-project `watchedFiles` answers. Each claim is then resolved onto the TASKS of that project whose declared inputs select the file — the same declarations the build cache keys on: a task's `inputs` ports (`from: project`, `workspace` and `closure` file patterns), the project's option layers attributed to the tasks that read them, and its `options.generate.assets`, which every job of the project keys on. A file no declaration selects claims nothing, so a `.md` no action reads selects no project at all. Four claims are not task declarations and keep the project **full** instead: a scope's `putnami.json`, which decides which tasks exist rather than what one reads; a caller's own workspace-input pattern; a workspace-root entry a provider claimed that no `from: "workspace"` input declares; and a file of an in-workspace extension that no task reads, which may be the runtime its consumers execute.
4. **Propagate through task edges** — A fixpoint walk from the seeded projects over three edge kinds: declared and provider-derived dependencies (`ImpactDependentsOf`), the derived contract edge from a provider to each generated client of its contract, which fires only when the change includes the provider's committed contract (`schema/openapi.json`), and the impact-only extension-consumer edge from an in-workspace extension to every project that runs under it. A dependency or contract edge carries into the other project the tasks whose `^` step reference names one of the tasks this one runs — `^generate` in a step of `build` names the `build~generate` of every project it depends on, the same relation the planner resolves — together with the tasks that depend on those inside the reached project. A task nothing references, such as `test` or `lint`, therefore reaches no dependent at all, and an edge that carries nothing is not crossed. A project a claim could not be attributed to is **full**: every task of every command runs, and a full project carries every task across its edges. Full dominates, a project reached twice takes the union of what reached it, and an upgrade re-propagates. The extension-consumer edge fires from an extension whose binary the change rebuilt — one that is full, or one whose tasks reach a dependent's task at all — and scopes each consumer to the tasks that extension declares, named with the extension project's id so another extension's job of the same command and step is not kept. Nothing propagates out of the tasks that edge added: the consumer's own sources did not change. The implicit edge an activated scope has to each of its includes is excluded: it orders the schedule and carries no inputs, so a change to a scope-self's own files reaches an include only through a declared `dependencies` entry.
5. **Return affected projects** — The union of directly changed and transitively affected projects, with a trace that explains each one and names the tasks each task-scoped one runs. Planning keeps, on a task-scoped project, the jobs its scope names, the `publish` and `deploy` jobs a release set decides, and every job those depend on, however many hops away and whichever extension owns it; every other job of the project is dropped. A task scope is spelled `[<extension-project-id>#]<command>~<step>` — the plan name a run prints — and is matched against a job's own command and step, never against its display name, which namespacing rewrites.

### Baseline

For explicit `--impacted`, Putnami resolves a baseline from the current git context. Override with `--baseline`:

```bash
putnami test --impacted --baseline develop
putnami test --impacted --baseline origin/release/v2
```

Workspace config can also declare a default baseline, and name long-lived integration branches so their sub-branches measure against the epic instead of the trunk:

```json
{
  "baseline": "origin/main",
  "epicBranches": ["epic/*"]
}
```

With `epicBranches` configured, resolution picks the candidate (epic branches and trunk) whose merge-base with HEAD is the most recent; a tie keeps the trunk, and the epic branch itself still measures against the trunk like any other branch.

If no baseline can be resolved, `--impacted` falls back to all projects and says so on stderr, naming the reason and `--impacted-strict`. Use `--impacted-strict` to fail instead. This is the only path that turns a narrow change into a whole-workspace run, so it is never silent. When resolution lands on a fallback tier — a local `main`/`master` with no remote trunk, or the upstream tracking ref — the run prints which ref it measured against, because a stale or self-referential baseline distorts the selection in both directions.

Bare job commands that auto-select impacted projects resolve the same trunk (`origin/HEAD`, `origin/main`, local `main`/`master`), so explicit `--impacted` and bare auto-selection can no longer disagree about what a feature branch is measured against.

A workspace root Git does not manage has no history to compare: no `git` program is on `PATH`, or the root is outside every repository. There a bare job command selects every project and says `all projects (no git repository)`, and a named project needs no Git. `--impacted` and `--baseline` refuse with one line that names Git and the command to run, as do `version`, a `publish` or a `deploy` that executes. Any other Git failure keeps its own message.

### Edge Cases

- **No changed files** — Structured JSON/JSONL output reports the empty selection through the typed plan or terminal session, without human notices. Human output prints "No impacted projects found against `<baseline>`" and exits with code 0; the message names the resolved ref because "nothing changed" is only as trustworthy as the baseline it was computed from.
- **No resolvable baseline** — Falls back to all projects with an explanation on stderr, or fails with a clear error when `--impacted-strict` is set.
- **File outside any project** — Ignored (does not trigger any project rebuild).
- **File inside a nested project** — Owned by the nested project only; the enclosing project is selected through a declared dependency on the nested project or a declared file input (`options.<layer>.filePatterns` covering the subtree). A project at the workspace root is the owner of last resort: it owns what no deeper project claims.
- **Changed file at the workspace root** — Belongs to no project by path, so core consults workspace-root entries from providers' per-project `watchedFiles` answers. An exact match selects only the projects that claimed the file, then applies normal transitive-dependent propagation. If no provider claims the root entry, it selects nothing on its own; the run names that path, and the `impacted` MCP tool returns it in `unownedRootFiles`, so a non-empty diff is distinguishable from no changes. Providers own the filename vocabulary and project mapping; core does not infer dependency manifests or lockfiles. Reusing `watchedFiles` keeps the v1 answer readable by strict older consumers.
- **New untracked files** — Included in the diff via `git ls-files --others`.
- **Staged but uncommitted** — Included via `git diff --cached`.

### Declared File Inputs

A project declares the files its commands read as `options.<layer>.filePatterns` in `putnami.json`. Patterns are relative to the project directory, so a `../` prefix reaches outside it:

```json
{
  "options": {
    "test": { "filePatterns": ["src/**", "../../.agents/skills/**", "!**/*.snap"] }
  }
}
```

One declaration serves two readers. The build cache hashes the files a pattern set selects, and `--impacted` selects the declaring project when a changed file matches one. Every option layer counts, so `lint`, `build`, `validate` and extension-keyed layers (`options."/typescript/extension"`) all select; the plan then decides which of the selected project's jobs to schedule.

Both readers share one grammar, in `protocols/workspace`: `*` matches inside a segment, `**` matches any number of segments including none, and a leading `!` excludes.

Prefix an input with `git:` to read only tracked files and non-ignored new
files from the containing Git repository. `git:**` covers that entire candidate
tree; `git:../sibling/doc/**` narrows it using the same project-relative paths.
Git is required. Ignored untracked local files stay outside this input set,
while tracked files remain inputs even when an ignore rule matches them.
Deleted and renamed paths still select their readers under `--impacted`.

An exclusion applies inside the layer that declares it, and no further. A cache key concatenates only the layers that apply to its own job — `options.<cmd>`, `options.<ext>` and `options.<ext>:<cmd>` — so `options.lint` excluding a path says nothing about what a `test` job reads, and one layer matching is enough to select. Selection is therefore at least as wide as every key it stands in for.

A declared input is not ownership — the `find_owner` MCP tool still answers with the nearest project whose directory holds the file (plus any asset claimant), because reading a file and owning it are different relations.

A scope's `putnami.json` is an input of the same kind for every project below it, without a declaration: the scope chain merges its `tags`, `extensions` and publish name pattern into each include, so a change to that file selects every project that inherits from it, whether or not the scope is activated, and for each scope of a nested chain. An activated scope-self is selected on top, as the file's directory owner. A child could not declare this itself — a `filePatterns` entry cannot name a file above the project's own directory without repeating the scope layout in every include.

### Explaining a selection

A selection that is too small explains itself unprompted (the unowned root
files above). A selection that is too large explains itself under `--verbose`
or `--debug`, before the plan table when `--plan` is set:

```
  --impacted: 4 changed file(s) reached 4 project(s) directly; propagation added 94 (dependency 71, contract 1, extension-consumer 22, 22 task-scoped)
    tooling/extension-sdk/sdk.go → /tooling/extension-sdk  [path-owner tooling/extension-sdk]
    go/extension/putnami.extension.json → /go/extension  [path-owner go/extension]
    services/catalog/schema/openapi.json → /services/catalog  [path-owner services/catalog]
    .agents/skills/fix/SKILL.md → /tooling/cli  [declared-input ../../.agents/skills/** !**/*.snap]
    /typescript/extension ← /tooling/extension-sdk  [dependency; tasks build~compile, build~describe, build~generate, test~test (+3)]
    /clients/catalog-ts ← /services/catalog  [contract services/catalog/schema/openapi.json sha256:5f1c0a9e3b72; tasks build~generate, build~transpile]
    /apps/web ← /go/extension  [extension-consumer; tasks /go/extension#build~describe, /go/extension#test~test]
    … +91 more edge(s); the `impacted` MCP tool returns every reason.
```

Each seed line is one changed file's claim on one project, with the kind of
claim and what it rests on:

| Seed kind | The file was claimed because | `Via` |
|-----------|------------------------------|-------|
| `path-owner` | the nearest project directory contains it, or an asset path claims it; a nested file yields one seed for the nested project, and a project rooted at the workspace owns what no deeper project claims | the containing path (`.` for a root project) |
| `root-watched-file` | a provider's per-project `watchedFiles` answer names this workspace-root entry | the entry |
| `declared-input` | an `options.<layer>.filePatterns` set selects it | the pattern set |
| `workspace-input` | a caller's workspace-scoped input pattern matches it (watch only; `--impacted` passes none) | the pattern |
| `scope-config` | it is a scope's `putnami.json` the project inherits its `tags`, `extensions` or name pattern from | the scope directory |

Each edge line names the project that pulled another one in, and the edge kind:

| Edge kind | The project was reached because | Selected |
|-----------|---------------------------------|----------|
| `dependency` | it declares, or a provider derived for it, a dependency on a project already in the set | the tasks whose `^` reference names one the other project runs, and what those carry inside it |
| `contract` | it is a generated client of a provider already in the set — its committed `client.putnami.json` names that provider's service — and the change includes the provider's committed contract; the line names that contract and its sha256 | the same rule, with the provider standing in for a declared dependency |
| `extension-consumer` | it runs under an in-workspace extension whose binary the change rebuilt | the tasks that extension declares, qualified by its project id |

Each line ends with the project's task scope, and a line with none names a
project that runs every task — one a claim could not be attributed to, or one an
edge reached from such a project. There is no scope kind: the implicit edge from
an activated scope to an include that did not declare it orders the schedule and
carries no impact, so it can never be the reason a project was selected. The
edge shown is the one that made the project full when anything did, and the
first edge that reached it otherwise; a project's scope can still grow through
another edge, and the scope shown is the union.

A `contract` edge fires only when the change includes the provider's committed
contract, `schema/openapi.json`. The client's committed manifest states the
exact contract bytes it was generated at (`contractSha256`), and that digest is
the one provider-side input in its identity: the contract edge orders the
client's `generate` after the provider's contract producer and contributes
nothing to its cache key. So a provider change that leaves the committed
contract alone cannot change what the client or its consumers compile against,
and selects none of them — a test file or a handler inside an API workload
selects the workload and its dependency dependents only. A provider change that
would move the contract without committing it fails the provider's own
`clientgen` drift check, and a regenerated client's files select the client
directly, as its own path owner.

When the edge fires, its line names the contract that changed and the first 12
hex digits of its sha256 in the tree the selection read; the `selection:impacted`
record and the `impacted` MCP answer carry the whole digest. `why_impacted`
answers for a change to the provider as a whole, which includes its contract, so
it still names the contract hop. See
[ADR 0054](adr/0054-the-contract-edge-fires-on-the-committed-contract.md).

### The contract edge

A project whose root commits a `client.putnami.json` is a generated client
target. The manifest's `service.id` names its provider, and the provider is the
project whose committed contract (`schema/openapi.json`) declares that same
service identity in its document-level `x-putnami-client` block. Nearness in
the tree is not a rule: a client beside its provider and one in another scope
are the same relation.

Both ends are read from committed files only. A contract that exists just under
`.gen` describes whatever build last ran, so resolving from it would give a cold
clone and a warm checkout two different graphs for one commit. A service
identity two projects both declare resolves to neither — an edge that depended
on project order would name a different provider from one run to the next — and
a workspace that commits no client manifest reads no contract at all.

In a plan, the edge orders jobs and changes no cache key. A client's `^` steps
wait for the provider's steps of the same name, and every client job waits for
the provider jobs that can write the client's directory. See
[Generated-Client Ordering](04-job-execution.md#generated-client-ordering).

The block is capped at 5 seeds and 10 edges. The full record is the `impacted`
MCP tool's `reasons` list, one entry per selected project, each with its
`taskScope` when it has one; `why_impacted` answers with the same edges for one
pair and the same `taskScope` for the target. Both read one propagation, so
they cannot disagree on a project or on its scope.

Every run that executes an `--impacted` selection also records the whole
trace, uncapped, as one `selection:impacted` session event: on the live
`--output=jsonl` stream, and in the session's `events.jsonl`, whatever the
output format. Its `event` carries:

| Member | Meaning |
|--------|---------|
| `baseline`, `baselineSource` | the ref the diff was measured against, and the resolution tier that chose it |
| `diffBase` | the commit the diff was taken against: the merge base of the baseline and `HEAD` |
| `changedFiles` | every changed path, in diff order |
| `uncommittedFiles` | the changed paths no commit records — staged, unstaged or untracked; absent on a clean tree |
| `unownedRootFiles` | changed workspace-root paths that reached no project; absent when there are none |
| `seeds` | one `{project, file, kind, via}` per claim, in the block's order |
| `edges` | one `{project, from, kind}` per propagated project, in selection order; a `contract` edge adds `via`, the committed contract that changed, and `contractSha256`, its sha256 |
| `scopes` | one `{project, tasks}` per task-scoped project, in selection order; `tasks` are the task scopes it runs, sorted |

Two runs with the same `HEAD`, the same `diffBase` and no `uncommittedFiles`
read the same diff and select the same projects. A path in `uncommittedFiles`
on a CI runner is one the run itself wrote before it planned — the first place
to look when a hosted run selects more than a local one.

The `--plan` footer counts every project that owns a planned job, which is more
than the selection when a job's `^step` need pulled a dependency in. It then
reads `106 projects (72 selected · 34 dependency builds)`; when nothing was
pulled in, the footer is the plain count. `lint` pulls no dependency jobs, so
`lint --impacted --plan` and `build --impacted --plan` print different project
counts for the same diff.

### Change size and mixed intent

A run that executes an `--impacted` selection on this machine prints the size
of the change after the run, on standard error. A `--plan` run and a run placed
on a runner provider neither print nor record it. The size is split by category:

```
  Change size against origin/main: 14 files +412 -37 (code 6 files +210 -30 · tests 4 files +150 -5 · docs 2 files +40 -2 · generated 2 files +12 -0)
```

| Category | Files |
|----------|-------|
| `generated` | lockfiles (a name ending in `.lock`, `.lockb`, `.lock.json`, `.lock.yaml`, `-lock.json`, `-lock.yaml` or `.sum`, such as `go.sum` or `putnami.lock.json`), `client.putnami.json`, `*.gen.*`, `*.pb.*`, `*_pb2.*`, anything under `.gen/` or `__generated__/`, a file whose leading comment says it is generated (`Code generated … DO NOT EDIT.`, `@generated`, `Generated by …`), and the agent files Putnami installed from an extension's agent content |
| `tests` | `*_test.*`, `*.test.*`, `*.spec.*`, `test_*.py`, `conftest.py`, and anything under `test/`, `tests/`, `testdata/`, `fixtures/`, `__tests__/` or `__snapshots__/` |
| `docs` | Markdown, MDX, reStructuredText and AsciiDoc files, and anything under `doc/` or `docs/` |
| `code` | every other file |

The first matching row wins, so a generated test fixture counts as generated.
Git counts the lines of tracked files; an untracked file counts its own lines.
A binary file, and an untracked file over 8 MiB, count as a file with no
lines.

The size is information. It never fails the run, and there is no line-count
threshold: a change that updates its tests and docs is larger by design.

When the authored code of the change spans projects that no dependency
relates, the run also prints a warning:

```
  warning: this change touches 2 unrelated areas (no dependency links them): @acme/api, @acme/worker | docs-site. Keep one intent per pull request, or deliver the work on an epic branch (workspace epicBranches), one gated phase per commit.
```

An area is a group of changed projects that depend on each other, directly or
transitively. A changed file belongs to the project whose directory is nearest
above it; a declared asset or input elsewhere selects a project without making
the file its own. Only `code` files open an area: tests, docs and generated
files follow the code they sit beside.

The warning is not shown when the branch is one of the configured
`epicBranches`, or forks from one that is nearer than the trunk, because an
epic branch declares that its work spans several areas.
A branch forked from an epic branch that has no commit of its own yet is
indistinguishable from a trunk branch, and still gets the warning. Like the
size, the warning never changes the exit code. `--quiet` hides the size line
and keeps the warning.

Both are recorded as one `selection:change-shape` session event, on the live
`--output=jsonl` stream and in the session's `events.jsonl`. The two lines go to
standard error in every output mode, so they never mix with structured output
on standard output. The event carries:

| Member | Meaning |
|--------|---------|
| `baseline` | the ref the diff was measured against |
| `total` | `{files, added, deleted}` for the whole change |
| `categories` | `{files, added, deleted}` for each of `code`, `tests`, `docs` and `generated`; every category is present |
| `areas` | the project names of each area, in workspace order |
| `onEpic` | whether the branch is on a configured epic branch; checked only when there are several areas |
| `mixedIntent` | whether the warning applies |

### What `--impacted` Still Does Not Select

Selection is not the last word on what runs. Two rules narrow it afterwards, and both can make a green gate weaker than it looks:

- **Tags excluded by the workspace** — `disable.tags` drops matching projects from every default run, including an `--impacted` one, even when the change is inside such a project. Name them explicitly (`--projects <id>` or `--tag <tag>`) to gate them.
- **Jobs a project disabled** — `disable.jobs` removes the task, so a selected project can contribute no jobs at all and disappear from the plan's project count.

When either applies to your change, say so in the pull request and run the affected projects by name. A related trap lives one level down: a file a task reads but the project never declared as an input is in neither the cache key nor the selection, so the task can replay a stale verdict over it. Declare it.

## Introspection Commands

```bash
# List all projects with IDs, paths and tags
putnami projects list

# Full project details (deps, extensions, config) — accepts ID or name
putnami projects describe /typescript/frameworks/web
putnami projects describe @putnami/web
putnami projects describe /typescript/frameworks/web --output=jsonl

# Workspace overview
putnami workspace describe

# Preview what would run without executing
putnami build --impacted --plan
```

## Visibility: Who May Import a Project

A project declares who may import it, in one field of its `putnami.json`:

```json
{
  "name": "go.putnami.dev/logger",
  "visibility": "public"
}
```

| Value | Who may import it |
|---|---|
| `scope` | the projects of its own scope — **the default** |
| `public` | every project of the workspace |

**Two projects share a scope when the nearest ancestor directory whose
`putnami.json` contributes scope configuration is the same directory.** That is
the same file that gives them their tags, their extensions and their name
pattern. A project with no such ancestor is in the workspace root scope, and a
nested scope wins over its parent: with `go/putnami.json` and
`go/framework/putnami.json` both present, `go/framework/http` is in
`go/framework` and `go/samples/task-api` is in `go/samples`, so an import
between them crosses a boundary.

The check judges the imports the build really performs — a `go.mod` `require`
or `replace` of a workspace module, a `package.json` dependency on a workspace
package — and never a `dependencies` entry a `putnami.json` merely declares. A
declared edge with no import behind it is a boundary crossing that never
happens, and the implicit ordering edge an activated scope adds to its includes
reads nothing, so neither is judged.

A service declares nothing. Its committed contract is its public surface, so the
edge from a generated client to its provider is always allowed, and the
provider's implementation stays private. A generated client is importable
from every scope, wherever it sits: it is the service's published way in. That
holds when its committed `client.putnami.json` names a service a provider in
the workspace commits, and its `putnami.json` declares no `visibility`; a client
that declares `scope` keeps that boundary.

### Reporting and enforcing

The check runs in every command that resolves the workspace graph, which
includes the `validate` gate. One workspace switch sets what a finding means:

```json
{
  "options": {
    "workspace": { "visibility": "report" }
  }
}
```

| Value | Effect |
|---|---|
| `off` | the check is not evaluated |
| `report` | each violated import is a warning; the run continues, exit 0 |
| `enforce` | the graph is refused: every command that plans over it fails, and `install`, `deps` and `projects sync` stay reachable so the workspace can be repaired — **the default** |

A finding names the importer, the imported project, both scopes and the manifest
family the import came from:

```text
putnami: workspace probe failed: visibility-violation: 1 import(s) cross a scope
boundary the imported project did not open (options.workspace.visibility is
"enforce", the default); mark each imported project "public", move it into the
importer's scope, or remove the import — declare options.workspace.visibility
"report" to see them as warnings while you fix them
putnami:   [error] /sites/putnami.dev: /sites/putnami.dev imports
/typescript/framework/web across a scope boundary: /sites/putnami.dev is in
scope sites, /typescript/framework/web is in scope typescript/framework, and
/typescript/framework/web declares visibility "scope" — mark it "public", move
it into the importer's scope, or drop the package-json import
(workspace.visibility_violation)
```

An unknown value of the switch is a warning
(`workspace.visibility_mode_invalid`), and the check runs in the default mode.
`putnami projects sync` prints the same findings; outside `--dry-run` it still
writes the workspace index.

### Upgrading to the `enforce` default

`report` was the default until 2026-09-29. A workspace that declares no
mode now refuses its graph when any import crosses a scope boundary, and the
refusal lists each one. To find those imports before you upgrade, set
`"visibility": "report"` and run `putnami projects sync`: each warning is one
import. Then fix each one:

1. Mark the imported project `public` when other scopes are meant to use it.
2. Move it into the importer's scope when only that scope uses it.
3. Drop the import when nothing needs it.

To keep the warnings while you work through them, keep
`"visibility": "report"` in `putnami.workspace.json`. A declared `report`
stays `report`.


### A declared edge no import backs

The reference graph is the one derived from real imports. The same
synchronization that evaluates visibility also reports every **declaration** no
import backs. Such an edge is a phantom: it propagates impact to a project that
reads nothing of the change, it orders a schedule nothing waits for, and it
opens a boundary the visibility check can never enforce, because no import
crosses it.

A provider claims an import family for an edge only when its own sources import
the target — the Go provider parses the module's `.go` files under `go mod
tidy`'s own walk rule (test files count, build tags do not, `testdata`,
`vendor`, dot-directories and nested modules are skipped), and the TypeScript
provider reads the package's committed sources and skips `node_modules`,
`dist`, `build`, `coverage` and dot-directories. Neither runs a toolchain, and
neither reads a tree a build wrote, so a cold clone and a warm checkout answer
the same thing. A scan that cannot read a file attributes nothing: the manifest
stands, and a phantom is never invented from an unreadable tree.

Four classes, one per file a phantom lives in:

| Class | What it is | Repair |
|---|---|---|
| `putnami-json` | a `dependencies` entry of the project's own `putnami.json` | `putnami deps prune` rewrites the file |
| `manifest` | the project's own language manifest states the edge and its sources do not read it: a `require` or `replace` of a workspace module no `.go` file imports, a `workspace:` dependency (or a `putnami.dependencies` entry) no source file imports | `putnami deps prune` runs `go mod edit -droprequire=<module>` and `go mod tidy` for a Go module; a TypeScript package is reported — edit `package.json` and run `putnami deps install` |
| `client-manifest` | a committed `client.putnami.json` whose `service.id` no provider declares any more | reported; delete the generated target, or restore the provider's contract |

An edge is also backed — and never reported — when the dependent declares a
file input that reads inside the dependency: `options.<layer>.filePatterns`,
`build.assets[].from`, `options.generate.assets[].from`. That is the same
declaration the cache hashes and `--impacted` seeds from, so declaring what a
project really reads both silences the finding and makes its key honest.
A project for which no provider reported any provenance at all is skipped with
one warning (`attribution unavailable`), because silence is not evidence.

A `workspace:` **devDependency** is out of scope: that is how a project requests
an extension, and the build reads it by running it.

The severity is `options.workspace.declaredEdges`, a sibling of
`options.workspace.visibility` with the same three values — the two checks
refuse for different reasons, and a workspace has to be able to enforce one
while it still repairs the other:

```json
{
  "options": {
    "workspace": { "declaredEdges": "enforce" }
  }
}
```

| Value | Effect |
|---|---|
| `off` | the check is not evaluated |
| `report` | each declaration is a warning; the run continues, exit 0 — **the default** |
| `enforce` | the graph is refused with a typed `declared-edge` probe failure, and the repair commands stay reachable |

A declaration that duplicates a **contract** edge is reported too, and it is the
one worth repairing first: it re-creates the full dependency edge the contract
edge exists to avoid, folding the provider's whole cache key into the client's
instead of the contract digest alone. The contract edge outlives the removal and
keeps ordering the client after its provider.
