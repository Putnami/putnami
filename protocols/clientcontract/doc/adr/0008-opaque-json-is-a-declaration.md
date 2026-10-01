# ADR 0008 — Opaque JSON is a declaration, not the absence of one

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`, both readers, both
  emitters, both client runtimes, the Go provider projection

## Context

A provider must be able to publish a value it does not interpret
(`map[string]any`, `any`, `json.RawMessage`, the framework error envelope's
`details`). The empty schema `{}` cannot mean that: it is also what a
projection writes when it cannot describe a type (a map keyed by integers, a
channel). A reader that accepted `{}` could not tell "any JSON value on
purpose" from "a lost declaration".

## Decision

**A provider declares a value it does not interpret in one of two closed
spellings, and nothing else means the same thing.**

| Declaration | Schema | Go client | TypeScript client |
|---|---|---|---|
| Any JSON value | `{"x-putnami-json": "any"}` | `json.RawMessage` | `unknown` |
| JSON object with free-form values | `{"type": "object", "additionalProperties": true}` | `map[string]json.RawMessage` | `Record<string, unknown>` |

1. **`x-putnami-json` is closed.** Its only value is `"any"`. Only `title` and
   `description` may stand beside it. A `type`, `format`, `nullable`, `$ref`,
   `default`, bound or union beside it is `client_contract.invalid_schema`; any
   other value is `client_contract.invalid_enum`. It already admits null, so
   `nullable` would be a second spelling.
2. **A free-form object is `additionalProperties: true`**, which every OpenAPI
   reader understands. `additionalProperties: {"x-putnami-json": "any"}` is
   refused as a second spelling of the same shape. An object that mixes named
   properties with `additionalProperties: true` stays unemittable.
3. **The empty schema stays refused** with `client_contract.invalid_schema` in
   both readers (`empty-schema.openapi.json`).
4. **The keyword is a vendor extension.** A third-party reader reads the schema
   as `{}`, which is the right meaning for it.
5. **The Go provider projects it mechanically.** `json.RawMessage` and the empty
   interface become `{"x-putnami-json": "any"}`. A string-keyed map of either
   becomes the free-form object. The first-party body validator accepts `null`
   for them. The framework error envelope declares `details` as
   `{"x-putnami-json": "any"}`.
6. **Generated clients carry the value and never interpret it.** The Go client
   keeps the provider's bytes in a `json.RawMessage`; an optional member uses
   `omitempty`, so an explicit `null` and an absent member stay distinct. The
   TypeScript client decodes plain JSON values; an integer outside the safe
   range becomes a `bigint` and encodes back as the same digits; a non-integer
   number is an IEEE-754 double.
7. **Opaque JSON has no Connect form.** `google.protobuf.Value` carries every
   number as a double and would round wide integers. The Go proto projection
   leaves a route that carries opaque JSON out of the descriptor, the bridge
   mounts no Connect URL for it, and the route keeps its REST, SSE and
   WebSocket transports, as a raw octet payload does. The published descriptor
   decides: a route it leaves out gets no Connect URL, and only a provider with
   no descriptor falls back to computed names. Both emitters refuse a Connect
   dispatch whose shapes carry opaque JSON. An opaque value is never a path,
   query or header parameter, inline or through a component reference.

## Consequences

- The corpus pins both spellings (`WidgetAudit`) and four invalid fixtures;
  both readers replay them with the same codes.
- An `any` field still goes through Go `encoding/json` on the provider: the
  handler sees `float64` numbers and a re-marshaled map sorts its keys. A
  provider that must return exact bytes declares `json.RawMessage`.
