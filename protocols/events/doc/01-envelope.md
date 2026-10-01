# Event Envelope

The event envelope is the canonical message exchanged by Putnami event
publishers, transports, handlers, broker adapters, and Putnami Cloud.

Required fields:

| Field | Description |
|-------|-------------|
| `id` | Stable event message id. |
| `topic` | Event topic name. |
| `payload` | JSON payload. |
| `timestamp` | RFC3339 timestamp. |
| `attempt` | Delivery attempt, starting at `1`. |

Optional fields:

| Field | Description |
|-------|-------------|
| `protocol` | `putnami.events.v1` when emitted on protocol boundaries. |
| `channel` | Logical topic channel used by routing. |
| `key` | Routing or partition key. |
| `dedupeKey` | Idempotency key. |
| `topicVersion` | Topic contract version. |
| `attributes` | String metadata. |
| `traceId` | Distributed tracing correlation id. |
