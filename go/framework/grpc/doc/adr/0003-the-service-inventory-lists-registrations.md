# ADR 0003 — The service inventory lists registrations, from one writer

- **Status**: accepted
- **Scope**: `go.putnami.dev/grpc` (`go/framework/grpc`)

## Context

A build writes `schema/http-routes.json` from what the HTTP server registered.
Nothing records which gRPC and Connect services an application serves, so a
reader of the committed tree has to run the workload or read its source. Three
surfaces serve them: the native `Plugin`, the Connect `GatewayPlugin`, and the
`ApiBridge`. An application composes any of them, and a describe can target one
by name.

## Decision

Each surface is a describer of `putnami.grpc-services.v1` at
`schema/grpc-services.json`.

1. **Registrations, not guesses.** The native plugin runs the registrars
   `Start` runs on a server it never starts, then reads `GetServiceInfo`.
   Reflection is listed only when it is enabled. The bridge lists the Connect
   URLs `Configure` mounted. The gateway lists the services `RegisterOn`
   mounted; its handlers are opaque, so their methods are unknown.
2. **One writer.** Configure records the application root. At describe time
   every surface collects all surfaces under that root, and the first one, in
   module-tree order, that the describe asks for writes the union. A describe
   that targets `grpc`, `grpc-gateway`, `grpc-api-bridge` or `grpc-services`
   therefore writes the same complete file.
3. **No root, no inventory.** A surface added with `UseForDescribe` is never
   configured. Its describe fails and names `Use`, rather than writing an
   inventory without the other surfaces.
4. **Duplicates.** Identical services collapse. Two different services with one
   name, transport and basePath fail the describe.
5. **Presence is evidence.** A composed surface that serves nothing writes an
   empty inventory. An application without a gRPC surface writes no file.

## Rejected alternatives

- **One file per surface.** A consumer would merge files and could miss one.
- **A dedicated describe-only plugin.** It would be one more thing to compose,
  and an application that forgot it would publish nothing.
- **Read the methods of a gateway handler.** The handler is an `http.Handler`;
  any method list would be a guess.
- **Start the native server to read it.** Describe opens no socket.

## Consequences

- A registrar runs at describe time, so it registers services and does nothing
  else.
- A gateway handler path that is not a service name fails the describe.
- The inventory moves when a registration moves, and only then.
