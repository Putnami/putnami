# ADR 0066 — An extension installs and loads its extension dependencies

- **Status**: accepted
- **Scope**: `@putnami/cli` (`internal/commands/extensions`,
  `internal/commands/lifecycle`, `internal/commands/agentctx`,
  `internal/extension`, `internal/engine`, `internal/jobs`),
  `go.putnami.dev/protocol/cli` and `go.putnami.dev/protocol/extension`
  (contract rung 8), and the Go and TypeScript packagers
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

### The graph

1. **The workspace declares roots; the graph follows the manifests.** The
   roots are the extensions `putnami.workspace.json` declares. The graph is
   the roots plus every extension a graph member's manifest names in
   `extensionDependencies`, transitively. The workspace file never receives
   a dependency: it keeps what the user wrote.
2. **The lock pins every node, with no new field.** A dependency gets an
   ordinary entry in `putnami.lock.json`. The edges are not recorded in the
   lock: they are in the manifests the lock binds by hash. The lock format
   stays at version 4.
3. **Which edges are followed.** The CLI follows the dependencies of an
   extension that loaded from the artifact store or from a path the
   workspace declares, and of a declared name the lock pins, wherever
   discovery finds it. It follows only a dependency whose name is a
   registry name (`@scope/name`), and always resolves it from the registry.
   The edges of any other extension that loaded from `node_modules` are not
   followed: the package manager installed it and owns its dependencies.
4. **What satisfies a dependency.**
   - Only `putnami.workspace.json` satisfies a dependency: it declares it by
     name, or by a path whose manifest has that name. A dependency it
     declares by name is still checked against every constraint of the
     graph, with the failure of point 6.
   - A key of the global configuration that names a graph member is
     ignored for resolution and loading, with one warning that names the
     global configuration. That extension stays a dependency for points 5,
     6 and 9.
   - A workspace project that the workspace does not declare never
     satisfies a dependency, and never replaces its build. Before it scans
     the projects, discovery computes the dependency names: every registry
     name that an edge of a graph member names, pinned or not. It sets
     aside each project of that name, as it does for a name pin under
     [ADR 0053](0053-a-registry-pin-wins-over-a-same-name-project-manifest.md),
     and the installed build records `PinnedOver`. When that build does not
     load, or the lock does not pin it, the dependency is reported with its
     dependent, and the project does not load in its place, locally or on a
     hosted run.

### Resolution

5. **An explicit channel flows down the graph.** A request is a channel
   when it is in the portable channel alphabet of
   [ADR 0056](0056-init-resolves-on-one-channel.md) and is not a semantic
   version, with or without a leading `v`, nor a range. An explicit channel
   is a channel other than `latest` that the run asks for: `--channel`,
   `PUTNAMI_CHANNEL` or the install record during `init`, or a
   `<name>@<channel>` argument.
   - Under an explicit channel, every dependency below the requested
     extension resolves on that channel, at every hop. Its own non-empty
     constraint is then checked against the version that channel resolves.
     So `putnami init --channel canary` resolves the whole graph on `canary`,
     and no hop reads `latest`.
   - Otherwise, including a bare declaration, `*` and `latest`, a non-empty
     dependency constraint resolves as written, and the registry picks the
     highest matching version. An empty one resolves on `latest`.
6. **The lock wins while it satisfies the graph.**
   - A locked dependency resolves from its lock entry while that version
     satisfies every constraint known when it resolves.
   - An explicit channel request on the command line re-resolves the
     requested extension's subgraph on that channel, whatever the lock
     holds. `--latest` and `putnami upgrade` re-resolve the whole graph by
     point 5.
   - When the locked version misses a constraint, the CLI resolves it again
     by point 5 and reports the move.
   - The install fails when the resolved version still misses a constraint
     that parses as a semantic version range, or when a registry answers a
     channel without naming the resolved version and a range must be
     checked. The error names the dependency, its version or the missing
     version, and each dependent with its constraint.
