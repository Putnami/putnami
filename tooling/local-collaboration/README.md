# Local collaboration provider

`@putnami/local-collaboration` implements the **tasks** and **proposals**
collaboration contracts ([`protocols/collaboration`](../../protocols/collaboration/README.md))
over a store directory inside the workspace. It needs no account and no
network, and it is the worked example of the
[provider-author guide](../../protocols/collaboration/doc/02-provider-authoring.md).

## Use it

Declare the extension and bind the contracts in `putnami.workspace.json`:

```json
{
  "extensions": ["/tooling/local-collaboration"],
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

Then `putnami tasks create --input '{"title":"…","idempotencyKey":"…"}'`,
`putnami proposals upsert --input '{"change":{"base":"main","head":"my-branch"},"title":"…"}'`,
or the MCP tools `tasks.*` and `proposals.*`
([CLI guide](../cli/doc/25-collaboration-providers.md)). The first call compiles
the runtime from this directory (`bin/prepare`, Go toolchain resolved like any
extension runtime).

| Setting | Default | Meaning |
|---|---|---|
| `root` | `.putnami/collaboration/local` | The store directory, absolute or relative to the workspace root. Each worktree has its own unless `root` is absolute. |
| `repository` | `local` | The repository a proposal belongs to when the request names none. |

## What it offers

| Contract | Offered | Not offered |
|---|---|---|
| tasks v1 | `find`, `get`, `create`, `update`, `transition` | `assign`, `link`, `claim` |
| proposals v1 | `find`, `upsert`, `status`, `review` | `merge` |

- It runs **no hosted checks**: `proposals status` always reports
  `checks.state: unsupported`, with a detail that says so. Run the workspace
  gate and reference its session instead.
- It offers **no merge**, and nothing it records is pushed or opened on a
  hosting service.
- A proposal has **no labels and no assignees**: every proposal answer
  reports both as `[]`, so a caller that verifies them sees none rather than
  a provider that does not report them.
- `update`, `transition` and `upsert` declare `preconditions: atomic`: the
  revision comparison and the write happen under an exclusive `flock` on the
  store. A write waits at most 10 s for a lock another process holds, then
  answers `unavailable` (`store.busy`, retryable) having written nothing. On a
  platform without `flock` every write is refused as `unavailable` instead of
  running unlocked.
- `create` and `review` replay a repeated idempotency key and refuse the same
  key with other content; a key names one write of one operation, so the same
  key given to `tasks create` and `proposals review` names two writes, as on
  the GitHub provider. `upsert` keeps at most one open proposal per exact
  repository/base/head.
- References are `{"source": "local:<store id>", "id": "T-1" | "P-1" | "R-1"}`;
  a reference from another store is `not_found`.

## Layout

- `putnami.extension.json` — the runtime declaration and one tool per
  operation, each marked `putnami.dev/provider`.
- `cmd/putnami-local-collaboration` — the runtime: the `__putnami runtime-info`
  handshake and the `provider-tool` bridge (`collaboration.Serve`).
- `internal/provider` — the operation handlers.
- `internal/store` — the store: one `state.json`, written by atomic rename
  under the lock file `lock`.

Run `./putnamiw test --projects @putnami/local-collaboration --enforce-coverage`.
It includes the shared contract scenarios of
[`providertest`](../../protocols/collaboration/providertest/providertest.go),
which the GitHub provider runs too. The end-to-end proof through the CLI and MCP entry paths is
`tooling/cli/internal/cli/collaboration_local_provider_test.go`.
