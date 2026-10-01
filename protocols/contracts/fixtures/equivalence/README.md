# Cross-language equivalence fixtures

Each fixture in this directory is one golden contract IR that BOTH the Go and
the future TypeScript emitters of the contract compiler must emit
byte-for-byte, given the conceptually-equivalent contract documented in the
per-fixture comment.

Both producer tests reference the same file from this directory. A divergence
between languages — JSON key order, indentation, trailing newline, `omitempty`
handling, or the deterministic collection order — fails one side and gets caught
immediately rather than silently emitting two different IR documents for the
same contract at build time.

The canonical serialization is `json.MarshalIndent(m, "", "  ") + "\n"` in Go.
TypeScript reproduces it with matching field order, indentation, omitted-empty
fields, HTML and JavaScript-separator escaping, and trailing newline.

`contracts.golden.json` is also the shared **input** the Slice 3 emitters
consume: the Go emitter (`protocols/contracts`) and the TypeScript emitter
(`@putnami/application`'s `src/contracts/`) both read it by relative path and
must emit the same JSON Schema bytes (`testdata/contracts.schema.json`). The
same MarshalIndent/escaping discipline is what makes that JSON Schema
byte-identical across the two languages.

## Fixtures

- `contracts.golden.json` — an identity/authorization contract that exercises
  every IR node kind: an enum, a tagged union with both struct-ref and inline
  variants, structs whose fields reference primitives and named types, config
  fields with typed defaults, scopes, capabilities that draw on scopes, grants
  that confer capabilities, claims, principal kinds, and discovery metadata. The
  Go emitter and TypeScript builder must reproduce these exact bytes from the
  equivalent contract.

## Adding a new producer

1. Drop a `<producer>.golden.json` next to the existing one, formatted exactly
   as `json.MarshalIndent(m, "", "  ") + "\n"` would emit.
2. Assert against it from both languages that implement the producer.
3. Both tests must reference this file by relative path — the fixture is the
   single source of truth that ties them together.
