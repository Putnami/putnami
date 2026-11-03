# Run an epic

An epic is one outcome that needs several issues, in dependency order. The
`epic` skill plans the issues, has the owner approve one envelope for all of
them, and launches one `build` session per issue, wave by wave. It never
codes. Two commands, which the `@putnami/contributor` executable serves, check
the plan and follow the children:

```bash
putnami epic plan --epic 42
putnami epic status --epic 42
putnami epic status --plan-file epic.json --output=json
```

`--epic <id>` reads `.putnami/<id>/epic.json` under the workspace root, and
refuses a plan that names another epic. `--plan-file <file>` reads another
file; a relative path resolves against the directory you typed the command in.
Neither command writes anything.

## The epic plan

An epic plan is one JSON object: the epic, the branch its children start
from, the envelope the owner approves, and its issues. Each issue carries the
slices of its change plan, in the format `change estimate` reads (see
[Estimate a change plan](07-change-estimate.md)).

```json
{
  "protocolVersion": 1,
  "epic": "42",
  "title": "Plans are sized and launched in waves",
  "base": "main",
  "envelope": { "tokens": 330000000, "activeMinutes": 640, "sessions": 3 },
  "issues": [
    {
      "key": "plan-command",
      "title": "putnami epic plan orders the issues",
      "task": { "source": "<tasks source>", "id": "43" },
      "dependsOn": [],
      "slices": [
        {
          "key": "plan-format",
          "title": "The epic plan format",
          "projects": ["@putnami/contributor"],
          "files": 4,
          "endpoints": 0,
          "screens": 0,
          "sensitiveZones": []
        }
      ]
    }
  ]
}
```

| Member | Rule |
|---|---|
| `protocolVersion` | `1`. |
| `epic` | Id of the epic: letters, digits, `.`, `_` and `-`, at most 64. It names the folder `.putnami/<epic>`. |
| `title` | One line of 1 to 200 characters. |
| `base` | Branch every child starts from unless it stacks on a parent, 1 to 255 printable ASCII characters. |
| `envelope.tokens` | Tokens every child session may spend in total, 1 to 10^12. |
| `envelope.activeMinutes` | Active minutes every child session may spend in total, 1 to 100000. |
| `envelope.sessions` | Child sessions that run at once, 1 to 20. |
| `issues` | 1 to 50 issues. |
| `key` | Lowercase letters, digits and hyphens, at most 64; unique in the epic. |
| `task` | Optional. The whole `{source, id}` reference of the issue's task, once it exists. |
| `dependsOn` | 0 to 50 distinct keys of other issues of the epic. |
| `slices` | 1 to 50 slices of the issue's change plan; slice keys are unique within the issue. |

The decoder is strict, as the change plan's is. It also refuses a dependency
on an unknown issue or on the issue itself, and a cycle, which it names:
`read-contract -> skill -> status-command -> read-contract`. The JSON Schema
of the format is `internal/estimate/schemas/epic-plan.v1.json` in this
extension; a test holds it and the decoder to the same members and bounds.

## Waves and the estimate

`epic plan` sizes each issue from its slices with the change estimate table,
including the members `options["@putnami/contributor"].estimate` sets, and
sums the issues. An issue's wave is 1 with no dependency, else one more than
the latest wave of its dependencies. Issues of a wave keep plan order.

```text
Epic .putnami/42/epic.json: Plans are sized and launched in waves
  Estimate: L, 52.0M to 147.1M tokens, 181 to 546 minutes
  Envelope: 330.0M tokens, 640 minutes, 3 sessions at once
Wave 1
  read-contract (task 41): M, 17.6M to 48.5M tokens, 44 to 144 minutes
  plan-command (task 43): S, 5.6M to 16.7M tokens, 31 to 86 minutes
Wave 2
  status-command (no task yet): M, 23.2M to 65.2M tokens, 75 to 230 minutes, after read-contract, plan-command
```

