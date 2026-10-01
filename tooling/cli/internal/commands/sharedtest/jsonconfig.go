package sharedtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// WriteJSONConfig marshals data as JSON and writes it to path, creating any
// missing parent directories first.
func WriteJSONConfig(t *testing.T, path string, data map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}
