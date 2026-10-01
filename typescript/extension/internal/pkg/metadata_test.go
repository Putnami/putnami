package pkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

func readChannelRecord(t *testing.T, outputDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outputDir, pkgmeta.ChannelRecordFile))
	if err != nil {
		t.Fatalf("failed to read channel record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("failed to parse channel record: %v", err)
	}
	return record
}

func TestWritePackageMetadata_WritesInsideTheOwnedDirectory(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "package-channel-index", "a-channel-is-recorded-inside-the-directory-its-packager-owns")
	dir := t.TempDir()
	npmDir := pkgmeta.PackageOutputDir(dir, "test-project", "npm")

	if err := WritePackageMetadata(npmDir, "npm", "1.0.0", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	record := readChannelRecord(t, npmDir)
	if record["version"] != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %v", record["version"])
	}
	if record["stable"] != true {
		t.Errorf("expected stable true, got %v", record["stable"])
	}
	channels, ok := record["channels"].([]any)
	if !ok || len(channels) != 1 || channels[0] != "npm" {
		t.Errorf("expected channels [npm], got %v", record["channels"])
	}

	// Nothing is written at the package root: the record travels with the
	// package directory, which is what makes the step cache-restorable.
	packageDir := filepath.Dir(npmDir)
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "npm" {
			t.Errorf("npm packaging wrote %q outside the directory it owns", entry.Name())
		}
	}
}

// The two TypeScript packagers own two directories. Neither reads nor rewrites
// the other's record, so the derived index reports both whichever order they
// ran in — no merge, and nothing for a cache hit to skip.
func TestWritePackageMetadata_SiblingPackagersRecordIndependently(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "package-channel-index", "sibling-packagers-record-independently-and-the-index-reports-both")
	dir := t.TempDir()

	if err := WritePackageMetadata(pkgmeta.PackageOutputDir(dir, "test-project", "npm"), "npm", "1.0.0", false); err != nil {
		t.Fatalf("npm record failed: %v", err)
	}
	if err := WritePackageMetadata(pkgmeta.PackageOutputDir(dir, "test-project", "docker"), "docker", "1.0.0", false); err != nil {
		t.Fatalf("docker record failed: %v", err)
	}

	index, err := pkgmeta.ReadChannelIndex(dir, "test-project")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	if !index.HasChannel("npm") || !index.HasChannel("docker") {
		t.Errorf("expected both channels, got %v", index.Channels)
	}
}

func TestWritePackageMetadata_ARepackagedChannelIsNotRecordedTwice(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "package-channel-index", "a-channel-is-never-recorded-twice")
	dir := t.TempDir()
	npmDir := pkgmeta.PackageOutputDir(dir, "test-project", "npm")

	if err := WritePackageMetadata(npmDir, "npm", "1.0.0", false); err != nil {
		t.Fatalf("first write failed: %v", err)
	}
	if err := WritePackageMetadata(npmDir, "npm", "1.0.1", false); err != nil {
		t.Fatalf("second write failed: %v", err)
	}

	index, err := pkgmeta.ReadChannelIndex(dir, "test-project")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	if len(index.Channels) != 1 || index.Channels[0] != "npm" {
		t.Errorf("expected 1 channel (no duplicates), got %v", index.Channels)
	}
	if record := readChannelRecord(t, npmDir); record["version"] != "1.0.1" {
		t.Errorf("expected version 1.0.1 (updated), got %v", record["version"])
	}
}
