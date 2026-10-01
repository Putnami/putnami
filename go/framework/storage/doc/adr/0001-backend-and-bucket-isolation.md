# ADR 0001 — Isolate backend lifecycles, bucket mappings, and filesystem roots

- **Status**: accepted
- **Scope**: `go.putnami.dev/storage` (`go/framework/storage`)

## Context

Application code needs one object API across local and provider backends, but
bucket names, credentials, transports, and filesystem roots are deployment
boundaries. A literal fallback for an unbound bucket can write to the wrong
tenant. A shared HTTP transport lets one backend's close disrupt others. A
whole-request HTTP timeout can cut a large body after `Get` returned it.

## Decision

Application operations name logical buckets. Managed bindings map each logical
name to one backend, provider bucket, and optional key prefix. A missing
mapping fails closed. Signed URLs also need an explicit binding grant and a
backend that implements `URLSigner`.

Filesystem paths are cleaned and checked beneath one absolute root; traversal
and null bytes are rejected. Each S3 or GCS backend owns a cloned HTTP
transport and closes only its own idle connections. Control-plane calls use
bounded request contexts. Streaming `Get` does not use `http.Client.Timeout`,
and the caller owns and closes the returned body.

Bucket constraints are owned by
[ADR 0002](0002-enforce-constraints-on-the-plugin-backend.md).

## Rejected alternatives

- **Use the logical name when no binding exists.** A typo crosses an isolation
  boundary.
- **Share `http.DefaultTransport`.** One backend affects unrelated traffic.
- **`http.Client.Timeout` everywhere.** It can end a valid large download
  after the method returned.

## Consequences

- Callers close every successful `Get` body.
- Provider consistency and durability stay backend properties; the common
  interface invents no cross-provider guarantee.
