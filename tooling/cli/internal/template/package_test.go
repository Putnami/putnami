package template

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTemplateDir lays out a minimal valid template on disk: a manifest,
// a README, and a content file (the staging step copies these into a
// tarball that Package writes out).
func makeTemplateDir(t *testing.T, name, version string) string {
	t.Helper()
	dir := t.TempDir()

	manifest := map[string]any{
		"name":        name,
		"version":     version,
		"description": "test template",
	}
	manifestBytes, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, ManifestFilename), manifestBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LICENSE.md"), []byte("MIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go.template"), []byte("package {{.Name}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestPackage_WritesArchiveAndMetadata exercises the happy-path Package
// flow: staging, version stamping, tarball creation, integrity hashing,
// and metadata.json sidecar.
func TestPackage_WritesArchiveAndMetadata(t *testing.T) {
	templateDir := makeTemplateDir(t, "sample-tpl", "1.2.3")
	outputDir := filepath.Join(t.TempDir(), "out")

	result, err := Package(context.Background(), templateDir, PackageOptions{OutputDir: outputDir})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if result.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", result.Version, "1.2.3")
	}
	if result.Integrity == "" {
		t.Error("Integrity should be populated")
	}

	// The archive must exist and be gzip-formatted.
	f, err := os.Open(result.ArchivePath)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	if _, err := gzip.NewReader(f); err != nil {
		t.Errorf("archive is not valid gzip: %v", err)
	}

	// metadata.json should be one directory above the archives folder, per Package().
	metaData, err := os.ReadFile(result.MetadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if meta["version"] != "1.2.3" {
		t.Errorf("metadata version = %v, want 1.2.3", meta["version"])
	}
	if meta["artifact"] != "sample-tpl" {
		t.Errorf("metadata artifact = %v, want sample-tpl", meta["artifact"])
	}
	if meta["template"] != true {
		t.Errorf("metadata template = %v, want true", meta["template"])
	}
}

// TestPackage_StampsVersionOverride asserts that PackageOptions.Version
// overrides whatever is in the manifest on disk.
func TestPackage_StampsVersionOverride(t *testing.T) {
	templateDir := makeTemplateDir(t, "stamp-tpl", "0.0.0")
	outputDir := filepath.Join(t.TempDir(), "out")

	result, err := Package(context.Background(), templateDir, PackageOptions{
		OutputDir: outputDir,
		Version:   "9.9.9",
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if result.Version != "9.9.9" {
		t.Errorf("Version = %q, want %q", result.Version, "9.9.9")
	}
	// Archive name includes the stamped version.
	if want := "stamp-tpl-9.9.9.tar.gz"; filepath.Base(result.ArchivePath) != want {
		t.Errorf("archive name = %q, want %q", filepath.Base(result.ArchivePath), want)
	}
}

// TestPackage_StableStripsPreRelease verifies the Stable option drops a
// pre-release suffix from the stamped version.
func TestPackage_StableStripsPreRelease(t *testing.T) {
	templateDir := makeTemplateDir(t, "stable-tpl", "1.0.0-beta.2")
	outputDir := filepath.Join(t.TempDir(), "out")

	result, err := Package(context.Background(), templateDir, PackageOptions{
		OutputDir: outputDir,
		Stable:    true,
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	if result.Version != "1.0.0" {
		t.Errorf("Stable version = %q, want %q", result.Version, "1.0.0")
	}
}

// TestPackage_MissingManifest is a negative test for the manifest-not-found path.
func TestPackage_MissingManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Package(context.Background(), dir, PackageOptions{})
	if err == nil {
		t.Fatal("expected error when manifest is missing")
	}
}

func TestPackage_ExcludesWorkspaceDeclarationsAndKeepsTemplateContent(t *testing.T) {
	templateDir := makeTemplateDir(t, "clean-tpl", "1.0.0")
	if err := os.WriteFile(filepath.Join(templateDir, "putnami.json"), []byte(`{"name":"template-project"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "putnami.features.json"), []byte(`{"protocolVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(templateDir, "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "specs", "template.json"), []byte(`{"protocolVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Package(context.Background(), templateDir, PackageOptions{OutputDir: filepath.Join(t.TempDir(), "out")})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	entries := archiveEntrySet(t, result.ArchivePath)
	for _, excluded := range []string{"putnami.json", "putnami.features.json", "specs/template.json"} {
		if entries[excluded] {
			t.Errorf("archive contains workspace-side declaration %q", excluded)
		}
	}
	for _, included := range []string{ManifestFilename, "README.md", "LICENSE.md", "main.go.template"} {
		if !entries[included] {
			t.Errorf("archive omits template content %q; entries: %v", included, entries)
		}
	}
}

func TestPackage_ReadmeAndLicenseAreOptional(t *testing.T) {
	templateDir := makeTemplateDir(t, "minimal-tpl", "1.0.0")
	for _, name := range []string{"README.md", "LICENSE.md"} {
		if err := os.Remove(filepath.Join(templateDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Package(context.Background(), templateDir, PackageOptions{OutputDir: filepath.Join(t.TempDir(), "out")})
	if err != nil {
		t.Fatalf("Package without README/LICENSE: %v", err)
	}
	entries := archiveEntrySet(t, result.ArchivePath)
	if !entries[ManifestFilename] {
		t.Fatalf("archive omits required manifest; entries: %v", entries)
	}
}

func archiveEntrySet(t *testing.T, archive string) map[string]bool {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("open gzip stream: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := make(map[string]bool)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		name := strings.TrimPrefix(filepath.ToSlash(header.Name), "./")
		if name != "" && header.Typeflag != tar.TypeDir {
			entries[name] = true
		}
	}
	return entries
}

func TestStripPreRelease(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"1.2.3", "1.2.3"},
		{"1.2.3-beta.1", "1.2.3"},
		{"1.0.0-rc.1+build.7", "1.0.0"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripPreRelease(tt.in); got != tt.want {
			t.Errorf("stripPreRelease(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
