# ADR 0007 — A call selects an endpoint without rebinding the service

- **Status**: accepted
- **Scope**: generated Go and TypeScript service clients

## Context

Fleet maintenance calls one operation on several deployment endpoints, and
workspace jobs resolve one endpoint from state. A fixed endpoint-set
configuration cannot describe both without a discovery and fan-out subsystem
in the client.

## Decision

This record owns the shared rule; the TypeScript surface is
[`@putnami/client` ADR 0005](../../../../../typescript/framework/client/doc/adr/0005-a-call-selects-an-endpoint-without-rebinding-the-service.md).

Go callers use `WithEndpoint(ctx, serviceID, url)`; TypeScript callers pass
the generated `endpoint` option. The service identity keeps a Go selection
invisible to other generated clients sharing the context. The selection is
consumed by one generated call: credential providers, interceptors and other
callbacks cannot carry it into a nested call.

- The runtime validates the URL with the registered binding's policy, then
  selects an immutable endpoint view.
- The caller owns trusted endpoint discovery, concurrency, per-target
  deadlines and outcomes. An override grants no authority beyond the binding.
  An arbitrary user URL is not suitable input: the declared credential is sent
  to it.
- Each canonical endpoint has its own operation circuits; scheme and host
  case variants resolve to one view. Views are kept for the bound client's
  lifetime, so repeated calls keep circuit state.
- Response-cache identity includes the endpoint and forwarded identity;
  invalidation spans all endpoints of the service.
- Credential identity is unchanged: a GCP token without an explicit audience
  uses the selected URL; explicit audiences and OAuth audience precedence are
  unchanged. A shared audience may share a credential, never a circuit or a
  response.
- The descriptor, schemas and policies stay immutable. No wire change or
  service discovery is added.

## Consequences

- One bound client supports fan-out and dynamic single-target dispatch. Go
  methods need no signature change.
- An application with an ever-growing target set scopes the client's lifetime
  to bound the retained views.
