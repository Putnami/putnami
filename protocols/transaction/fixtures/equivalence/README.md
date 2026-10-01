# Cross-language equivalence fixtures

Each fixture in this directory is one golden transaction wire document that BOTH
the Go and the (future) TypeScript adapters of the `transaction` protocol must
emit byte-for-byte, given the conceptually-equivalent transaction outcome
documented in the per-fixture comment.

Both sides reference the same file. A divergence between languages — JSON key
order, indentation, trailing newline, `omitempty` handling, or the
retryable/outcome consistency — fails one side and gets caught immediately
rather than silently emitting two different result envelopes for the same
outcome.

The canonical serialization is `json.MarshalIndent(m, "", "  ") + "\n"` in Go.
TypeScript reproduces it with matching field order, indentation, omitted-empty
fields, HTML and JavaScript-separator escaping, and trailing newline.

## Fixtures

- `transaction.golden.json` — a `Result` for the retryable
  serialization-failure outcome: `outcome: "retryable-serialization-failure"`
  with the `retryable: true` advisory the taxonomy requires and a human-readable
  `message`. The Go emitter (`determinism_test.go`) pins these exact bytes; a
  future TypeScript adapter must reproduce them from the equivalent outcome.

## Adding a new producer

1. Drop a `<producer>.golden.json` next to the existing one, formatted exactly
   as `json.MarshalIndent(m, "", "  ") + "\n"` would emit.
2. Assert against it from both languages that implement the producer.
3. Both tests must reference this file by relative path — the fixture is the
   single source of truth that ties them together.
