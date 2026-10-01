# ADR 0001 — One bucket contract, and the contract is what is tested

- **Status**: accepted
- **Scope**: `@putnami/storage` (`typescript/framework/storage`)

## Context

Object-storage backends diverge in the corners: a missing object returns null or
throws, a copy from a missing source is a 404 or a typed error, a timestamp is a
string after JSON. This package has four backends: in-memory, filesystem, S3,
and the remote HTTP service. Per-backend suites prove each does what it does;
only one suite run against all of them catches divergence.

Keys often come from users. A key is a path, so `../` is traversal, and an
unencoded key in a URL is injection. Browsers must upload and download directly
without holding a storage credential.

## Decision

1. **One `StorageBackend` contract, one suite.** `get` of a missing object or
   bucket returns `null`; `delete` of a missing key is a no-op; `copy` from a
   missing source throws `StorageError` with code `NOT_FOUND`; an overwrite
   replaces; a zero-byte object round-trips with size 0. `runStorageContract`
   runs against memory, the filesystem, and the remote backend over its
   documented HTTP shape served on loopback from an in-memory store. S3 is not
   in the shared suite: it keeps its own unit tests and an endpoint-gated live
   suite, and joining needs a harness whose bucket name and destructive reset
   work against a real bucket.
2. **Declared types are runtime types.** `ObjectInfo.lastModified` is a `Date` on
   every backend; the remote backend revives it from JSON. An unparseable
   timestamp is omitted, never an `Invalid Date`.
3. **Keys are validated before any I/O, in every backend**, including memory and
   signed-URL minting. Empty keys, null bytes, absolute paths, and `..` segments
   are rejected, so a test against memory cannot accept a key production
   rejects. Each `/`-separated segment is percent-encoded when a key becomes a
   URL, so a key cannot add a query, fragment, or separator. The validator is
   backend-agnostic and imports no `node:fs`.
4. **Browsers get signed URLs, not credentials.** A signed URL is bound to method,
   bucket, and key, and expires: default 15 minutes, maximum 7 days. Expired,
   forged, unsigned, wrong-method, or wrong-key tokens are refused. Uploads
   enforce the bucket's size limit and MIME allowlist, and a streamed body is
   rejected while streaming once it passes the limit.
5. **No cloud SDK.** S3 uses the runtime's `Bun.S3Client`; remote uses `fetch`.
6. **Public buckets are announced.** The storage server logs a startup warning
   naming every `public: true` bucket, because each serves every object to
   anonymous readers.

## Rejected alternatives

- **Test each backend against itself.** Cannot fail on divergence, the one
  failure that matters for a substitutable abstraction.
- **Exempt the remote backend because it needs a network.** A loopback service
  serves the documented shape; exempting it exempts the production path.
- **Throw on `get` of a missing object.** Turns an ordinary branch into
  exception handling.
- **Return parsed JSON from `list` unchanged.** The declared type becomes a lie
  on the production backend.
- **Validate keys only in the filesystem backend.** URL injection is not a
  filesystem concern; an unvalidated key in a URL builder is a request-forgery
  primitive.
- **Give browsers a scoped credential.** Moves expiry, revocation, and scope to
  the issuer, and browsers keep none of it well.

## Consequences

- A new backend passes the shared contract instead of writing its own suite;
  S3 has not yet.
- The contract stays small: encryption, versioning, and storage classes stay off
  the shared interface instead of throwing on most backends.
- The remote backend's run measures this package's HTTP mapping, not the real
  service's semantics.
- Key rules are the same on every backend, so callers with unusual keys learn
  one set.
- Backends are shared between clients and closed when the last managed client is
  removed.
