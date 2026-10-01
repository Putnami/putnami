package workspace

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review. The workspace/project config has
// no on-the-wire protocol version field (Config.Version is a free-form authored
// semver, not this constant), so this is the only anchor — bumping requires a
// migration story.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

func TestConformance_ValidWorkspaceFixtures(t *testing.T) {
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

			c, diags := ParseAndValidateWorkspaceConfig(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if c == nil {
				t.Errorf("valid fixture %s returned nil config", path)
			}
		})
	}
}

func TestConformance_InvalidWorkspaceFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			c, pDiags := ParseWorkspaceConfig(data)
			if diag.HasErrors(pDiags) {
				return // parse error is sufficient for invalid fixtures
			}

			vDiags := ValidateWorkspaceConfig(c)
			pDiags = append(pDiags, vDiags...)
			if len(pDiags) == 0 {
				t.Errorf("invalid fixture %s should produce diagnostics but none found", path)
			}
		})
	}
}
