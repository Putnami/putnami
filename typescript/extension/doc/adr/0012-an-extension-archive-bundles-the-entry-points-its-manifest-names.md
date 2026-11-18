# ADR 0012 — An extension archive bundles the entry points its manifest names

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`)

## Context

The CLI installs an extension from the registry as an archive and loads its
manifest from the artifact store. Until now only the Go packager wrote
extension archives, and `@putnami/scaffold` wrote those of content-only
extensions. A TypeScript framework package that is also an extension, such as
`@putnami/web`, shipped to npm alone. Its commands were out of reach in any
workspace but this repository: `putnami extensions install @putnami/web` found
no archive, and `putnami i18n sync` found no extension that declares it.

Such an extension runs no executable of its own. Its tasks and hooks call Bun
on a file under `{extensionRoot}`, for example
`bun run {extensionRoot}/bin/i18n`, and that file imports the project's
sources and its workspace dependencies. An installed archive has no
`node_modules`, so a copy of the source tree would fail at its first import.
Shipping the npm package's transpiled tree would fail the same way, and the
npm package does not even hold every entry a task names.

## Decision

The TypeScript extension's `package` command gains an `archives` step, which
runs when the project publishes on the archives channel and its extension
manifest declares tasks, commands or hooks. The step:

1. stages the manifest with the archive's version, without its `preBuild`
   and `configExtract` hooks, the project's `README.md`, `AI.md` and
   `LICENSE.md`, and its agent content, built and bound by the same SDK step
   every packager uses;
2. collects every path a task or a kept hook names under `{extensionRoot}`,
   in its command or its arguments, resolves it in the project in Bun's order
   (the exact file or directory, else the first of the name with `.tsx`,
   `.jsx`, `.mts`, `.ts`, `.mjs`, `.js`, `.cts` or `.cjs`), and bundles each
   TypeScript or JSX entry with `bun build --target=bun` into the stage under
   its name with the `.js` extension. Any other file or directory is copied as
   is;
3. fails, and writes no archive, when a path a task or hook names does not
   resolve in the stage the way Bun would resolve it at run time;
4. gates and stamps the manifest's contract in its archive form, then writes
   the same reproducible archive under each registry platform key, and
   records the `archives` channel and the publication metadata the archive
   uploader reads, in the layout the Go packager uses.

The `preBuild` and `configExtract` hooks of a TypeScript framework package
are the TypeScript extension's: it discovers them in the `node_modules` of
each project that depends on the package and runs them from the npm package
the project installed. The CLI also runs an extension's own `preBuild` hook
before each job of that extension. An archived copy would therefore run a
second generator, bundled at the version this repository tested, against the
consumer's project before every `putnami i18n` job, with its own copies of the
framework's classes and a `.gen` tree another version writes. The archive
leaves both hooks out; a hook the CLI runs from the archive, such as
`onInstall`, stays, and its entry point is bundled.

A manifest names an entry point without its extension, as the
`preBuild` hooks already do: `{extensionRoot}/bin/i18n` runs `bin/i18n.ts`
from the source and `bin/i18n.js` from the archive.

The release-set probe of `@putnami/cloud` routes an archive member to this
step when the project has a `package.json` and no `go.mod`, and its manifest
is not content-only. A Go module keeps the Go packager, and a content-only
manifest keeps `@putnami/scaffold`.

## Consequences

- An archive is self-contained: it runs with the Bun the workspace provides,
  whatever the consumer's `node_modules` holds.
- A bundle inlines the extension's own code and its workspace dependencies at
  the version this repository tested, independently of the npm package the
  consumer's project depends on. It runs only the extension's commands, which
  read the project's sources and catalogs; code generation stays with the npm
  package the project installed.
- An extension whose commands need the generator's output first must run
  `putnami build` or `generate` before them in a consumer workspace: the
  archive does not regenerate.
- An entry a manifest names with its `.ts` extension fails packaging: the
  archive holds its bundle under `.js`, and a name that only resolves in the
  source would break every consumer.
- An extension that depends on another registry extension, as `@putnami/web`
  depends on `@putnami/application`, publishes both: the CLI resolves a
  dependency from the registry when it installs the dependent.
