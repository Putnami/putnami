# Estimate a change plan

`putnami change estimate`, which the `@putnami/contributor` executable serves,
reads a change plan and sizes it S, M or L, with a range of tokens and a range
of minutes for the plan and for each slice. The same plan and the same policy
always give the same estimate.

```bash
putnami change estimate --issue 42
putnami change estimate --plan-file plan.json --output=json
```

`--issue <id>` reads `.putnami/<id>/plan.json` under the workspace root, and
refuses a plan that names another issue. `--plan-file <file>` reads another file; a
relative path resolves against the directory you typed the command in. The
command reaches no provider and writes nothing.

## The plan format

A plan is one JSON object: the format version, the issue the change
delivers, and its slices in the order they run. A slice is one step the owner
can validate on its own.

```json
{
  "protocolVersion": 1,
  "issue": "42",
  "slices": [
    {
      "key": "plan-format",
      "title": "The plan format and its strict decoder",
      "projects": ["@putnami/contributor"],
      "files": 6,
      "endpoints": 0,
      "screens": 0,
      "sensitiveZones": []
    },
    {
      "key": "estimate-command",
      "title": "putnami change estimate sizes a plan",
      "projects": ["@putnami/contributor", "go.putnami.dev/protocol/telemetry"],
      "files": 9,
      "endpoints": 1,
      "screens": 0,
      "sensitiveZones": ["public API"],
      "effort": { "model": "opus", "reasoning": "high" }
    }
  ]
}
```

| Member | Rule |
|---|---|
| `protocolVersion` | `1`. |
| `issue` | Id of the issue, as its tracker writes it: letters, digits, `.`, `_` and `-`, at most 64. It names the folder `.putnami/<issue>`. |
| `slices` | 1 to 50 slices. |
| `key` | Lowercase letters, digits and hyphens, at most 64; unique in the plan. |
| `title` | One line of 1 to 200 characters. |
| `projects` | 1 to 50 distinct workspace project ids. |
| `files` | Expected count of files the slice touches, 0 to 10000. |
| `endpoints` | HTTP or RPC endpoints the slice adds or changes, 0 to 1000. |
| `screens` | User interface screens the slice adds or changes, 0 to 1000. |
| `sensitiveZones` | 0 to 20 distinct one-line names, such as `data migration` or `public API`. |
| `effort` | Optional. `model` and `reasoning`, free tokens the estimate does not read. |

The decoder is strict: it refuses invalid UTF-8, a repeated member, a member
the format does not name, a missing member, a number with a fraction, and any
value out of its bounds. It names the path of the first value it refuses and
exits 1. The JSON Schema of the format is
`internal/estimate/schemas/change-plan.v1.json` in this extension; a test
holds it and the decoder to the same members and bounds. The command
does not check that a project id names a project of the workspace.

## The sizing rule

1. A slice counts its files, plus 2 per endpoint and 3 per screen.
2. The slice takes the token and minute ranges of the first bucket of the
   table that holds that count.
3. The plan's ranges are the sums of its slices' ranges.
4. A range whose high bound stays within 90 minutes is S, within 240 minutes
   M, and L above. A slice or a plan that touches a sensitive zone is never S.

| Files counted | Tokens | Minutes | Sessions |
|---|---|---|---|
| 0 to 5 | 5.6M to 16.7M | 31 to 86 | 15 |
| 6 to 15 | 17.6M to 48.5M | 44 to 144 | 37 |
| 16 to 40 | 42.2M to 132.1M | 79 to 220 | 32 |
| 41 and more | 136.0M to 365.8M | 195 to 388 | 32 |

On the plan above, both slices count 6 and 11 files and land in the second
bucket; the plan sums to 88 to 288 minutes and is L:

```text
Plan .putnami/42/plan.json: L, 35.2M to 97.0M tokens, 88 to 288 minutes
  1. plan-format: M, 17.6M to 48.5M tokens, 44 to 144 minutes, 6 files counted
  2. estimate-command: M, 17.6M to 48.5M tokens, 44 to 144 minutes, 11 files counted
```

`--output=json` prints the same estimate as one JSON line: `protocolVersion`,
`plan`, `issue`, `size`, `tokens` and `minutes` (each `{"low", "high"}`), and
`slices`, each with its `key`, `files` counted, `size`, `tokens` and
`minutes`. A usage error exits 2.

## How the table was fitted

The table comes from 116 agent sessions, from 27 September to 27 October
2026, that each opened exactly one pull request, merged since. A session's
file count is its pull request's changed files. Its tokens are every token the
model read or wrote: input, cache creation, cache read and output, subagents
included. Its minutes are the time between consecutive events of the main
session, less every wait that ends at an owner message, with each gap capped
at 30 minutes. Each bucket holds the 25th and the 75th percentiles of its
sessions' tokens and minutes. The minutes cover the whole session: code,
review, gate and publication.

The sessions record no endpoint and no screen, so the allowance of 2 files per
endpoint and 3 per screen is fixed, not fitted. The size thresholds are not
fitted either. The smallest bucket's sessions ran 31 to 86 minutes end to end,
so S stops at 90 minutes on the high bound: a slice of 5 files or fewer, with
no sensitive zone, is the change whose code starts without waiting for an
approval.

## Change the coefficients

`options["@putnami/contributor"].estimate` in `putnami.workspace.json`
replaces each member it sets, whole:

```json
{
  "options": {
    "@putnami/contributor": {
      "version": 1,
      "estimate": {
        "files": [
          { "upTo": 5, "tokens": { "low": 5600000, "high": 16700000 }, "minutes": { "low": 31, "high": 86 } },
          { "tokens": { "low": 17600000, "high": 48500000 }, "minutes": { "low": 44, "high": 144 } }
        ],
        "endpointFiles": 2,
        "screenFiles": 3,
        "sizeMinutes": { "S": 90, "M": 240 }
      }
    }
  }
}
```

Every bucket but the last names `upTo`, the largest count it holds, in
ascending order; the last bucket holds every larger count. A range never runs
backwards, and `S` never exceeds `M`. The command refuses any other table, and
any member it does not name, with exit 1.
