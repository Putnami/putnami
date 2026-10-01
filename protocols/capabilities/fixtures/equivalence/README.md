# Cross-language equivalence fixtures

Each fixture in this directory is one golden capability manifest that BOTH the
Go and TypeScript emitters of the `capabilities` producer must emit
byte-for-byte, given the conceptually-equivalent workload documented in the
per-fixture comment.

Both producer tests reference the same file from this directory. A divergence
between languages — JSON key order, indentation, trailing newline, `omitempty`
handling, or the deterministic collection sort — fails one side and gets caught
immediately rather than silently emitting two different manifests for the same
workload at build time.

The canonical serialization is `json.MarshalIndent(m, "", "  ") + "\n"` in Go.
TypeScript reproduces it with matching field order, indentation, omitted-empty
fields, HTML and JavaScript-separator escaping, and trailing newline.

## Fixtures

- `capabilities.golden.json` — the aggregate manifest for a representative
  workload exercising every collectable kind: a config block with a sensitive
  field, a migration bound to a datasource, the datasource's infra requirement,
  a health probe, a readiness probe, and a start/stop lifecycle hook. The Go
  emitter and TypeScript builder must reproduce these exact bytes from the
  equivalent contributions (see
  `typescript/framework/application/test/capabilities/cross-language.test.ts`).

## Adding a new producer

1. Drop a `<producer>.golden.json` next to the existing one, formatted exactly
   as `json.MarshalIndent(m, "", "  ") + "\n"` would emit.
2. Assert against it from both languages that implement the producer.
3. Both tests must reference this file by relative path — the fixture is the
   single source of truth that ties them together.
