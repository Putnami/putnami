# Event Server HTTP Profile

The HTTP profile is for publishing events into the proxied infrastructure. The
publish endpoint is advertised as `endpoints.publish`. The default path is:

```text
POST /events/publish
```

Request body is a `publish` Event Server frame:

```json
{
  "protocol": "putnami.events.v1",
  "type": "publish",
  "topic": "analytics.page_view",
  "channel": "analytics",
  "payload": { "path": "/docs" },
  "key": "session-123",
  "dedupeKey": "page_view:session-123:1",
  "topicVersion": "v1",
  "attributes": { "region": "eu" },
  "traceId": "trace-123"
}
```

Successful responses return a minimal accepted envelope reference:

```json
{
  "protocol": "putnami.events.v1",
  "id": "1713980000000-0",
  "topic": "analytics.page_view",
  "timestamp": "2026-04-24T10:00:00.000Z"
}
```

Errors return `EventServerError`.

Managed cloud workloads use the stricter profile in
[`12-managed-publish.md`](12-managed-publish.md). It requires caller-generated
stable `id` and `dedupeKey`, requires `topicVersion`, forbids caller `channel`
and reserved identity/routing attributes, and pins retry/outcome behavior while
remaining on `putnami.events.v1`.

## Rules

- Servers MUST validate `protocol`, `type`, `topic`, and `payload`.
- Servers SHOULD preserve `key`, `dedupeKey`, `topicVersion`, `attributes`,
  and `traceId` when mapping to brokers.
- Servers MUST reject payloads larger than `limits.maxPayloadBytes` when that
  limit is advertised.
- HTTP publish is accepted/durable only when the selected backend transport is
  durable. Live-only brokers may accept and fan out but cannot guarantee replay.
