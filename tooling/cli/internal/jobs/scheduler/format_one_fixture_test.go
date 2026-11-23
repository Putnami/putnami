package scheduler

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFormatOneEntry writes the files-less format-1 entry an older CLI leaves
// at hash in the store rooted at storeRoot: meta.json and result.json under the
// raw address.
func writeFormatOneEntry(t *testing.T, storeRoot, hash string) {
	t.Helper()
	blob := filepath.Join(storeRoot, "blobs", hash[:2], hash)
	if err := os.MkdirAll(blob, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"hash":"` + hash + `","createdAt":"2026-01-01T00:00:00Z","task":"build-generate"}`
	if err := os.WriteFile(filepath.Join(blob, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blob, "result.json"), []byte(`{"status":"success"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireNoRawEntry fails when the store rooted at storeRoot holds a blob at
// hash itself, the address of a format-1 entry.
func requireNoRawEntry(t *testing.T, storeRoot, hash string) {
	t.Helper()
	blob := filepath.Join(storeRoot, "blobs", hash[:2], hash)
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatalf("a blob stands at the raw cache key %s (stat err=%v)", blob, err)
	}
}
