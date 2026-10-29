# Collaboration provider contracts

This package owns the four collaboration contracts — **tasks**, **change
proposals**, **memory** and **missions** — that a workspace binds to provider
extensions, and every document they exchange: the request and result of each
operation, the provider's response, the envelope every caller reads, the
binding block, and the capability document.

## Why

Contributor workflows find and update work items, open and review change
proposals, keep a mission's memory between sessions, and report an agent
session to its mission. Calling a hosting service or a storage layout directly
from a skill ties the workflow to that backend. The contracts put a
versioned, provider-neutral operation surface in between: skills call
`tasks.find` or `putnami proposals upsert`, and the extension the workspace
bound owns the backend calls, authentication, identifiers and mappings.

## What

### Contracts and operations

A contract version is a closed list of operations, each a read or a mutation,
required or optional. The catalog in [`collaboration.go`](collaboration.go) is
the single definition; [`doc/01-operations.md`](doc/01-operations.md)
describes every operation.

| Contract | Required | Optional |
|---|---|---|
| `tasks` v1 | `find`, `get`, `create`, `update`, `transition` | `assign`, `link`, `claim` |
| `proposals` v1 | `find`, `upsert`, `status`, `review` | `comments`, `merge` |
| `memory` v1 | `context`, `mission`, `checkpoint` | `search` |
| `missions` v1 | `sync`, `ask`, `reply` | — |

`capabilities` is reserved in every contract: the orchestrator answers it from
the binding and the provider's manifest, without running the provider.
`missions.sync` is host-only: the host that runs an agent calls it through the
CLI or the host channel, and
`missions.capabilities` marks it `hostOnly`.

Every result carries source-qualified opaque references (`{"source":
"<kind>:<locator>", "id": "…"}`), optional display URLs, revision tokens,
provider-neutral semantic states, and — for proposals — the exact
repository/base/head. Missions messages and decisions are the one exception:
they carry bare ids, which an agent types. Lists are bounded pages with an
opaque cursor.

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

A provider declares each contract it implements under the extension
manifest's `providers` member (`ProviderDeclaration` of
`go.putnami.dev/protocol/extension`): the contract version, the command every
operation runs, and the operations, each with a description, `openWorld` and —
for an operation that takes a revision precondition — how it enforces it
(`atomic`, `checked`, `none`). `ReadProviderOffer` checks a manifest's
providers against the catalog: known operations, the precondition rule of
each, `workspaceSelection` on operations that take the canonical selection
arguments, and every required operation of a declared version. The
orchestrator applies it when it resolves a binding; a provider author applies
it in a manifest test.

The orchestrator writes one `ProviderRequest` to the provider's standard input
and reads one `Response` from its standard output. `Serve` is the provider
side of one routed call.

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
  (`tasks.v1.json`, `proposals.v1.json`, `memory.v1.json`,
  `missions.v1.json`) with an `<operation>Input` and `<operation>Result`
  definition per operation.
  `InputSchema` inlines one operation's request schema.
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
- [`providertest`](providertest/providertest.go) holds the tasks, proposals,
  memory and missions scenarios every provider runs (`RunTasks`,
  `RunProposals`, `RunMemory`, `RunMissions`); it names nothing a particular
  backend defines, so a provider can run it against a shared one. A scenario
  of an optional operation runs only against a provider that offers it.
  `@putnami/memory-store` runs `RunMemory` against its file backend, its Git
  backend and its Git backend behind a bare remote. No provider in this
  repository implements missions yet: `RunMissions` runs against an in-test
  store.
- The CLI routes calls: [`tooling/cli/doc/25-collaboration-providers.md`](../../tooling/cli/doc/25-collaboration-providers.md).
- [`@putnami/memory-store`](../../tooling/memory-store/README.md) implements
  the memory contract with a file or a Git backend.

There is no TypeScript twin: the contracts are called through the CLI and the
extension host RPC, and providers written in any language speak JSON.

Run `./putnamiw test --projects go.putnami.dev/protocol/collaboration --enforce-coverage`.

## Feature and spec

The module owns the feature of its contracts,
`collaboration/provider-contract`, specified in
[`specs/provider-contract.json`](specs/provider-contract.json). The tests here
prove the catalog, strict parsing and bounds, the fixture corpus, the schemas,
envelope outcomes, bindings, provider declarations, `Serve`, the document
bound, and the shared scenarios against a reference provider. Each conforming
implementation proves the rest in its own tests:

- The CLI (`tooling/cli`) refuses before it starts a provider what the
  provider cannot serve, checks every answer against its request, missions
  answers included, reports an uncertain write as unresolved, keeps
  credentials out of every envelope, and keeps pages within their bounds. The
  CLI's `cli/collaboration-providers` keeps what the CLI adds: the binding,
  capabilities, the two entry paths, the routed providers, host-only operations
  and contract names that never shadow a command.
- `@putnami/local-collaboration` and `@putnami/github-collaboration` pass the
  task and proposal scenarios, and their manifests draw no diagnostic.
  `@putnami/local-collaboration` also passes the missions scenarios, across
  concurrent processes on one store.
- `@putnami/memory-store` passes the memory scenarios on every backend, and
  its manifest draws no diagnostic.
- `@putnami/contributor` stops `putnami change finalize` at an unresolved
  write without repeating it.

## Decisions

- [ADR 0001 — Collaboration provider contracts](doc/adr/0001-collaboration-provider-contracts.md):
  placement, binding location, provider declaration, versioning, routing names,
  outcome semantics, the 4 MiB document bound and shorter pages, proposal
  `labels` and `assignees`, what a revision covers, and the shared scenarios.
- [ADR 0002 — The contracts are published as preview](doc/adr/0002-the-contracts-are-published-as-preview.md):
  publication, the `preview` status, the task `key` and `createdAt`, and why the
  version 1 documents are fixed from the first published module.
- [ADR 0003 — The missions contract](doc/adr/0003-missions-contract.md): the
  missions operations, host-only operations, natural keys for `sync` and
  `reply`, and bare ids. It amends ADR 0001 decisions 5 and 6.

## Support

- **Status:** `preview`, recorded as
  `{"id": "go.putnami.dev/protocol/collaboration", "kind": "protocol", "status": "preview"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
- **Evidence:** the CLI routes every collaboration call through this module;
  `@putnami/local-collaboration`, `@putnami/github-collaboration` and
  `@putnami/memory-store` run the shared `providertest` scenarios; the fixture
  corpus, the drift tests and the catalog tests pin every document.
- **What may still change:** new optional operations, the Go helpers (`Serve`,
  `ReadProviderOffer`, `providertest`) and the guides. The version 1 documents
  do not change: a new member is a new contract version
  ([ADR 0002](doc/adr/0002-the-contracts-are-published-as-preview.md)).
- **Why not `stable`:** the CLI route and every provider live in this
  repository. No consumer across a boundary this repository does not control
  uses the contracts yet.
