package stackstest

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteFile writes content at rel under root, a slash-separated path, creating
// the directories it needs.
func WriteFile(t testing.TB, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
