# ADR 0006 — A node build reads its whole graph for Bun APIs

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`), the transpile
  phase of `build` with `--target node`

## Context

`putnami build --target node` asks bun for Node.js output. Bun builds the code
whether or not it uses Bun: a reference to the `Bun` global stays in the
output, and an import of `bun` or a `bun:` module stays external. The build
succeeded on a project that imports `@putnami/runtime`, which parses
configuration with `Bun.YAML`, and the output threw on Node.js at the first
call. A Node.js team adopting Putnami learns which packages it may use only in
production.

The transpile phase externalizes the project's dependencies by default, so the
bundle it writes does not contain the code that needs Bun. An unbundled
Node.js consumer still loads every module that code imports, and
`@putnami/utils` reaches `Bun.file` from its root entry.

## Decision

With `--target node`, the transpile phase reads the whole module graph of the
server graph, dependencies included, and fails with
`typescript.bun_api_on_node` on each module that needs Bun outside a
`typeof Bun` guard. The check is part of the build, not a separate validator:
a node build that passes loads on Node.js as far as the check can see.

1. **The graph is bun's.** A second `bun build --target node --metafile` into a
   temporary directory, with no dependency external, resolves the node export
   conditions, the tsconfig paths and the workspace links exactly as the
   build. A hand-written resolver would disagree with bun on the cases that
   matter. `bun` and `bun:` imports stay external so the metafile records
   them. A bare package that does not resolve at build time stays external
   and is not read: it cannot be read, and the consumer that installs it
   loads it.
2. **Modules are parsed, not type-checked.** Each JavaScript or TypeScript
   module is parsed with the `typescript` package the project resolves, for
   the reasons ADR 0004 gives: a real parser, no compilation, no declaration
   files. The scan is syntactic. A binding named `Bun` shadows the global; an
   ambient `declare` does not, since it binds nothing at run time.
3. **A guard is a `typeof Bun` test that proves Bun present.** Polarity
   counts: a reference in the branch where Bun is absent is the failure the
   check exists for. Only `typeof Bun` compared with a string literal, its
   negation, and `&&` and `||` of such tests are read; any other condition
   guards nothing. `globalThis.Bun?.` reads the global without needing it.
4. **Every module the graph imports is read**, whether or not the bundler
   keeps its code, because the unbundled output loads each of them.
5. **The check fails closed.** A graph bun cannot bundle, a module the scanner
   cannot read, or a project without `typescript` fails the build; the check
   never reports a graph it did not read as loadable.

## Consequences

- A node build that reaches `@putnami/runtime`, the root of `@putnami/utils`,
  `@putnami/application` or `@putnami/spectest` fails until those packages
  stop needing Bun on their server entries. The build documentation lists
  the framework packages that load on Node.js, and a test fails when the list
  and the check disagree, so the list moves with the packages.
- A node build runs a second bun process and one parser process; both are
  bounded by a timeout and leave nothing behind.
- The check can miss a use it cannot see syntactically: a Bun reference built
  at run time, or a guard written another way. It can report a value import
  from `bun` that is used only for its types; `import type` removes it.
- `--target bun` and `--target browser` builds run nothing new.

## When to revisit

- When bun reports Bun API use for a non-bun target itself, the scanner can
  give way to bun's own diagnostic.
- When the framework packages split their Bun-only code behind a `bun` export
  condition, the node graph stops reaching it and the documented list grows;
  the check needs no change.

## Rejected alternatives

- **Check only the emitted bundle.** With dependencies external, the bundle
  holds none of the code that fails, and the issue's own case passes.
- **Scan text for `Bun.` and `bun:`.** Comments, strings, property names,
  shadowing bindings and guards make a text match fail working code and miss
  `typeof`-polarity mistakes; the parser is already installed.
- **Run the output under Node.js.** Putnami never installs Node.js
  (`no-node-on-the-host`), and loading a module shows only the Bun use that
  runs at load time, not the one in a function that runs in production.
