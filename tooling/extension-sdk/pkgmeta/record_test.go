package pkgmeta

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A packager writes its channel record INSIDE the directory it owns, and the
// index publishers read is derived over those records. These tests pin the two
// halves that replaced the shared merge point: a record cannot reach
// outside its owner's directory, and the derived index reports every record
// without any writer cooperating with another.

func readRecord(t *testing.T, outputDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outputDir, ChannelRecordFile))
	if err != nil {
		t.Fatalf("read channel record: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse channel record: %v", err)
	}
	return document
}

func writeRecordOrFail(t *testing.T, root, channel string, record ChannelRecord) string {
	t.Helper()
	outputDir := PackageOutputDir(root, "app", channel)
	if err := WriteChannelRecord(outputDir, record); err != nil {
		t.Fatalf("WriteChannelRecord(%s): %v", channel, err)
	}
	return outputDir
}

func TestWriteChannelRecordWritesInsideTheOwnedDirectory(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "shared-package-metadata-writer", "a-record-is-written-inside-the-directory-its-packager-owns")
	root := t.TempDir()

	outputDir := writeRecordOrFail(t, root, "npm", ChannelRecord{
		Version: "1.2.3", Artifact: "app", Stable: true, Channels: []string{"npm"},
	})

	document := readRecord(t, outputDir)
	if document["version"] != "1.2.3" || document["artifact"] != "app" || document["stable"] != true {
		t.Fatalf("identity = %v", document)
	}
	channels, ok := document["channels"].([]any)
	if !ok || len(channels) != 1 || channels[0] != "npm" {
		t.Fatalf("channels = %v, want [npm]", document["channels"])
	}
	// Nothing lands outside the owned directory: that is what makes the record
	// restorable by the same declared output as the artifact it describes.
	entries, err := os.ReadDir(filepath.Dir(outputDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "npm" {
			t.Errorf("packaging wrote %q outside its owned directory", entry.Name())
		}
	}
}

// Two packagers of one project write two records in two directories. Neither
// reads nor rewrites the other's, so the second cannot erase the first — the
// property the shared index needed a read-merge-write to approximate.
func TestReadChannelIndexUnionsRecordsInSortedOrder(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "shared-package-metadata-writer", "the-index-unions-every-owned-record-in-sorted-directory-order")
	root := t.TempDir()

	writeRecordOrFail(t, root, "npm", ChannelRecord{Version: "1.2.3", Channels: []string{"npm"}})
	writeRecordOrFail(t, root, "archives", ChannelRecord{Version: "1.2.3", Channels: []string{"archives", "legacy"}})
	writeRecordOrFail(t, root, "docker", ChannelRecord{Version: "1.2.3", Channels: []string{"docker"}})

	index, err := ReadChannelIndex(root, "app")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	want := []string{"archives", "legacy", "docker", "npm"}
	if len(index.Channels) != len(want) {
		t.Fatalf("channels = %v, want %v", index.Channels, want)
	}
	for i := range want {
		if index.Channels[i] != want[i] {
			t.Fatalf("channels = %v, want %v", index.Channels, want)
		}
	}
	for _, channel := range want {
		if !index.HasChannel(channel) {
			t.Errorf("HasChannel(%q) = false", channel)
		}
	}
	if len(index.Records) != 3 || index.Records["npm"].Version != "1.2.3" {
		t.Fatalf("records = %+v", index.Records)
	}
}

// A record a sibling left behind is still reported: the artifact it describes is
// still on disk, so the index answers what packaging produced, not what this
// invocation rebuilt.
func TestReadChannelIndexReportsARecordThisRunDidNotRewrite(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "shared-package-metadata-writer", "a-record-left-by-an-earlier-run-is-still-reported")
	root := t.TempDir()
	writeRecordOrFail(t, root, "archives", ChannelRecord{Version: "1.0.0", Channels: []string{"archives"}})

	// A second packager runs now and states only its own channel.
	writeRecordOrFail(t, root, "go", ChannelRecord{Version: "2.0.0", Channels: []string{"go"}})

	index, err := ReadChannelIndex(root, "app")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	if !index.HasChannel("archives") || !index.HasChannel("go") {
		t.Fatalf("channels = %v, want both records reported", index.Channels)
	}
	if index.Records["archives"].Version != "1.0.0" || index.Records["go"].Version != "2.0.0" {
		t.Fatalf("each record keeps its own version: %+v", index.Records)
	}
}

