# ADR 0007 — A generated client is built, not committed

- **Status**: accepted. Replaces [ADR 0003](0003-drift-is-the-generator-tasks-verdict.md)
  decisions 1, 2 and 4, and amends [ADR 0001](0001-clients-are-generated-from-the-published-contract.md)
  and [ADR 0006](0006-the-guard-reads-and-keys-on-the-git-candidate-cut.md).
- **Scope**: `@putnami/clientgen` (`tooling/clientgen-extension`), the
  `generatedFrom` member of `putnami.json` (`protocols/workspace`), the client
  generators of `go.putnami.dev/api` and `@putnami/client`, the `@putnami/go`
  and `@putnami/typescript` lifecycle commands
- **Date**: 2026-11-11

## Context

A committed client carries a manifest, `client.putnami.json`, that records the
sha256 of the provider's contract and of every generated file. Two pull
requests that change one API in different operations conflict on those
digests, even when the contract and the code merge cleanly. Regenerating
resolves the conflict, so it carries no content. A workspace with 46 committed
clients risks one on every API change.

The provider writes into its client's directory today: `build-describe`,
`build-generate`, `clientgen-go` and `clientgen-ts` all run as the provider and
own the client directory through `pathFrom`. A consumer-only selection plans no
generation, which works only because the bytes are committed.

## Decision

**1. Git holds no generated client file.** A client commits only stable,
authored files: its `putnami.json`, its `go.mod` or `package.json` and
`tsconfig.json`, and handwritten code or tests. The emitters write no
`client.putnami.json`. Every generated path is ignored by Git.

**2. A client project names its provider in `generatedFrom`.** The member of
its `putnami.json` holds the provider's `x-putnami-client` service id, the
language, and the generation options: the Go package name, the Go client type
name and the omitted operations. The Go import path is the client's own
`go.mod` module; the TypeScript package name is its own `package.json` name.
The core resolves the service id to the provider whose committed
`schema/openapi.json` declares it, and passes that contract's path to the
client's jobs.

**3. The client project generates itself, from committed files only.** The
`client-generate` command runs as the client and reads the provider's committed
contract and its own `generatedFrom`. No project writes into another, and no
generation builds the provider. The output is the generated closure only: the
Go `client.gen.go` file, or the TypeScript `src` directory. Authored files
beside it are never captured or restored.

A provider whose client lives inside its own module (a Go package of the same
module) declares that target in its committed `clientgen.json`. The provider's
`client-generate` writes it, inside the provider's own tree.

**4. Code declarations name no target.** `api.Clients()` and
`clientGenerator()` emit the contract's `x-putnami-client` metadata, including
each operation's producer project and feature, into the committed
`schema/openapi.json`. They no longer choose targets, outputs or names, and no
build writes `.gen/clientgen/config.json` or stages a client.

**5. Generation runs before every language command that reads it.** The
`@putnami/go` and `@putnami/typescript` `build`, `test`, `lint`, `serve` and
`package` commands depend on `client-generate`. The dependency plans nothing
in a project without the command. A consumer reaches a client through its
ordinary dependency, so a consumer-only selection generates or restores it.

**6. The cache keeps today's reuse.** `client-generate` is cacheable. The
client's identity folds its provider's contract digest
([CLI ADR 0054](../../../cli/doc/adr/0054-the-contract-edge-fires-on-the-committed-contract.md),
as amended), so a provider change that leaves its contract alone reuses every
client entry, and a contract change moves every client key.

**7. The guard keeps what Git can show.** `clientgen-workspace-check` still
validates provider discovery, the inventories, committed `.gen` contracts and
the handwritten-transport scan. It reports a tracked file that carries the
generated marker as `clientgen.committed-generated-client`. It no longer reads
manifests, hashes or coverage. `drift: "fail"` leaves every client output,
because no committed bytes remain to compare.

**8. Sync and adopt are removed.** With nothing committed, `clientgen-sync` has
nothing to update, and `clientgen-adopt` has no manifest pair to prove a move.

## Consequences

- A fresh worktree has no client until a Putnami command plans it. The core
  restores clients on the first-run pass and from a SessionStart hook
  ([CLI ADR 0087](../../../cli/doc/adr/0087-a-fresh-worktree-restores-its-generated-clients.md)).
- A program the CLI builds before it plans any job cannot wait for
  `client-generate`. The `@putnami/cloud` runtime is one: the workspace probe
  needs it first. Its `prepare-runtime` emits the clients of its replaced
  modules from their committed contracts into a temporary directory and builds
  through `go build -overlay`. It writes nothing into the source tree, and its
  runtime digest already covers each replaced module's `clientgen.json` and
  contract.
- A published client holds generated code: packaging runs after
  `client-generate`, and both publishers package the worktree. An external Go
  consumer resolves through the module proxy; `GOPROXY=direct` reads Git, which
  holds no client.
- `schema/openapi.json` stays committed and keeps its own conflicts: it anchors
  each client's identity.
- The contract edge no longer orders jobs: a client reads the provider's
  committed contract, not its build. A provider may depend on its own client.
- Consumer lineage from manifest symbols is gone. The handwritten-transport
  scan still names the binding to call.

## Alternatives rejected

- **A reduced manifest with stable fields.** Keeps a second file per client and
  the name of a generated artifact.
- **The provider keeps writing, with a consumer→provider generation edge.**
  Keeps a cross-project write and a special planner edge.
- **A core planner rule ordering generation first.** A command dependency
  already expresses it, without a special case in the core.
