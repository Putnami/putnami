# 0001: Collaboration provider contracts

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/collaboration` (`protocols/collaboration`),
  its providers, and the CLI route that calls them

## Context

Contributor workflows read and update work items, open and review change
proposals, and keep a mission's memory across sessions. A workspace must be
able to use any tracker, work without a hosting service, and move its memory
without rewriting skills. The extension model already gives a transport to
extension code (manifest-declared tools, run as a lazy subprocess, reached
from the MCP server). What it lacks is a provider-neutral operation surface, a
way for a workspace to choose which extension serves it, and failure semantics
a caller can act on.

## Decision

1. **A separate protocol module.** The contracts (requests, results,
   envelopes, bindings) are one domain wire contract, so they live in
   `go.putnami.dev/protocol/collaboration`. `protocols/extension` does not
   import it; its only change is the additive `ToolCallRequest.provider`
   member. The module is not published: in-repository providers consume it
   by `replace`, and publication is a reviewed decision.
2. **Three contracts, each versioned alone.** `tasks`, `proposals` and
   `memory` each have an integer major version (all `1`). A published version
   never changes meaning: moving an operation between required and optional,
   or adding a member an older reader would misread, is a new version.
   Version 1 is unpublished and may still gain members. The catalog in
   `collaboration.go` is the single definition of every operation's access,
   requirement, destructiveness, precondition rule and documents; drift tests
   hold the JSON Schemas to it.
3. **A provider declares operations on its tools.** An operation is a
   manifest-declared tool whose `_meta["putnami.dev/provider"]` names the
   contract, version and operation and, where the operation takes a revision,
   how it enforces it (`atomic`, `checked`, `none`). No manifest field and no
   loader negotiation are added: an older CLI sees an ordinary namespaced
   tool. A provider tool is never advertised under its own name.
4. **The binding lives in `options.collaboration` of
   `putnami.workspace.json`, and nowhere else**: a core-read namespace every
   CLI version accepts, read from the workspace document alone. Each contract
   names one provider extension and one version; `require` lists optional
   operations the workspace depends on; `settings` is forwarded verbatim to
   the provider. The binding is refused for a repeated member, an unknown
   contract or member, an unsupported version, a credential-named setting,
   and a setting value that is a URL with credentials: a password under any
   scheme, or any user under `http`/`https`. An SSH account without a password
   (`ssh://git@host/…`) is not a credential. Putnami never selects a provider
   the binding does not name, and never substitutes another when it fails.
5. **One route, two entry paths.** The MCP server exposes
   `<contract>.<operation>` and `<contract>.capabilities`; the command line
   exposes `putnami <contract> <operation> --input <json>`. Both call
   `mcp.CallProviderOperation` through the extension tool transport and print
   the same envelope. The names exist only in a workspace that declares
   `options.collaboration`. Capabilities are answered from the binding and the
   manifest without running the provider.
6. **Outcomes are closed, and uncertainty is explicit.** `ok`, `not_found`,
   `conflict`, `unsupported`, `invalid`, `denied`, `unavailable`,
   `unresolved`. The router never retries. A mutation whose provider started
   and did not deliver a valid answer (timeout, cancellation, crash, a refused
   response, an answer about another item) is `unresolved`, never retryable.
   Its `reconcile` hint is prose that names the read, or the idempotent
   repeat, as `<contract>.<operation>` of the same contract;
   `ReconcileOperation` extracts it for automation. Every operation that adds
   an item takes an idempotency key, `proposals.upsert` is keyed by its exact
   repository/base/head, and `memory.checkpoint` requires `expectedRevision`
   or `mustNotExist` and an atomic provider. A precondition the provider
   cannot enforce is refused, never dropped.
7. **Credentials stay with the provider.** Requests, bindings and envelopes
   carry none; provider stderr never reaches an envelope; the router redacts
   credential-named environment values and URL userinfo from every message
   and result.
8. **Documents are bounded, and a list page shrinks to fit.** The document
   bound is 4 MiB, sized to hold every request, and every one-item result,
   whose members are all at their bounds and made of characters the encoder
   expands to six bytes (`TestMaxDocumentBytesHoldsEveryMaximalDocument`).
   `page.size` is a maximum; 0 means the default, 20, in schema and Go. A
   provider returns fewer items, with `page.next`, rather than exceed the
   bound, and `Serve` re-runs a list handler with the count that fits (a read
   has no effect, and the size shrinks each time). A cursor names a position,
   not a page number, so the size may change within one traversal. An
   oversized page is refused as `provider.invalid_response` with a hint to
   ask for a smaller `page.size`.
9. **A proposal carries optional `labels` and `assignees`**, including what
   the provider applies from its own settings. Absent means not reported;
   `[]` means none. A provider reports each on every answer about a proposal
   or on none. Labels follow the task label rules (at most 50, 64 characters,
   distinct); assignees follow the task assignee rules (at most 100 distinct
   tokens) and are information, never a lease. The Go members use `omitzero`
   so a re-encoding keeps absent apart from empty.
10. **Revisions change whenever an answered member changes.** A proposal's
    revision covers its labels and assignees. A task's parent is the one
    member a provider may leave outside the revision, and the description of
    its `link` tool, which `capabilities` reports, must say so; an
    `expectedRevision` then does not detect a concurrent `link`.
11. **Shared conformance scenarios.** `providertest.RunTasks`,
    `RunProposals` and `RunMemory` run against any provider. `RunProposals`
    checks that an upsert answer and the reads after it agree on labels and
    assignees. `RunMemory` checks creation with `mustNotExist`, key replay,
    `revision.conflict` and `record.exists` with `current`, resume from a
    second instance on the same store, exactly one of several concurrent
    writers landing, pages with a changing size, workspace isolation, and the
    largest checkpoint the contract accepts read back unchanged
    (`providertest.MaximalCheckpoint`).
12. **A proof provider ships in the repository.** `@putnami/local-collaboration`
    implements tasks and proposals over a store directory inside the
    workspace, depends only on protocol modules, and is the provider-author
    guide's worked example. It offers no `link` and answers `labels: []` and
    `assignees: []`. Memory adapters are separate providers.

## Rejected alternatives

- **A structured `reconcile` member.** Every producer already names the
  operation in prose; one rule keeps the envelope unchanged.
- **Include the parent in the task revision when it is read.** `find` does
  not read parents, so `find` and `get` would report two revisions for one
  issue, and a precondition taken from `find` would always fail.
- **A capability member for the parent exception.** An operation's
  description is already where provider-specific limits live.

## Consequences

- Skills speak one surface and learn what a workspace offers from
  `capabilities`; a contract moves between providers by editing one block.
- A provider cannot claim more than the contract checks: annotations that
  disagree with an operation, a missing required operation, or an undeclared
  precondition withdraw the contract version from its offer, and the binding
  fails with the reason. Package-time validation is the provider's own
  manifest test (`ReadProviderOffer`).
- Operations the bound provider does not offer have no MCP tool and answer
  `unsupported` on the command line.
- No CLI contract level moves. An older CLI exposes nothing for a binding,
  and advertises provider tools under their own names, where each answers
  `invalid` because a provider serves only routed calls.
- A provider in another language shortens an oversized page itself, and a
  cursor that encodes a page number must change encoding.
- A caller holding a proposal revision across someone else's label or
  assignee change gets `conflict` on upsert.
