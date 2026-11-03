# Linear collaboration provider

`@putnami/linear-collaboration` implements the **tasks** collaboration
contract ([`protocols/collaboration`](../../protocols/collaboration/README.md))
on Linear: tasks are the issues of one team. It offers `find`, `get`,
`create`, `update` and `transition`.

## Use it

Declare the extension and bind the `tasks` contract in
`putnami.workspace.json`. Contracts bind independently, so `proposals` can
stay on GitHub:

```json
{
  "extensions": ["@putnami/linear-collaboration"],
  "options": {
    "collaboration": {
      "tasks": {
        "provider": "@putnami/linear-collaboration",
        "version": 1,
        "settings": { "team": "ENG" }
      }
    }
  }
}
```

Set `LINEAR_API_KEY` to a Linear API key (Linear settings → Security & access
→ Personal API keys), then call `putnami tasks <operation>`
([CLI guide](../cli/doc/25-collaboration-providers.md)):

```bash
export LINEAR_API_KEY=lin_api_...
putnami tasks find --input '{"states":["open"],"labels":["Bug"]}'
putnami tasks transition --input '{"ref":{"source":"linear:acme","id":"<issue UUID>"},"state":"in_progress"}'
```

The first call compiles the runtime from this directory (`bin/prepare`).

### Settings

| Setting | Default | Meaning |
|---|---|---|
| `team` | required | The key of the team served, such as `ENG`. `find` and `create` work in this team. |

The binding carries no credential. The provider reads `LINEAR_API_KEY` and
nothing else, sends it only to `https://api.linear.app/graphql`, and removes it
from every answer.

## What a task is

| Task member | Linear |
|---|---|
| `ref` | `{"source": "linear:<organization urlKey>", "id": "<issue UUID>"}`. The UUID stays the same when the issue moves to another team. |
| `key` | The identifier, `ENG-42`. |
| `state` | The workflow state's type: triage, backlog and unstarted are `open`, started is `in_progress`, completed is `done`, canceled and duplicate are `canceled`. |
| `providerState` | The workflow state's name, such as `In Review`. |
| `labels` | Label names. |
| `assignees` | The assignee's display name. |
| `revision` | `v` and 32 hex digits of a digest of title, description, state name, labels and assignee. |

- `transition` moves the issue to its team's first workflow state, by
  position, of the target type: `unstarted` for `open`, `started` for
  `in_progress`, `completed` for `done`, `canceled` for `canceled`. Linear has
  no type for `blocked`, so a transition to it is `unsupported`.
- `create` names the issue by an id derived from the team and the idempotency
  key, and records a digest of the request in an HTML comment at the end of
  the description. Linear displays that line in the issue; a task's body does
  not carry it. A repeat returns the same issue; the same key with other
  content is a conflict.
- A label name no team or workspace label carries creates a team label. A
  label group is never applied.
- `update` and `transition` check `expectedRevision` against the issue read
  just before the write: Linear has no conditional write.

## Live proof

`bin/live-test <team key>` runs the shared task scenarios against a real team
with `LINEAR_API_KEY`. The team's description must contain
`putnami-collaboration-live-test`; the run deletes the issues and `pt-` labels
it created.

Decisions: [ADR 0001](doc/adr/0001-linear-collaboration-provider.md).
