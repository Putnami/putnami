# ADR 0002 — The descriptor states the wire shape the JSON schema states

- **Status**: accepted
- **Scope**: `go.putnami.dev/proto` (`go/framework/proto`), the descriptor it
  publishes into `go.putnami.dev/protocol/clientcontract`

## Context

One Go declaration produces an OpenAPI document for REST clients and a
Protobuf descriptor for Connect clients. Any disagreement is silent: the
client compiles, calls, and decodes the wrong value. Maps as `bytes`, `int` as
`int32`, `[]byte` as a number list, dropped non-struct bodies, nested embedded
structs, and missing presence are each such a disagreement.

## Decision

**The descriptor states exactly what the published JSON schema states.** Each
field carries its kind, exact wire number, explicit proto3 presence, oneof
group, and a map's exact key and value type. Presence is set exactly when the
schema lets the property be absent or null. `int` is `int64` and `uint` is
`uint64`; a 32-bit width needs a 32-bit Go type, never a wire-format guess.
`[]byte` is one base64 `bytes` value. A non-struct request section (a `[]Item`
or map body) is carried, not dropped. The field set comes from the selector
the schema and the body validator use, so embedded structs flatten identically
in all three.

**A declaration proto3 cannot carry is refused, never degraded.** A non-string
map key, a nested list, or a map of lists fails generation with the
declaration and remedy named, and the document does not render.
`Document.GenerationErr` carries the reason, and the plugin's `Configure`
returns it, so the provider does not start with a wrong descriptor.

**No proto3 enum or oneof is synthesized from a Go declaration.** A contract
enum is a closed set of strings; proto3 `string` keeps the values
byte-identical on both transports, while a proto3 enum renames them
(`active` becomes `WIDGET_STATE_ACTIVE`). A tagged union's JSON discriminator
has no proto3 oneof form either. The shared descriptor, strict reader, and
corpus support both shapes for providers that declare them.

**`Document.RouteMethods()` is the single join** between a route and its
protobuf method identity. The OpenAPI projection and the Connect bridge read
it instead of recomputing an RPC name.

## Consequences

- The strict reader joins a descriptor message to a component schema by name.
  Both projections disambiguate same-named Go types as `Name`, `Name2`,
  `Name3` in their own first-encounter order, so such a pair can be refused
  with a message naming a declared field. The remedy is a distinct type name.
