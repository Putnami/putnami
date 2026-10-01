package transaction

import _ "embed"

// conformanceManifestJSON is the canonical transaction conformance corpus,
// embedded from conformance/manifest.json at build time. Embedding makes the
// protocol module the single source of truth for the corpus bytes and lets the
// exported cross-language runners load it from any working directory without a
// repo-relative path.
//
//go:embed conformance/manifest.json
var conformanceManifestJSON []byte

// ConformanceManifestJSON returns a copy of the canonical transaction
// conformance corpus (conformance/manifest.json). The exported database runners
// (Go: go.putnami.dev/database/conformance; TypeScript: a drift-guarded bundled
// copy) consume this single source of truth so both language adapters execute
// the identical, ordered manifest. A fresh copy is returned on every call so a
// caller cannot mutate the embedded corpus.
func ConformanceManifestJSON() []byte {
	out := make([]byte, len(conformanceManifestJSON))
	copy(out, conformanceManifestJSON)
	return out
}
