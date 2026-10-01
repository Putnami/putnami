package migration

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// writeReadableBundle writes a minimal one-operation bundle to a temp dir and
// returns the root plus the on-disk up-payload path. Tests then tamper the tree
// to drive LoadBundle's payload reject branches (bundle_read.go:36 and the
// hash-verify loop).
func writeReadableBundle(t *testing.T) (root, upPath string) {
	t.Helper()
	root = t.TempDir()
	up := []byte("CREATE TABLE iam.users (id uuid primary key);\n")
	upPath = "payload/sql/default/iam/0001_init.up.sql"
	b := Bundle{
		AppName: "wealth",
		Operations: []BundleOperation{{
			Kind:      KindSQL,
			Target:    "default",
			Namespace: "iam",
			Name:      "iam/0001_init",
			Up:        PayloadRef{Path: upPath, Hash: ComputePayloadHash(up)},
		}},
	}
	if err := WriteBundle(root, b, []BundlePayload{{Path: upPath, Bytes: up}}); err != nil {
		t.Fatal(err)
	}
	return root, upPath
}

func TestLoadBundle_RoundTripsWriteBundle(t *testing.T) {
	root := t.TempDir()

	up := []byte("CREATE TABLE iam.users (id uuid primary key);\n")
	down := []byte("DROP TABLE iam.users;\n")
	upPath := "payload/sql/default/iam/0001_init.up.sql"
	downPath := "payload/sql/default/iam/0001_init.down.sql"

	b := Bundle{
		AppName: "wealth",
		Operations: []BundleOperation{{
			Kind:      KindSQL,
			Target:    "default",
			Namespace: "iam",
			Name:      "iam/0001_init",
			Up:        PayloadRef{Path: upPath, Hash: ComputePayloadHash(up)},
			Down:      &PayloadRef{Path: downPath, Hash: ComputePayloadHash(down)},
		}},
	}
	if err := WriteBundle(root, b, []BundlePayload{{Path: upPath, Bytes: up}, {Path: downPath, Bytes: down}}); err != nil {
		t.Fatal(err)
	}

	loaded, payloads, diags := LoadBundle(os.DirFS(root))
	if diag.HasErrors(diags) {
		t.Fatalf("LoadBundle: %v", diags)
	}
	if loaded.AppName != "wealth" || len(loaded.Operations) != 1 {
		t.Fatalf("unexpected loaded bundle: %+v", loaded)
	}
	if string(payloads[upPath]) != string(up) {
		t.Errorf("up payload mismatch: %q", payloads[upPath])
	}
	if string(payloads[downPath]) != string(down) {
		t.Errorf("down payload mismatch: %q", payloads[downPath])
	}
}

func TestLoadBundle_MissingManifest(t *testing.T) {
	_, _, diags := LoadBundle(os.DirFS(t.TempDir()))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for a directory with no bundle.json")
	}
}

// TestLoadBundle_MissingPayload covers the reject branch at bundle_read.go:36:
// a manifest that references a payload file which is not present on disk must
// fail to load rather than silently materialize a bundle with missing bytes.
func TestLoadBundle_MissingPayload(t *testing.T) {
	root, upPath := writeReadableBundle(t)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(upPath))); err != nil {
		t.Fatal(err)
	}

	_, _, diags := LoadBundle(os.DirFS(root))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error when a referenced payload file is missing")
	}
	if !hasCode(diags, ErrorCodeInvalidPayload) {
		t.Fatalf("want %q diagnostic for missing payload, got %v", ErrorCodeInvalidPayload, diags)
	}
}

// TestLoadBundle_HashMismatch covers the payload hash-verify reject branch: a
// payload whose bytes no longer match the digest the manifest pins is a
// content-integrity failure and must be rejected. This is the guarantee that a
// published bundle cannot be tampered post-hash.
func TestLoadBundle_HashMismatch(t *testing.T) {
	root, upPath := writeReadableBundle(t)
	// Rewrite the payload with different bytes; the manifest still pins the
	// original hash, so verification must fail.
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(upPath)), []byte("DROP TABLE iam.users;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, diags := LoadBundle(os.DirFS(root))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error when a payload no longer matches its pinned hash")
	}
	if !hasCode(diags, ErrorCodeInvalidPayload) {
		t.Fatalf("want %q diagnostic for hash mismatch, got %v", ErrorCodeInvalidPayload, diags)
	}
}
