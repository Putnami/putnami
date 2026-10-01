# ADR 0006 — Static binding headers are non-credential defaults

- **Status**: accepted
- **Scope**: `@putnami/client` (`typescript/framework/client`)

The shared rule is [go/framework/client ADR 0007](../../../../../go/framework/client/doc/adr/0007-static-binding-headers-are-non-credential-defaults.md); this record states the TypeScript surface.

`ServiceBinding.headers` and `clients.services.<service>.headers` accept the
same maps as the Go runtime and refuse the same ones with
`ClientServiceConfigError` (`client.config`), which
`protocols/clientcontract/fixtures/binding/headers.json` pins for both. The
binding snapshots the map into a frozen object without a prototype, so a later
change to the caller's object has no effect.

One interceptor applies the defaults to each call's private request. It runs
first in the unary chain, before the response cache renders its key, and first
in the stream header chain that SSE, Connect, first-party WebSocket and
provider-owned WebSocket carriers share. Retries reuse that request, and
credential selection is unchanged. An explicit operation header wins, and a
binding that names the operation's idempotency key header fails the call before
dispatch or cache lookup. A stream reports that refusal through its own error
path.

`X-Trace-Id`, `X-Region` and `X-Experiments` are reserved because the context
interceptor writes them only when the request lacks them. Values are visible
ASCII without surrounding whitespace, because `Headers` refuses code points
above U+00FF, sends Latin-1 as single bytes where Go sends UTF-8, and trims
surrounding whitespace.
