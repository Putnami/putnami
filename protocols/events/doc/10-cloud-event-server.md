# Cloud Event Server Implementation Guide

This guide fills the implementation-level details needed to build a compatible
managed or self-hosted Putnami Event Server. The other protocol documents define
wire shapes; this document defines the minimum server behavior behind those
shapes.

## Required Server Surface

A cloud Event Server should expose these profiles when advertised in discovery:

| Profile | Endpoint | Responsibility |
|---------|----------|----------------|
| Discovery | `GET /.well-known/putnami/events` | Return `EventServerCapabilities`. |
| HTTP publish | `POST /events/publish` | Accept publish frames and return accepted event refs. |
| SSE | `GET /events/stream` | Stream event/error/complete frames to browser/mobile clients. |
| WebSocket | `GET /events/ws` | Bidirectional subscribe/publish/ack/nack frames. |
| gRPC | `putnami.events.v1.EventServer` | Capabilities, Subscribe, Publish, Health. |
| Health | `GET /events/health` | Return readiness for routing, broker, and auth dependencies. |

Only advertise a profile when it is implemented and tested. The discovery
document is the client contract.

## Publish Flow

This flow describes the general Event Server publish profile. Managed cloud
workloads additionally follow the stricter admission, authoritative stamping,
physical-route, and retry rules in
[`12-managed-publish.md`](12-managed-publish.md); a managed request never trusts
caller channel or identity/routing attributes.

1. Authenticate the caller.
2. Validate `protocol`, frame `type`, `topic`, and JSON `payload`.
3. Enforce `limits.maxPayloadBytes` and tenant/project authorization.
4. Resolve the backend route using `channel`, `topic`, `attributes`, and server
   policy.
5. Build a canonical `Envelope`:
   - Generate `id` when absent.
   - Set `timestamp` to server time.
   - Set `attempt` to `1`.
   - Preserve `key`, `dedupeKey`, `topicVersion`, `attributes`, and `traceId`.
6. Apply dedupe policy when `dedupeKey` is present.
7. Persist or fan out through the selected backend.
8. Return `PublishResponse` with `protocol`, `id`, `topic`, and `timestamp`.

Durability depends on backend selection. If a live-only backend is selected, the
server may accept the publish but must not advertise replay for that route.

## Subscribe Flow

1. Authenticate the caller.
2. Validate topic, channel, attributes, and `from` cursor.
3. Enforce subscription limits from discovery.
4. Resolve the backend route.
5. Start delivery from:
   - `latest`: only new messages.
   - `earliest`: oldest retained message.
   - `message-id`: message after the given canonical event id.
   - `broker-cursor`: backend-native cursor.
6. Emit `event` frames with canonical field names.
7. Emit structured `error` frames before closing on protocol, auth, cursor, or
   upstream failures.
8. Emit `complete` when the server intentionally finishes a finite stream.

SSE clients acknowledge receipt only through cursor resume. WebSocket/gRPC
profiles may support explicit ack/nack when `features.ack` is true.

## Routing Contract

Routing is server policy, not client choice. Clients may provide `channel`,
`topic`, `key`, and `attributes`; the Event Server chooses a backend using an
ordered policy such as:

1. Exact channel match.
2. Topic pattern match.
3. Attribute match.
4. Default route.

Routes should declare backend capabilities internally:

| Capability | Meaning |
|------------|---------|
| durable | Publish returns after a durable write. |
| replay | Subscribe can resume from retained events. |
| ack | Backend supports explicit consumer acknowledgement. |
| ordered | Backend preserves order for a key or partition. |

Do not advertise a global feature unless every route exposed to that client can
satisfy it, or the server can reject unsupported route/profile combinations with
`unsupported_feature`.

## Handler Delivery Semantics

Server-side handler transports are at-least-once:

- `auto` ack means handler success acknowledges and handler failure nacks.
- `manual` ack requires explicit ack or nack from the handler/client.
- Retries use exponential backoff with jitter and cap at `maxBackoffMs`.
- DLQ messages publish to `{topic}.dlq` with `dlq.original_topic`,
  `dlq.original_attempt`, and `dlq.error`.
- Handlers must be idempotent because retries and reconnects may duplicate
  delivery.

## Backend Mapping Requirements

| Backend | Required Mapping |
|---------|------------------|
| Redis Streams | Store envelope JSON in field `message`; stream key derives from topic; group derives from handler group/distribution. |
| Google Pub/Sub | Store envelope JSON in message data; copy attributes to Pub/Sub attributes; map `key` to ordering key. |
| Redis Pub/Sub | Use only for live fanout; no replay, ack, retry, or DLQ guarantees. |
| Postgres realtime | Use NOTIFY for live fanout only unless paired with a durable event table. |

## Auth and Tenancy

Every request must resolve tenant/project/workspace identity before route
selection. Authorization should check:

- caller can publish or subscribe to the topic/channel,
- caller can use the requested endpoint profile,
- requested replay cursor belongs to the same tenant/project scope,
- server-to-server calls use workload identity or service account auth where
  available.

Do not put raw secrets in protocol frames. Use request metadata, headers,
cookies, or managed identity bindings.

## Errors

Return `EventServerError` for protocol-level failures. Use canonical codes:

- `unauthorized`, `forbidden`
- `protocol_mismatch`, `invalid_frame`, `invalid_envelope`
- `invalid_topic`, `invalid_cursor`
- `unsupported_feature`
- `payload_too_large`, `rate_limited`
- `upstream_unavailable`, `internal`

Mark `retryable: true` only when the same request may succeed without caller
changes.

## Health and Observability

Health should report `ok`, `degraded`, or `unavailable`. Include checks for:

- auth provider,
- routing config,
- each advertised backend,
- durable write path,
- replay/read path.

Emit metrics for accepted publishes, publish failures, active subscriptions,
delivered events, handler failures, retries, DLQ writes, dropped messages,
backend latency, and cursor lag.

## Conformance

Before production use, a cloud Event Server should pass:

- discovery validation,
- HTTP publish success and invalid-frame rejection,
- SSE realtime and replay cases when advertised,
- WebSocket subscribe and protocol-error cases when advertised,
- gRPC capabilities, publish/subscribe, and health when advertised,
- payload limit rejection when limits are advertised.

The public manifest is `conformance/manifest.json`.

Managed admission implementations must also run the shared
`fixtures/managed-publish/v1` request and outcome corpus. These cases are local
and deterministic: token verification and Runtime/Data lookups use fakes, never
live Google credentials or provider mutations.
