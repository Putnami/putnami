package oci

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review.
func TestConformance_ProtocolVersion(t *testing.T) {
	spectest.Proves(t, "oci/registry-fast-path-contract", "the-version-and-endpoints-are-pinned", "the-protocol-version-is-1")
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_Endpoints pins the capability/endpoint contract the cloud OCI
// registry implements. Changing any of these is a protocol change.
func TestConformance_Endpoints(t *testing.T) {
	spectest.Proves(t, "oci/registry-fast-path-contract", "the-version-and-endpoints-are-pinned", "the-endpoints-and-the-capability-are-pinned")
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
	spectest.Proves(t, "oci/registry-fast-path-contract", "the-fast-path-is-taken-only-when-advertised", "the-capability-gate-needs-tag-digest-v1")
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
	spectest.Proves(t, "oci/registry-fast-path-contract", "the-error-codes-are-closed", "the-error-codes-are-the-five-canonical-ones")
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
	spectest.Proves(t, "oci/registry-fast-path-contract", "the-fixture-corpus-is-the-contract", "every-fixture-gets-its-verdict")
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

// registryHostCases is fixtures/registry-host/cases.json: repositories read by
// RegistryHost and hosts read by NormalizeRegistryHost, each with the host and
// scheme it yields, or the reason it is refused.
type registryHostCases struct {
	Repositories []struct {
		Repository string `json:"repository"`
		Host       string `json:"host"`
		Scheme     string `json:"scheme"`
		Refused    string `json:"refused"`
	} `json:"repositories"`
	Hosts []struct {
		Host     string `json:"host"`
		Registry string `json:"registry"`
		Scheme   string `json:"scheme"`
		Refused  string `json:"refused"`
	} `json:"hosts"`
}

// TestConformance_RegistryHost runs the registry host table: every accepted
// repository and host yields its registry host and scheme, every refused one
// is refused, and no error repeats what it refuses.
func TestConformance_RegistryHost(t *testing.T) {
	spectest.Proves(t, "oci/registry-fast-path-contract", "the-registry-host-rules-are-one-table", "every-registry-host-case-gets-its-verdict")
	data, err := os.ReadFile(filepath.Join("fixtures", "registry-host", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cases registryHostCases
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	if len(cases.Repositories) == 0 || len(cases.Hosts) == 0 {
		t.Fatal("the registry host table is empty")
	}
	for _, test := range cases.Repositories {
		host, err := RegistryHost(test.Repository)
		switch {
		case test.Refused == "" && (test.Host == "" || test.Scheme == ""):
			t.Errorf("repository %q: an accepted case names its host and scheme", test.Repository)
		case test.Refused != "" && err == nil:
			t.Errorf("repository %q: accepted as %q, want refused: %s", test.Repository, host, test.Refused)
		case test.Refused != "" && len(test.Repository) > 3 && strings.Contains(err.Error(), test.Repository):
			t.Errorf("repository %q: the error repeats it: %v", test.Repository, err)
		case test.Refused == "" && err != nil:
			t.Errorf("repository %q: %v", test.Repository, err)
		case test.Refused == "" && (host != test.Host || RegistryScheme(host) != test.Scheme):
			t.Errorf("repository %q: %s://%s, want %s://%s", test.Repository, RegistryScheme(host), host, test.Scheme, test.Host)
		}
	}
	for _, test := range cases.Hosts {
		registry, err := NormalizeRegistryHost(test.Host)
		switch {
		case test.Refused == "" && (test.Registry == "" || test.Scheme == ""):
			t.Errorf("host %q: an accepted case names its registry and scheme", test.Host)
		case test.Refused != "" && err == nil:
			t.Errorf("host %q: accepted as %q, want refused: %s", test.Host, registry, test.Refused)
		case test.Refused != "" && len(test.Host) > 3 && strings.Contains(err.Error(), test.Host):
			t.Errorf("host %q: the error repeats it: %v", test.Host, err)
		case test.Refused == "" && err != nil:
			t.Errorf("host %q: %v", test.Host, err)
		case test.Refused == "" && (registry != test.Registry || RegistryScheme(registry) != test.Scheme || RegistryScheme(test.Host) != test.Scheme):
			t.Errorf("host %q: %s://%s, want %s://%s", test.Host, RegistryScheme(registry), registry, test.Scheme, test.Registry)
		}
	}
}

// TestRegistryHostIsCaseInsensitive proves the one rule that differs from
// go-containerregistry: a host is lowercased before any other rule applies,
// and only its ASCII letters change.
func TestRegistryHostIsCaseInsensitive(t *testing.T) {
	spectest.Proves(t, "oci/registry-fast-path-contract", "the-registry-host-rules-are-one-table", "a-host-is-lowercased-first")
	for _, test := range []struct{ host, want string }{
		{"Docker.Io", "index.docker.io"},
		{"REGISTRY.LOCALHOST:5000", "registry.localhost:5000"},
		{"Ünicode.Example", "Ünicode.example"},
		{"\xffA.io", "\xffa.io"},
	} {
		registry, err := NormalizeRegistryHost(test.host)
		if err != nil || registry != test.want {
			t.Errorf("NormalizeRegistryHost(%q) = %q, %v; want %q", test.host, registry, err, test.want)
		}
		if RegistryScheme(test.host) != RegistryScheme(test.want) {
			t.Errorf("RegistryScheme(%q) differs from RegistryScheme(%q)", test.host, test.want)
		}
	}
	if path := strings.Repeat("a", maxRepositoryPathBytes); !validRepositoryPath(path) || validRepositoryPath(path+"a") {
		t.Errorf("the repository path bound is not %d bytes", maxRepositoryPathBytes)
	}
}
