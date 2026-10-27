# Local collaboration provider

`@putnami/local-collaboration` implements the **tasks**, **proposals** and
**missions** collaboration contracts ([`protocols/collaboration`](../../protocols/collaboration/README.md))
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
      },
      "missions": { "provider": "@putnami/local-collaboration", "version": 1 }
    }
  }
}
```

Then `putnami tasks create --input '{"title":"…","idempotencyKey":"…"}'`,
`putnami proposals upsert --input '{"change":{"base":"main","head":"my-branch"},"title":"…"}'`,
or the MCP tools `tasks.*`, `proposals.*`, `missions.ask` and `missions.reply`
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
| missions v1 | `sync` (host-only), `ask`, `reply` | — |

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

## Missions

A local mission has one owner: you, the person who works in the workspace.
Agent sessions publish their state and plan with `missions sync`, ask you
decisions with `missions ask`, and reply to your messages with
`missions reply`. Decision ids are `D-<n>`, message ids `M-<n>`.

You answer a decision and write a message as **files in the store
directory** (`.putnami/collaboration/local` by default). The provider never
changes or removes them. Every missions call first takes the files it has not
taken yet, under the store lock and in the same write as the call, so two
processes never take one file twice.

A file the provider does not take stays as it is, and gets a note beside it:
`answers/D-3.json.refused` says why `answers/D-3.json` is not taken. Fix or
remove the file; the next call takes it or drops the note. The note is the
only report, because a sync result has no member for your files, and the
agent session that calls is not the person who wrote them.

To answer decision `D-3`, write `answers/D-3.json`:

```json
{ "option": "ship", "text": "Ship it after the gate passes." }
```

- `option` is the id of one of the decision's options, `text` is your answer.
  Give one or both; without `text`, the answer is the option's label.
- The first answer taken stands: a file for a decision that is no longer
  open is not taken, and gets no note. A file for an unknown decision or an
  unknown option, or that is not valid JSON with only these members, stays
  untaken, with a note.
- The session gets the answer in its inbox on its next sync. The decision is
  `acknowledged` once the session confirms it received the answer.

To send a message, write any `messages/<name>.json`:

```json
{ "mission": { "repository": "acme/app", "key": "issue-7" }, "kind": "prompt", "text": "Does the gate pass?" }
```

- `kind` is `info` (the default) or `prompt`. A session may reply to any
  message once, with `missions reply`; a prompt asks for that reply.
- A file is taken once for each content. Change the file's content to send a
  new message under the same name.
- A file that breaks the contract's rules (an invalid mission key, an empty
  or too long text, unknown members) or holds more than 256 KiB stays
  untaken, with a note, so it never reaches a session. Names that start with
  `.` and names that do not end in `.json` are not read.

The inbox lists the messages no session received yet, oldest first, at most
50 messages and 64 KiB per sync. The mission view lists at most 50 open
decisions, oldest first. `sync` answers `minIntervalMs: 5000`.

Owner files keep this extension command-free: it declares no command group,
and a file needs nothing but an editor. A command to answer and send can be
added later on top of the same files.

## Layout

- `putnami.extension.json` — the runtime declaration and one tool per
  operation, each marked `putnami.dev/provider`.
- `cmd/putnami-local-collaboration` — the runtime: the `__putnami runtime-info`
  handshake and the `provider-tool` bridge (`collaboration.Serve`).
- `internal/provider` — the operation handlers.
- `internal/store` — the store: `state.json` for tasks and proposals and
  `missions.json` for missions, each written by atomic rename under the one
  lock file `lock`; `answers/` and `messages/` hold the owner's files and the
  provider's `.refused` notes. The store directory is created with mode 0700
  and every file it writes has mode 0600.

Run `./putnamiw test --projects @putnami/local-collaboration --enforce-coverage`.
It includes the shared contract scenarios of
[`providertest`](../../protocols/collaboration/providertest/providertest.go),
which the GitHub provider runs too, the missions scenarios against a store on
disk, and two processes that sync, ask and reply on one store at once. The
end-to-end proof of tasks and proposals through the CLI and MCP entry paths is
`tooling/cli/internal/cli/collaboration_local_provider_test.go`. No test runs
missions through the CLI yet.
