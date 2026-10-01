package gomod

import (
	"encoding/json"
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

// TestConformance_Paths pins the endpoint path templates the cloud gomod
// registry serves. Changing any is a protocol change.
func TestConformance_Paths(t *testing.T) {
	if got := BlobUploadPath("go.putnami.dev/http"); got != "/go.putnami.dev/http/-/blobs/upload" {
		t.Errorf("BlobUploadPath = %q, want /go.putnami.dev/http/-/blobs/upload", got)
	}
	if got := VersionPath("go.putnami.dev/http", "v1.2.3"); got != "/go.putnami.dev/http/@v/v1.2.3" {
		t.Errorf("VersionPath = %q, want /go.putnami.dev/http/@v/v1.2.3", got)
	}
	if got := ReleasePath("go.putnami.dev/http", "v1.2.3"); got != "/go.putnami.dev/http/@v/v1.2.3/release" {
		t.Errorf("ReleasePath = %q, want /go.putnami.dev/http/@v/v1.2.3/release", got)
	}
	if BlobContentType != "application/zip" {
		t.Errorf("BlobContentType = %q, want application/zip", BlobContentType)
	}
	if VersionContentType != "application/json" {
		t.Errorf("VersionContentType = %q, want application/json", VersionContentType)
	}
	if ReleaseContentType != "application/json" {
		t.Errorf("ReleaseContentType = %q, want application/json", ReleaseContentType)
	}
}

// TestConformance_WireFieldNames pins the JSON field names the server owns.
// Drifting them would silently break the cross-repo contract.
func TestConformance_WireFieldNames(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	full, err := json.Marshal(PublishVersionRequest{GoMod: "module x\n", ZipDigest: digest, DistTag: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"go_mod"`, `"zip_digest"`, `"dist_tag"`} {
		if !strings.Contains(string(full), key) {
			t.Errorf("PublishVersionRequest JSON missing %s: %s", key, full)
		}
	}
	// dist_tag must be omitted (not "") when no channel is set.
	noTag, err := json.Marshal(PublishVersionRequest{GoMod: "module x\n", ZipDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(noTag), "dist_tag") {
		t.Errorf("dist_tag must be omitted when empty, got %s", noTag)
	}
	release, err := json.Marshal(ReleaseVersionResponse{
		Module: "go.putnami.dev/http", Version: "v1.2.3", Visibility: ReleaseVisibilityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"module"`, `"version"`, `"visibility"`} {
		if !strings.Contains(string(release), key) {
			t.Errorf("ReleaseVersionResponse JSON missing %s: %s", key, release)
		}
	}
}

// TestConformance_ErrorCodes validates that every protocol error code uses the
// gomod.* prefix and that the canonical set is complete.
func TestConformance_ErrorCodes(t *testing.T) {
	canonical := []string{
		"gomod.invalid_blob_upload",
		"gomod.invalid_publish",
		"gomod.invalid_digest",
		"gomod.invalid_go_mod",
		"gomod.invalid_release",
	}
	for _, code := range canonical {
		if !ValidErrorCodes[code] {
			t.Errorf("canonical error code %q missing from ValidErrorCodes", code)
		}
		if !strings.HasPrefix(code, "gomod.") {
			t.Errorf("error code %q must use gomod.* prefix", code)
		}
	}
	if len(ValidErrorCodes) != len(canonical) {
		t.Errorf("ValidErrorCodes has %d entries, want %d canonical codes", len(ValidErrorCodes), len(canonical))
	}
}

type parseFunc func([]byte) []diag.Diagnostic

// TestConformance_Fixtures runs every fixture under fixtures/<message>: those in
// valid/ must produce no errors, those in invalid/ must produce at least one.
func TestConformance_Fixtures(t *testing.T) {
	messages := map[string]parseFunc{
		"blob-upload-response":    func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateBlobUploadResponse(b); return d },
		"publish-version-request": func(b []byte) []diag.Diagnostic { _, d := ParseAndValidatePublishVersionRequest(b); return d },
		"release-version-response": func(b []byte) []diag.Diagnostic {
			_, d := ParseAndValidateReleaseVersionResponse(b)
			return d
		},
	}
	for msg, parse := range messages {
		runFixtureDir(t, msg, "valid", parse, false)
		runFixtureDir(t, msg, "invalid", parse, true)
	}
}

func TestConformance_ReleaseResponseRejectsTrailingJSON(t *testing.T) {
	body := []byte(`{"module":"go.putnami.dev/http","version":"v1.2.3","visibility":"public"} null`)
	if _, diagnostics := ParseAndValidateReleaseVersionResponse(body); !diag.HasErrors(diagnostics) {
		t.Fatal("release response with a trailing JSON value must fail strict parsing")
	}
}

func TestConformance_ReleaseResponseMustMatchRequestedCoordinate(t *testing.T) {
	body := []byte(`{"module":"go.putnami.dev/http","version":"v1.2.4","visibility":"public"}`)
	if _, diagnostics := ParseAndValidateReleaseVersionResponseFor(body, "go.putnami.dev/http", "v1.2.3"); !diag.HasErrors(diagnostics) {
		t.Fatal("release response for another immutable version must fail validation")
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
