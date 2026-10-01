# Event Server Conformance

A compatible implementation should pass static, fixture, and endpoint
behavior checks. Putnami Cloud can keep its Event Server implementation private
and still prove compatibility by running the public black-box conformance
runner against a deployed endpoint.

## Static Shape

- Validate envelopes with `schemas/envelope.json`.
- Validate WebSocket/SSE/gRPC frames with `schemas/gateway-frame.json`.
- Validate discovery with `schemas/gateway-capabilities.json`.
- Validate structured errors with `schemas/gateway-error.json`.
- Validate provider push wrappers with `schemas/push-delivery.json`.
- Validate managed workload requests with
  `schemas/managed-publish-frame-v1.json` and portable publisher outcomes with
  `schemas/managed-publish-outcome-v1.json`.

## Fixture Compatibility

Implementations should parse all files under `fixtures/valid` and reject all
files under `fixtures/invalid`.

The Go package exposes strict parsers for this:

```go
events.ParseEnvelopeStrict(data)
events.ParseEventServerFrameStrict(data)
events.ParseEventServerCapabilitiesStrict(data)
events.ParseEventServerErrorStrict(data)
events.ParsePushEnvelopeStrict(data)
```

Provider push wrappers have their own corpus under `fixtures/push/valid` and
`fixtures/push/invalid` (their body is the push wrapper, not a top-level
canonical wire shape).

Managed workload publish has a versioned corpus under
`fixtures/managed-publish/v1/{valid,invalid,outcomes}`. Go consumers load the
exact embedded bytes with `ManagedPublishV1Fixtures`; TypeScript tests read the
same canonical directory. Valid request fixtures include minimal, full, and a
byte-identical retry pair. Invalid fixtures cover every required field,
caller-forbidden channel, and every reserved attribute class. Outcome fixtures
distinguish matching accepted responses, permanent rejection, retryable
rejection, and ambiguous transport/unstructured failures.

The framework-language consumers are
`go/framework/events/managed_publish_conformance_test.go` and
`typescript/framework/events/test/protocol-conformance.test.ts`. The Go test
imports the embedded protocol filesystem; the TypeScript test reads the
repository-owned files directly. Neither framework owns a fixture copy.

Managed retry tests must prove the entire request body remains byte-identical
and stays on the same Event Server route. They must not model automatic direct
Pub/Sub fallback, dual publish, exactly-once delivery, or a global server-side
idempotency store.

TypeScript implementations should import constants and types from:

```typescript
import {
  PUTNAMI_EVENTS_PROTOCOL,
  EVENT_SERVER_DISCOVERY_PATH,
  isCompatibleEventServer,
} from '@putnami/events/protocol';
```

## Runner Contract

The public runner manifest lives at:

```text
protocols/events/conformance/manifest.json
```

A runner should accept a target URL, optional gRPC target, externally resolved
credentials, case filters, and output format. It should then:

- generate an isolated run id and `conformance.<runId>` topic prefix
- fetch `GET /.well-known/putnami/events`
- validate the response as `EventServerCapabilities`
- select manifest cases matching advertised transports and features
- run applicable cases against generated ephemeral topics
- emit JSON and/or JUnit results with pass, fail, skip, and diagnostics

Auth auto-wiring is intentionally out of scope for the runner. The runner
consumes credentials supplied by CI, local env, or deploy tooling; managed
identity wiring belongs in the Putnami Cloud deployment layer.

## Endpoint Behavior

For each advertised transport, a conformance suite should verify:

- Discovery returns `protocol: "putnami.events.v1"`.
- Listed transports have matching endpoint entries.
- Subscribe rejects unsupported cursor modes.
- Event frames use canonical envelope fields.
- Error responses use canonical error codes.
- Replay behavior matches `features.replay`.
- Publish behavior matches `features.publish`.
- Ack behavior matches `features.ack`.

For a `push` delivery profile, a conformance suite should also verify:

- The receiver rejects unauthenticated or non-allowlisted pushes fail-closed
  (`401`/`403`) without dispatching the handler.
- A valid push is dispatched once and acknowledged with `2xx`.
- The receiver maps handler outcomes to `2xx` (ack), `4xx` (dead-letter), and
  `5xx` (retry).

The runner must fail CI when any required applicable case fails. Recommended
cases can be reported as warnings while a newly advertised feature is still
being stabilized.
