package transaction

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// parseFixture routes a fixture to the right strict parser by its declared
// $schema id: transaction-result documents go through the result parser, every
// other document through the unit-of-work parser. This mirrors a real
// conformance runner that dispatches on the declared schema rather than
// guessing from field presence.
func parseFixture(data []byte) []diag.Diagnostic {
	var head struct {
		Schema string `json:"$schema"`
	}
	// Ignore the peek error: a malformed document still reaches a strict parser
	// below, which reports the parse error deterministically.
	_ = json.Unmarshal(data, &head)
	if strings.Contains(head.Schema, "transaction-result") {
		_, diags := ParseAndValidateResult(data)
		return diags
	}
	_, diags := ParseAndValidateUnitOfWork(data)
	return diags
}

// TestConformance runs the shared fixture corpus: every document under
// fixtures/valid must parse+validate clean, every document under
// fixtures/invalid must produce at least one diagnostic. This is the
// cross-language surface a future TypeScript adapter validates against too.
func TestConformance(t *testing.T) {
	valid, err := filepath.Glob(filepath.Join("fixtures", "valid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) == 0 {
		t.Fatal("no valid fixtures in fixtures/valid")
	}
	for _, path := range valid {
		t.Run("valid/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diags := parseFixture(data); diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}

	invalid, err := filepath.Glob(filepath.Join("fixtures", "invalid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) == 0 {
		t.Fatal("no invalid fixtures in fixtures/invalid")
	}
	for _, path := range invalid {
		t.Run("invalid/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diags := parseFixture(data); !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
		})
	}
}

// TestErrorCodesRegistered asserts every error code the validators can emit is
// registered in ValidErrorCodes, so the taxonomy stays complete.
func TestErrorCodesRegistered(t *testing.T) {
	codes := []string{
		ErrorCodeParseError,
		ErrorCodeUnknownField,
		ErrorCodeInvalidProtocolVersion,
		ErrorCodeInvalidName,
		ErrorCodeMissingPropagation,
		ErrorCodeInvalidPropagation,
		ErrorCodeInvalidIsolation,
		ErrorCodeMissingOutcome,
		ErrorCodeInvalidOutcome,
		ErrorCodeRetryableMismatch,
	}
	for _, code := range codes {
		if !ValidErrorCodes[code] {
			t.Errorf("error code %q is not registered in ValidErrorCodes", code)
		}
	}
	if len(codes) != len(ValidErrorCodes) {
		t.Errorf("ValidErrorCodes has %d entries, expected %d", len(ValidErrorCodes), len(codes))
	}
}

// TestRetryableTaxonomy pins the Outcome.Retryable() invariant that the
// validator enforces: exactly one outcome is retryable.
func TestRetryableTaxonomy(t *testing.T) {
	retryable := map[Outcome]bool{
		OutcomeApplied:                       false,
		OutcomeAlreadyConsumedConflict:       false,
		OutcomeNotFound:                      false,
		OutcomeRetryableSerializationFailure: true,
	}
	for o, want := range retryable {
		if got := o.Retryable(); got != want {
			t.Errorf("Outcome(%q).Retryable() = %t, want %t", o, got, want)
		}
	}
}
