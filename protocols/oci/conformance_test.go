package oci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_Endpoints pins the capability/endpoint contract the cloud OCI
// registry implements. Changing any of these is a protocol change.
func TestConformance_Endpoints(t *testing.T) {
	if CapabilitiesPath != "/v2/_putnami/capabilities" {
		t.Errorf("CapabilitiesPath = %q, want /v2/_putnami/capabilities", CapabilitiesPath)
	}
	if TagDigestPath != "/v2/_putnami/tag-digest" {
		t.Errorf("TagDigestPath = %q, want /v2/_putnami/tag-digest", TagDigestPath)
	}
	if APITagDigestV1 != "tag-digest/v1" {
		t.Errorf("APITagDigestV1 = %q, want tag-digest/v1", APITagDigestV1)
	}
}

// TestConformance_SupportsTagDigest pins the capability-gate semantics.
func TestConformance_SupportsTagDigest(t *testing.T) {
	if !(CapabilitiesResponse{APIs: []string{"tag-digest/v1"}}).SupportsTagDigest() {
		t.Error("SupportsTagDigest must be true when apis contains tag-digest/v1")
	}
	if (CapabilitiesResponse{APIs: []string{"other/v9"}}).SupportsTagDigest() {
		t.Error("SupportsTagDigest must be false without tag-digest/v1")
	}
	if (CapabilitiesResponse{}).SupportsTagDigest() {
		t.Error("SupportsTagDigest must be false for an empty capability set")
	}
}

// TestConformance_ErrorCodes validates that every protocol error code uses the
// oci.* prefix and that the canonical set is complete.
func TestConformance_ErrorCodes(t *testing.T) {
	canonical := []string{
		"oci.invalid_capabilities",
		"oci.invalid_tag_digest",
		"oci.invalid_digest",
		"oci.invalid_repository",
		"oci.invalid_tag",
	}
	for _, code := range canonical {
		if !ValidErrorCodes[code] {
			t.Errorf("canonical error code %q missing from ValidErrorCodes", code)
		}
		if !strings.HasPrefix(code, "oci.") {
			t.Errorf("error code %q must use oci.* prefix", code)
		}
	}
	if len(ValidErrorCodes) != len(canonical) {
		t.Errorf("ValidErrorCodes has %d entries, want %d canonical codes", len(ValidErrorCodes), len(canonical))
	}
}

type parseFunc func([]byte) []diag.Diagnostic

// TestConformance_Fixtures runs every fixture under fixtures/<message>: those in
// valid/ must produce no errors, those in invalid/ must produce at least one. A
// cloud registry in another language validates its bodies against the same corpus.
func TestConformance_Fixtures(t *testing.T) {
	messages := map[string]parseFunc{
		"capabilities-response": func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateCapabilitiesResponse(b); return d },
		"tag-digest-request":    func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateTagDigestRequest(b); return d },
	}
	for msg, parse := range messages {
		runFixtureDir(t, msg, "valid", parse, false)
		runFixtureDir(t, msg, "invalid", parse, true)
	}
}

func runFixtureDir(t *testing.T, msg, kind string, parse parseFunc, wantErrors bool) {
	t.Helper()
	glob := filepath.Join("fixtures", msg, kind, "*.json")
	paths, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures matched %s", glob)
	}
	for _, path := range paths {
		t.Run(msg+"/"+kind+"/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			diags := parse(data)
			hasErr := diag.HasErrors(diags)
			if wantErrors && !hasErr {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
			if !wantErrors && hasErr {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}
}
