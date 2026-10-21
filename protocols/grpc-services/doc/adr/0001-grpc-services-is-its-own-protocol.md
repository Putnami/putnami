# ADR 0001 — The gRPC service inventory is its own protocol

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/grpc-services` (`protocols/grpc-services`)

## Context

A consumer of a committed tree needs to know which gRPC and Connect services an
application serves, without running it. The HTTP route inventory lists HTTP
reachability. A native gRPC server has no HTTP route, and a Connect gateway is
one opaque prefix route whose methods the framework cannot see.

## Decision

The framework writes `putnami.grpc-services.v1`, a canonical, digest-addressed
artifact at `schema/grpc-services.json`.

1. **A separate protocol.** Services are not a field of
   `putnami.http-routes.v1`. A Connect service names its `basePath`, and a
   consumer joins it to the route inventory of the same build.
2. **Facts come from registration.** A producer lists what it registered: the
   service descriptors of an unstarted gRPC server, or the method paths a
   bridge mounts. It never reads source, protobuf files or `go.mod`.
3. **Unknown methods stay unknown.** A surface that cannot observe its methods
   writes `methodsKnown: false` with an empty list.
4. **Identity is name, transport and basePath.** A duplicate triple is an
   error, whatever its other fields.
5. **No environment.** No address, port, secret, clock value or absolute path is
   part of the wire.

## Rejected alternatives

- **Extend the HTTP route inventory.** A native gRPC service has no route, and
  a new route field would change a stable protocol.
- **Infer services from protobuf files.** A file declares a service; it does not
  prove the application registers it.
- **Guess the methods of an opaque handler.** A guess becomes a false fact.

## Consequences

- One Go producer exists. A TypeScript producer would follow this corpus.
- The inventory changes when a registration changes, so its digest moves with
  the served surface and with nothing else.
