# ADR 0066 — An extension installs and loads its extension dependencies

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/commands/extensions`,
  `internal/commands/lifecycle`, `internal/commands/agentctx`,
  `internal/extension`, `internal/engine`, `internal/jobs`)
- **Decisions**: D-007 of the root `decisions.json`

## Context

An extension manifest can declare `extensionDependencies`: the other
extensions it needs, each with a version constraint. The CLI parses the field
and nothing reads it. A workspace gets exactly the extensions its
`putnami.workspace.json` names, so an extension that needs another one tells
its users to declare both.

D-007 settles that installing an extension installs its dependencies, and
that this graph decides what a workspace gets. It is the prerequisite of
`@putnami/dev` (D-006): the language extensions depend on it, and a user who
declares `@putnami/go` must get the development verbs without declaring
`@putnami/dev`.

The lock already binds every installed manifest by its hash. The manifest of
a locked extension, and so its dependency list, cannot change without a lock
change.

## Decision

1. **The workspace declares roots; the graph follows the manifests.** The
   extensions of a workspace are the ones it declares, plus every extension a
   loaded extension's manifest names in `extensionDependencies`, transitively.
   The workspace file never receives a dependency: it keeps what the user
   wrote.
2. **The lock pins every node, with no new field.** A dependency gets an
   ordinary entry in `putnami.lock.json`. The edges are not recorded in the
   lock: they are in the manifests the lock binds by hash. The lock format
   stays at version 4.
3. **Which edges are followed.** The CLI follows the dependencies of an
   extension installed from the registry or declared by a workspace path. It
   follows only a dependency whose name is a registry name (`@scope/name`).
   An extension that the root `package.json` installs keeps its dependencies
   with the package manager, which installed it.
4. **What satisfies a dependency.** A dependency is satisfied, and the CLI
   installs nothing for it, when the workspace declares it, by name or by a
   path whose manifest has that name, or when a workspace project's manifest
   has that name. Otherwise the CLI installs it from the registry.
5. **How a dependency resolves.**
   - The lock entry wins when the lock pins the dependency, as for a declared
     extension, unless the run asks for `--latest` or upgrades.
   - Otherwise a non-empty constraint resolves as written.
   - Otherwise, for an empty constraint, the dependency resolves on the
     channel its dependent was requested on, when that request names a
     channel in the portable channel alphabet of
     [ADR 0056](0056-init-resolves-on-one-channel.md). Every other request
     resolves the dependency on `latest`. So `putnami init --channel canary`
     resolves the whole graph on `canary`.
   - Several dependents can require one dependency. The first one in
     breadth-first order, with names sorted at each level, resolves it. Every
     constraint that parses as a semantic version range must then match the
     resolved version. Otherwise the install fails and names the dependency,
     its version, and each dependent with its constraint.
   - A cycle installs each extension once.
6. **Every path that restores or loads extensions walks the graph.**
   - Discovery loads a dependency from the artifact store, after the
     extensions the workspace declares.
   - The bootstrap restore and the read preparation for MCP restore each
     locked dependency of a restored extension.
   - Context generation, option ownership and the missing-extension check
     include them too.
   - A dependency that does not load is reported with the dependent that
     requires it.
7. **Update and upgrade move the graph.** `extensions update` and
   `putnami upgrade` re-resolve every dependency with the rule above and
   write their lock entries. They never write a dependency into the
   workspace file's pins.
8. **A hosted run writes nothing.** A hosted run installs the graph from the
   committed lock. A dependency the lock does not pin fails the install, as a
   declared extension does ([ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)).

## Consequences

- Removing an extension from the workspace file stops loading its
  dependencies, unless another root still requires them. Their lock entries
  stay until `putnami extensions remove`, as a declared extension's entry
  does today.
- `putnami extensions list` shows a dependency with the extension that
  requires it.
- An older CLI reads the same lock and loads only the declared extensions.
  This change needs no lock migration.
- Agent content stays opt-in by declared extension. How `@putnami/dev`
  brings the contributor skills (D-012) is decided with `@putnami/dev`.
- Impact analysis still counts only the declared extensions as
  workspace-wide consumers. A workspace that develops an extension in place
  declares it by path.
