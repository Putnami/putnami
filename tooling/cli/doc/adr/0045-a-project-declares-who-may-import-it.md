# ADR 0045 — A project declares who may import it

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/workspace` synchronization),
  `@putnami/cli-model` (`workspace.VisibilityViolations`), the project config
  field `visibility` and the probe member `dependencySources` of
  `protocols/workspace`

## Context

A dependency graph records what imports what, but nothing records what may
import what. A sample imports a framework internal, a tool imports a
service's implementation, and the graph grows edges nobody agreed to. Each
unagreed edge is also an impact path: a change to the imported project
selects the importer and everything above it.

The workspace already has a unit of belonging, the scope, resolved once per
project (`Scope.ConfigPaths`). And the graph can tell an import (a `go.mod`
require) from a declaration (a `putnami.json` entry). A boundary enforced on
declarations is a boundary the build never respects.

## Decision

### 1. One field, two values

`"visibility": "scope"` (the default) makes a project importable by the
projects of its own scope. `"visibility": "public"` makes it importable from
anywhere in the workspace. The default is the closed value, so a new shared
library is a decision its owner takes.

### 2. Same scope

Two projects share a scope when the nearest ancestor directory whose
`putnami.json` contributes scope configuration is the same directory: the
last entry of `Scope.ConfigPaths`. No second walk of the tree runs.

- A project with no such ancestor belongs to the workspace root scope.
- A nested scope wins over its parent: `go/framework/http` is in
  `go/framework` and `go/samples/task-api` is in `go/samples`, so an import
  between them crosses a boundary.
- A project's own `putnami.json` is not part of its chain, so an activated
  scope sits in its parent's scope.

### 3. Only real imports are judged

- Each graph edge carries the manifest family it came from (`declared`,
  `go-module`, `package-json`, `contract`), reported by the provider in
  `dependencySources` and recorded as `DependencyGraph.EdgeSource`.
  Provenance changes no edge the scheduler, impact or cache keys see.
- The check judges a `go.mod` require or replace of a workspace module and a
  `package.json` dependency on a workspace package, and only when the
  importer's sources import it
  ([ADR 0046](0046-a-declared-edge-no-import-backs-is-a-finding.md)). A
  declared edge and an activated scope's implicit ordering edge are ignored.
- An import inside one scope is always allowed.
- A contract edge is always allowed. A service's committed contract is its
  public surface; its implementation stays private.
- An import of a generated client is always allowed when the client commits
  `client.putnami.json`, the manifest names a service a workspace provider
  commits, and the client declares no visibility. The client is the contract
  rendered for one language. A client that declares `scope` keeps it, and a
  manifest no provider backs opens nothing.

### 4. Severity

`options.workspace.visibility` takes `off` (not evaluated), `report`
(warnings, exit 0) or `enforce` (the graph is refused). The default is
`enforce`. An unknown value falls back to `enforce` with a warning that names
it, and a refusal the fallback causes carries the same warning.

`enforce` refuses through the typed probe failure `visibility-violation`.
Every graph-planning command fails, and the commands that repair a workspace
stay reachable. A read-only command still serves the recorded graph with the
findings. The refusal prints its diagnostics and omits the generic
"run `putnami projects sync`" remedy, which does not repair an import.

### 5. Where the check runs

At the end of every workspace synchronization, after the merged provider
view is adopted: the first moment the graph is the real one. Every command
that plans over the graph synchronizes, so an illegal import fails all of
them, not only `validate`.

## Consequences

- The workspace index keys every recorded provider answer on the provider's
  implementation identity as well as its inputs. Provenance is a
  provider-derived fact, and an answer carried across a provider rebuild hid
  every TypeScript import edge from the check.
- `dependencySources` enters the probe digest. Two trees whose edges agree
  and whose provenance differs are two graphs.
- A provider that folds a declaration into its edge list attributes each edge
  without changing which ones it reports. The TypeScript probe reports the
  `putnami` block's `dependencies` when present, and the package's
  `workspace:` dependencies otherwise.
- This repository marks `public` every project a cross-scope import reaches:
  the `protocols/*` modules, the published `go/framework/*` and
  `typescript/framework/*` packages, and `tooling/extension-sdk`.
