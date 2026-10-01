# Exact dependency documentation

The TypeScript extension contributes the read-only MCP tool
`putnami.typescript_docs`. Agent hosts use it when they need API documentation
for the package an application actually imports:

```json
{"reference":"@putnami/web","project":"apps/storefront"}
```

`reference` is an exact npm package name. `project` is optional when precisely
one TypeScript project in the workspace declares that package; otherwise pass a
Putnami project name, npm package name, or workspace-relative directory. The
workspace root comes from the MCP session and is not a tool argument.

Resolution begins at the selected project and checks each `node_modules`
ancestor in Node order. This makes a nested application copy win over a
different hoisted version. The extension reads the resolved package's own
`package.json` for its exact name and version and never invokes Bun, installs a
dependency, or consults a registry. A workspace package symlink is reported as
`workspace-replacement`; other materialized packages are `installed-package`.

Project discovery walks the workspace, so it runs under the same bounded budget
as `putnami.go_docs`. A caller that goes away, or a workspace deep enough to
exhaust the budget, gets `unresolved` rather than an unbounded scan.

Unlike `putnami.go_docs`, this tool has no "single root project" fallback. A
root `go.mod` is itself a module an agent can ask about; a monorepo's root
`package.json` is normally the workspace manifest rather than a project, so
answering from it would resolve against the wrong dependency tree. An ambiguous
or unmatched request stays an explicit `unresolved` instead.

An available result includes the exact package and installed version, the
resolved `AI.md` path, a SHA-256 digest of its bytes, and the content. Reads are
limited to 1 MiB and reject a documentation symlink that leaves the resolved
package directory. Missing installed state returns `offline_missing`; an
installed package without safe bounded documentation returns `docs_missing`;
an invalid reference or ambiguous project returns `unresolved`. These are
successful tool responses with a fallback, so an agent can continue from local
source without retrying or triggering installation.