func TestReadChannelIndexDropsEmptyAndDuplicateChannels(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "shared-package-metadata-writer", "empty-and-duplicate-channel-names-are-dropped")
	root := t.TempDir()
	writeRecordOrFail(t, root, "npm", ChannelRecord{Channels: []string{"npm", "", "npm"}})
	writeRecordOrFail(t, root, "docker", ChannelRecord{Channels: []string{"docker", "npm"}})

	index, err := ReadChannelIndex(root, "app")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	want := []string{"docker", "npm"}
	if len(index.Channels) != len(want) || index.Channels[0] != want[0] || index.Channels[1] != want[1] {
		t.Fatalf("channels = %v, want %v", index.Channels, want)
	}
}

// A damaged record must not fail the read: the index is derived from what is on
// disk, and a readable sibling still answers for its own channel.
func TestReadChannelIndexSkipsADamagedRecord(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "shared-package-metadata-writer", "a-damaged-record-is-skipped-rather-than-failing-the-read")
	root := t.TempDir()
	damaged := PackageOutputDir(root, "app", "docker")
	if err := os.MkdirAll(damaged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(damaged, ChannelRecordFile), []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRecordOrFail(t, root, "npm", ChannelRecord{Channels: []string{"npm"}})

	index, err := ReadChannelIndex(root, "app")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	if len(index.Channels) != 1 || index.Channels[0] != "npm" {
		t.Fatalf("channels = %v, want [npm]", index.Channels)
	}
	if _, recorded := index.Records["docker"]; recorded {
		t.Error("a damaged record contributed to the index")
	}
}

// "Run package first" is the answer publish depends on, and it must be
// recognizable as such rather than as an arbitrary read failure.
func TestReadChannelIndexErrorsWhenNoRecordExists(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "shared-package-metadata-writer", "an-absent-record-set-reports-not-exist")
	root := t.TempDir()

	_, err := ReadChannelIndex(root, "app")
	if err == nil {
		t.Fatal("ReadChannelIndex with no package output = nil, want an error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want it to wrap fs.ErrNotExist", err)
	}

	// A package directory that exists but holds no record is the same answer:
	// a dry run or a skipped packager must not read as "packaged".
	if err := os.MkdirAll(PackageOutputDir(root, "app", "archives"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadChannelIndex(root, "app"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want it to wrap fs.ErrNotExist", err)
	}
}

func TestWriteChannelRecordReportsAnUnwritableDirectory(t *testing.T) {
	root := t.TempDir()
	outputDir := PackageOutputDir(root, "app", "npm")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	denyWrites(t, outputDir)

	if err := WriteChannelRecord(outputDir, ChannelRecord{Channels: []string{"npm"}}); err == nil {
		t.Fatal("WriteChannelRecord into a read-only directory = nil, want error")
	}
}

// The archive publication manifest has ONE writer that states the whole
// document, so a second run replaces it rather than merging into it.
func TestWritePackageMetadataOverwritesTheArchiveManifest(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "shared-package-metadata-writer", "the-manifest-is-overwritten-by-its-single-owner")
	root := t.TempDir()

	if err := WritePackageMetadata(root, "app", PackageMetadata{
		Version: "1.0.0", Artifact: "app", Channels: []string{"archives"},
	}); err != nil {
		t.Fatalf("WritePackageMetadata: %v", err)
	}
	if err := WritePackageMetadata(root, "app", PackageMetadata{
		Version: "2.0.0", Artifact: "app", Stable: true, Channels: []string{"template-archives"}, Template: true,
	}); err != nil {
		t.Fatalf("WritePackageMetadata: %v", err)
	}

	meta, err := ReadPackageMetadata(root, "app")
	if err != nil {
		t.Fatalf("ReadPackageMetadata: %v", err)
	}
	if meta.Version != "2.0.0" || !meta.Stable || !meta.Template {
		t.Fatalf("manifest = %+v, want the second run's whole document", meta)
	}
	if len(meta.Channels) != 1 || meta.Channels[0] != "template-archives" {
		t.Fatalf("channels = %v, want only this run's channel", meta.Channels)
	}
	if meta.HasChannel("archives") {
		t.Error("the manifest merged a previous run's channel; it has a single owner and states the whole document")
	}
}
