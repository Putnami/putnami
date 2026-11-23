# ADR 0005 — A request pays only for the per-request work it uses

- **Status**: accepted
- **Scope**: `go.putnami.dev/http` (`go/framework/http`), `go.putnami.dev/inject`

## Context

The server arms three things on every request before the handler runs: a
request deadline ([ADR 0002](0002-framework-owned-bounds.md)), a dependency
scope ([ADR 0001](0001-request-scope-follows-the-response.md)), and the access
record of the logging middleware. On a route that does no I/O, such as a
health check, that work costs more CPU than the handler itself:
`context.WithTimeout` allocates a context and starts a runtime timer, and
creating the scope allocates a child container and takes the root
container's lock to register it.

Each of those guarantees must hold for a request that needs it: a handler
blocked on I/O ends with `context.DeadlineExceeded`, over HTTP/1.1 too, where
the server write timeout cancels nothing; a request-scoped service gets its
own instance, finalized against the response.

## Decision

The server arms each piece of per-request work on first use, with the same
observable result as arming it up front, except for the one request context
ending the consequences state.

- The request deadline is fixed when the request starts. The request context
  answers `Deadline` and `Err` from that instant without a timer, and starts
  the timer the first time something asks for `Done`, as every blocking call
  that honors a context does. It ends as `context.WithDeadline` would, except
  when it arms past its deadline, as the consequences below state.
- The request scope materializes its container the first time something
  resolves from it. A scope nothing resolved from finalizes and closes as a
  no-op.
- The logging middleware builds the fields of its debug access record only
  when the logger emits debug records. A failure record is always built.

## Rejected alternatives

- **Rely on the server write timeout and drop the request deadline.** Over
  HTTP/1.1, the write timeout cancels no context: a handler blocked on a
  query would run unbounded.
- **Drop the scope for routes that declare no scoped dependency.** A handler
  can resolve a scoped service through `inject.Resolve` on its context, which
  no registration declares.
- **Pool contexts or containers across requests.** A value that escapes its
  request through a goroutine would observe the next request's state.

## Consequences

- The request context is a framework type, not a `context.WithTimeout`
  result. Once armed, it delegates to a `context.WithDeadline` context, and a
  lookup of the context package's cancellation key arms it, so a context
  derived from it registers its cancellation without a goroutine.
- The request context fixes how it ends when it arms: on the first wait
  (`Done`, a derived context), on the first read (`Err`, `Cause`) once it
  ended, or on the server's cancel once the handler returns. Armed before its
  deadline, it ends as `context.WithDeadline` does. Armed at or past its
  deadline, it ends with `context.DeadlineExceeded` even when its parent ended
  before the deadline: with no timer, the order of the two ends is unknown,
  and a stalled handler is a server fault, so the request answers 5xx at
  ERROR, as it does when the HTTP/2 write timeout resets the stream after the
  deadline. A context the server canceled before its deadline ends with
  `context.Canceled`.
- A request scope is identified by its handle, not its container: code that
  compares containers across one request sees a single container once
  materialized.
