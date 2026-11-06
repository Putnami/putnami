# How observe reads a moon workspace

The moon detector of `@putnami/observe` finds the projects of a
[moon](https://moonrepo.dev) repository, the directory each one owns and the
projects each one depends on. A project is a component whatever its
language: a Rust, Go, Python or plain directory is one, with or without a
`package.json`. The detector reads files only: it never runs `moon` or a
package manager.

Each project is a component of ecosystem `moon` and source `orchestrator`, so
it wins over the Node workspace, Go or Python component of the same
directory. Its name is its moon project id.

## The graph export comes first

When the CI ran `moon project-graph --json` before `putnami observe`, the
export is the authority. It holds every edge moon knows, including the ones
its language toolchains infer, which no static read sees.

```bash
moon project-graph --json > moon-graph.json
putnami observe
```

The detector reads `moon-graph.json` at the repository root, or the file
`--moon-graph` names, resolved against the directory you run in:

- each project is a component at its `source`. moon 1.x writes the projects
  in `graph.nodes`; moon 2.x writes node indices there and the projects in
  `data`, keyed by index;
- each `[from, to, scope]` entry of `graph.edges` is an edge from the first
  project to the second, except the `root` scope.

moon 1.10 and earlier write labels without directories: their export is not
read.

A missing `moon-graph.json` is not a finding. A file `--moon-graph` names that
is missing, and a file that is not a moon project graph export, get the
`observe.moon_graph_unreadable` warning when `--moon-graph` named it or the
repository holds `.moon/workspace.yml`, and the detector reads the
configuration instead. A project without a source, or with the source of
another project, gets the same warning and is skipped.

## Without an export: the moon configuration

A repository whose root holds `.moon/workspace.yml` is a moon workspace. Its
`projects` setting takes one of three forms.

A list of globs, each matched directory a project named by its last segment:

```yaml
projects:
  - 'apps/*'
  - 'crates/*/moon.yml'
  - '!apps/legacy'
```

A map of project id to directory:

```yaml
projects:
  api: 'apps/api'
  pricing: 'packages/pricing'
```

Or both:

```yaml
projects:
  globs: ['apps/*']
  sources:
    pricing: 'packages/pricing'
```

A glob takes `*`, `?`, `[...]`, `{a,b}` and `**`. A glob that ends with
`moon.yml` selects the directory that holds it. A `!` glob removes
directories. `.` selects the repository root. Globs never select hidden
directories, `node_modules`, or a directory a map entry already names. An `id`
in a project's `moon.yml` renames it.

| Edge | Read from |
|---|---|
| Declared | `dependsOn` in the project's `moon.yml`: a project id or alias, as a string or as an object with an `id` |
| Inferred | the `dependencies`, `devDependencies`, `peerDependencies` and `optionalDependencies` of the project's `package.json` that name another project's package, as moon infers them |

A project's alias is the `name` of its `package.json`. A dependency on the
project itself, or on no project of the workspace, is no edge.

```yaml
# apps/api/moon.yml
language: 'typescript'
dependsOn:
  - 'pricing'
  - id: 'auth'
    scope: 'development'
```

A change to `pricing` marks `api` dependent.

An edge that only a moon toolchain infers, such as a Cargo or Go module
dependency, is missing from this read. Write the graph export in the CI to
get it.

The detector reads YAML for the `projects` key of `.moon/workspace.yml` and
the `id` and `dependsOn` keys of each `moon.yml`: block and flow mappings and
sequences, and plain and quoted scalars. Anchors, aliases and tags in those
keys are an error; the rest of each file may use any YAML. `workspace.pkl`,
`moon.pkl` and the JSON and TOML forms of moon 2 are not read.

## Reports and lockfiles

The detector reads no report and no lockfile. Test and coverage reports land
on the project that holds their files, and each lockfile is read by the reader
of its language, as the [change analyses](06-change-analyses.md) describe.

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.moon_graph_unreadable` | The graph export is missing, is not a moon project graph export, or names a project without a source or with another project's source. |
| `observe.moon_workspace_unreadable` | `.moon/workspace.yml` cannot be read, or its `projects` setting is not a list of globs, a map of projects or both: the repository is left to the other detectors. A mapped directory that does not exist or leaves the repository is skipped with the same code. |
| `observe.moon_manifest_unreadable` | A project's `moon.yml` or `package.json` cannot be read: the project keeps no declared, or no package, edge. |
