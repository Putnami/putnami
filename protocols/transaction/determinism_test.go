package transaction

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleResult builds the representative Result behind the
// cross-language equivalence golden: the retryable serialization-failure
// outcome, carrying the retryable advisory the taxonomy requires and a
// human-readable message. It is the shared input for the byte-stability and
// canonical-form guards.
func sampleResult() *Result {
	return &Result{
		Schema:          "https://putnami.dev/schemas/putnami-transaction-result.json",
		ProtocolVersion: ProtocolVersion,
		Outcome:         OutcomeRetryableSerializationFailure,
		Retryable:       true,
		Message:         "could not serialize access due to concurrent update; the unit of work may be retried",
	}
}

// canonical returns the canonical serialization of v: json.MarshalIndent with
// two-space indentation and a trailing newline. This is the exact byte form both
// the Go and a future TypeScript emitter must reproduce.
func canonical(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// TestSerialization_Stable100 verifies the canonical serialization of the same
// result is byte-identical across 100 marshals — no nondeterminism leaks into
// the wire form.
func TestSerialization_Stable100(t *testing.T) {
	r := sampleResult()
	first := canonical(t, r)
	for i := range 100 {
		if got := canonical(t, r); !bytes.Equal(got, first) {
			t.Fatalf("iteration %d: serialization changed\nfirst: %s\ngot:   %s", i, first, got)
		}
	}
}

// TestSerialization_CanonicalByteForm pins the exact canonical bytes: the
// committed fixtures/equivalence/transaction.golden.json must equal the Go
// serialization of sampleResult(). This is the cross-language contract — a
// future TypeScript adapter must produce this same file byte-for-byte.
func TestSerialization_CanonicalByteForm(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "transaction.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := canonical(t, sampleResult())
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical serialization does not match fixtures/equivalence/transaction.golden.json.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// parseAny routes a valid fixture through the right strict parser by its
// declared $schema id and returns the parsed value for re-serialization.
func parseAny(t *testing.T, data []byte) any {
	t.Helper()
	var head struct {
		Schema string `json:"$schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatalf("peek $schema: %v", err)
	}
	if strings.Contains(head.Schema, "transaction-result") {
		r, diags := ParseResult(data)
		if r == nil {
			t.Fatalf("parse result: %v", diags)
		}
		return r
	}
	u, diags := ParseUnitOfWork(data)
	if u == nil {
		t.Fatalf("parse unit of work: %v", diags)
	}
	return u
}

// TestRoundTrip_Idempotent verifies parse → marshal → parse → marshal is
// byte-stable for every valid fixture: re-serializing a parsed document yields
// the same bytes the second time, so the wire form is a fixed point.
func TestRoundTrip_Idempotent(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			once := canonical(t, parseAny(t, data))
			twice := canonical(t, parseAny(t, once))
			if !bytes.Equal(once, twice) {
				t.Fatalf("%s: round-trip not idempotent\nonce:  %s\ntwice: %s", path, once, twice)
			}
		})
	}
}