7. **The order is deterministic.** The graph resolves level by level,
   breadth-first. Every node of a level resolves, and its manifest is read,
   before any node of the next level resolves, whatever order the parallel
   workers finish in. Within a level, a dependent resolved under an
   explicit channel comes first, then the dependents sort by manifest name,
   then the dependencies by name. A node resolves once, at the first level
   that reaches it; the first dependent in that order picks the request it
   resolves on. A dependency that a dependent outside the channel's
   subgraph reaches at an earlier level keeps that request, and the channel
   only checks it. A constraint found at a later level is only checked, and a
   miss fails with the error of point 6. A cycle installs each extension
   once.

### Loading

8. **Every path that loads extensions walks the graph.**
   - Discovery loads a dependency from the artifact store, after the
     extensions the workspace declares and before the development
     dependencies of the root `package.json`, which skip a dependency
     name.
   - The bootstrap restore and the MCP read preparation restore each locked
     dependency of a restored extension.
   - Context generation, option ownership and the missing-extension check
     include the dependencies.
   - A dependency that does not load is reported with the dependent that
     requires it.
   - An implicit install, a bootstrap restore and an MCP read preparation
     never resolve a dependency the lock does not pin. They report it with
     its dependent and `putnami install`, and the missing-extension check
     names it.
9. **A dependency serves no reserved provider.** A dependency the workspace
   does not declare loses every reserved provider command: credential
   provider, cache provider, runner provider, session reporter, log reporter
   and Cloud release set. Discovery records each removal, as it does for a
   path extension under
   [ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md),
   and `putnami extensions list` names it. A hosted run's credential reaches
   only an extension its user named.

### Commands

10. **Update, upgrade and remove keep the graph whole.**
    - `extensions update` and `putnami upgrade` re-resolve every dependency
      by points 5 to 7 and write their lock entries. They never write a
      dependency into the workspace file's pins.
    - `extensions update --dry-run` previews the declared extensions only,
      and says so.
    - The remaining roots of `extensions remove <name>` are the workspace
      declarations without `<name>`. The command refuses when `<name>` is
      still reachable from them, and names the members on that path. So the
      root of a cycle can be removed. Otherwise it removes the lock entry of
      `<name>` and of each dependency the remaining roots no longer reach.
      It still asks the user to remove `<name>` from the workspace file.
11. **A hosted run writes nothing.** A hosted run installs the graph from the
    committed lock. A dependency the lock does not pin fails the install, as
    a declared extension does (ADR 0055).
12. **The user scope takes no dependency.** `putnami extensions install
    --user` refuses an extension whose manifest names a registry dependency,
    and names the limit. The user scope has no workspace file, so it cannot
    tell a root from a dependency.

### Contract

13. **An older CLI refuses a dependent.** A manifest whose
    `extensionDependencies` names a registry dependency requires CLI contract
    8, `ExtensionDependenciesContract`.
    - For the stamper, such a list is contract surface. The Go archive
      packager stamps 8 on every extension it packages with such a list. An
      extension that declares only hooks and dependencies takes the stamp
      without the strict requirement of a command, tool or agent content.
      The TypeScript npm packager leaves the list out of its stamp decision.
    - The floor applies to a manifest loaded from the artifact store or from
      a declared path. A CLI below rung 8 refuses it with "requires a newer
      putnami" instead of loading it without its dependencies.
    - A manifest loaded from `node_modules` keeps loading unstamped, because
      point 3 does not follow its edges. The npm framework packages, such as
      `@putnami/web` and `@putnami/analytics`, are not stamped for their
      `extensionDependencies`.

## Consequences

- Removing an extension from the workspace file stops loading its
  dependencies, unless another root still requires them.
- `putnami extensions list` shows a dependency with the extension that
  requires it.
- An agent-content opt-in (`extension:<name>`) may name any extension of the
  graph. Installing a dependency does not opt into its agent content. How
  `@putnami/dev` brings the contributor skills (D-012) is decided with
  `@putnami/dev`.
- Impact analysis still counts only the declared extensions as
  workspace-wide consumers. A workspace that develops an extension in place
  declares it by path, and point 4 makes that the only way its source runs.
- Because of point 12, two kinds of extension must declare no registry
  dependency:
  - an extension that the install-script run form installs
    ([ADR 0012](0012-install-trust-and-release-smoke.md) §6), whose map is
    empty today;
  - the user-scope credential provider of a hosted runner (ADR 0055 §3).
  The refusal of point 12 enforces both when they install, and their
  owners check it before adding a dependency.
