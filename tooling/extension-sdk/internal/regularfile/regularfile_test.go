package regularfile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenReadsARegularFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(name, []byte("artifact bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(name)
	if err != nil {
		t.Fatalf("Open(regular file) = %v", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "artifact bytes" {
		t.Fatalf("read %q, %v", data, err)
	}
}

func TestOpenRefusesWhatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Open(directory) = %v, want a refusal", err)
	}
	if _, err := Open(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open(missing) = %v, want not-exist", err)
	}
}
