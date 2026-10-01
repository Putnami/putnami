# Event Server WebSocket Profile

Mobile and browser clients communicate with Event Servers using JSON
frames over the endpoint advertised as `endpoints.websocket` by
`GET /.well-known/putnami/events`. The default path is:

```text
GET /events/ws
```

Every frame includes:

```json
{ "protocol": "putnami.events.v1", "type": "..." }
```

Frame types:

| Type | Direction | Required Fields |
|------|-----------|-----------------|
| `auth` | client -> server | none; `token` or `headers` are optional |
| `subscribe` | client -> server | `topic` |
| `publish` | client -> server | `topic`, `payload` |
| `event` | server -> client | `id`, `topic`, `payload` |
| `ack` | client -> server | `id` |
| `nack` | client -> server | `id` |
| `error` | server -> client | `message` or `error` |
| `complete` | server -> client | none |

Subscriptions may include `from` with `latest`, `earliest`, or a broker cursor.
Event Servers backed by Redis Streams should resume from the supplied cursor.

## Compatibility Requirements

- The first client frame MAY be `auth`; if omitted, the Event Server uses HTTP
  upgrade auth context.
- Subscribe frames MUST include `topic` and MAY include `channel`, `from`, and
  `attributes`.
- Event Servers MUST emit `event` frames using the canonical envelope field names.
- Event Servers MUST emit `error` frames before closing on protocol, auth, cursor,
  or upstream failures.
- Event Servers that advertise `features.ack: true` MUST accept `ack` and `nack`
  frames with `id`.

Browser and mobile clients authenticate with user-facing credentials: bearer
tokens, cookies, or custom headers. Server-to-server auth is not sent by a
browser/mobile client; managed Putnami Event Plane deployments should auto-wire
that from workload identity or service account configuration.
