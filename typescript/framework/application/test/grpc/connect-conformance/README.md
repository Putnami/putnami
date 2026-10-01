# Connect protocol conformance corpus

`corpus.json` is transcribed from the **Connect protocol specification, version 1**,
published at <https://connectrpc.com/docs/protocol/> and read on 2026-09-09. It is
the arbiter for both halves of a first-party Connect call:

- the provider in `typescript/framework/application/src/grpc`, and
- the generated-client runtime in `typescript/framework/client/src/runtime`.

Both halves import their tables from
`typescript/framework/application/src/grpc/connect-protocol.ts`. That shared module
stops them drifting from each other; it cannot show either is right. Every claim
below is therefore checked against this file, not against the other half:

| Section | What it pins | Where the specification says it |
| --- | --- | --- |
| `codes` | the sixteen codes, their HTTP status and their `google.rpc.Code` number | "Error Codes" table |
| `httpInference` | the code a client infers from a bare status | "HTTP to Error Code" table |
| `errors` | which error bodies are valid, and what they decode to | "Error and EndStreamResponse" |
| `endStream` | which terminals are valid, and what they decode to | "Error and EndStreamResponse" |
| `envelope` | the byte layout and the meaning of each flag bit | "Enveloped-Message", `Envelope-Flags` |
| `streams` | frame sequences a conforming client accepts or refuses | "Streaming-Response" |
| `detailValue` | a published base64 detail payload and the protobuf it carries | the `google.rpc.RetryInfo` example |
| `contentTypes` | the media types unary and streaming calls use | "Protocol Buffers" |

Two notes on provenance:

1. The specification renders its failed-server-stream example as
   `<flags: 2><length: 58>{"error": {"code": "unavailable", "message": "overloaded"}}`.
   That payload is 59 bytes; the rendered length is off by one in the published
   page. The corpus carries the payload text and the flag value — both
   unambiguous — and derives the length, so a documentation typo cannot become a
   framework bug.
2. The published detail example pairs `"value": "CgIIPA"` with
   `"debug": {"retryDelay": "30s"}`. Those bytes decode to a 60-second delay
   (`0a 02 08 3c`). The corpus keeps the bytes, which the specification calls
   normative, and records the mismatch — clients "must not depend on data in the
   `debug` key".

## Adding a scene

Add it to `corpus.json` and cite the sentence it comes from in `source`. Both
`test/grpc/connect-conformance.test.ts` (provider) and
`typescript/framework/client/test/runtime/connect-conformance.test.ts` (client)
enumerate the file, so an unread scene fails the non-vacuity guard rather than
passing silently.
