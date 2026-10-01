# Cross-language equivalence fixtures

Each fixture in this directory is one golden per-producer sidecar that BOTH
the Go and TypeScript implementations of that producer must emit byte-for-byte,
given the conceptually-equivalent inputs documented in the per-fixture comment.

Both producer tests reference the same file from this directory. A
divergence between languages — JSON key order, indentation, trailing
newline, retention handling, name canonicalization — fails one side and
gets caught immediately rather than silently dropping a workload's
requirement at deploy time.

This is the test surface the audit's S1 finding would have caught
(TypeScript stamped `protocolVersion: 1` while Go required `2`).

## Adding a new producer

1. Drop a `<producer>.golden.json` next to the existing ones, formatted as
   `json.MarshalIndent(m, "", "  ") + "\n"` would emit (the same
   `JSON.stringify(m, null, 2) + "\n"` produces).
2. Add a Go test alongside the producer that calls its `buildInfraManifest`,
   marshals the result with `json.MarshalIndent(m, "", "  ") + "\n"`, and
   compares against the fixture bytes.
3. Add the TypeScript equivalent.
4. Both tests must reference this file by relative path — the fixture is
   the single source of truth that ties them together.

A producer that exists in only one language (today: `document`, which is
TypeScript-only) still gets a fixture. Only the language that implements it
asserts against the golden; the fixture pins the wire shape so a future
implementation in the other language is constrained to byte-equivalence.
