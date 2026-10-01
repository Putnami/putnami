# ADR 0007 — A response cache is declared per operation and honored by both runtimes

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`, `go.putnami.dev/client`,
  `@putnami/client`, both provider projections, both generators

## Context

A consumer that authorizes every inbound request against a sibling's owner API
cannot afford one network hop per request. A generated client replaces
hand-written memoizing clients only if the contract carries the cache policy
and both runtimes honor it. An authorization consumer also needs two more
things: a mutation route must read the provider's current answer without a
second client, and a revocation event names a value the request never carried
(an answer about `principalId`, requested by issuer and subject), which no key
prefix reaches.

## Decision

**A provider declares `resilience.cache` on one operation. Both generated
client runtimes honor it with the same bounded in-process cache, a per-call
bypass, and invalidation by key prefix or by a declared response field.**

1. **Scope.** Only a unary operation whose idempotency is `safe` or
   `idempotent` may declare it. A stream has no single answer to store, and a
   non-idempotent call made twice is not the same as made once. It is refused
   in `defaults.resilience`: a document default would cache operations that
   never asked for it.
2. **Values.** `freshMs` is required. `staleMs`, when present, is the maximum
   age of an answer served after a provider failure and exceeds `freshMs`.
   `maxEntries` bounds the least-recently-used entries per operation and
   service binding; absent, 1000.
3. **Key.** The key is `<operationId>?<part>&<part>…`. Each part is
   `<field>=<value>`: the canonical JSON of the field (object keys sorted by
   code point, no insignificant whitespace, number lexemes kept as sent),
   percent-encoded outside `A-Z a-z 0-9 - . _ ~`. A declared field absent from
   the request is the bare `<field>`. With `keyFields`, the parts are those
   fields in declared order. Without them, the parts are every `path.<name>`,
   then every `query.<name>`, then every `header.<name>` (lower-cased,
   `content-type` and the idempotency key header excluded), each group sorted
   by name, then `body`. A header's values are split at `", "`, so a repeated
   field renders as one JSON array whether it arrived repeated (Go
   `http.Header`) or combined (Fetch `Headers`). A key field must name an input
   the operation declares; providers and readers refuse one that names nothing.
4. **Identity.** Each entry is partitioned by the SHA-256 of the forwarded
   bearer token (or the empty identity) and by service binding, next to the key
   and never inside it. That is the whole partition. Context carried any other
   way (a header an interceptor adds after the cache, a credential shared by
   every tenant, a tenant field left out of `keyFields`) is not partitioned. An
   operation whose answer depends on an input declares it and keeps it in the
   key. One whose answer depends on anything it cannot declare declares no
   cache.
5. **Behavior.** A fresh entry answers without a call. Otherwise one call per
   key and identity goes upstream at a time, and concurrent callers wait for
   it. A success is stored. A transport error, a timeout of the operation's own
   budget, an open circuit, or a retryable remote answer left after the retries
   is masked by the stored answer while it is younger than `staleMs`; the
   runtime emits `rpc.client.cache.stale_served` and a warning log line. The
   operation's own budget bounds the shared call, not a caller's. A caller
   whose own deadline passes while it waits (a Go context deadline; a
   TypeScript `TimeoutError` abort or the ambient `deadlineAt`) takes the stored
   answer, counted and logged the same way, and the shared call keeps running.
   An explicit cancellation is always answered as a cancellation. In
   TypeScript, a transport error is one the transport's own network call
   raised; a `TypeError` from anywhere else is a defect and reaches the caller.
   Every other failure, and every failure after `staleMs`, reaches the caller.
6. **Bypass.** A Go call with the context from `client.WithoutResponseCache(ctx)`
   and a TypeScript call with `{ withoutResponseCache: true }` skip the cache:
   they read no entry, store no answer, join no shared call, and no stored
   answer masks their failure. The Go switch travels with the context. The
   TypeScript option appears in a generated `ClientCallOptions` only when the
   contract declares a cache.
7. **Invalidation fields.** `resilience.cache.invalidationFields` lists one or
   more distinct top-level properties of the JSON success body: each
   three-digit `2xx` status, each media type equal to `application/json`
   ignoring case. A name has no leading or trailing space and no control
   character. Every such body that declares the property declares it as a
   `string`, `integer` or `boolean`, following one local component reference
   for the body and for the property. Refused, with the reason:
   - `number`: a TypeScript double keeps no lexeme, so `1.50` and `1.5` would
     tag differently in the two runtimes;
   - `string` with `format: byte` or `binary`: TypeScript decodes it to bytes
     that no invalidation value can equal;
   - composites, unions and opaque JSON: no single value names them;
   - a field that names nothing: every invalidation by it would drop nothing.

   Both strict readers refuse these with `client_contract.invalid_resilience`
   and a duplicate with `client_contract.duplicate`. Both providers refuse them
   at publication.
8. **Tags.** Storing an answer reads each declared field from the decoded body
   and keeps its canonical text beside the entry: a string as
   `JSON.stringify` quotes it, a boolean as `true` or `false`, an integer as
   its decimal digits with no sign on zero. An absent or `null` field, any
   other value, or a body that is not a JSON object tags nothing.
9. **Invalidation.** `InvalidateResponses(serviceID, keyPrefix)` on the Go
   registry (`InvalidateResponses(keyPrefix)` on a Go or TypeScript client)
   drops every entry whose key starts with the prefix.
   `InvalidateResponsesByField(serviceID, field, value)` on the Go registry, and
   `InvalidateResponsesByField` / `invalidateResponsesByField(field, value)` on
   a client, render `value` by the tag rules and drop every entry of the
   service whose tag for `field` equals it, across operations. `"42"` and `42`
   differ. A value the rules cannot render (a fraction, `null`, a composite, a
   TypeScript number outside the safe range) is refused (`client.config` in Go,
   `TypeError` in TypeScript). A field no operation declares drops nothing.
   Both forms act on every identity and detach every matching call in flight:
   it still answers its waiting callers, its answer is never stored, and the
   next caller goes upstream. A field invalidation detaches every call in
   flight of an operation that declares the field, since it cannot know what
   that call will answer.
10. **Ownership.** The cache lives on the application's service registry, like
    the credential cache: the Go `ServiceBindings` and the TypeScript
    per-application registry lease. Closing the registry drops every entry and
    cancels every shared call; waiting callers see the registry closed.
11. **Capability gate.** A generated manifest repeats each operation's policy
    and lists `runtimeCapabilities: ["response-cache"]` exactly when an
    operation declares a cache. The generated code pins that list against its
    runtime (`client.RequireRuntimeCapabilities` in Go,
    `requireClientRuntimeCapabilities` in TypeScript), so an older runtime fails
    to build or load instead of running uncached. Bypass and field invalidation
    belong to the same capability. `clientgen-check` refuses a manifest that
    declares a policy without the capability, or requires one this protocol
    version's runtimes do not implement.
12. **Shared vectors.** `fixtures/cache/invalidation.json` pins value
    rendering, refused values and stored-body tags for both runtime test
    suites, and its `propertySchemas` pin which property schemas a field may
    name for the Go rule, the TypeScript reader and the TypeScript provider.
    The invalid OpenAPI corpus pins the declaration refusals.

## Consequences

- A generated client replaces hand-written memoizing clients, and cache
  wrappers that existed only to bypass or revoke, without losing latency or
  outage tolerance.
- Every stale answer is counted and logged.
- The key format is shared, so a prefix built for one runtime works for the
  other on string and integer fields.
- A consumer that needs revocation to act at once subscribes to the event and
  invalidates; it never waits for `freshMs`. A revocation drops exactly the
  answers that name the revoked value.
- Storing an answer costs one JSON decode when the operation declares
  invalidation fields. A hit costs nothing more.
- A field invalidation sends the next call of a declaring operation upstream
  when a call was in flight, even if that answer would not have matched.
