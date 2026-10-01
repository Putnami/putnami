# ADR 0001 — One endpoint declaration, many contract consumers

- **Status**: accepted
- **Scope**: `go.putnami.dev/api` (`go/framework/api`)

## Context

One HTTP operation is described to the router, the validation pipeline, the
OpenAPI document, the Protobuf service and the generated clients. Five
descriptions drift silently: a spec that disagrees with the route still
serves, validates and generates. The declaration holds nothing HTTP-specific
beyond a method and a path, so it does not belong in the HTTP package.

## Decision

`go.putnami.dev/api` owns the single endpoint declaration. An
`EndpointDefinition` carries the method, path, parameter, query and body
schemas, responses, errors, security rule, middleware and injection tokens.
The api plugin is the only component that binds it to a transport and the only
source of route metadata: OpenAPI, Protobuf, the Connect bridge and the client
generator read `DiscoveredRoutes` or `Definitions`, never their own inventory.

- The primary success status is part of the declaration. `Returns` keeps the
  200 default; `ReturnsStatus` records another 2xx status for a unary
  endpoint. OpenAPI publishes it and client generation selects the response
  type from it.
- Binding happens in `Configure`, so later consumers see the complete route
  set. `Definitions` is readable before `Configure`; `DiscoveredRoutes` is the
  flattened post-binding view. An endpoint registered after configure is never
  served.
- One canonical function derives the operation id from method and path. The
  OpenAPI operation id, the Protobuf RPC name, the Connect URL and the
  generated client descriptors all use it. Language symbols are derived on top
  of it and never replace it.
- `Document()` describes a route mounted outside the api builder. A consumer
  that describes routes includes it (OpenAPI, typed-client generators); a
  consumer that maps a handler skips it (Protobuf service, Connect bridge),
  because an emitted RPC would advertise a URL that answers nothing.

## Rejected alternatives

- **Put the builder in `go.putnami.dev/http`.** Every future transport would
  depend on the HTTP server, and runtime primitives would mix with
  declarations.
- **Let each consumer take its own route list.** `AddRoute` allows it for
  low-level callers; as the primary path it is the drift this prevents.
- **Derive contracts from the router trie.** The trie knows no schemas,
  errors or descriptions.
- **Give each artifact its own operation id.** Cross-artifact lineage becomes
  impossible to compute.

## Consequences

- A metadata consumer runs after `Configure`, or reads `Definitions` and
  accepts the pre-binding view.
- A new declaration field must reach the builder, `DiscoveredRoute` and each
  consumer that honours it.
- Changing the canonical operation id derivation renames operations in every
  artifact at once; it is a breaking change.
