# Managed Workload Publish Profile v1

The managed workload profile is a stricter admission profile for the existing
`putnami.events.v1` HTTP publish contract. It does not create a second event
envelope or change the provider push receiver contract in
[`11-push-delivery.md`](11-push-delivery.md).

Managed Go and TypeScript publishers send:

```http
POST /events/publish
Authorization: Bearer <Google workload ID token>
Content-Type: application/json
traceparent: <W3C trace context, when present>
tracestate: <W3C trace state, when present>
```

The body remains an `EventServerFrame`, constrained by
`schemas/managed-publish-frame-v1.json`:

```json
{
  "protocol": "putnami.events.v1",
  "type": "publish",
  "id": "evt-01JAZ6Y5K1W8FQ2CP3R4T5V6X7",
  "dedupeKey": "order-created:order-123",
  "topic": "orders.created",
  "topicVersion": "1",
  "payload": { "orderId": "order-123" }
}
```

## Version boundaries

`putnami.events.v1` remains the wire protocol version. Managed config's
`eventServer.contractVersion: 1` is a separate compatibility gate for admission
and resolved configuration. It must not be substituted for the wire protocol or
used to invent a cloud-only envelope.

## Request invariants

- `protocol`, `type`, stable non-empty `id`, stable non-empty `dedupeKey`,
  logical `topic`, non-empty `topicVersion`, and JSON `payload` are required.
- `type` is exactly `publish`. A fully qualified provider topic such as
  `projects/.../topics/...` is not a logical topic and is rejected.
- `key`, string-valued caller `attributes`, and `traceId` are optional. When
  present, `key` and every request byte stay unchanged while an identity is
  retried.
- `channel` must be omitted, including an explicitly empty value. Managed
  admission stamps it from authenticated state.
- `workspace_id`, `environment`, `workload`, `service`, `channel`,
  `topology_generation_id`, `traceparent`, `tracestate`, `auth.*`, and
  `putnami.*` are reserved attributes. Caller-supplied reserved attributes are
  rejected with `invalid_frame`; they are never silently trusted.
- W3C HTTP trace headers are authoritative when present. `traceId` remains an
  accepted v1 compatibility field, not an authorization or routing source.

After authentication and authorization, Event Server stamps workspace,
environment, workload, service, workspace-derived channel, topology generation,
and request-span trace context. It resolves the exact Runtime-owned physical
topic out of band; the logical frame is never rewritten to a provider topic and
there is no topic-template fallback.

## Outcomes and retry safety

Success is a structured `200` `PublishRef` whose `protocol`, `id`, and logical
`topic` match the request. It is returned only after the configured backend
reports acceptance.

| Class | Inputs | Publisher action |
|---|---|---|
| `accepted` | Matching structured `200`. | Complete. |
| `permanent` | Structured `400`, `401`, or `403` with explicit `retryable: false`. | Fail without an automatic retry. |
| `retryable` | Structured `429` or `503` with `retryable: true`. | Back off, honor `Retry-After` when present, and retry identical bytes on the same Event Server route. |
| `ambiguous` | Timeout/reset/cancellation after send, malformed response, intermediary `502`/`504`, or another unstructured `5xx`. | Acceptance is unknown; retry identical bytes only on the same Event Server route or park for reconciliation. |

The contract is at-least-once. Stable identity supports downstream deduplication
but does not claim exactly-once delivery or a global server-side idempotency
store. Publishers must not automatically dual-publish, shadow-publish, or fall
back to direct Pub/Sub after a retryable or ambiguous Event Server attempt.

## Shared corpus

Canonical fixtures live at:

```text
fixtures/managed-publish/v1/
  valid/
  invalid/
  outcomes/
```

The Go protocol module embeds these bytes and exposes them through
`ManagedPublishV1Fixtures` and `ReadManagedPublishV1Fixture`. TypeScript
conformance tests read the same files from the monorepo. Consumers must not keep
an editable copy.

`valid/retry-attempt-1.json` and `valid/retry-attempt-2.json` are intentionally
byte-for-byte identical. Outcome fixtures use
`schemas/managed-publish-outcome-v1.json` and record the portable decision a Go,
TypeScript, or cloud publisher must make without contacting Google or mutating a
provider.
