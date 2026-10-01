# Event Server Discovery

Compatible Putnami Event Servers expose a discovery document at:

```text
GET /.well-known/putnami/events
```

The response body is `EventServerCapabilities` and MUST include:

| Field | Description |
|-------|-------------|
| `protocol` | Must be `putnami.events.v1`. |
| `transports` | Supported endpoint profiles: `sse`, `websocket`, `http`, `grpc`. |
| `deliveryProfiles` | Optional. Supported delivery models: `pull`, `stream`, `push`. |
| `features` | Replay, publish, ack, auth, cursor, and payload support. |
| `endpoints` | Concrete paths or gRPC service names. |
| `limits` | Optional payload/frame/subscription limits. |

Example:

```json
{
  "protocol": "putnami.events.v1",
  "server": { "name": "acme-events", "version": "1.0.0" },
  "transports": ["sse", "websocket", "http", "grpc"],
  "features": {
    "replay": true,
    "publish": true,
    "ack": true,
    "auth": ["bearer", "headers", "serviceAccount", "workloadIdentity"],
    "cursor": ["latest", "earliest", "message-id"],
    "payload": ["json"]
  },
  "endpoints": {
    "sse": { "method": "GET", "path": "/events/stream" },
    "websocket": { "method": "GET", "path": "/events/ws" },
    "publish": { "method": "POST", "path": "/events/publish" },
    "health": { "method": "GET", "path": "/events/health" },
    "grpc": { "service": "putnami.events.v1.EventServer" }
  }
}
```

## Rules

- A listed transport MUST have its matching endpoint entry.
- `sse` and `websocket` endpoints use `GET`.
- `http` publish endpoints use `POST`.
- `grpc` endpoints advertise a service name, not an HTTP path.
- `features.payload` MUST include `json`.
- Event Servers with `features.replay: true` MUST support `message-id` or
  `broker-cursor` resume.
- `auth: ["none"]` MUST NOT be combined with authenticated schemes.
- `deliveryProfiles` entries MUST be unique and one of `pull`, `stream`, `push`.

## Delivery Profiles

`deliveryProfiles` is orthogonal to `transports`. A transport is a surface the
Event Server hosts; a delivery profile is the delivery model a subscription
uses (`pull`, `stream`, or `push`). `push` is realized by a provider-native push
subscription that POSTs events to a workload receiver — see
[11-push-delivery](11-push-delivery.md). A server advertising `push` should
report `features.ack` truthfully, because push acknowledges on HTTP `2xx`.

## Auth Defaults

Putnami Event Plane should be the zero-config secure path for server-to-server
events. When an application calls a managed Putnami Event Server from a
Putnami-managed runtime, auth should be auto-wired from the deployment identity
by default:

- `workloadIdentity` is the preferred managed default.
- `serviceAccount` is the portable machine-to-machine identity model.
- `bearer`, `headers`, and `cookie` are supported protocol edges for clients,
  self-hosted servers, and custom integrations.
- Raw tokens and secrets should be referenced from env or secret managers, not
  embedded in protocol or deploy configuration.

Application code should not need to provide a token callback for the common
managed case. Explicit token/header providers are escape hatches for
self-hosted deployments, non-Putnami runtimes, and local integration tests.

## Putnami Event Plane

Putnami Event Plane is the managed Event Server implementation. It should
advertise the same capabilities document and pass the same conformance checks
as a self-hosted server, while providing the simplest hosted path for full
features: realtime fanout, replay, publish, routing, provider abstraction, and
future gRPC streaming.
