# Context timeouts

`go.putnami.dev/ctxutil` provides one dependency-free helper for bounded,
non-streaming request I/O.

```go
ctx, cancel := ctxutil.WithRequestTimeout(ctx, 5*time.Second)
defer cancel()

resp, err := client.Do(req.WithContext(ctx))
```

For a positive timeout, `WithRequestTimeout` derives a
`context.WithTimeout` child. A zero or negative timeout returns the original
context and a no-op cancel function, so callers may always defer cancellation.

## Detached work

`context.WithoutCancel` deliberately removes both cancellation and deadlines.
When detached work performs request I/O, re-bound it immediately:

```go
detached := context.WithoutCancel(requestCtx)
ctx, cancel := ctxutil.WithRequestTimeout(detached, cfg.RequestTimeout)
defer cancel()
```

This is appropriate for bounded control-plane calls and singleflight leaders
that must survive one waiter's cancellation. It is not appropriate for a
streaming response body that must remain readable after the method returns;
cancelling the derived context on return would close the stream.

## Support and compatibility

`go.putnami.dev/ctxutil` is a public, documented, maintained package classified
`stable`. Its bounded-context specification and accepted detachment ADR live
next to the package source. Before v1.0.0, minor `0.x` releases may still
contain documented breaking changes; strict compatibility across every pre-1.0
minor is not promised.
