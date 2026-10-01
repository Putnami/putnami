# Event Server gRPC Profile

The gRPC profile is advertised as `endpoints.grpc.service`. The default
service name is:

```text
putnami.events.v1.EventServer
```

The gRPC endpoint profile is illustrated by a **non-normative** protobuf file
(reference only — the canonical contract is the JSON Schemas in
`protocols/events/schemas`):

```text
protocols/events/proto/putnami/events/v1/event_server.proto
```

Required RPCs:

| RPC | Shape | Description |
|-----|-------|-------------|
| `Capabilities` | unary | Returns `EventServerCapabilities`, matching the HTTP discovery document. |
| `Subscribe` | server stream | Accepts subscribe request and streams event frames. |
| `Publish` | unary | Accepts publish frame and returns accepted envelope reference. |
| `Health` | unary | Returns server health. |

The protobuf IDL preserves canonical field names from this protocol. Generated
field casing may follow the target language, but JSON/proto names must remain
compatible with `putnami.events.v1`.

Payload fields use `google.protobuf.Value` so the gRPC profile can carry the
same arbitrary JSON payloads as HTTP, SSE, and WebSocket frames.

## Rules

- `Subscribe` request semantics match WebSocket/SSE subscribe: `topic`,
  optional `channel`, optional `from`, and optional attributes.
- `Subscribe` responses carry event, error, and complete frames.
- `Publish` semantics match `POST /events/publish`.
- gRPC status codes MAY be used for transport failures, but application
  failures SHOULD include `EventServerError` details.
- gRPC authentication SHOULD use request metadata. Frame-level `token` and
  `headers` fields are reserved for transports that need explicit auth frames.
