// Package conformance provides the exported capability-manifest determinism
// pack runner. A downstream project asserts its own committed capability
// manifest is byte-deterministic and complete with a single committed line:
//
//	func TestCapabilityManifestDeterministic(t *testing.T) {
//		conformance.RunFile(t, "schema/capabilities.json")
//	}
//
// The runner reuses the byte-stability machinery pinned by determinism_test.go:
// it re-emits the manifest in the canonical json.MarshalIndent form (two-space
// indent + trailing newline) and byte-compares it to the committed bytes, so any
// non-canonical key ordering, indentation, or escaping fails; it asserts the
// parse -> marshal round-trip is idempotent; and it strict-validates the manifest
// so an out-of-vocabulary kind or an incomplete provenance fails too.
//
// It is a PURE pack: no external service, no skip gate — it runs in the normal
// unit gate. This package deliberately imports "testing" in non-test source: it
// exists to be called by a downstream project's own test binary, mirroring the
// other exported protocol conformance packs.
package conformance

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
)

// Run asserts that committed — the canonical bytes of a project's capability
// manifest (schema/capabilities.json) — is byte-deterministic and complete:
//
//   - it strict-parses and validates with no error diagnostics (completeness: a
//     valid protocolVersion, closed-enum kinds, and complete provenance on every
//     contribution),
//   - re-emitting it in the canonical serialization reproduces committed exactly
//     (byte-determinism: the committed file is already canonical, so two emitters
//     — or two runs — yield identical bytes rather than a semantically-equal but
//     differently-ordered document), and
//   - the parse -> marshal round-trip is a fixed point (idempotent).
//
// The byte-equality is deliberately not weakened to a semantic compare: the
// canonical form is the cross-language contract (the Go emitter in
// go.putnami.dev/app and the TypeScript emitter in @putnami/application must both
// reproduce it byte-for-byte), so exact bytes are the property under test.
func Run(t *testing.T, committed []byte) {
	t.Helper()

	document, diags := capabilities.ParseAndValidateManifestDocument(committed)
	if document == nil || diag.HasErrors(diags) {
		t.Fatalf("capability manifest failed strict validation: %v", diags)
	}

	canonical := canonicalizeDocument(t, document)
	if !bytes.Equal(canonical, committed) {
		t.Fatalf("capability manifest is not byte-deterministic: re-emitting it in canonical form does not reproduce the committed bytes.\n--- committed ---\n%s\n--- re-emitted ---\n%s", committed, canonical)
	}

	// Round-trip: re-parsing the canonical bytes and re-emitting must be a fixed
	// point, so the wire form never drifts across a parse/emit cycle.
	document2, diags2 := capabilities.ParseManifestDocument(canonical)
	if document2 == nil {
		t.Fatalf("re-parse of canonical manifest failed: %v", diags2)
	}
	if twice := canonicalizeDocument(t, document2); !bytes.Equal(twice, canonical) {
		t.Fatalf("capability manifest round-trip is not idempotent.\n--- once ---\n%s\n--- twice ---\n%s", canonical, twice)
	}
}

func canonicalizeDocument(t *testing.T, document *capabilities.ManifestDocument) []byte {
	t.Helper()
	if document.V2 != nil {
		data, err := capabilities.MarshalManifestV2(document.V2)
		if err != nil {
			t.Fatalf("marshal v2 capability manifest: %v", err)
		}
		return data
	}
	return canonicalize(t, document.V1)
}

// RunFile is Run against the manifest at path — the one-line downstream opt-in
// against a committed schema/capabilities.json.
func RunFile(t *testing.T, path string) {
	t.Helper()
	//nolint:gosec // path is the downstream's own committed manifest path, supplied by its test
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capability manifest %q: %v", path, err)
	}
	Run(t, data)
}

// canonicalize returns the canonical serialization of m: json.MarshalIndent with
// two-space indentation plus a trailing newline — byte-identical to the form the
// Go emitter (go.putnami.dev/app) and the TypeScript emitter
// (@putnami/application) both produce.
func canonicalize(t *testing.T, m *capabilities.Manifest) []byte {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal capability manifest: %v", err)
	}
	return append(data, '\n')
}
