# ADR 0046 — A declared edge no import backs is a finding

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/workspace` declared-edge check,
  `putnami deps prune`), the Go and TypeScript workspace probes

## Context

Every dependency edge carries the manifest family it came from
([ADR 0045](0045-a-project-declares-who-may-import-it.md)). An edge a manifest
declares and nothing backs is a phantom. It propagates `--impacted` to a
project that reads nothing of the change, orders a schedule nothing waits
for, and opens a boundary the visibility check cannot enforce. Where a
dependent's key folds its dependencies, a phantom is paid for on every run.

A `go.mod` require or a `workspace:` specifier states what is available, not
what is read, so the manifest alone cannot tell a phantom from a real edge.

## Decision

### 1. A provider claims an import family only for an edge its sources import

Each provider scans the project's committed sources once per probe and
attributes an unimported manifest entry `declared`, the standing of a
`putnami.json` entry. The edge set is unchanged.

- The Go scan follows `go mod tidy`'s walk: test files count, build
  constraints are ignored, and `testdata`, `vendor`, nested modules and
  directories starting with `.` or `_` are skipped.
- The TypeScript scan skips what an install or a build writes
  (`node_modules`, `dist`, `build`, `coverage`, dot-directories).
- Neither runs a toolchain, a resolver or a network call, and neither reads a
  generated tree. A cold clone and a warm checkout answer the same, because
  the probe digest keys the workspace snapshot.
- An incomplete scan (unreadable directory, unparsable file) attributes
  nothing: every edge of that project keeps its manifest family. A missed
  phantom costs one stale edge; an invented one deletes a real dependency.

### 2. What backs an edge

- An import, as above.
- A contract: a generated client reads its provider's committed contract.
- A declared file input: `options.<layer>.filePatterns`,
  `build.assets[].from` or `options.generate.assets[].from` reading inside
  the dependency. A pattern is reduced to its literal prefix, never expanded
  against the tree, so the answer cannot depend on build leftovers. A pattern
  that is an ancestor of the dependency counts, because its glob tail can
  reach in.
- A test that executes a sibling project (`bun src/serve.ts` of a provider, a
  Go binary of a sample) declares what it executes as a `test` file input
  reaching into that project, and keeps the `dependencies` entry for its
  `^generate` ordering. `typescript/samples/10-service-to-service/clients/go`
  is the reference declaration.
- A `closure` task input is not read as a backing source: resolving it needs
  each project's extension tasks, which the workspace model does not resolve.

### 3. Findings

- A `dependencies` entry that duplicates a contract edge is reported. It
  re-creates the full dependency edge the contract edge replaces, folding
  the provider's whole key into the client's `generate`. The contract edge
  keeps ordering the client after its removal.
- A client manifest no provider backs is reported.
- A project for which no provider reported any `dependencySources` has
  unknown attribution. It is skipped and named once as a warning, even under
  `enforce`: a check that could not run is not a violation. An entry absent
  from a map a provider did report stays `declared`.

### 4. Severity and placement

The check runs where the visibility check runs, on the resolved graph in
synchronization, with its own switch `options.workspace.declaredEdges`
(`off` | `report` | `enforce`, default `report`) and its own typed refusal
`declared-edge`. Two switches, because the checks refuse for different
reasons and a workspace must be able to enforce boundaries while it still
prunes edges.

### 5. `putnami deps prune` repairs through the writer that owns each file

- A `putnami.json` entry goes through core's project-config writer: key
  order preserved, unrelated members byte-identical.
- A `go.mod` require is dropped with `go mod edit -droprequire=<module>`,
  then `go mod tidy`. `go get <module>@none` would resolve the module through
  the proxy, and a workspace module no proxy serves is exactly the phantom
  being removed. `deps remove` keeps `go get`.
- A `package.json` dependency has no removal path in this CLI, and an orphan
  client manifest is a deletion. Both are reported with the exact edit, and
  the run fails. A prune that cannot repair everything never reports success.
- Prune re-hashes the recorded provider inputs, as `putnami context map`
  does. When the tree moved past the recorded view, it names the changed
  files and `putnami install` instead of acting on stale edges.

## Rejected alternatives

- **A provenance value per unbacked family** (`go-module-unused`): it doubles
  the closed set for a fact the pair (family, import) already states, and
  every `IsImport()` consumer would have to learn it.
- **Deriving import facts in core**: core would parse Go and TypeScript,
  the language knowledge the probe protocol keeps out of it.
- **Pruning `package.json` by rewriting it**: a hand-rolled rewrite of a file
  the package manager owns starts lockfile churn.

## Consequences

- The visibility check no longer enforces a boundary on a require nothing
  imports.
- Each project pays one imports-only parse of its own sources per probe, and
  nothing when it has no workspace edge. The walk stops once every edge is
  accounted for.
- A workspace enforces once `putnami deps prune` has run and the
  `package-json` and client-manifest findings are repaired by hand.
