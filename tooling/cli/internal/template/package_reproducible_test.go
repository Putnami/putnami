package template

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// Packaging runs no external program and the archive depends only on the
// template's content: one template packaged from two checkouts with different
// timestamps gives the same bytes, and the installer's extractor accepts them.
func TestPackage_IsReproducibleAndExtractable(t *testing.T) {
	first := makeTemplateDir(t, "repro", "1.0.0")
	second := makeTemplateDir(t, "repro", "1.0.0")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(filepath.Join(dir, "src", "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "src", "nested", "index.ts"), []byte("export const x = 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := filepath.WalkDir(second, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, stamp, stamp)
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no cp, no tar

	a, err := Package(context.Background(), first, PackageOptions{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	b, err := Package(context.Background(), second, PackageOptions{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if a.Integrity != b.Integrity {
		t.Fatalf("one template packaged twice gave integrities %s and %s", a.Integrity, b.Integrity)
	}

	dest := t.TempDir()
	if err := extension.ExtractTarGz(a.ArchivePath, dest); err != nil {
		t.Fatalf("the installer refuses the archive: %v", err)
	}
	for _, rel := range []string{ManifestFilename, "README.md", "main.go.template", filepath.Join("src", "nested", "index.ts")} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("extracted archive lacks %s: %v", rel, err)
		}
	}
}
