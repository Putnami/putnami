# Event Server SSE Profile

The SSE endpoint is advertised as `endpoints.sse`. The default path is:

```text
GET /events/stream
```

Clients subscribe with query parameters:

| Parameter | Required | Description |
|-----------|----------|-------------|
| `topic` | yes | Topic name. |
| `channel` | no | Logical routing channel. |
| `from` | no | `latest`, `earliest`, message id, or broker cursor. |

Authentication uses the HTTP request context: bearer token, cookies, or
Event Server-specific headers advertised in `features.auth`.

## Event Format

Event Servers emit canonical stream frames as SSE data payloads:

```text
event: event
id: 1713980000000-0
data: {"protocol":"putnami.events.v1","type":"event","id":"1713980000000-0","topic":"user.updated","payload":{"id":"..."}}
```

Error and completion frames use the same JSON frame shape:

```text
event: error
data: {"protocol":"putnami.events.v1","type":"error","message":"Unauthorized"}
```

## Rules

- `id:` MUST match the emitted frame `id` when the frame has one.
- If the client sends `Last-Event-ID`, the Event Server MUST treat it as `from`
  unless an explicit `from` query parameter is present.
- Event Servers that advertise `features.replay: false` MUST reject `from` values
  other than `latest`.
- SSE does not support client `ack`/`nack`; advertise `features.ack: false`
  unless another endpoint profile provides ack semantics.
