# ADR 0001 — Provider contracts drive generated service clients

- **Status**: accepted
- **Scope**: `@putnami/clientgen` (`tooling/clientgen-extension`)

## Context

A provider may be written in Go while its consumer is written in TypeScript, or
the reverse. Its client must preserve service identity, schemas, typed errors,
security, idempotency, resilience, stream direction and every declared wire
transport. Generating from provider source would need a semantic reader per
language and could disagree with the artifacts the provider publishes.

Generated outputs also need exact cache ownership, so that deleting them cannot
turn a cache hit into a false success.

## Decision

### Generation is provider configured and contract mediated

A provider's native `clientGenerator()` or `api.Clients()` declaration emits the
versioned `x-putnami-client` OpenAPI metadata, the exact Proto projection and
`.gen/clientgen/config.json`. These are the shared inputs of the Go and
TypeScript readers. First-party metadata is strict: an unsupported semantic
fails rather than becoming an untyped value.

`@putnami/clientgen` has a prepared runtime. The locked Go toolchain packages
the Go emitter beside it; the TypeScript emitter is resolved through the
installed `@putnami/client` package shim. A consumer workspace needs neither the
framework source tree nor global language binaries. The executables come from
the extension runtime's lock-resolved toolchain declarations: preparation
requests only the compiler, and generation requests the TypeScript runtime only
for a task that may emit a TypeScript target. A missing TypeScript runtime
never falls through to an ambient version: TypeScript emission fails before
invoking the shim, and Go-only generation still works.

### Project tasks own their output and name every input

The project command depends on the provider build and runs both language
targets. Each task reports a language-specific dynamic output port and its
declaration owns that exact directory. Every generated directory carries a
strict `client.putnami.json` manifest binding the provider contract hash to the
operation semantics, the public client and registration symbols, and every
generated file hash.

Both tasks are cacheable, so their keys name everything that decides an emitted
byte: the configuration, the built contract, the provider `package.json` and,
for `clientgen-ts`, the Biome configuration, its `extends` chain and the
EditorConfig beside it, because the emitter canonicalizes every file with
Biome. A configuration the resolution cannot name as a workspace path fails
generation instead of leaving the key incomplete.

### Sync and adopt rewrite the workspace

`clientgen-sync` and `clientgen-adopt` are `workspace-once` commands. They
build every provider through the spawning Putnami CLI, bypassing the cache for
the providers only, so a consumer-only selection cannot leave a provider on a
stale contract. They
regenerate every target in place, rebuild every project as a compile gate, then
verify the worktree against a fresh isolated render.

The isolated render copies only generator inputs: the provider contract and
configuration, the provider package or module identity, scaffold-once Go
target metadata, and the workspace-root files whose presence or bytes decide
what an emitter writes (`putnami.workspace.json`, `package.json`,
`tsconfig.base.json`). It never seeds the mirror with generated source or its
manifest. The TypeScript emitter runs with `--format-project` naming the real
provider root, so Biome resolves against the provider's own configuration.

`clientgen-adopt` snapshots the manifests before any write and applies only the
moves two real emitter manifests prove: a generated package that moved, and the
constructor or registration call the contract promises for a service. Every
file is staged before any is written. Argument, credential and call-shape
changes are never inferred.

### External and framework transports are inventoried exactly

The workspace guard ([ADR 0003](0003-drift-is-the-generator-tasks-verdict.md))
derives coverage and consumer lineage from the provider descriptor and the
generated manifest symbols, and scans production Go and TypeScript sources for
handwritten first-party HTTP, Connect, SSE and WebSocket construction. Each
normalized transport expression is classified on its own. Low-level APIs stay
available in tests and exact external adapters. An unmarked external contract
needs both `thirdParty: true` and an entry in the strict workspace inventory
naming its authority, adapter, callsites, owner, contract tests and reason; that
inventory cannot exempt a first-party marker. Framework runtime transports use a
separate exact callsite inventory whose runtime name must equal the indexed
owning project, so the implementation boundary cannot become a consumer waiver.

## Invariants

- The provider declaration and emitted versioned contract are authoritative.
- Both readers consume the same strict semantic IR and fail on loss.
- Generated source and manifests never contain a credential value.
- Manifest symbols come from compiled emitters; workspace tooling does not
  recreate language naming rules.
- Project generation owns the complete dynamic output directory.
- Everything that decides an emitted byte is a declared cache-key entry.
- Sync and adopt materialize every indexed provider and render in isolation.
- External classification is explicit and cannot bypass first-party checks.
- The source guard proposes; it never edits an authored source.

## Consequences

- Adding a language means a reader and emitter for the same contract and a
  generated target declaration, without parsing another provider language.
- Provider builds publish contracts; `clientgen-sync` is the explicit operation
  that updates tracked client packages.
- Changing a formatting rule is a cache miss for every TypeScript target.
- Consumer lineage proves a binding exists for each generated operation. It does
  not prove application code calls every method.
