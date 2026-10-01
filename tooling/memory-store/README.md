# Memory store provider

`@putnami/memory-store` implements the **memory** collaboration contract
([`protocols/collaboration`](../../protocols/collaboration/README.md)): skills
load context, read a mission and checkpoint it, and this provider keeps the
records in a **directory** (`file`) or on a **branch of a Git repository**
(`git`), locally or behind a remote. No account, service or bundled repository
is involved: the workspace binding names the store.

## Use it

Declare the extension and bind the memory contract in `putnami.workspace.json`.

A directory in the workspace:

```json
{
  "extensions": ["/tooling/memory-store"],
  "options": {
    "collaboration": {
      "memory": {
        "provider": "@putnami/memory-store",
        "version": 1,
        "settings": { "backend": "file" }
      }
    }
  }
}
```

A Git repository shared by every clone through a remote:

```json
"memory": {
  "provider": "@putnami/memory-store",
  "version": 1,
  "settings": {
    "backend": "git",
    "remote": "git@example.com:acme/agent-memory.git",
    "branch": "main",
    "repository": "acme/app"
  }
}
```

Then `putnami memory checkpoint --input '{"mission":"…","precondition":{"mustNotExist":true},"idempotencyKey":"…","content":"…"}'`,
`putnami memory mission --input '{"mission":"…"}'`, `putnami memory context`,
or the MCP tools `memory.*` ([CLI guide](../cli/doc/25-collaboration-providers.md)).
The first call compiles the runtime from this directory (`bin/prepare`).

| Setting | Backend | Default | Meaning |
|---|---|---|---|
| `backend` | both | — (required) | `file` or `git`. Nothing is chosen by default. |
| `path` | both | `.putnami/collaboration/memory` (file), `.putnami/collaboration/memory.git` (git) | The store directory, or the local Git repository: absolute or relative to the workspace root. A missing or empty directory becomes a bare repository. The defaults belong to one worktree. |
| `remote` | git | none | A remote name of `path`, a URL, or a path (`./…` and `../…` are relative to the workspace root). Without it the records live on `branch` of `path`. |
| `branch` | git | `putnami-memory` | The branch holding the records. |
| `repository` | both | none | The repository identity a request that names none belongs to. |

**Credentials.** The Git backend authenticates the way `git` does for you:
credential helpers, the SSH agent, `GIT_SSH_COMMAND`. It never prompts
(`GIT_TERMINAL_PROMPT=0`), so a missing credential is an `unavailable` answer.
A `remote` URL carrying a password, an HTTP user, a query or a fragment is
refused as invalid settings; nothing in settings, records or answers carries a
credential.

## What it offers

| Operation | Offered | Behavior |
|---|---|---|
| `context` | yes | Records selected by identity, kinds and the resolved project selection, one bounded page in kind-then-id order. |
| `mission` | yes | One mission's record, with revision, provenance and freshness. |
| `checkpoint` | yes, `preconditions: atomic` | Compare-and-set against `expectedRevision`, or `mustNotExist` to create. |
| `search` | yes | Case-insensitive substring of title or content, under the same selection. No index. |

The semantics every backend shares — selection, revisions, concurrency,
idempotency, failures and retries — are in [doc/01-memory-store.md](doc/01-memory-store.md).
The choices behind them are in [ADR 0001](doc/adr/0001-one-memory-provider-two-backends.md).

## Layout

- `putnami.extension.json` — the runtime declaration and one tool per
  operation, each marked `putnami.dev/provider`.
- `cmd/putnami-memory-store` — the runtime: the `__putnami runtime-info`
  handshake and the `provider-tool` bridge (`collaboration.Serve`).
- `internal/memory` — the operations, the settings and the selection policy.
- `internal/store` — the record document, revisions and the backend interface.
- `internal/filestore` — the file backend.
- `internal/refstore` — the Git backend.

Run `./putnamiw test --projects @putnami/memory-store --enforce-coverage`. The
contract scenarios in `internal/memory/contract_test.go` run against the file
backend, the Git backend on a local branch and the Git backend behind a bare
remote; they need `git` on `PATH`.
