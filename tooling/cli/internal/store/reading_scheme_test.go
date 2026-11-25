package store

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// TestTheReadingsHoldTheirReadingScheme pins the digest of each reading the
// file index records under a variant name. A change that moves one of them
// raises fileindex.ReadingScheme and records the new digests here under it:
// an index written by a CLI that read files the old way must not answer a CLI
// that reads them the new way.
func TestTheReadingsHoldTheirReadingScheme(t *testing.T) {
	golden := map[string]map[string]string{
		"1": {
			"raw":            "df1d036cbbf3df46e2045071e082245ece204c7f53ecf0a4e022bff9bb228f47",
			"version-stamp":  "f40b5d64f7c7a4ab43f72bac9d999fee7607c061c578d02c5bb8669247a27d37",
			"project-config": "2bb99ae32945ad4f5057560acc9d04abf5f3b09e0e1476007782e7a932bfa13c",
			"git-file":       "ab91a96dc29855ba3ef2f2dd8973760e156026f698494a9382b58c45ea7a3ae8",
		},
	}
	want, ok := golden[fileindex.ReadingScheme]
	if !ok {
		t.Fatalf("no golden digests are recorded for reading scheme %q", fileindex.ReadingScheme)
	}

	dir := t.TempDir()
	write := func(rel, content string) string {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	raw := write("src/main.go", "package main\n")
	stamp := write("app/.gen/version.json", `{"name":"app","sha":"abc","buildTime":"now","contentHash":"h"}`)
	config := write("app/putnami.json", `{"name":"app","tasks":{"x":{}},"options":{"@a/ext":{"k":1},"other":{"k":2}}}`)
	info, err := os.Lstat(raw)
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 32*1024)
	got := map[string]string{
		"raw":            hex.EncodeToString(fileContentDigest(raw, buf, ProjectConfigScope{})),
		"version-stamp":  hex.EncodeToString(fileContentDigest(stamp, buf, ProjectConfigScope{})),
		"project-config": hex.EncodeToString(fileContentDigest(config, buf, ProjectConfigScope{ExtensionLayers: []string{"@a/ext"}, ForeignNamespaces: []string{"other"}})),
		"git-file":       hex.EncodeToString(gitCandidateDigest(fileEntry{path: raw, info: info, gitCandidate: true, indexMode: "100644"}, buf, true)),
	}
	for reading, digest := range got {
		if digest != want[reading] {
			t.Errorf("reading %s under scheme %s = %s, want %s: raise fileindex.ReadingScheme and record the new digests", reading, fileindex.ReadingScheme, digest, want[reading])
		}
	}
}
