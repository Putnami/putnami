package migration

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// tamperFS wraps a base filesystem and overrides the bytes returned for a
// single path, so tests can simulate a payload whose content drifted from its
// pinned hash.
type tamperFS struct {
	base  fs.FS
	path  string
	bytes []byte
}

func (t tamperFS) Open(name string) (fs.File, error) { return t.base.Open(name) }

func (t tamperFS) ReadFile(name string) ([]byte, error) {
	if name == t.path {
		return t.bytes, nil
	}
	return fs.ReadFile(t.base, name)
}

// TestConformance_ValidBundleFixtures asserts every golden bundle under
// fixtures/valid parses and validates with no errors.
func TestConformance_ValidBundleFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid bundle fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b, diags := ParseAndValidateBundle(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if b == nil {
				t.Errorf("valid fixture %s returned nil bundle", path)
			}
		})
	}
}

// TestConformance_InvalidBundleFixtures asserts every golden bundle under
// fixtures/invalid is rejected at parse or validate time.
func TestConformance_InvalidBundleFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid bundle fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b, pDiags := ParseBundle(data)
			if diag.HasErrors(pDiags) {
				return // strict parse rejection is sufficient
			}
			vDiags := ValidateBundle(b)
			if !diag.HasErrors(vDiags) {
				t.Errorf("invalid fixture %s should produce errors but validated clean", path)
			}
		})
	}
}

// TestConformance_PayloadTree verifies a fully materialized bundle directory:
// the manifest validates, its declared digest matches, and every payload file
// hashes to the pinned reference.
func TestConformance_PayloadTree(t *testing.T) {
	root := "fixtures/payload-tree"
	data, err := os.ReadFile(filepath.Join(root, BundleFileName))
	if err != nil {
		t.Fatal(err)
	}

	b, diags := ParseAndValidateBundle(data)
	if diag.HasErrors(diags) {
		t.Fatalf("payload-tree bundle failed structural validation: %v", diags)
	}

	if vDiags := VerifyBundlePayloads(os.DirFS(root), b); diag.HasErrors(vDiags) {
		t.Fatalf("payload-tree payload verification failed: %v", vDiags)
	}

	want := []string{
		"payload/sql/default/iam/0001_create_users.down.sql",
		"payload/sql/default/iam/0001_create_users.up.sql",
	}
	got := PayloadPaths(b)
	if len(got) != len(want) {
		t.Fatalf("PayloadPaths returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PayloadPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestConformance_PayloadTree_TamperDetected confirms payload verification
// fails loudly when an on-disk payload no longer matches its pinned hash.
func TestConformance_PayloadTree_TamperDetected(t *testing.T) {
	root := "fixtures/payload-tree"
	data, err := os.ReadFile(filepath.Join(root, BundleFileName))
	if err != nil {
		t.Fatal(err)
	}
	b, diags := ParseAndValidateBundle(data)
	if diag.HasErrors(diags) {
		t.Fatalf("payload-tree bundle failed structural validation: %v", diags)
	}

	// Build an in-memory FS that returns tampered content for the up payload.
	tampered := tamperFS{
		base:  os.DirFS(root),
		path:  b.Operations[0].Up.Path,
		bytes: []byte("DROP DATABASE production;\n"),
	}
	vDiags := VerifyBundlePayloads(tampered, b)
	if !diag.HasErrors(vDiags) {
		t.Fatal("tampered payload must fail verification")
	}
	if !hasCode(vDiags, ErrorCodeInvalidPayload) {
		t.Fatalf("expected invalid payload diagnostic, got: %v", vDiags)
	}
}

// TestConformance_GoFilesFormatted keeps the protocol package gofmt-clean so
// the published Go module matches the repository style.
func TestConformance_GoFilesFormatted(t *testing.T) {
	out, err := exec.Command("gofmt", "-l", ".").CombinedOutput()
	if err != nil {
		t.Skipf("gofmt unavailable: %v", err)
	}
	if len(out) > 0 {
		t.Fatalf("gofmt reported unformatted files:\n%s", out)
	}
}
