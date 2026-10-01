# ADR 0004 — Connect carries the first-party error envelope as a typed detail

- **Status**: accepted
- **Scope**: `@putnami/application` (`typescript/framework/application`), `@putnami/client`

## Context

Connect defines sixteen error codes and allows no user-defined ones. They are
categories: `not_found`, `invalid_argument`, `unavailable`.

A Putnami operation declares something finer: the first-party envelope, whose
`code` is a stable framework code (`not_found`, `http.bad_request`, or a domain
code) and whose `details` is the payload declared for that code
([clientcontract ADR 0006](../../../../../protocols/clientcontract/doc/adr/0006-a-declared-error-schema-describes-details.md)).
A generated client narrows an error by that code and validates those details.
A `code` outside the sixteen is not a Connect code, so a conforming client
ignores it and infers one from the HTTP status, where a bare 404 means
`unimplemented`.

## Decision

The Connect `code` is one of the sixteen categories. The first-party envelope
travels in a Connect error detail, the protocol's mechanism for typed error
data:

```proto
message FrameworkError {
  string code = 1;         // the stable framework code
  int32 http_status = 2;   // the status the same error carries over REST
  string details_json = 3; // the declared `details` member, canonical JSON
}
```

It is published as `putnami.client.v1.FrameworkError`. The detail's `value` is
the binary protobuf as unpadded base64, the encoding the specification calls
normative, plus a `debug` rendering no client may depend on. `details_json` is a
string because the detail schema differs per operation: the descriptor stays
fixed and the payload stays the exact bytes a client validates.

1. The HTTP status shipped is the one the specification pairs with the Connect
   code. The finer framework status travels in the detail, so a conforming
   client sees no contradiction between status and code.
2. The generated Putnami client rebuilds the same `ClientFrameworkError` a REST
   call raises.
3. A third-party Connect client reads a code it knows, ignores the unknown
   detail type, and still gets `google.rpc.BadRequest` for a validation failure.

## Consequences

- A validation failure carries a binary `google.rpc.BadRequest` detail.
- Error mapping is a status-to-code table on the server and a different one on
  the client. They are not inverses; the corpus in
  `test/grpc/connect-conformance` pins both.
- An `HttpException` with a 2xx status is reported `unknown`: Connect has no
  success code, and `OK` would tell a gRPC-Web caller the RPC succeeded.

## Rejected alternatives

- **The gRPC status name (`NOT_FOUND`) in `code`.** Not a Connect code, so every
  conforming client falls back to status inference.
- **A header.** `Trailer-` metadata carries strings, so the detail payload would
  be re-encoded by hand, and a stream has no header channel after admission.
- **`details` as the whole envelope.** Every declared schema would carry three
  envelope members no consumer chooses.
