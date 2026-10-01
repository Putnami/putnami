package sharedtest

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteFeatureFixture writes contents to filename, creating any missing
// parent directories first.
func WriteFeatureFixture(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
