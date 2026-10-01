# ADR 0005 — A call selects an endpoint without rebinding the service

- **Status**: accepted
- **Scope**: `@putnami/client` (`typescript/framework/client`)

The shared rule is [go/framework/client ADR 0007](../../../../../go/framework/client/doc/adr/0007-a-call-selects-an-endpoint-without-rebinding-the-service.md); this record states the TypeScript surface.

Generated TypeScript methods accept `endpoint?: string` in their call options.
They select an immutable bound client view, leaving all transport, credential,
schema and resilience mechanisms intact. Each view has its own circuits and
uses the selected URL in response-cache identity. Views share the existing
application credential/cache lease, and disposing the original client disposes
every view and stream. Caller-owned signals and `Promise.allSettled` support
independent outcomes without introducing an endpoint-set scheduler.
