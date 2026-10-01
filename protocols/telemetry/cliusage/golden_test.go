package cliusage

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	telemetry "go.putnami.dev/protocol/telemetry"
)

// TestGoldenLogsConformsToVocabulary proves the single importable fixture is both
// a structurally valid OTLP logs request and a conforming CLI usage payload, so
// the CLI encoder and the receiver share one trustworthy reference.
func TestGoldenLogsConformsToVocabulary(t *testing.T) {
	req, diags := telemetry.ParseAndValidateLogs(GoldenLogsJSON)
	if diag.HasErrors(diags) {
		t.Fatalf("golden fixture failed structural OTLP validation: %v", diags)
	}
	if got := ValidateLogs(*req); diag.HasErrors(got) {
		t.Fatalf("golden fixture failed CLI usage vocabulary: %v", got)
	}
}
