# Event Server Conformance Runner

The conformance runner is a public black-box test harness contract for any
implementation of the Putnami Event Server protocol, including private
implementations such as Putnami Cloud.

The runner target is a deployed Event Server, not its source code. That keeps
private server implementation details out of the open-source repo while still
making compatibility enforceable.

## Inputs

A runner should accept these inputs from flags, environment variables, or CI:

| Input | Description |
|-------|-------------|
| `targetUrl` | Base HTTP(S) URL for discovery, SSE, WebSocket, health, and HTTP publish. |
| `grpcTarget` | Optional gRPC authority/address when different from `targetUrl`. |
| `authMode` | Auth mode used by the target: `none`, `bearer`, `headers`, `cookie`, `serviceAccount`, or `workloadIdentity`. |
| `token` | Optional bearer/service-account token supplied by CI or local env. |
| `headers` | Optional custom headers to attach to HTTP, SSE, WebSocket, and gRPC metadata. |
| `cases` | Optional case filter for focused debugging. |
| `output` | `json`, `junit`, or both. |

Auth auto-wiring is intentionally outside this runner. The runner consumes
already-resolved credentials; deploy/runtime identity wiring belongs in a
dedicated Putnami Cloud integration PR.

## Execution Model

1. Generate a run id and topic prefix such as `conformance.<runId>`.
2. Fetch `GET /.well-known/putnami/events`.
3. Validate the discovery document against `EventServerCapabilities`.
4. Select applicable cases from `manifest.json` based on advertised transports,
   feature flags, and runner filters.
5. Run each case against generated ephemeral topics.
6. Emit a machine-readable report with pass/fail/skip status and diagnostics.

## Transport Coverage

The manifest defines required and recommended checks for:

- discovery shape and endpoint consistency
- HTTP publish success and canonical error responses
- SSE realtime delivery and replay behavior
- WebSocket auth/subscribe/error behavior
- gRPC capabilities, publish/subscribe streaming, and health
- advertised limits such as `maxPayloadBytes`

## CI Usage

A provider can run the same public conformance manifest against its own Event
Server deployment:

```text
putnami-events-conformance \
  --target-url https://events.example.com \
  --grpc-target events.example.com:443 \
  --auth-mode bearer \
  --token-env PUTNAMI_EVENTS_CONFORMANCE_TOKEN \
  --output json,junit
```

The runner should fail CI when any required applicable case fails. Recommended
cases may be reported as warnings until the advertised feature is considered
stable.

## Public Contract

The public protocol package owns:

- `proto/putnami/events/v1/event_server.proto`
- `schemas/*.json`
- `fixtures/valid/*.json`
- `fixtures/invalid/*.json`
- `fixtures/managed-publish/v1/{valid,invalid,outcomes}/*.json`
- `conformance/manifest.json`

The private server owns implementation details only. It is compatible when its
deployed endpoints pass the public conformance runner.
