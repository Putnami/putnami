# Handler Semantics

Handler transports are at-least-once. Implementations must assume handlers are
idempotent and may see duplicates.

Canonical defaults:

| Option | Default |
|--------|---------|
| `distribution` | `competing` |
| `maxRetries` | `10` |
| `maxBackoffMs` | `60000` |
| `timeoutMs` | `30000` |
| `concurrency` | `0` |
| `queueLimit` | `0` |
| `overflow` | `throw` |
| `dlq` | `true` |
| `ack` | `auto` |

Retry backoff starts at approximately `1000ms`, doubles per attempt, caps at
`maxBackoffMs`, and should include jitter.

DLQ envelopes use `{topic}.dlq` and must include:

| Attribute | Description |
|-----------|-------------|
| `dlq.original_topic` | Source topic before DLQ routing. |
| `dlq.original_attempt` | Attempt that exhausted retries. |
| `dlq.error` | Failure reason. |
