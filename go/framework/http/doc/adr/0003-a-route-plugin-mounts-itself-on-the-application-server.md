# ADR 0003 — A route plugin mounts itself on the application's single server

- **Status**: accepted
- **Scope**: `go.putnami.dev/http` (`go/framework/http`),
  `go.putnami.dev/platform` (`go/framework/platform`), and
  `go.putnami.dev/events` (`go/framework/events`)

## Context

A plugin that exists to answer routes has two wiring steps when it mounts only
through `RegisterOn(server)`: add the plugin to the application, and register
it on the server. An application that does the first and forgets the second
starts, runs the plugin's lifecycle, and answers 404 on the plugin's paths.
Nothing reports the gap. An orchestrator that probes `/_/health` restarts a
healthy process. The TypeScript platform and events plugins mount themselves,
so the same composition behaves differently in the two runtimes.

## Decision

A plugin whose purpose is to answer routes mounts itself when the application
configures. It registers its routes on the single `ServerPlugin` of the
application's module tree, which `http.SingleServer` returns. The rule covers
`http.HealthPlugin`, `platform.Plugin`, and `events.Plugin` under push
delivery.

An application that holds no server, or several, fails configure. The error
names the plugin and `RegisterOn`, so the application stops before it serves.

`RegisterOn(server)` stays. It chooses the server for an application that
holds several, or that keeps its server outside the module tree. A plugin
mounted through `RegisterOn` registers nothing more. `HealthPlugin.Handler`
counts the same way: the caller that takes the handler mounts it.

A plugin records that it is mounted, so an application that configures more
than once (`Validate`, `Prepare`, `Describe`, then `Start`) registers each
route once. The lookup walks the whole tree, so plugin order does not matter.

The events plugin mounts itself after it resolves its runtime configuration.
The self-mounted receiver authenticates with the resolved issuer, key set, and
audience. Pull and stream delivery need no server.

A plugin that publishes a document as a side effect stays explicit. The
OpenAPI plugin renders its document without serving it, and serving it is the
application's choice. The gRPC gateway keeps its explicit mount
(`go/framework/grpc/doc/adr/0002-bounded-by-default-mounted-explicitly.md`).

## Rejected alternatives

- **Keep `RegisterOn` as the only way to mount.** The forgotten call is silent
  and the failure shows up as an orchestrator restart loop.
- **Create a server when the tree holds none.** The port, middleware, and body
  limits of a server are the application's decisions.
- **Mount on the first server when the tree holds several.** Plugin order would
  decide which listener exposes operational routes.
- **Warn instead of failing.** A warning in a log does not stop a deployment
  that answers 404 on its probe path.
- **Mount from `Start`.** Describe reads the route inventory after configure
  and before start, so the routes would be missing from it.

## Consequences

- A health, platform, or push-delivery events plugin in an application without
  a server fails configure. A test that configures one in a bare module adds a
  server, calls `RegisterOn`, or, for health, takes `Handler`.
- An application that adds a health plugin now describes `/_/health` in its
  route inventory.
- An application that added a health plugin and also registered its own
  `/_/health` route stops at configure on the router's duplicate-route panic.
- `RegisterOn` runs before the application configures. A call after the plugin
  mounted itself on the same server is a duplicate route.
