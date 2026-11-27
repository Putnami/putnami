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
			"raw":                       "df1d036cbbf3df46e2045071e082245ece204c7f53ecf0a4e022bff9bb228f47",
			"version-stamp":             "f40b5d64f7c7a4ab43f72bac9d999fee7607c061c578d02c5bb8669247a27d37",
			"project-config":            "2bb99ae32945ad4f5057560acc9d04abf5f3b09e0e1476007782e7a932bfa13c",
			"git-file":                  "ab91a96dc29855ba3ef2f2dd8973760e156026f698494a9382b58c45ea7a3ae8",
			"version-stamp-every-field": "747cff34ba85d9583b2fd33ce14c1ce3222435fc72a77c52778a0e103e1804a0",
			"project-config-verbatim":   "531056ef1a608fb1e0152ec905781d6c56a6eb76eae78fc2fa7e906113b2e406",
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
	// Every field the tree digest drops, beside the fields it keeps: removing
	// a field from the drop list, or adding one this fixture holds, moves
	// this digest.
	everyField := write("lib/.gen/version.json", `{"name":"lib","capabilityRoot":"r","capabilityPackages":["p"],"contentHash":"h",`+
		`"merged":"m","buildTime":"now","sha":"abc","suffix":"s","version":"1.0.0","branch":"main","isDirty":true,`+
		`"image":"i","image_digest":"d","publish":{"k":"v"}}`)
	info, err := os.Lstat(raw)
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 32*1024)
	got := map[string]string{
		"raw":                       hex.EncodeToString(fileContentDigest(raw, buf, ProjectConfigScope{})),
		"version-stamp":             hex.EncodeToString(fileContentDigest(stamp, buf, ProjectConfigScope{})),
		"project-config":            hex.EncodeToString(fileContentDigest(config, buf, ProjectConfigScope{ExtensionLayers: []string{"@a/ext"}, ForeignNamespaces: []string{"other"}})),
		"git-file":                  hex.EncodeToString(gitCandidateDigest(fileEntry{path: raw, info: info, gitCandidate: true, indexMode: "100644"}, buf, true)),
		"version-stamp-every-field": hex.EncodeToString(fileContentDigest(everyField, buf, ProjectConfigScope{})),
		"project-config-verbatim":   hex.EncodeToString(fileContentDigest(config, buf, ProjectConfigScope{Verbatim: true, ExtensionLayers: []string{"@a/ext"}})),
	}
	for reading, digest := range got {
		if digest != want[reading] {
			t.Errorf("reading %s under scheme %s = %s, want %s: raise fileindex.ReadingScheme and record the new digests", reading, fileindex.ReadingScheme, digest, want[reading])
		}
	}
}

// TestTheReadingVariantsAreDistinct pins the variant name each reading records
// its digests under in the file index. Two readings that share a variant name
// would answer each other's digests, so a change that drops a scope's tag, such
// as the verbatim one, or maps one reading onto another fails here.
func TestTheReadingVariantsAreDistinct(t *testing.T) {
	want := map[string]string{
		"raw":                     fileindex.RawContent,
		"project-config":          "project-config\x00\x00layer\x00@a/ext\x00foreign\x00other",
		"project-config-verbatim": "project-config\x00verbatim\x00layer\x00@a/ext",
		"project-config-bare":     "project-config\x00",
		"version-stamp":           "version-stamp\x00tree",
		"git-file":                "git-file\x00100644",
		"git-file-executable":     "git-file\x00100755",
	}
	got := map[string]string{
		"raw":                     rawContentVariant,
		"project-config":          projectConfigVariant(ProjectConfigScope{ExtensionLayers: []string{"@a/ext"}, ForeignNamespaces: []string{"other"}}),
		"project-config-verbatim": projectConfigVariant(ProjectConfigScope{Verbatim: true, ExtensionLayers: []string{"@a/ext"}}),
		"project-config-bare":     projectConfigVariant(ProjectConfigScope{}),
		"version-stamp":           versionStampTreeVariant,
		"git-file":                gitFileVariantPrefix + "100644",
		"git-file-executable":     gitFileVariantPrefix + "100755",
	}
	seen := map[string]string{}
	for reading, variant := range got {
		if variant != want[reading] {
			t.Errorf("the variant of reading %s = %q, want %q", reading, variant, want[reading])
		}
		if other, ok := seen[variant]; ok {
			t.Errorf("readings %s and %s share the variant %q", reading, other, variant)
		}
		seen[variant] = reading
	}
	if projectConfigVariant(ProjectConfigScope{Verbatim: true}) == projectConfigVariant(ProjectConfigScope{}) {
		t.Error("a verbatim project config shares the variant of a decoded one")
	}
}