A line says so when the envelope is below the low bound of the estimate.
`--output=json` prints the same result as one JSON line: `epic`, `title`,
`base`, `envelope`, `size`, `tokens`, `minutes`, `waves` (the keys of each
wave) and `issues`, each with its `key`, `task`, `dependsOn`, `wave`, `size`,
`tokens` and `minutes`. A usage error exits 2; a plan or a policy it refuses
exits 1 with the path of the first value it refuses.

## Launch records

The `epic` skill records each launch in `.putnami/<epic>/launches.json`,
beside the plan, before the owner opens the session:

```json
{
  "protocolVersion": 1,
  "epic": "42",
  "launches": [
    { "issue": "plan-command", "target": "local", "branch": "epic-42/plan-command", "base": "main", "recordedAt": "2026-11-02T10:00:00Z" }
  ]
}
```

`target` is `local`, a local workspace the owner opens; it is the only launch
target. `branch` is the branch the child works on, and `base` the branch it
starts from: the epic's base, or the branch of the parent it stacks on. The
latest launch of an issue wins. A record that names an unknown issue, another
target or another epic is refused with exit 1.

## The status

`epic status` reads, for each issue with a task:

1. the task, with `tasks.get`;
2. the sessions of its mission, `issue-<id>`, with `missions.status`, in the
   repository of the workspace's `origin` remote;
3. its proposal, with `proposals.find` on its branch: the launch record's,
   else the newest session's. It looks against the launch base, then against
   the epic's base, and keeps a merged proposal, else an open or draft one,
   else a closed one.

An issue is `merged` or `closed` when its proposal is; `ready` when its
proposal is open for review; `closed` when its task is done or canceled with
no merged proposal; `in_progress` once it has a session or a draft;
`launched` once recorded; `planned` otherwise. Its tokens are the sum of what
its sessions reported, input, output and cache included. Its share is the
high bound of its estimate; past 150 % of it, the child stops for the owner.

An issue may launch now when it is planned, has a task, and every dependency
merged, or exactly one dependency is not merged, its proposal is ready, and
it started from the epic's base: the issue then starts from that parent's
branch, and a stack is never two levels deep. Nothing launches once the
children's tokens reach the envelope, nor beyond the envelope's sessions at
once. A session counts when it works, waits or was handed back, so a ready
issue whose session still runs counts, and one whose sessions ended or failed
does not and shows `no live session`; a launched issue with no session yet
counts too. When no mission could be read, every launched, in-progress or
ready issue counts. A stacked issue whose parent merged gets its rebase,
`git rebase --onto <base> <parent's last head>`, with the head commit
`proposals.status` reports for the parent; its proposal then targets the
epic's base. A dependency closed without a merge blocks the issues that need
it: each shows `blocked: <key> closed without a merge` until the owner
replans it.

```text
Epic .putnami/42/epic.json: Plans are sized and launched in waves
  Spent: 17.0M of 330.0M tokens; 1 of 3 sessions running
Wave 1
  read-contract (task 41): ready, https://example.test/pull/101, 12.0M tokens of a 48.5M share
  plan-command (task 43): merged, https://example.test/pull/102, 5.0M tokens of a 16.7M share
Wave 2
  status-command (task 44): planned, no usage reported, share 65.2M
Launch now
  status-command: from epic-42/read-contract, stacked on read-contract
```

A contract whose provider answers `unsupported` or `denied` is called once,
and listed under `Not read`; any other answer that is not `ok` is listed with
its issue. A workspace whose `origin` remote names no repository reads no
mission, and `Not read` says so; a mission with more than 1000 sessions is
read up to that bound, and listed with its issue. The whole read stops after
9 minutes, within the 10 the task allows: the status then shows what it read
and lists the call where it stopped as `unavailable`. A provider that cannot
be reached exits 1, since a partial read would look complete. Without the
host channel, as when the executable runs by hand, the
command exits 1 and names `putnami epic status`. `--output=json` prints the
result document: `tokens`, `envelopeSpent`, `running`, `issues` (each with
its `sessions`, `live`, `sessionsRead` and `blockedBy`),
`launchable` and `unavailable`.
