//go:build unix

package template

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackage_RefusesASymbolicLink(t *testing.T) {
	dir := makeTemplateDir(t, "linked", "1.0.0")
	nested := filepath.Join(dir, "config")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "defaults.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("defaults.json", filepath.Join(nested, "current.json")); err != nil {
		t.Fatal(err)
	}

	_, err := Package(context.Background(), dir, PackageOptions{OutputDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected Package to refuse a symbolic link")
	}
	if !strings.Contains(err.Error(), "config/current.json is a symbolic link to defaults.json") {
		t.Fatalf("error does not name the link and its target: %v", err)
	}
}
