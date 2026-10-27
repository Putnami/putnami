package sitecontent

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review. This is the test the package doc
// (sitecontent.go) references: ProtocolVersion versions this package's Go/parser
// surface and is distinct from a bundle's FormatVersion (the on-the-wire bundle
// format a consumer gates on).
func TestConformance_ProtocolVersion(t *testing.T) {
	spectest.Proves(t, "sitecontent/content-bundle-contract", "the-versions-are-pinned", "the-protocol-version-is-pinned")
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

func TestConformance_ValidManifestFixtures(t *testing.T) {
	spectest.Proves(t, "sitecontent/content-bundle-contract", "the-manifest-is-closed-and-strict", "the-valid-manifests-parse")
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
			m, diags := ParseAndValidateManifest(data)
			if diag.HasErrors(diags) {
				t.Fatalf("valid fixture produced diagnostics: %v", diags)
			}
			if m == nil {
				t.Fatal("valid fixture returned nil manifest")
			}
		})
	}
}

func TestConformance_InvalidManifestFixtures(t *testing.T) {
	spectest.Proves(t, "sitecontent/content-bundle-contract", "the-manifest-is-closed-and-strict", "the-invalid-manifests-are-refused")
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
			_, diags := ParseAndValidateManifest(data)
			if !diag.HasErrors(diags) {
				t.Fatalf("invalid fixture should produce diagnostics")
			}
		})
	}
}
