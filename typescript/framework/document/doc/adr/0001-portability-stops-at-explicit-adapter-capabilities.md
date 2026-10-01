# ADR 0001 — Stop portability at explicit adapter capabilities

- **Status**: accepted
- **Scope**: `@putnami/document` (`typescript/framework/document`)

## Context

Firestore has scalar document paths, query-combination rules, batch limits, and
server-applied write transforms. The in-memory adapter has none of those limits.
A contract that hides the difference lets an application depend on an accident
of its development adapter and get a plausible but wrong result in production.
Echoing the request body from `save()` loses server transforms, an implicit patch
gives a missing field two meanings, a transaction that switches stores implies
atomicity no adapter supplies, and an empty bulk-delete filter could mean
"everything".

## Decision

The package owns one structured repository vocabulary, and adapter capabilities
are part of it.

- `Collection()`, `Field()`, and `DocumentId()` are the only schema-to-storage
  mapping. Repositories validate through it, take structured filters and
  ordering, and paginate by cursor only. Document-id fields are appended to the
  ordering as tie-breakers, so equal sort values never skip or repeat a document.
- `save()` replaces; `{ merge: true }` is the explicit patch. Every adapter reads
  the stored value back after `save()` and `saveMany()`, so the result carries
  backend transforms and is detached from caller input. `limit: 0` returns zero
  rows. `deleteMany()` requires at least one defined filter. Bulk work is chunked.
- A capability an adapter cannot implement exactly fails with a typed document
  error: Firestore rejects composite identifiers and incompatible negative
  filters. The `consistency` option defaults to `strong` and fails on an adapter
  that does not declare `strongConsistency`, instead of silently weakening.
- `runInTransaction()` binds to the named store of its first repository access;
  access to another store fails before joining. Inside the store, calls reuse the
  transaction with read-your-writes, and commit publishes all writes or none.
- Managed backends are cached per store name and closed by the document plugin.
  Build-time infrastructure output groups only Firestore collections by store and
  never carries credentials; credential parse errors name the setting, not the
  value.

## Rejected alternatives

- **Expose each backend client through `Repository`.** The common API becomes an
  alias for the first backend and portability disappears.
- **Emulate unsupported filters in memory after a broad scan.** Correct only
  below an unstated data size, and unbounded work above it.
- **Return the input from writes.** Saves a read and lies whenever the backend
  transforms the value.
- **Infer merge from missing fields.** The same object would replace on one path
  and patch on another.
- **Cross-store transactions.** Without a shared commit protocol it is a false
  atomicity guarantee.
- **Empty delete filter means all documents.** A missing form value could erase
  a collection.

## Consequences

- Adapter authors pass the contract test suite and declare capability
  differences; they do not reinterpret the surface.
- A write can cost a read-back; stored-state semantics win over the round trip.
- Firestore-native queries and cross-store coordination use the native client,
  outside the portable boundary.
- A new query operator or consistency guarantee needs evidence for every adapter
  that claims it.
