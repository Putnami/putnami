# Broker Mappings

Broker adapters preserve the canonical envelope fields wherever the backend
supports them.

| Broker | Mapping |
|--------|---------|
| Redis Streams handler transport | Envelope JSON stored in a stream field named `message`. |
| Google Pub/Sub | Envelope JSON stored in message data; envelope attributes copied to Pub/Sub attributes; `key` maps to ordering key. |
| Redis Streams realtime | Gateway event `id` uses the Redis stream id for reconnect cursors. |
| Redis Pub/Sub realtime | Gateway event `id` is publisher supplied/generated; no replay. |
| Postgres LISTEN/NOTIFY realtime | Gateway event JSON is the NOTIFY payload; no replay; payload size is bounded by Postgres. |

## Cloud Event Server Notes

- Durable handler transports must preserve the full envelope JSON, not just the
  payload, so retries, DLQ routing, trace correlation, topic versions, and
  attributes remain portable across languages.
- Live-only brokers must not be advertised as replay-capable routes unless they
  are paired with a durable event store.
- Backend-native ids may be used as cursors, but event frames still expose the
  canonical envelope fields. If a backend id replaces the public event `id`,
  the server should keep the publisher id in metadata or attributes for
  diagnostics.
- `dedupeKey` policy is server-defined, but a compatible server should treat it
  as an idempotency hint and document the retention window.
