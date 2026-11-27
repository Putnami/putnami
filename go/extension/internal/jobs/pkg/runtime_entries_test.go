package pkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEntriesStage(t *testing.T, manifest string, files ...string) string {
	t.Helper()
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		path := filepath.Join(stage, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return stage
}

const entriesManifest = `{"runtime":{"executable":"compiled/putnami-go","entries":{"codegen":{"executable":"compiled/putnami-go-codegen"}}}}`

// A platform archive that misses an entry's executable fails packaging, and
// one that carries it lists it among the files the archive marks executable.
func TestValidateStagedRuntimeEntries(t *testing.T) {
	stage := writeEntriesStage(t, entriesManifest, "compiled/putnami-go", "compiled/putnami-go-codegen")
	got, err := validateStagedRuntimeEntries(stage, "linux")
	if err != nil || strings.Join(got, ",") != "compiled/putnami-go-codegen" {
		t.Fatalf("entries = %v, %v", got, err)
	}

	missing := writeEntriesStage(t, entriesManifest, "compiled/putnami-go")
	if _, err := validateStagedRuntimeEntries(missing, "linux"); err == nil || !strings.Contains(err.Error(), `entry "codegen"`) {
		t.Fatalf("a missing entry = %v, want it named", err)
	}

	windows := writeEntriesStage(t, entriesManifest, "compiled/putnami-go.exe", "compiled/putnami-go-codegen")
	if _, err := validateStagedRuntimeEntries(windows, "windows"); err == nil {
		t.Fatal("a Windows archive whose entry lacks its .exe name passed")
	}

	linked := writeEntriesStage(t, entriesManifest, "compiled/putnami-go", "compiled/real")
	if err := os.Symlink("real", filepath.Join(linked, "compiled", "putnami-go-codegen")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := validateStagedRuntimeEntries(linked, "linux"); err == nil {
		t.Fatal("an entry that is a symbolic link passed")
	}

	none := writeEntriesStage(t, `{"runtime":{"executable":"compiled/putnami-go"}}`, "compiled/putnami-go")
	if got, err := validateStagedRuntimeEntries(none, "linux"); err != nil || len(got) != 0 {
		t.Fatalf("a runtime without entries = %v, %v", got, err)
	}
}

func TestAppendMissing(t *testing.T) {
	if got := appendMissing([]string{"a", "b"}, "b", "c", "c"); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("appendMissing = %v", got)
	}
}
