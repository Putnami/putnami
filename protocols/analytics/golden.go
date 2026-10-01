package analytics

import _ "embed"

// GoldenBatchJSON is the canonical analytics batch a browser tracker produces —
// one fully populated page view followed by one action with all three property
// kinds. It is the single importable copy of the wire shape: a consumer that
// needs a reference payload imports this instead of vendoring a copy, and this
// package's own golden test proves it satisfies ParseAndValidateBatch.
//
//go:embed fixtures/equivalence/batch.golden.json
var GoldenBatchJSON []byte

// BotsJSON is the closed bot-token list IsBotUserAgent matches against,
// `{ "tokens": [...] }` with lowercase, unique entries. It is embedded rather
// than hard-coded so the TypeScript tracker filters on the same bytes: a token
// added on one side and not the other would silently change what each runtime
// counts as human traffic.
//
//go:embed fixtures/bots.json
var BotsJSON []byte
