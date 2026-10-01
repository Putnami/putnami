package sharedtest

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteLock writes content to <ws>/<name> and returns the file's path. Despite
// the name (its first caller was always a putnami.lock.json fixture), it
// writes any file verbatim — the point is bytes this CLI's own writer did not
// produce, so a fixture can exercise the reader on a shape the writer would
// never emit.
func WriteLock(t *testing.T, ws, name, content string) string {
	t.Helper()
	p := filepath.Join(ws, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}
