# ADR 0054 — The contract edge fires on the committed contract

- **Status**: accepted
- **Scope**: `@putnami/cli-model` (`workspace.TraceChangeImpact`,
  `ImpactReach`, `Project.ContractPath`), `@putnami/cli` (`internal/workspace`
  contract bindings, `--impacted` trace, `impacted` MCP tool)

## Context

On a consumer workspace of 154 projects, one `_test.go` inside an API workload
selected 1002 jobs across 153 projects. The path ran workload →[contract]→ its
generated Go client →[dependency]→ the workspace's CLI, an in-workspace
extension →[extension-consumer]→ every project. The contract edge fired on
any provider change, because the workload's `build~describe` reads its test
files and a client's `^describe` reference carries it.

Whether a change moved the contract is readable from the diff:

- A client's committed `client.putnami.json` states `contractSha256`, the
  sha256 of the provider's committed `schema/openapi.json`. The client's
  identity folds that digest and nothing else of the provider.
- A provider change that moves its generated contract without committing it
  fails the provider's own `clientgen` task (`drift: fail`), and the provider
  is selected by its own change.
- A regenerated client changes files the client owns, which select it
  directly.

## Decision

1. **The contract edge from a provider fires only when the provider's
   committed contract is one of the changed files.** At load, the workspace
   records on each provider the contract's workspace-relative path
   (`Project.ContractPath`) and the sha256 of its bytes
   (`Project.ContractSHA256`), from the read that resolves the service
   identity. `TraceChangeImpact` crosses a contract edge only out of the
   providers whose `ContractPath` is a changed file, cleaned as path
   ownership cleans it. When it fires, it carries tasks by the `^` rule of
   [ADR 0044](0044-selection-is-task-level.md).
2. **`why_impacted` fires every contract edge.** `ImpactReach` answers for a
   whole-project change with no file list, which includes the contract. The
   reach stays a superset of every real selection, so `why_impacted` finds a
   path to every project `impacted` lists.
3. **A fired edge names what it rests on.** The `--verbose` line prints the
   contract path and the first 12 hex digits of its digest. The
   `selection:impacted` record's `edges[]` and the `impacted` answer's
   `reasons[]` carry `via` and the full `contractSha256`, and omit both on
   every other edge.
4. **Neither field enters an identity.** `ContractPath` and `ContractSHA256`
   are trace evidence. The provider's metadata digest and every cache key
   stay unchanged; the client's identity folds the digest its own manifest
   states.

## Consequences

- An implementation-only provider change selects the provider and its
  dependency dependents, and the extension-consumer edges those reach.
- A change to the contract file alone seeds only the provider's tasks that
  read it, which no `^` reference names, so it reaches no client. The
  provider's `clientgen` drift check guards the clients in that case.
- A change that commits a regenerated contract with its regenerated clients
  reaches each client twice. The trace names one reason per project.
- `--impacted` reads uncommitted files, so a contract edited in the working
  tree fires the edge, and the recorded digest is the working tree's.
- A long-lived MCP session reports the `contractSha256` of its last workspace
  load. The path, which gates the edge, does not depend on content.
