# Push Delivery (Events as Webhooks)

Push delivery lets event receivers run on **scale-to-zero / serverless**
workloads. Instead of holding a long-lived process that pulls or streams events,
the workload exposes an HTTPS **receiver** endpoint and the provider POSTs each
event to it. An HTTP `2xx` acknowledges delivery.

Push is a **delivery profile**, not an Event Server transport. The four server
transports (`sse`, `websocket`, `http`, `grpc`, see
[05-gateway-discovery](05-gateway-discovery.md)) are surfaces an Event Server
hosts. A delivery profile is the delivery *model* a subscription uses:

| Profile | Who drives delivery | Process model |
|---------|---------------------|---------------|
| `pull` | subscriber pulls / long-polls a transport | long-lived |
| `stream` | subscriber holds an open stream | long-lived |
| `push` | **provider POSTs to a subscriber receiver** | **scale-to-zero** |

Push is realized by a provider-native push subscription (for example a GCP
Pub/Sub push subscription). The provider owns retry, backoff, and dead-lettering
on the happy path; there is no always-on delivery engine on the receiver.

## Receiver Route

A workload exposes one receiver route (both framework receivers register it):

```text
POST /_putnami/events/:subscription
```

The `:subscription` segment carries the provider subscription name.

## Request Body

The provider POSTs a push wrapper. The canonical schema is
[`schemas/push-delivery.json`](../schemas/push-delivery.json); the shape mirrors
the GCP Pub/Sub push body:

```json
{
  "message": {
    "data": "<base64-encoded canonical Envelope JSON>",
    "attributes": { "topic": "orders.created" },
    "messageId": "1405",
    "publishTime": "2026-05-01T12:00:00.123456789Z",
    "orderingKey": "o-1"
  },
  "subscription": "projects/<p>/subscriptions/<s>"
}
```

`message.data` base64-decodes to a canonical [`Envelope`](01-envelope.md). The
receiver MUST:

1. Authenticate the request (see **Authentication**) **before** decoding.
2. Enforce a payload-size limit before reading the body.
3. Base64-decode `message.data` and validate it as a canonical `Envelope`.
4. Strip caller-supplied `auth.*` attributes before dispatch. This is
   anti-spoofing, not hygiene: the identity that authenticated the push is the
   pusher's, and it is distinct from any end-user identity the envelope carries.
   A caller that could set `auth.*` would be asserting an end-user identity it
   never proved, so the receiver drops those attributes and re-derives them from
   the authenticated request.
5. Dispatch the envelope through the normal handler pipeline.
6. Respond with the status that encodes the acknowledgement outcome.

Go strict parser:

```go
push, diags := events.ParsePushEnvelopeStrict(body)   // validates wrapper + carried envelope
envelope, diags := events.DecodePushEnvelope(push)    // base64 -> canonical Envelope
```

## Authentication (OIDC, fail-closed)

Push delivery in v1 is authenticated with an **OIDC bearer token** in the
`Authorization` header — never with a field in the body. The provider attaches
an audience-pinned OIDC token minted as the **events-server service account**.
The receiver MUST verify, fail-closed:

- the token **signature** against the provider JWKS,
- the **issuer**,
- the **audience** (pinned to the receiver endpoint — omitting the audience
  check accepts any token from the trusted issuer),
- the token subject/email against a **service-account-email allowlist**.

Any missing, expired, wrong-audience, wrong-issuer, or non-allowlisted token is
rejected with `401`/`403` and the handler is not dispatched. The pusher service
account identity is used **only for admission**; it MUST NOT become the carried
end-user identity.

Cloud Run invoker IAM is defense-in-depth. HMAC / non-OIDC portable auth is
deferred beyond v1.

## Acknowledgement Mapping

Push delivery is **at-least-once**, so receivers MUST be idempotent. The
receiver's HTTP response status encodes the acknowledgement outcome:

| Status | Outcome | Provider behavior |
|--------|---------|-------------------|
| `2xx` | `ack` | message acknowledged and dropped |
| `4xx` | `dlq` | permanent failure: dead-letter, no retry |
| other (`5xx`, timeout) | `retry` | transient failure: redeliver with backoff |

Go helper:

```go
events.PushOutcomeForStatus(status) // -> events.PushAck | events.PushDLQ | events.PushRetry
```

Long-running handlers and manual-ack handlers stay on `pull`/`stream`; do not
route them through push status mapping.

## Discovery

An Event Server advertises push support in its capabilities document via
`deliveryProfiles` (distinct from `transports`), and reports `features.ack`
truthfully:

```json
{
  "protocol": "putnami.events.v1",
  "transports": ["http"],
  "deliveryProfiles": ["push"],
  "features": { "replay": false, "publish": true, "ack": true, "payload": ["json"] },
  "endpoints": { "publish": { "method": "POST", "path": "/events/publish" } }
}
```

## Conformance

The public runner manifest ([`conformance/manifest.json`](../conformance/manifest.json))
adds push receiver cases: `push.receiver.valid`, `push.receiver.auth-rejected`,
and `push.receiver.ack-dlq-retry`. See [09-conformance](09-conformance.md).
