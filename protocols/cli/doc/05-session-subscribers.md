# Session subscriber evidence v1

A recorded session has one event stream: its `events.jsonl`. The engine is the
only producer. It appends one LF-terminated record per write and never rewrites
a byte, and no record carries a sequence member. A position in the stream is a
byte `offset`. Its `records` count is the number of LF-terminated records that
end at or before that offset.

A subscriber is a named consumer of that stream. It starts from a position,
reads bytes from the file on disk by offset, and acknowledges positions
explicitly. The producer wakes it with a signal that never blocks, so a slow or
dead subscriber cannot delay a task. Closing the stream is the terminal `final`
marker: no record follows it. The native session reporter and log reporter are
today's live subscribers, `session-reporter` and `log-reporter`. Each is
selected on its own and records its own entry. `putnami sessions inspect` reads a finalized
stream through the same reader, from offset 0, and records no evidence.

After its live subscribers finish, the engine writes `subscribers.json` beside
`session.json`. It never changes `session.json` or `events.jsonl`. A session
with no live subscriber has no `subscribers.json`. `putnami sessions replay`
replays only the selected reporters whose entry is not `delivered`. It rewrites
only their entries and keeps the others.

```json
{
  "protocolVersion": 1,
  "sessionId": "20260917-120000-abc123",
  "stream": { "offset": 1200, "records": 4 },
  "subscribers": [
    {
      "name": "log-reporter",
      "evidence": "delivered",
      "acknowledged": { "offset": 1200, "records": 4 },
      "lost": 0
    },
    {
      "name": "session-reporter",
      "evidence": "partial",
      "acknowledged": { "offset": 700, "records": 2 },
      "lost": 2
    }
  ]
}
```

| Member | Meaning |
| --- | --- |
| `protocolVersion` | Always `1`. |
| `sessionId` | The recorded session, with the session reporter's id pattern. |
| `stream` | The final extent of `events.jsonl`. |
| `subscribers` | 1 to 32 entries, sorted by `name`, names unique. |
| `name` | The declared subscriber name, `[a-z][a-z0-9-]{0,63}`. |
| `evidence` | `delivered`, `partial` or `lost` (below). |
| `acknowledged` | The last position the subscriber acknowledged. |
| `lost` | `stream.records` minus `acknowledged.records`: the records never acknowledged within the subscriber's budget. |

| Evidence | Rule |
| --- | --- |
| `delivered` | The subscriber acknowledged the final marker; `acknowledged` equals `stream`. |
| `partial` | The subscriber acknowledged some bytes and never the final marker. An offset inside a record is valid: the reporter frames bytes, not records. |
| `lost` | The subscriber acknowledged nothing; `acknowledged.offset` is `0`. |

Integers are nonnegative JavaScript-safe integers, `records` never exceeds
`offset`, and `acknowledged` never exceeds `stream`. Readers refuse unknown or
null members, trailing JSON, and documents over 64 KiB.

The schema is [`session-subscribers.json`](../schemas/session-subscribers.json).
Go publishes `SessionSubscribersFile`, `NewSessionSubscriberEvidence` and
`ParseSessionSubscribersFile` from `go.putnami.dev/protocol/cli`; TypeScript
exports `parseSessionSubscribersFile` and the types from
`@putnami/cli-protocol`. Both runtimes execute the
[shared corpus](../conformance/session-subscribers.json) and pin their member
sets to the schema.

Evidence is written after delivery ends, so it cannot live inside the
`session.json` the reporter delivers. The telemetry observer is not a
subscriber: it reports runs that record no session, such as `--dry-run` and the
outer `--watch` loop (`tooling/cli` ADR 0001 §4). The failed-task record is not
one either: it is written and forgotten with task finalization (`tooling/cli`
ADR 0030).
