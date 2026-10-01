# ADR 0007 — A call delivers its success body through a caller-owned sink

- **Status**: accepted
- **Scope**: `@putnami/client` (`typescript/framework/client`)

The shared rule is [go/framework/client ADR 0008](../../../../../go/framework/client/doc/adr/0008-a-call-delivers-its-success-body-through-a-caller-owned-sink.md); this record states the TypeScript surface.

Generated TypeScript methods accept `successBody?: SuccessBody` in their call
options. A unary JSON call that succeeds fills the sink with the declared
success body exactly as the provider sent it, after the same checks the ordinary
call applies; a cached answer fills it with the stored bytes, copied. The HTTP
transport and the Connect JSON transport keep the bytes they decoded from;
Connect with the proto encoding, raw octet calls and every stream shape refuse
the sink before anything is sent. The generator forwards the option on every
method shape so the refusal lives in the runtime, not in the emitted code.
