# Collaboration provider contracts

This package owns the three collaboration contracts — **tasks**, **change
proposals** and **memory** — that a workspace binds to provider extensions,
and every document they exchange: the request and result of each operation,
the provider's response, the envelope every caller reads, the binding block,
and the capability document.

## Why

Contributor workflows find and update work items, open and review change
proposals, and keep a mission's memory between sessions. Calling a hosting
service or a storage layout directly from a skill ties the workflow to that
backend. The contracts put a versioned, provider-neutral operation surface in
between: skills call `tasks.find` or `putnami proposals upsert`, and the
extension the workspace bound owns the backend calls, authentication,
identifiers and mappings.

## What

### Contracts and operations

A contract version is a closed list of operations, each a read or a mutation,
required or optional. The catalog in [`collaboration.go`](collaboration.go) is
the single definition; [`doc/01-operations.md`](doc/01-operations.md)
describes every operation.

| Contract | Required | Optional |
|---|---|---|
| `tasks` v1 | `find`, `get`, `create`, `update`, `transition` | `assign`, `link`, `claim` |
| `proposals` v1 | `find`, `upsert`, `status`, `review` | `merge` |
| `memory` v1 | `context`, `mission`, `checkpoint` | `search` |

`capabilities` is reserved in every contract: the orchestrator answers it from
the binding and the provider's manifest, without running the provider.

Every result carries source-qualified opaque references (`{"source":
"<kind>:<locator>", "id": "…"}`), optional display URLs, revision tokens,
provider-neutral semantic states, and — for proposals — the exact
repository/base/head. Lists are bounded pages with an opaque cursor.

### Envelope and outcomes

Every call answers one `Envelope` (`schemas/collaboration.json`): contract,
version, operation, the provider that answered, an `outcome`, and a `result`
(only when `ok`) or an `error`. The outcome vocabulary is closed: `ok`,
`not_found`, `conflict`, `unsupported`, `invalid`, `denied`, `unavailable`,
`unresolved`. `unresolved` means a mutation may or may not have happened; it is
never retryable and names the read that reconciles it.

### Binding

A workspace binds contracts in the `options.collaboration` block of
`putnami.workspace.json` (`Binding`, `ParseBindings`): one provider extension
and one contract version per contract, an optional list of required optional
operations, and the provider's settings. A repeated member, an unknown
contract or member, an unsupported version and a credential-named setting are
refused.

### Provider declaration

A provider implements an operation as a manifest-declared extension tool whose
`_meta` carries `putnami.dev/provider` (`ProviderDeclaration`): the contract,
version and operation, and — for an operation that takes a revision
precondition — how it enforces it (`atomic`, `checked`, `none`).
`ReadProviderOffer` checks a manifest's declarations against the catalog:
annotations that agree with the operation's access and destructiveness,
`workspaceSelection` on operations that take the canonical selection
arguments, one tool per operation, and every required operation of a declared
version. The orchestrator applies it when it resolves a binding; a provider
author applies it in a manifest test. `Serve` is the provider side of one
routed call.

### Strict parsing

`ParseRequest`, `ParseResult`, `ParseResponse`, `ParseEnvelope` and
`ParseBindings` refuse a document that is not one JSON object, repeats a
member, carries a member the Go type does not declare (compared
case-sensitively), or has a member of the wrong type, and then validate its
semantics. Every diagnostic code carries the `collaboration.` prefix;
`ValidErrorCodes` is the closed set.

## How

- `schemas/collaboration.json` (envelope, response, binding, declaration,
  capabilities, shared definitions) and one schema per contract version
  (`tasks.v1.json`, `proposals.v1.json`, `memory.v1.json`) with an
  `<operation>Input` and `<operation>Result` definition per operation.
  `InputSchema` inlines one operation's request schema for an MCP tool.
- `fixtures/valid/` and `fixtures/invalid/` hold the corpus, named
  `<kind>.<…>.json`; `fixtures/expectations.json` pins the codes of every
  invalid fixture. `conformance_test.go` runs it, `drift_test.go` keeps the
  schemas, the Go types and the closed vocabularies on one member set, and
  `catalog_test.go` pins what each contract version requires.
- [`doc/02-provider-authoring.md`](doc/02-provider-authoring.md) is the
  provider-author guide, with
  [`@putnami/local-collaboration`](../../tooling/local-collaboration/README.md)
  as the worked example;
  [`@putnami/github-collaboration`](../../tooling/github-collaboration/README.md)
  implements the same contracts on GitHub.
- [`providertest`](providertest/providertest.go) holds the tasks, proposals
  and memory scenarios every provider runs (`RunTasks`, `RunProposals`,
  `RunMemory`); it names nothing a particular backend defines, so a provider
  can run it against a shared one. `@putnami/memory-store` runs `RunMemory`
  against its file backend, its Git backend and its Git backend behind a bare
  remote.
- The CLI routes calls: [`tooling/cli/doc/25-collaboration-providers.md`](../../tooling/cli/doc/25-collaboration-providers.md).
- [`@putnami/memory-store`](../../tooling/memory-store/README.md) implements
  the memory contract with a file or a Git backend.

There is no TypeScript twin: the contracts are called through the CLI and the
MCP server, and providers written in any language speak JSON.

Run `./putnamiw test --projects go.putnami.dev/protocol/collaboration --enforce-coverage`.

## Decisions

- [ADR 0001 — Collaboration provider contracts](doc/adr/0001-collaboration-provider-contracts.md):
  placement, binding location, provider declaration, versioning, routing names,
  outcome semantics, the 4 MiB document bound and shorter pages, proposal
  `labels` and `assignees`, what a revision covers, and the shared scenarios.

## Support

The module is not published yet and claims no support status: publication and
a status are a separate reviewed decision recorded in
[`putnami.support.json`](../../putnami.support.json). Providers in this
repository consume it by `replace`.
