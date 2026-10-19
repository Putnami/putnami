# ADR 0001 — The dev extension declares the development verbs and answers only the runtime handshake

- **Status**: accepted. Decisions 2 and 3 are amended by
  [ADR 0073 of `@putnami/cli`](../../../cli/doc/adr/0073-the-core-declares-no-development-verb.md):
  the core presents no verb and gives none a default, so this vocabulary is
  the only presentation and the only source of default traits, and `serve`
  is long-running through the `workload` it declares, not `WellKnownVerbs`.
- **Scope**: `@putnami/dev` (`tooling/dev`), the manifests of `@putnami/go`,
  `@putnami/typescript` and `@putnami/python`, and this repository's
  `putnami.workspace.json`
- **Decisions**: D-006, D-007 and D-012 of the root `decisions.json`;
  [ADR 0066](../../../cli/doc/adr/0066-an-extension-installs-and-loads-its-extension-dependencies.md)
  and [ADR 0067](../../../cli/doc/adr/0067-putnami-dev-declares-the-development-verbs.md)
  of `@putnami/cli`

## Context

D-006 settles that `build`, `test`, `lint`, `serve`, `run`, `format`,
`package`, `publish`, `deploy`, `generate` and `validate` come from
`@putnami/dev`, which the language extensions depend on. ADR 0067 gives a
manifest a `verbs` section that declares a verb without running it, and
ADR 0066 makes an extension install and load its extension dependencies.

The core still presents these verbs: their order, descriptions, aliases,
related commands and default traits. A vocabulary that a workspace loads
takes over that presentation, so the first vocabulary must repeat it exactly.

## Decision

1. **A vocabulary-only manifest.** `putnami.extension.json` declares the
   eleven verbs in help order: build, test, lint, serve, run, format,
   package, publish, deploy, generate, validate. It declares no command,
   command group, task, tool, hook, agent content or runtime, and it claims
   CLI contract 11, the contract its command kinds require.
2. **The core's presentation, copied.** Each verb carries the description,
   aliases and related commands the core shows for it today. A verb that
   `WellKnownVerbs` describes takes that description; `generate` and
   `validate` take the core catalog's.
3. **Explicit traits.** They keep the core defaults: `serve` and `run`
   require a runnable project, `package` injects the publish channels,
   `publish` injects them and changes a registry, and `deploy` changes a
   cloud. From contract 11 they also declare what the core reads without a
   verb name: `build`, `test`, `lint` and `validate` their kind, `serve` and
   `run` their workload, `format` that it rewrites sources, and `deploy` that
   it consumes the release set. `generate` declares no traits. Until the core
   reads `workload`, `serve` stays long-running through `WellKnownVerbs`.
4. **A handshake-only executable.** `cmd/putnami-dev` answers
   `__putnami runtime-info` and refuses every other invocation. `@putnami/go`
   builds and packages it into the extension archive, as it does for
   `@putnami/contributor`. The manifest declares no `runtime`, so no host
   starts it.
5. **The language extensions depend on it.** The Go, TypeScript and Python
   manifests name `@putnami/dev` in `extensionDependencies` with an empty
   constraint. A dependency requires CLI contract 8, so the TypeScript and
   Python manifests move from 7 to 8. That stamp is not a breaking change
   (ADR 0067, D7).
6. **This repository loads it by path.** `putnami.workspace.json` lists
   `/tooling/dev`, which satisfies the dependency of every language extension
   without a registry lookup. `putnami.lock.json` does not change.
7. **Support and architecture.** `putnami.support.json` classifies
   `@putnami/dev` as `preview`. The project starts outside the architecture
   frontier, as every new project does, until a review names the domain that
   owns the vocabulary.

## Consequences

- Loading `@putnami/dev` changes no help text, alias, completion entry or job
  trait.
- The live view of a terminal run orders its command columns by the
  vocabulary, which is the help order: `putnami lint,test,build` shows build,
  test, lint, where the core's fallback order showed lint, test, build.
- Installing a language extension from a registry installs `@putnami/dev`
  too, so `@putnami/dev` publishes before any language extension build that
  names it.
- A workspace that loads a language extension by path without
  `@putnami/dev` records a skipped dependency and keeps the core's
  presentation.
