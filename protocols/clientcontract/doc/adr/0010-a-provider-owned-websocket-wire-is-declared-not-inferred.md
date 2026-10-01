# ADR 0010 — A provider-owned WebSocket wire is declared, not inferred

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`, both provider
  projections, both readers, both emitters, both client runtimes

## Context

The first-party WebSocket conversation
([ADR 0002](0002-websocket-admission-uses-a-first-frame-state-machine.md)) is
not the only wire Putnami routes speak. A database gateway tunnels raw
PostgreSQL bytes in binary RFC 6455 messages, with credentials and the target
in the upgrade request. An event server speaks `putnami.events.v1` JSON
messages with its own admission and resume cursor. Declared as first-party
streams, they would publish a wire the server does not speak, and their
consumers would keep hand-written codecs.

## Decision

**A WebSocket transport declares `wire: "provider"` when the provider owns the
vocabulary of its messages. The encoding says what one message is.**

```json
{"protocol":"websocket","path":"/v1/databases/connect","encoding":"binary",
 "websocket":{"resume":false,"wire":"provider"}}

{"protocol":"websocket","path":"/events/ws","encoding":"json",
 "websocket":{"subprotocol":"putnami.events.v1","resume":false,"wire":"provider"}}
```

| Encoding | One message | Subprotocol | `messages` |
|---|---|---|---|
| `binary` | a binary RFC 6455 message holding raw octets | optional | absent |
| `json` | a text message holding one JSON value of the declared type | required | `input` and `output`, both required |

Both readers refuse everything else with the contract's codes:

- The stream is `bidirectional`, and the provider wire is the operation's only
  transport: a fallback between two wires would reopen a conversation whose
  messages mean something else.
- The token is a valid RFC 9110 token outside the `putnami.service.`
  namespace, so a token cannot carry a second token or a header.
- `resume` is `false`. A provider wire resumes by its own protocol.
- `encoding: "binary"` travels on no other transport. An unknown `wire` value
  is `client_contract.invalid_enum`.
- A first-party runtime never admits on a provider wire:
  `ValidateWebSocketInitForOperation` refuses the transport.

`wire` is absent for the first-party conversation, and `subprotocol` is omitted
only when a byte stream declares none.

**Admission is the upgrade request.** There is no in-band `init`. The framework
applies what a unary call applies, on the upgrade: the declared credential
profile in its own header, client identity, declared ordinary headers, trace
context and request identifier. The provider runs the endpoint's security and
validation chain and answers with an ordinary HTTP refusal, which the client
decodes into the declared errors, or with `101` echoing exactly the declared
token. An offered token the route does not declare, or none where it declares
one, gets `400`.

**After admission the framework owns the socket, not the vocabulary.** Both
runtimes apply the declared stream budgets (handshake, idle, operation
duration, frame size), send RFC 6455 pings at the declared heartbeat, refuse a
message of the wrong opcode with close code `1003`, report a `1000` close as
the end of the stream and any other close as a typed error naming the code, and
measure the call once. The generated method returns `*client.ByteStream` (an
`io.ReadWriteCloser`) or a readable and writable `ByteStream` for octets, and
`*client.FrameStream` / `FrameStream` for JSON messages.

## Rejected alternatives

- **Infer the provider wire from the subprotocol.** A mistyped first-party
  token would silently change the wire.
- **Carry octets as base64 in first-party `message` frames.** It adds a third
  to every byte, puts a JSON parser on a database tunnel, and keeps an in-band
  admission the tunnel's peer does not speak.
- **One `wire` value per shape (`bytes`, `frames`).** The encoding already says
  what one message is; a second field is a second place to disagree.

## Consequences

- A provider's own admission frame (an events `auth` frame) is one of its
  declared input frames; the framework puts nothing inside it.
- A browser cannot put a header on a WebSocket upgrade. Where the platform
  cannot, the TypeScript client refuses a provider wire whose operation
  declares a credential before dialing, instead of opening an anonymous socket.
