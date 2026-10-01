package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review. The wire `v` field is validated
// against this constant in validate.go, so a bump here without a corresponding
// migration of every emitter and the fixture corpus would break conformance.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

func TestConformance_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			events, err := DecodeAll(f)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}

			diags := ValidateEventStream(events)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}
}

func TestConformance_InvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			events, err := DecodeAll(f)
			if err != nil {
				// Parse error is also a valid failure mode for invalid fixtures.
				return
			}

			diags := ValidateEventStream(events)
			if !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
		})
	}
}

func TestConformance_ValidFixture_MinimalStream(t *testing.T) {
	data := `{"v":1,"type":"result","data":{"status":"OK"}}`
	events, err := DecodeAll(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Type != EventResult {
		t.Errorf("Type = %q, want result", events[0].Type)
	}
}
