package features

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	workspaceproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

func TestOSReaderReadsOnlyContainedBoundedFilesAndComputesBindings(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	projectRoot := filepath.Join(root, "project")
	evidenceRoot := filepath.Join(projectRoot, filepath.FromSlash(featureproto.EvidenceDirectory))
	if err := os.MkdirAll(evidenceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "reader",
		Features: []featureproto.Feature{{
			ID: "reader/contained", Type: featureproto.FeatureTypeFeature, Name: "Contained", Outcome: "Reads stay contained", Owner: "platform", Target: featureproto.MaturityModeled,
		}},
	}
	manifestData := mustFeatureManifest(t, manifest)
	if err := os.WriteFile(filepath.Join(projectRoot, featureproto.ManifestFilename), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceRoot, "empty.json"), []byte("{\n  \"protocolVersion\": 1,\n  \"evidence\": []\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reader, err := NewOSReader(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadFile("project", featureproto.ManifestFilename)
	if err != nil || !strings.Contains(string(got), "reader/contained") {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	entries, err := reader.ReadDir("project", featureproto.EvidenceDirectory)
	if err != nil || len(entries) != 1 || entries[0].Name != "empty.json" {
		t.Fatalf("ReadDir = %#v, %v", entries, err)
	}
	for _, sourceRoot := range []string{"", "project"} {
		binding, bindingErr := reader.SourceBinding(sourceRoot)
		if bindingErr != nil || !strings.HasPrefix(binding, "source-v1:sha256:") {
			t.Fatalf("SourceBinding(%q) = %q, %v", sourceRoot, binding, bindingErr)
		}
	}

	ws := workspace.NewWorkspace(root, &workspaceproto.Config{}, []*workspace.Project{{Name: "project", Path: "project"}})
	result := EvaluateWorkspace(ws, Revision{Kind: RevisionKindWorktree}, nil)
	if result.Snapshot == nil || len(result.Snapshot.Features) != 1 {
		t.Fatalf("EvaluateWorkspace = %#v", result)
	}

	if _, err := reader.ReadFile("project", "missing.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing ReadFile error = %v", err)
	}
	if _, err := reader.ReadFile("project", "schema"); kindOfReaderError(err) != ReaderErrorUnsupportedFile {
		t.Fatalf("directory ReadFile error = %v", err)
	}
	large := filepath.Join(projectRoot, "large.json")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxReadBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadFile("project", "large.json"); kindOfReaderError(err) != ReaderErrorReadLimit {
		t.Fatalf("large ReadFile error = %v", err)
	}
	if _, err := reader.SourceBinding("../outside"); kindOfReaderError(err) != ReaderErrorPathEscape {
		t.Fatalf("escaping SourceBinding error = %v", err)
	}
}

func TestReaderRejectsInvalidRootsAndPaths(t *testing.T) {
	if result := EvaluateWorkspace(nil, Revision{}, nil); result.Snapshot != nil || len(result.Diagnostics) != 1 {
		t.Fatalf("EvaluateWorkspace(nil) = %#v", result)
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOSReader(file); err == nil {
		t.Fatal("NewOSReader accepted a regular file")
	}
	if _, err := NewOSReader(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("NewOSReader accepted a missing root")
	}

	tests := []struct {
		value      string
		allowEmpty bool
		want       ReaderErrorKind
	}{
		{"", true, ""},
		{"", false, ReaderErrorInvalidPath},
		{".", false, ReaderErrorInvalidPath},
		{"a/../b", false, ReaderErrorPathEscape},
		{"../b", false, ReaderErrorPathEscape},
		{"/absolute", false, ReaderErrorPathEscape},
		{"C:/absolute", false, ReaderErrorPathEscape},
		{"a\\b", false, ReaderErrorPathEscape},
		{"a/\nb", false, ReaderErrorInvalidPath},
		{"a//b", false, ReaderErrorInvalidPath},
		{"a/./b", false, ReaderErrorInvalidPath},
		{strings.Repeat("a", 4097), false, ReaderErrorInvalidPath},
		{"a/b", false, ""},
	}
	for _, test := range tests {
		if got := validateRelativePath(test.value, test.allowEmpty); got != test.want {
			t.Errorf("validateRelativePath(%q, %v) = %q, want %q", test.value, test.allowEmpty, got, test.want)
		}
	}
	err := newReaderError(ReaderErrorInvalidPath, fs.ErrInvalid)
	if err.Error() != "feature reader: invalid-path" || !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("ReaderError = %q, unwrap=%v", err, errors.Is(err, fs.ErrInvalid))
	}
}

func kindOfReaderError(err error) ReaderErrorKind {
	kind, _ := readerErrorKind(err)
	return kind
}
