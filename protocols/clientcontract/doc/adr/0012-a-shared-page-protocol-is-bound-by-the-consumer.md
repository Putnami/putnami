# ADR 0012 — A shared page protocol is bound by the consumer

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract` page protocol, both
  generators and both client runtimes

## Context

A snapshot page belongs to a protocol that several domain owners implement.
Generating one client per owner and consumer pair does not scale. The
provider identity must be the protocol, and deployment URLs, audiences and
owner route paths must be consumer configuration.

## Decision

**Wire shape.** The protocol publishes `PageQuery`, `Page[Rows]`, query-name
constants and `PageTransportSchemas` in Go, and `pageTransportSchemas` in the
TypeScript generator. `ValidatePageTransportSchemas` /
`validatePageTransportSchemas` check resolved owner schemas against it. Owners
may narrow relations, rows and bounds. The canonical transport leaves rows
opaque. An owner conformance test checks runtime behavior: `relation` echoes
the request; `afterKey` carries the preceding `nextKey`; only an empty or
absent `nextKey` terminates, even on a page with no rows.

**Watermark.** `watermark` is a nonnegative int64 captured before rows are
materialized. A consumer anchors several pages at the minimum watermark it
observed. `ownerConfirmedAt`, optional, is the owner's authoritative clock read
with that watermark, not local receipt time. Requiring confirmation and
enforcing clock skew, cursor byte bounds, relation vocabulary, page limits and
freshness belong to the domain: a schema cannot prove the ordering of database
reads.

**Binding.** Generated `Bind<Client>` / `bind<Client>` reuse the safe service
binding chain with a caller-supplied URL and credential audience. A
programmatic `OperationPaths` / `operationPaths` map may replace a known
operation's fixed REST JSON path; method, schemas, security, response bounds
and resilience stay declared. Unknown operations, path templates, streams,
alternate transports, authority changes, queries, fragments, controls, percent
escapes and traversal are refused before credential acquisition. Routing is an
explicit consumer choice and confers no authorization. No ambient endpoint
override can reroute a nested client call.

**Lifetime.** Bindings snapshot caller maps. Each directly bound client owns
its credential and response caches and stream lifetime. Reuse it while paging
one owner, then call generated `Close<Client>` in Go or `dispose()` in
TypeScript. Closing refuses later calls and leaves application-owned registries
alive. Redirects stay disabled, binding audiences keep precedence, and provider
error prose stays opaque unless `carryRemoteMessage` admits it.

## Consequences

One generated client serves every conforming owner. Backfill and outbox
processing are unchanged.
