# Collaboration providers

A workspace chooses where its work items, change proposals and agent memory
live, one contract at a time, and every tool and script calls the same
operations whatever the backend is. Putnami routes each call to the provider
extension the workspace binds; the provider owns the backend, its
credentials, its identifiers and its mappings.

The wire contract lives in
[`protocols/collaboration`](../../../protocols/collaboration/README.md), with
[every operation](../../../protocols/collaboration/doc/01-operations.md) and a
[provider-author guide](../../../protocols/collaboration/doc/02-provider-authoring.md).
[ADR 0001 of the protocol](../../../protocols/collaboration/doc/adr/0001-collaboration-provider-contracts.md)
records why the contracts, the binding and the routing are shaped this way.

## Bind a provider

Bind each contract in the `options.collaboration` block of
`putnami.workspace.json`, and nowhere else. A project `putnami.json` and the
user's global configuration never bind a provider.

```json
{
  "extensions": ["@putnami/local-collaboration"],
  "options": {
    "collaboration": {
      "tasks": { "provider": "@putnami/local-collaboration", "version": 1 },
      "proposals": {
        "provider": "@putnami/local-collaboration",
        "version": 1,
        "settings": { "repository": "acme/app" }
      }
    }
  }
}
```

| Member | Meaning |
|---|---|
| `provider` | The provider extension's name. Exactly one installed, enabled extension must carry it. |
| `version` | The contract version the workspace speaks. The provider must implement it. |
| `require` | Optional operations the workspace depends on, such as `["merge"]`. A provider that lacks one makes the binding invalid instead of failing the first call that needs it. |
| `settings` | The provider's own configuration, passed verbatim on every call. A member whose name looks like a credential (`token`, `secret`, `password`, `apiKey`, …) invalidates the binding: the provider resolves its own credentials. |

Contracts are independent: `tasks` and `proposals` can name two providers, and
leaving `memory` out leaves it unbound. Putnami never picks a provider the
workspace did not name, even when exactly one is installed, and never falls
back to another one when the bound provider fails.

A workspace without `options.collaboration` has none of the commands or tools
below, and reserves none of their names.

## Call an operation

```bash
putnami <contract> <operation> [--input <json> | --input-file <path|->] [--output=json]
putnami tasks                                  # the contract, its binding and every operation
putnami tasks capabilities --output=json       # the same, as a document
putnami tasks create --input '{"title":"Write the guide","idempotencyKey":"docs:guide"}'
putnami proposals find --input '{"change":{"base":"main","head":"my-branch"}}'
putnami memory checkpoint --input-file checkpoint.json
```

Every call prints one envelope: the contract, version and operation, the
provider that answered, an `outcome`, and either a `result` or an `error`.
Human output indents it; `--output=json` prints it on one line. The command
exits `0` only when the outcome is `ok`, `1` for every other outcome, and `2`
for a usage error (an unknown flag, both `--input` and `--input-file`, or a
project-selection flag: an operation that takes `projects`, `impacted` or
`baseline` takes them inside its request document, exactly as the MCP tool
does).

The MCP server exposes the same operations as `<contract>.<operation>` tools,
plus `<contract>.capabilities`. Both entry paths run one routing function
against the same provider selection, so they return the same envelope for the
same request. A registry provider serves only from the release
`putnami.lock.json` pins: when that exact release cannot be prepared, both
paths answer `unavailable` (`binding.provider_missing`) and never run an
earlier release left behind the stable link. An operation the bound provider
does not offer has no MCP tool; on the command line it answers
`unsupported`.

## Outcomes

| Outcome | Meaning | Retry |
|---|---|---|
| `ok` | The operation completed. | — |
| `not_found` | A referenced item does not exist. | No |
| `conflict` | A precondition failed: a stale `expectedRevision`, an existing record under `mustNotExist`, an idempotency key reused with other content. `error.current` carries the current revision. Nothing was written. | After re-reading |
| `unsupported` | The contract is unbound, or the provider does not offer the operation, the version or the requested precondition. Nothing ran. | No |
| `invalid` | The request violates the contract. Nothing ran. | No |
| `denied` | The provider refused the credentials or permissions. | No |
| `unavailable` | The provider could not answer and no write can have happened. `error.retryable` says whether repeating can help. | If retryable |
| `unresolved` | A write may or may not have happened: the provider timed out, was canceled, crashed, or answered something the contract refuses. `error.reconcile` names the read that settles it. | Never automatically |

Putnami never retries a call. Operations that add items take an
`idempotencyKey`, and repeating one with the same key returns what the first
call produced, so reconciling an unresolved create or review means repeating
it with the same key. An upsert is keyed by its exact repository, base and
head, and never creates a second proposal for them. A checkpoint compares its
revision atomically.

`error.reason` is stable and machine-readable: `binding.missing`,
`binding.invalid`, `binding.ambiguous`, `binding.provider_missing`,
`binding.version_unsupported`, `binding.incomplete`, `operation.unsupported`,
`precondition.unsupported`, `request.invalid`, `provider.unavailable`,
`provider.timeout`, `provider.canceled`, `provider.failed`,
`provider.invalid_response`, `provider.identity_mismatch`, or a code the
provider chose.

## What Putnami checks

- The request is validated against the contract before the provider runs, and
  normalized: a list request always carries an explicit page (20 items unless
  it asks for up to 100), and a memory identity names the workspace.
- A request that carries a write precondition — `expectedRevision`, or
  `precondition.mustNotExist` on a creation — for an operation whose provider
  declares `preconditions: none` is refused as `unsupported`
  (`precondition.unsupported`); the precondition is never dropped.
- The answer must be about exactly what was asked: the same reference, the
  same repository, base and head, the requested state after a transition, no
  more items than the page allows. A read that answers for something else is
  `unavailable`; a write is `unresolved`.
- A provider's standard error never reaches an envelope, and the values of
  credential-named environment variables the provider inherits, and the
  userinfo of any URL, are redacted from every message and result. A result is
  redacted by value, so a secret is found however the provider's JSON encoder
  spelled it. A result that carries a secret redaction cannot remove in place,
  such as inside a number, is withheld: a read answers `unavailable` and a
  write `unresolved`.

## The local provider

[`@putnami/local-collaboration`](../../local-collaboration/README.md) implements
tasks and proposals over a store directory inside the workspace
(`.putnami/collaboration/local` unless `settings.root` says otherwise). It
needs no account and no network. It runs no hosted checks — `proposals status`
always reports checks `unsupported` — and offers no merge, assignment,
hierarchy or claim. Memory providers are separate extensions.

## Other shipped providers

[`@putnami/memory-store`](../../memory-store/README.md) implements the
**memory** contract, with a **file** backend (a directory in the workspace) and
a **git** backend (a branch of a Git repository, local or behind a remote).
Neither backend needs an account or a hosted service.

[`@putnami/github-collaboration`](../../github-collaboration/README.md)
implements **tasks** and **proposals** on GitHub issues and pull requests. It
reads the credential the way `gh` does, maps task states to labels through its
binding settings, and offers merge only as an optional capability.
