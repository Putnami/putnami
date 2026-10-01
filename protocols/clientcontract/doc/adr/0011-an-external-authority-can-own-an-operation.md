# ADR 0011 — An external authority can own one operation of a first-party document

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`, both readers, both
  providers, both proto projections, the Go Connect bridge, the workspace
  client guard

## Context

One API can serve a standard protocol beside Putnami's own routes: an OCI
registry (`/v2/{name}/manifests/…` beside `/v2/_putnami/capabilities`), a Go
module proxy, an npm registry. The standard legs belong to the standard; their
callers use the standards adapters each consumer project lists in its
`clientgen.external.json`. Document level `x-putnami-client` makes every
operation first-party, and every reader refuses an operation without operation
metadata. A second API for the Putnami routes does not help: both write
`schema/openapi.json`, and the client
describer reads the first document it finds.

## Decision

**An operation of a first-party document can name the external authority that
owns it, and every first-party reader skips it whole.**

1. **The marker is an operation-scope vendor extension**, a sibling of
   `x-putnami-client`: `"x-putnami-external-contract": "OCI Distribution
   Specification v1.1"`. Its value is a JSON string naming the authority
   (`$defs.externalContract` in `x-putnami-client-v1.json`).
2. **A marked operation stays in the document** with its path, parameters,
   request body and responses as served, and no `x-putnami-client`.
3. **Every first-party reader skips it.** It yields no contract operation,
   protobuf method, Connect URL or generated method. Its own schemas are not
   held to the first-party subset. Component schemas are shared by the whole
   document and stay subject to it.
4. **Contradictions are refused:**

   | Declaration | Code |
   |---|---|
   | Both `x-putnami-client` and the marker on one operation | `client_contract.duplicate` |
   | A marker that is not a JSON string | `client_contract.parse_error` |
   | A blank authority | `client_contract.required` |

   An unmarked operation without `x-putnami-client` stays
   `client_contract.required`. The marker is never a default.
5. **Providers declare it per endpoint.** Go:
   `api.Endpoint(…).Client(api.ClientOperationOptions{External: "…"})`.
   TypeScript: `endpoint().client({ external: '…' })`. It stands alone:
   combined with any other client option, left blank, or declared on an API
   that publishes no first-party contract, it fails the provider.
6. **Providers serve a marked route with the standard pipeline**: the request
   decoding, error body and raw stream transport of an API without a contract.
   The first-party body decoding, error envelope and WebSocket service protocol
   would contradict the standard.
7. **A document whose every operation is external is valid**, like a provider
   with no routes: no client is generated, and a target's generated files are
   removed.

## Consequences

- The valid fixture `external-operation.openapi.json` carries a schema the
  first-party subset refuses, so it passes only if the reader skips the
  operation. Three invalid fixtures pin the refusals in both readers.
- The marker has meaning only inside a first-party document. A third-party
  document ignores it.
