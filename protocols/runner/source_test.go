package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSourceManifestFixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "source-manifest", "digests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]string
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	for _, validity := range []string{"valid", "invalid"} {
		paths, err := filepath.Glob(filepath.Join("fixtures", "source-manifest", validity, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("fixture corpus: %v, %v", paths, err)
		}
		for _, path := range paths {
			t.Run(validity+"/"+filepath.Base(path), func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				manifest, err := ParseSourceManifest(data)
				if validity == "invalid" {
					if err == nil {
						t.Fatalf("invalid fixture accepted: %s", data)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				canonical, err := CanonicalSourceManifest(manifest)
				if err != nil {
					t.Fatal(err)
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, data); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(canonical, compact.Bytes()) {
					t.Fatalf("noncanonical fixture: %s", canonical)
				}
				digest, err := SourceDigest(manifest)
				if err != nil || digest != golden[filepath.Base(path)] {
					t.Fatalf("digest = %s, %v; want %s", digest, err, golden[filepath.Base(path)])
				}
			})
		}
	}
}

func TestSchemaTracksWireShapesAndBounds(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("schemas", "source-manifest-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties bool `json:"additionalProperties"`
		Properties           struct {
			Version struct {
				Const int `json:"const"`
			} `json:"version"`
			Entries struct {
				MaxItems int `json:"maxItems"`
				Items    struct {
					OneOf []struct {
						Required   []string                   `json:"required"`
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"oneOf"`
				} `json:"items"`
			} `json:"entries"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties || schema.Properties.Version.Const != SourceManifestVersion || schema.Properties.Entries.MaxItems != MaxSourceEntries {
		t.Fatal("schema version, strict fields or entry limit diverged")
	}
	shapes := schema.Properties.Entries.Items.OneOf
	if len(shapes) != 2 || strings.Join(shapes[0].Required, ",") != "path,kind,digest,size,mode" || strings.Join(shapes[1].Required, ",") != "path,kind,target" {
		t.Fatal("schema entry shapes diverged")
	}
	for index, shape := range shapes {
		var bound struct {
			Ref string `json:"$ref"`
		}
		if err := json.Unmarshal(shape.Properties["bound"], &bound); err != nil || bound.Ref != "#/definitions/bound" {
			t.Fatalf("schema shape %d lost the optional bound member: %v", index, err)
		}
	}
	var size struct {
		Maximum int64 `json:"maximum"`
	}
	if err := json.Unmarshal(shapes[0].Properties["size"], &size); err != nil || size.Maximum != MaxSourceFileBytes {
		t.Fatal("schema file size limit diverged")
	}
	var mode struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(shapes[0].Properties["mode"], &mode); err != nil || strings.Join(mode.Enum, ",") != "0644,0755" {
		t.Fatal("schema executable modes diverged")
	}
}

func sourceFile(name string) SourceEntry {
	return SourceEntry{Path: name, Kind: "file", Digest: BlobDigest(nil), Size: 0, Mode: "0644"}
}

func sourceManifest(entries ...SourceEntry) SourceManifest {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return SourceManifest{Version: SourceManifestVersion, Entries: entries}
}

func TestSourceIdentityBindsEveryContentProperty(t *testing.T) {
	original := sourceFile("file")
	originalDigest, err := SourceDigest(sourceManifest(original))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*SourceEntry){
		func(entry *SourceEntry) { entry.Path = "renamed" },
		func(entry *SourceEntry) { entry.Digest = BlobDigest([]byte("changed")) },
		func(entry *SourceEntry) { entry.Size = 1 },
		func(entry *SourceEntry) { entry.Mode = "0755" },
		func(entry *SourceEntry) { entry.Bound = true },
		func(entry *SourceEntry) { *entry = SourceEntry{Path: "file", Kind: "symlink", Target: "other"} },
		func(entry *SourceEntry) {
			*entry = SourceEntry{Path: "file", Kind: "symlink", Target: "other", Bound: true}
		},
	} {
		changed := original
		change(&changed)
		digest, err := SourceDigest(sourceManifest(changed))
		if err != nil || digest == originalDigest {
			t.Fatalf("content mutation did not change identity: %+v, %s, %v", changed, digest, err)
		}
	}
	a, _ := SourceDigest(sourceManifest(SourceEntry{Path: "link", Kind: "symlink", Target: "a"}))
	b, _ := SourceDigest(sourceManifest(SourceEntry{Path: "link", Kind: "symlink", Target: "b"}))
	if a == b {
		t.Fatal("symlink targets were not bound")
	}
	bound, _ := SourceDigest(sourceManifest(SourceEntry{Path: "link", Kind: "symlink", Target: "a", Bound: true}))
	if bound == a {
		t.Fatal("a bound symlink has the identity of an unbound one")
	}
	for _, context := range []GitContext{{}, {Head: strings.Repeat("a", 40), Branch: "main"}, {Head: strings.Repeat("b", 64), Branch: "feature/dirty", Dirty: true}} {
		if err := ValidateGitContext(context); err != nil {
			t.Fatal(err)
		}
		current, _ := SourceDigest(sourceManifest(original))
		if current != originalDigest {
			t.Fatal("source identity depends on Git context")
		}
	}
}

func TestSourceManifestBoundsAndPortablePaths(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a//b", "a/./b", "a/../b", "C:/file", "a\x00b", "a\nb", "file.", "file ", "NUL.txt", "COM1", "LPT0.txt", "a/.GIT/config", strings.Repeat("a", 256), strings.Repeat("a/", 64) + "a", strings.Repeat("a/", 513), string([]byte{0xff})} {
		t.Run(fmt.Sprintf("path_%q", name), func(t *testing.T) {
			if err := ValidateSourcePath(name); err == nil {
				t.Fatalf("unsafe path accepted before capture: %q", name)
			}
			if err := ValidateSourceManifest(sourceManifest(sourceFile(name))); err == nil {
				t.Fatalf("invalid portable path accepted: %q", name)
			}
		})
	}
	for _, size := range []int64{-1, MaxSourceFileBytes + 1} {
		entry := sourceFile("file")
		entry.Size = size
		if err := ValidateSourceManifest(sourceManifest(entry)); err == nil {
			t.Fatalf("invalid size accepted: %d", size)
		}
	}
	oversize := make([]SourceEntry, 17)
	for i := range oversize {
		oversize[i] = sourceFile(fmt.Sprintf("file-%02d", i))
		oversize[i].Size = MaxSourceFileBytes
	}
	if err := ValidateSourceManifest(sourceManifest(oversize...)); err == nil {
		t.Fatal("aggregate size limit not enforced")
	}
	if err := ValidateSourceManifest(SourceManifest{Version: 1, Entries: make([]SourceEntry, MaxSourceEntries+1)}); err == nil {
		t.Fatal("entry count limit not enforced")
	}
	if _, err := ParseSourceManifest(bytes.Repeat([]byte(" "), MaxManifestBytes+1)); err == nil {
		t.Fatal("manifest byte limit not enforced")
	}
	if _, err := CanonicalSourceManifest(SourceManifest{}); err == nil {
		t.Fatal("invalid in-memory manifest canonicalized")
	}
	if _, err := SourceDigest(SourceManifest{Version: 1}); err == nil {
		t.Fatal("nil entries admitted")
	}
	if _, err := json.Marshal(SourceEntry{}); err == nil {
		t.Fatal("invalid entry serialized")
	}
}

func FuzzParseSourceManifest(f *testing.F) {
	paths, err := filepath.Glob(filepath.Join("fixtures", "source-manifest", "*", "*.json"))
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		manifest, err := ParseSourceManifest(data)
		if err != nil {
			return
		}
		canonical, err := CanonicalSourceManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
		reparsed, err := ParseSourceManifest(canonical)
		if err != nil {
			t.Fatal(err)
		}
		again, err := CanonicalSourceManifest(reparsed)
		if err != nil || !bytes.Equal(canonical, again) {
			t.Fatalf("canonicalization is not idempotent: %v", err)
		}
	})
}

func TestSymlinkResolution(t *testing.T) {
	cases := []struct {
		name    string
		entries []SourceEntry
		valid   bool
	}{
		{"dangling internal", []SourceEntry{{Path: "link", Kind: "symlink", Target: "missing"}}, true},
		{"safe chain", []SourceEntry{{Path: "a", Kind: "symlink", Target: "dir/.."}, {Path: "b", Kind: "symlink", Target: "a/file"}}, true},
		{"regular ancestor", []SourceEntry{sourceFile("file"), {Path: "link", Kind: "symlink", Target: "file/child"}}, false},
		{"case alias", []SourceEntry{sourceFile("FILE"), {Path: "link", Kind: "symlink", Target: "file"}}, false},
		{"absolute target", []SourceEntry{{Path: "link", Kind: "symlink", Target: "/tmp"}}, false},
		{"empty component", []SourceEntry{{Path: "link", Kind: "symlink", Target: "a//b"}}, false},
		{"file fields", []SourceEntry{{Path: "link", Kind: "symlink", Target: "a", Mode: "0644"}}, false},
		{"unknown kind", []SourceEntry{{Path: "link", Kind: "directory"}}, false},
		{"case ancestor", []SourceEntry{sourceFile("A"), sourceFile("a/b")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSourceManifest(sourceManifest(tc.entries...))
			if (err == nil) != tc.valid {
				t.Fatalf("validation = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestStrictJSONAndGitContext(t *testing.T) {
	for _, raw := range []string{"", "[]", "null", "{", `{"version":"1","entries":[]}`, `{"version":1,"entries":{}}`, `{"version":1,"entries":[false]}`, `{"version":1,"entries":[]} trailing`, strings.Repeat("[", 10) + "0" + strings.Repeat("]", 10), string([]byte{0xff})} {
		if _, err := ParseSourceManifest([]byte(raw)); err == nil {
			t.Fatalf("malformed JSON accepted: %s", raw)
		}
	}
	for _, raw := range []string{
		`{"head":"","branch":"","dirty":false}`,
		`{"head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","branch":"feature/test","dirty":true}`,
	} {
		if _, err := ParseGitContext([]byte(raw)); err != nil {
			t.Fatalf("valid context rejected: %v", err)
		}
	}
	for _, raw := range []string{`null`, `{"head":"","branch":"main","dirty":false,"remote":"https://token@example.test"}`, `{"head":"","branch":"main","dirty":"false"}`, `{"head":"BAD","branch":"main","dirty":false}`} {
		if _, err := ParseGitContext([]byte(raw)); err == nil {
			t.Fatalf("invalid context accepted: %s", raw)
		}
	}
	for _, branch := range []string{"-bad", "a..b", "@", "a@{b", "a//b", ".bad", "a.lock", "bad.", "a b", "https://token@example.test", "a\x00b", strings.Repeat("a", 1025)} {
		if err := ValidateGitContext(GitContext{Branch: branch}); err == nil {
			t.Fatalf("invalid branch accepted: %q", branch)
		}
	}
}
