# ADR 0004 — An idle connection outlives the proxy's idle timeout

- **Status**: accepted
- **Scope**: `go.putnami.dev/http` (`go/framework/http`)

## Context

A reverse proxy or managed load balancer in front of the server pools
connections to it and reuses an idle one for the next request. When the server
closes an idle connection first, the proxy can send a request on a connection
the server is closing. The client gets an error from the proxy and the handler
never runs.

`net/http` bounds an idle HTTP/1.1 keep-alive connection with
`http.Server.IdleTimeout`, and uses `ReadTimeout` when it is zero. Its bundled
HTTP/2 server, h2c included, copies the same value. A server built without
`IdleTimeout` therefore closes an idle connection after `ReadTimeout`, 30
seconds by default.

Google Cloud application load balancers hold idle connections to their backends
for a fixed 600 seconds, and require the backend's own idle timeout to be
greater
([request distribution](https://docs.cloud.google.com/load-balancing/docs/https/request-distribution#timeout-keepalive-backends)).

## Decision

`ServerConfig.IdleTimeout` bounds an idle keep-alive connection, HTTP/1.1 and
HTTP/2 alike. The server sets it on its `http.Server`, so `net/http` never
falls back to `ReadTimeout`. Zero resolves to 620 seconds, above the
600-second backend keepalive of those load balancers, the way every bound in
[ADR 0002](0002-framework-owned-bounds.md) resolves its zero value.

The rule: the server's idle timeout exceeds the idle timeout of every proxy in
front of it, so the proxy always closes an idle connection first.

## Rejected alternatives

- **Raise `ReadTimeout` instead.** It also bounds how long a client may take to
  send one request, so a value above 600 seconds lets a stalled client hold a
  connection for ten minutes.
- **Disable keep-alive.** Every request then pays for a new connection.
- **Zero means no idle timeout.** An idle client then holds a descriptor until
  it leaves, and ADR 0002 reserves zero for the documented default.
- **Match the proxy's 600 seconds.** Equal timeouts still race; the margin lets
  the proxy close first.

## Consequences

- An idle connection holds a descriptor for up to 620 seconds instead of 30.
- A server behind a proxy that holds idle connections longer than 620 seconds
  sets `IdleTimeout` above that proxy's value.
- `ReadTimeout` bounds reading one request and no longer decides when an idle
  connection closes.
