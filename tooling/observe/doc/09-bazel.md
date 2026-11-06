# How observe reads a Bazel repository

The Bazel detector of `@putnami/observe` finds the packages of a Bazel
repository and the packages each one depends on. The change analysis uses
them to map each changed file to its package, to list the packages that
depend on it, and to place the tests and coverage Bazel wrote. The detector
reads files only: it never runs `bazel`.

## A Bazel workspace

A repository whose root holds `MODULE.bazel`, `WORKSPACE` or
`WORKSPACE.bazel` is a Bazel workspace. Each of its packages is a component:

| Member | Value |
|---|---|
| Id | The package's directory, `.` for the root package |
| Name | The package label: `//pricing`, or `//` for the root package |
| Ecosystem | `bazel` |
| Source | `orchestrator` with the query export, `manifest` without it |

A component is a package, never a target: the targets of one `BUILD` file
share its directory, and a changed file belongs to the package that holds it.

The Bazel detector runs after the others. With an export, its packages
outrank them by source, as an Nx graph export does. Without one, a package
yields to any component another detector places at or above its directory,
such as a pnpm workspace member or a Go module: that component keeps its
files and its edges.

## The query export comes first

Bazel computes its graph from the `BUILD` files, which are Starlark programs.
The detector does not evaluate them: the CI writes the graph with
`bazel query`, before `putnami observe`.

```bash
bazel query 'deps(//...)' --output=streamed_jsonproto > bazel-graph.json
putnami observe
```

The detector reads `bazel-graph.json` at the repository root, or the file
`--bazel-graph` names. It also reads the output of `--output=jsonproto`, one
query result that lists the targets.

- Each directory that holds a `BUILD` or `BUILD.bazel` file and at least one
  rule of the main repository in the export is a package. A `BUILD` file that
  declares no rule, such as one that only calls `exports_files`, makes no
  component: its files belong to the package above it, if any, and an edge to
  one of its files is no edge.
- A rule that names a target of another package among its inputs, through
  `deps`, `srcs`, `data` or any other label attribute, makes its package
  depend on the other one.
- A label of the main repository is `//pkg:name`, `@//pkg:name` or
  `@@//pkg:name`. A target of an external repository, such as `@maven//…`,
  `@npm//…` or `@@rules_go+//…`, is no edge.

The export reflects the commit that was built: write it in the same job, after
the checkout of that commit.

## Without an export: the BUILD files

Without an export, each directory that holds a `BUILD` or `BUILD.bazel` file
is a package of source `manifest`, and no package has an edge. When it finds
one, the run gets the info diagnostic `observe.bazel_graph_missing`, which
names the export command. The walk skips
hidden directories, `node_modules`, the `bazel-*` links of the root and the
directories `.bazelignore` names, and stops after 200 000 entries with the
`observe.bazel_walk_bounded` warning.

An export that is not valid JSON, that holds no rule of the main repository,
or that `--bazel-graph` names and that does not exist gets the
`observe.bazel_graph_unreadable` warning, and the `BUILD` files answer
instead. The run does not fail.

## Tests and coverage

`bazel test` and `bazel coverage` write their outputs in Bazel's output base,
outside the repository, and leave links to it at the root. Report discovery
follows two of them:

| Report | Path | Written by |
|---|---|---|
| JUnit | each `test.xml` under `bazel-testlogs`, except under a target's `test.outputs` and `test_attempts` | `bazel test`, one per test target, shard and run |
| LCOV | `bazel-out/_coverage/_coverage_report.dat` | `bazel coverage --combined_report=lcov` |

Each one is still recognized by its content, and named by its path through the
link, such as `bazel-testlogs/api/api_test/test.xml`.

Bazel writes the log of the test target `//api:api_test` under
`bazel-testlogs/api/api_test/`. Every case of that log goes to the package of
its test target, `api`: the rule of the export whose package and name match
the log's directory, else, without an export, the longest package whose
directory prefixes it. When that package yielded to another detector's
component, the cases go to the component that encloses it. A target whose name holds a `/`, such as
`//pricing:rules/tax_test`, is told from a package `pricing/rules` only with
an export.

The combined LCOV report names each file relative to the repository root, so
each covered file goes to the package that holds it.

The links point into the output base of the machine that ran Bazel: run
`putnami observe` in the job that ran `bazel test` and `bazel coverage`.
