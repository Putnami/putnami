// Package conformance provides reusable pure runners for feature intent and
// evidence artifacts. Downstream producers use these in their normal unit
// gate to pin strict validity, canonical bytes, and round-trip determinism.
package conformance

import (
	"bytes"
	"os"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features"
)

// RunManifest asserts strict validity, byte-canonical serialization, and an
// idempotent parse/marshal round trip for authored feature intent.
func RunManifest(t *testing.T, committed []byte) {
	t.Helper()
	manifest, diagnostics := features.ParseAndValidateManifest(committed)
	if manifest == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("feature manifest failed strict validation: %v", diagnostics)
	}
	canonical, err := features.MarshalManifest(manifest)
	if err != nil {
		t.Fatalf("marshal feature manifest: %v", err)
	}
	if !bytes.Equal(canonical, committed) {
		t.Fatalf("feature manifest is not canonical\n--- committed ---\n%s\n--- canonical ---\n%s", committed, canonical)
	}
	parsed, findings := features.ParseManifest(canonical)
	if parsed == nil {
		t.Fatalf("re-parse canonical feature manifest: %v", findings)
	}
	twice, err := features.MarshalManifest(parsed)
	if err != nil || !bytes.Equal(twice, canonical) {
		t.Fatalf("feature manifest round trip is not idempotent: %v", err)
	}
}

// RunEvidence asserts strict validity, canonical bytes, and an idempotent
// round trip for one evidence fragment. Workspace references are intentionally
// validated separately with features.ValidateRepository.
func RunEvidence(t *testing.T, committed []byte) {
	t.Helper()
	document, diagnostics := features.ParseAndValidateEvidenceDocument(committed)
	if document == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("feature evidence failed strict validation: %v", diagnostics)
	}
	canonical, err := features.MarshalEvidenceDocument(document)
	if err != nil {
		t.Fatalf("marshal feature evidence: %v", err)
	}
	if !bytes.Equal(canonical, committed) {
		t.Fatalf("feature evidence is not canonical\n--- committed ---\n%s\n--- canonical ---\n%s", committed, canonical)
	}
	parsed, findings := features.ParseEvidenceDocument(canonical)
	if parsed == nil {
		t.Fatalf("re-parse canonical feature evidence: %v", findings)
	}
	twice, err := features.MarshalEvidenceDocument(parsed)
	if err != nil || !bytes.Equal(twice, canonical) {
		t.Fatalf("feature evidence round trip is not idempotent: %v", err)
	}
}

// RunSpec asserts strict validity, canonical bytes, and an idempotent round
// trip for one durable specification. Whether the referenced feature exists and
// whether another document specifies it are workspace facts, so they are
// intentionally validated separately with features.ValidateSpecRepository.
func RunSpec(t *testing.T, committed []byte) {
	t.Helper()
	spec, diagnostics := features.ParseAndValidateSpec(committed)
	if spec == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("spec failed strict validation: %v", diagnostics)
	}
	canonical, err := features.MarshalSpec(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if !bytes.Equal(canonical, committed) {
		t.Fatalf("spec is not canonical\n--- committed ---\n%s\n--- canonical ---\n%s", committed, canonical)
	}
	parsed, findings := features.ParseSpec(canonical)
	if parsed == nil {
		t.Fatalf("re-parse canonical spec: %v", findings)
	}
	twice, err := features.MarshalSpec(parsed)
	if err != nil || !bytes.Equal(twice, canonical) {
		t.Fatalf("spec round trip is not idempotent: %v", err)
	}
}

// RunVerificationReport asserts strict validity, canonical bytes, and an
// idempotent round trip for one run-scoped verification report. Whether an
// observed check is declared by a criterion is deliberately checked separately
// by the evaluator, because one report cannot see the authored manifest.
func RunVerificationReport(t *testing.T, committed []byte) {
	t.Helper()
	report, diagnostics := features.ParseAndValidateVerificationReport(committed)
	if report == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("verification report failed strict validation: %v", diagnostics)
	}
	canonical, err := features.MarshalVerificationReport(report)
	if err != nil {
		t.Fatalf("marshal verification report: %v", err)
	}
	if !bytes.Equal(canonical, committed) {
		t.Fatalf("verification report is not canonical\n--- committed ---\n%s\n--- canonical ---\n%s", committed, canonical)
	}
	parsed, findings := features.ParseVerificationReport(canonical)
	if parsed == nil {
		t.Fatalf("re-parse canonical verification report: %v", findings)
	}
	twice, err := features.MarshalVerificationReport(parsed)
	if err != nil || !bytes.Equal(twice, canonical) {
		t.Fatalf("verification report round trip is not idempotent: %v", err)
	}
}

// RunDesignGraph asserts validity, canonical bytes, and deterministic
// round-trip behavior for the provisional cross-runtime graph fixture.
func RunDesignGraph(t *testing.T, committed []byte) {
	t.Helper()
	graph, err := features.ParseDesignGraph(committed)
	if err != nil {
		t.Fatalf("design graph failed strict validation: %v", err)
	}
	canonical, err := features.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatalf("marshal design graph: %v", err)
	}
	if !bytes.Equal(canonical, committed) {
		t.Fatalf("design graph is not canonical\n--- committed ---\n%s\n--- canonical ---\n%s", committed, canonical)
	}
	parsed, err := features.ParseDesignGraph(canonical)
	if err != nil {
		t.Fatalf("re-parse canonical design graph: %v", err)
	}
	twice, err := features.MarshalDesignGraph(parsed)
	if err != nil || !bytes.Equal(twice, canonical) {
		t.Fatalf("design graph round trip is not idempotent: %v", err)
	}
}

// RunManifestFile reads and checks one committed authored-intent artifact.
func RunManifestFile(t *testing.T, filename string) {
	t.Helper()
	//nolint:gosec // the downstream test supplies its own committed fixture path
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read feature manifest %q: %v", filename, err)
	}
	RunManifest(t, data)
}

// RunEvidenceFile reads and checks one committed evidence artifact.
func RunEvidenceFile(t *testing.T, filename string) {
	t.Helper()
	//nolint:gosec // the downstream test supplies its own committed fixture path
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read feature evidence %q: %v", filename, err)
	}
	RunEvidence(t, data)
}

// RunSpecFile reads and checks one committed durable specification.
func RunSpecFile(t *testing.T, filename string) {
	t.Helper()
	//nolint:gosec // the downstream test supplies its own committed fixture path
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read spec %q: %v", filename, err)
	}
	RunSpec(t, data)
}

// RunVerificationReportFile reads and checks one emitted verification report.
func RunVerificationReportFile(t *testing.T, filename string) {
	t.Helper()
	//nolint:gosec // the downstream test supplies its own committed fixture path
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read verification report %q: %v", filename, err)
	}
	RunVerificationReport(t, data)
}

// RunDesignGraphFile reads and checks one committed design graph fixture.
func RunDesignGraphFile(t *testing.T, filename string) {
	t.Helper()
	//nolint:gosec // the downstream test supplies its own committed fixture path
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read design graph %q: %v", filename, err)
	}
	RunDesignGraph(t, data)
}
