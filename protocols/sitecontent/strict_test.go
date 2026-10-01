package sitecontent

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestFormatVersionCompatibility(t *testing.T) {
	if !CompatibleFormatVersion("1.0") {
		t.Fatal("1.0 should be compatible")
	}
	if !CompatibleFormatVersion("1.2") {
		t.Fatal("same major minor bump should be compatible")
	}
	if CompatibleFormatVersion("2.0") {
		t.Fatal("newer major should be incompatible")
	}
	if CompatibleFormatVersion("not-a-version") {
		t.Fatal("malformed version should be incompatible")
	}
}

func TestValidRelPath(t *testing.T) {
	valid := []string{"docs/cloud/index.html", ".well-known/putnami.json"}
	for _, path := range valid {
		if !ValidRelPath(path) {
			t.Errorf("ValidRelPath(%q) = false, want true", path)
		}
	}

	invalid := []string{"", "/docs/cloud", "docs//cloud", "docs/./cloud", "docs/../cloud", `docs\cloud`}
	for _, path := range invalid {
		if ValidRelPath(path) {
			t.Errorf("ValidRelPath(%q) = true, want false", path)
		}
	}
}

func TestValidateManifest_DuplicateFile(t *testing.T) {
	m := validPayloadManifest()
	m.Files = append(m.Files, m.Files[0])
	diags := ValidateManifest(&m)
	if !hasCode(diags, ErrorCodeDuplicateFile) {
		t.Fatalf("want duplicate file diagnostic, got %v", diags)
	}
}

func TestSchemaIDMatchesFixture(t *testing.T) {
	data, err := os.ReadFile("fixtures/valid/bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("fixture should be valid: %v", diags)
	}
	if m.Schema != SchemaID {
		t.Fatalf("fixture $schema = %q, want %q", m.Schema, SchemaID)
	}
}

// TestValidDigest pins the bare-hex sha256 digest reject path (strict.go:245).
// A fixture proves the parser rejects a bad manifest; this proves the exact
// digest grammar an implementer must reproduce, which no single fixture pins.
func TestValidDigest(t *testing.T) {
	valid := []string{
		"1a31f208b4e0b7759528b7af8580d7beb300ba849259987113d8b8d7718ff4a5",
	}
	for _, d := range valid {
		if !ValidDigest(d) {
			t.Errorf("ValidDigest(%q) = false, want true", d)
		}
	}

	invalid := []string{
		"",
		"sha256:1a31f208b4e0b7759528b7af8580d7beb300ba849259987113d8b8d7718ff4a5", // prefixed
		"1a31f208b4e0b7759528b7af8580d7beb300ba849259987113d8b8d7718ff4a",         // too short
		"1a31f208b4e0b7759528b7af8580d7beb300ba849259987113d8b8d7718ff4a5a",       // too long
		"1A31F208B4E0B7759528B7AF8580D7BEB300BA849259987113D8B8D7718FF4A5",        // uppercase
		"1a31f208b4e0b7759528b7af8580d7beb300ba849259987113d8b8d7718ff4gg",        // non-hex
	}
	for _, d := range invalid {
		if ValidDigest(d) {
			t.Errorf("ValidDigest(%q) = true, want false", d)
		}
	}
}

// TestValidMountPrefix pins the mount-prefix reject path (strict.go:279): only
// absolute, normalized, lowercase, non-root, no-trailing-slash paths are valid.
func TestValidMountPrefix(t *testing.T) {
	valid := []string{"/docs", "/docs/cloud", "/a-b/c_d"}
	for _, p := range valid {
		if !ValidMountPrefix(p) {
			t.Errorf("ValidMountPrefix(%q) = false, want true", p)
		}
	}

	invalid := []string{
		"",             // empty
		"docs/cloud",   // not absolute
		"/",            // root
		"/docs/cloud/", // trailing slash
		"/Docs/Cloud",  // uppercase segment
		"/docs//cloud", // empty segment
		"/docs/../x",   // '..' segment
		"/docs/.",      // '.' segment
	}
	for _, p := range invalid {
		if ValidMountPrefix(p) {
			t.Errorf("ValidMountPrefix(%q) = true, want false", p)
		}
	}
}

// TestInvalidFixtureCoverage enforces the "corpus is the spec" contract for
// sitecontent: every fixtures/invalid case must be genuinely rejected by the
// strict parser, and each digest/mount reject code must be triggered by at
// least one invalid fixture. A fixture nobody rejects, or a rule no fixture
// reaches, would let a second implementation disagree while both suites pass.
func TestInvalidFixtureCoverage(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}

	triggered := map[string]bool{}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, diags := ParseAndValidateManifest(data)
		if !diag.HasErrors(diags) {
			t.Errorf("invalid fixture %s should produce diagnostics but none found", path)
		}
		for _, d := range diags {
			triggered[d.Code] = true
		}
	}

	// The reject codes that previously had no fixture must now be exercised.
	for _, code := range []string{ErrorCodeInvalidDigest, ErrorCodeInvalidMount} {
		if !triggered[code] {
			t.Errorf("no invalid fixture triggers %q — reject branch is unguarded", code)
		}
	}
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}
