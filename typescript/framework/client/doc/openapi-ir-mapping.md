# OpenAPI → IR mapping (client generation contract)

This is the canonical, fixture-backed contract that every per-language client
emitter consumes. The OpenAPI spec emitted by `openapi()` is the **single v1
source** (`schema/openapi.json` by default, or `.gen/schema/openapi.json` when
`output: false`); both the TypeScript reader (`src/generator/openapi-reader.ts`)
and the Go reader (Phase 2b) produce the same intermediate representation (IR)
from it, so the two emitters cannot drift.

The marked OpenAPI document is authoritative, and a Proto artifact never wins
over it. A first-party document carries the `x-putnami-client` contract, so
Connect transports and the protobuf descriptor travel inside it rather than in a
second source.

## Operations → services & methods

- Each `paths` entry × HTTP method becomes one **method**.
- Methods are grouped into a **service** by the first non-parameter path segment:
  `/users/{id}` and `/users` → `UsersService` (client class `UsersClient`).
- `operationId` drives the method name (camelCased). When absent, it is
  synthesized from method + path: `POST /users` → `postUsers`,
  `GET /users/{id}` → `getUsers_id`.

## Parameters

| OpenAPI | IR |
| --- | --- |
| `parameters[in=path]` | `method.params: FieldIR[]` |
| `parameters[in=query]` | `method.query: FieldIR[]` |

`required: false` → `optional: true`.

## Request body

- `requestBody.content['application/json'].schema`:
  - a `$ref` → `method.bodyType` = the referenced named type.
  - an inline object → `method.body: FieldIR[]` (per-operation shape).

## Response

The response type is taken from the **first JSON 2xx response**, preferring
`200`, then `201`, then the lowest remaining 2xx status that carries a JSON body.
A 2xx response without a JSON body (e.g. `204`, or a bodyless `200`) is skipped.

- chosen `schema` is a `$ref` → `method.responseType` = the referenced named type.
- chosen `schema` is an inline object → `method.response: FieldIR[]`.
- no JSON 2xx response → the method returns `void`.

## Type mapping

| OpenAPI schema | `FieldIR.tsType` | `array` |
| --- | --- | --- |
| `string` / (default) | `string` | `false` |
| `number` / `integer` | `number` | `false` |
| `boolean` | `boolean` | `false` |
| `array` of `T` | mapping of `T` | `true` |
| `$ref: #/components/schemas/X` | `X` (named type) | `false` |
| inline `object` (not promoted) | `Record<string, unknown>` | `false` |

The Go reader maps the same cases to Go types, with the inline-object **degrade
rule** producing `map[string]any` where TypeScript produces
`Record<string, unknown>`.

## Shared named types (`components` / `$ref`)

The OpenAPI generator promotes any **object model reused across operations**
(structurally identical — same property names, types, and required set;
descriptions ignored) into a single `components.schemas` entry, replacing each
reused occurrence with a `$ref`. A shape used only once stays inline. Component
names (`Model1`, `Model2`, …) are assigned by **sorting the shared signatures**,
so the same set of models always yields the same names regardless of route
discovery order — the emitted spec stays byte-stable and reviewable.

The reader lifts `components.schemas` into `SpecIR.namedTypes` (name → fields).
A field that is a `$ref` references the named type by name; a single-use inline
nested object degrades (it is *not* a named type).

> A nested object becomes a named type only when it is reused (promoted to a
> component). A single-use nested object is emitted inline, as an object literal
> type in TypeScript and a named struct in Go. Strict first-party generation
> never degrades a declared shape to `Record<string, unknown>` / `map[string]any`:
> a schema it cannot represent is refused with `clientgen_unsupported_semantic`.

## Out of scope

- **A bare Proto artifact** as the contract. Connect is declared inside the
  marked OpenAPI document; a standalone Proto artifact is read only as a legacy
  fallback when no OpenAPI spec exists.
- **An unmarked external document** in first-party mode. It is refused with
  `clientgen_first_party_required` rather than generated from a contract Putnami
  does not own.

## Generation options

No generation contract travels with the document. A client project names its
provider and its options in the `generatedFrom` member of its own
`putnami.json`:

```jsonc
{
  "generatedFrom": {
    "service": "users.api",              // the provider's x-putnami-client service id
    "language": "go",                    // "go" | "ts"
    "packageName": "usersclient",        // Go only: package name of client.gen.go
    "clientName": "UsersClient",         // Go only: client type name
    "omitOperations": ["deployWorkspace"] // Go only: operations the Go client leaves out
  }
}
```

The TypeScript package name is the client's own `package.json` name, and the Go
import path is its own `go.mod` module. A provider whose Go client is a package
of its own module commits the same Go options in `clientgen.json` at its root,
with `go.output` naming the package directory.

## Golden fixtures

`test/generator/fixtures/openapi/*.openapi.json` are the shared inputs; the
expected IR lives in `test/generator/fixtures/ir/*.ir.json`. `specHash` is
excluded from the comparison (it is a derived drift token, not structural). The
Go reader (Phase 2b) asserts against the **same** `*.openapi.json` inputs.
