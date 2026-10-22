package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeSelectorKeyTree writes a Go project that reaches every branch of the
// go-embed expansion: Go sources, a test source, an embed directive that names
// a file and one that names a directory, a test-only embed, a .go link the
// source scan reads, and a .go link only the ordinary `**/*.go` glob selects.
func writeSelectorKeyTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range map[string]string{
		"go.mod":               "module example.com/app\n\ngo 1.26\n",
		"main.go":              "package main\n\nimport \"embed\"\n\n//go:embed static.txt\nvar static string\n\n//go:embed assets\nvar assets embed.FS\n\nfunc main() {}\n",
		"main_test.go":         "package main\n\nimport (\n\t_ \"embed\"\n\t\"testing\"\n)\n\n//go:embed testdata/fixture.txt\nvar fixture string\n\nfunc TestRun(t *testing.T) {}\n",
		"static.txt":           "static\n",
		"assets/a.txt":         "a\n",
		"assets/sub/b.txt":     "b\n",
		"assets/.hidden":       "hidden\n",
		"src/alias.txt":        "package main\n\nconst Alias = 1\n",
		"shared/linked.txt":    "package testdata\n",
		"testdata/fixture.txt": "fixture\n",
		"testdata/helper.go":   "package testdata\n",
		".gen/client.gen.go":   "package client\n",
		"README.md":            "not an input\n",
	} {
		gitInputWrite(t, root, rel, content)
	}
	for link, target := range map[string]string{
		"alias.go":           "src/alias.txt",
		"testdata/linked.go": "../shared/linked.txt",
	} {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
			t.Skipf("source-file symlinks unavailable: %v", err)
		}
	}
	return root
}

// relativeSlashPaths rebases absolute paths onto root as slash paths.
func relativeSlashPaths(t *testing.T, root string, paths []string) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// TestGoEmbedSelectorKeysArePinned pins, for the build and the test pattern
// sets of the Go extension's source input, the files the key reads, the subset
// it hashes as exact bytes, the file digest and the whole key.
//
// The file digest is sha256 over each file in path order: its slash path, NUL,
// the 32 bytes of sha256 of its content, NUL; a link contributes its
// referent's content. The key is sha256 over "v9", "@putnami/go", "", "", "",
// the task, "", "app", "", "", "" and the file digest, each followed by NUL.
// Both were recomputed outside Go with shasum. A change that moves one moves
// the key of every Go task, a whole-cache miss for every user, so it is made on
// purpose or not at all.
func TestGoEmbedSelectorKeysArePinned(t *testing.T) {
	buildFiles := []string{
		"alias.go", "assets/a.txt", "assets/sub/b.txt", "main.go", "shared/linked.txt",
		"src/alias.txt", "static.txt", "testdata/helper.go", "testdata/linked.go",
	}
	testFiles := []string{
		"alias.go", "assets/a.txt", "assets/sub/b.txt", "main.go", "main_test.go", "shared/linked.txt",
		"src/alias.txt", "static.txt", "testdata/fixture.txt", "testdata/helper.go", "testdata/linked.go",
	}
	withoutHelper := func(files []string) []string {
		return slices.DeleteFunc(slices.Clone(files), func(file string) bool { return file == "testdata/helper.go" })
	}
	for _, tc := range []struct {
		name     string
		task     string
		patterns []string
		files    []string
		digest   string
		key      string
	}{
		{
			name:     "build",
			task:     "build~compile",
			patterns: []string{"**/*.go", "!**/*_test.go", "go-embed:build", "!**/.gen/**"},
			files:    buildFiles,
			digest:   "de2493cfa500958b1cf028b685e7a2c98b0210a1144f04d124fb22c8767c8cf0",
			key:      "e22f071de14ad9d36783893fcbdcf7f03d17294852d67d11c016072c2a33f772",
		},
		{
			name:     "test",
			task:     "test~test",
			patterns: []string{"**/*.go", "go-embed:test", "!**/.gen/**"},
			files:    testFiles,
			digest:   "6e78c44c9f6be19d29bb4fac08348ec498df8cce292f9d959a32d6c15a26fc10",
			key:      "2f03900c9e17d39ffbe0c104d8c8af5db1ed27d06bb558db8731648493e56641",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeSelectorKeyTree(t)
			selection, err := CollectKeyFileSelection(root, tc.patterns, GoEmbedExpander())
			if err != nil {
				t.Fatal(err)
			}
			if got := relativeSlashPaths(t, root, selection.Files); !slices.Equal(got, tc.files) {
				t.Errorf("files = %q, want %q", got, tc.files)
			}
			// Only the regular .go file in a directory the source scan skips is
			// read through the ordinary glob alone.
			if got, want := relativeSlashPaths(t, root, selection.GoEmbed), withoutHelper(tc.files); !slices.Equal(got, want) {
				t.Errorf("exact-bytes files = %q, want %q", got, want)
			}
			digest, err := hashFiles(root, tc.patterns, GoEmbedExpander(), ProjectConfigScope{})
			if err != nil {
				t.Fatal(err)
			}
			if digest != tc.digest {
				t.Errorf("file digest = %s, want %s", digest, tc.digest)
			}
			key := &CacheKey{
				Extension:     "@putnami/go",
				Task:          tc.task,
				Project:       "app",
				ProjectRoot:   root,
				FilePatterns:  tc.patterns,
				InputExpander: GoEmbedExpander(),
			}
			got, err := key.ComputeHashUsing(NewCacheManager(nil))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.key {
				t.Errorf("key = %s, want %s", got, tc.key)
			}
		})
	}
}

// The go-embed expander answers with sorted, unique slash paths relative to
// the directory: the referent of a link the scan reads and the same link
// selected by a glob appear once.
func TestGoEmbedExpanderListsRelativeSortedFiles(t *testing.T) {
	root := writeSelectorKeyTree(t)
	expansion, err := GoEmbedExpander().ExpandInput(root, "go-embed:build", []string{"**/*.go", "!**/*_test.go", "!**/.gen/**"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"alias.go", "assets/a.txt", "assets/sub/b.txt", "main.go", "shared/linked.txt",
		"src/alias.txt", "static.txt", "testdata/linked.go",
	}
	if !slices.Equal(expansion.Files, want) {
		t.Fatalf("expansion = %q, want %q", expansion.Files, want)
	}
	if _, err := GoEmbedExpander().ExpandInput(root, "go-embed:unknown", nil); !errors.Is(err, ErrInputSelector) {
		t.Fatalf("unsupported selector = %v, want ErrInputSelector", err)
	}
}

// recordingExpander answers every selector with files and err, and records
// the directory and patterns each call receives.
type recordingExpander struct {
	files []string
	err   error
	calls []string
}

func (e *recordingExpander) ExpandInput(dir, selector string, patterns []string) (InputExpansion, error) {
	e.calls = append(e.calls, dir+"|"+selector+"|"+strings.Join(patterns, ","))
	return InputExpansion{Files: e.files}, e.err
}

// The collector asks the expander once per selector, with the other patterns
// of the set, and hashes every file it lists as exact bytes: a project config
// it lists keeps its tasks block, which a pattern alone reads without.
func TestCollectorReadsTheExpansionAsExactBytes(t *testing.T) {
	root := t.TempDir()
	gitInputWrite(t, root, "src/a.txt", "a\n")
	gitInputWrite(t, root, "src/skip.txt", "skip\n")
	config := filepath.Join(root, "putnami.json")
	gitInputWrite(t, root, "putnami.json", `{"name":"app","tasks":{"a":1}}`)
	expander := &recordingExpander{files: []string{"putnami.json"}}
	patterns := []string{"src/*.txt", "go-embed:build", "!src/skip.txt", "go-embed:test"}

	selection, err := CollectKeyFileSelection(root, patterns, expander)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		root + "|go-embed:build|src/*.txt,!src/skip.txt",
		root + "|go-embed:test|src/*.txt,!src/skip.txt",
	}
	if !slices.Equal(expander.calls, wantCalls) {
		t.Fatalf("expander calls = %q, want %q", expander.calls, wantCalls)
	}
	if got := relativeSlashPaths(t, root, selection.Files); !slices.Equal(got, []string{"putnami.json", "src/a.txt"}) {
		t.Fatalf("files = %q", got)
	}
	if !slices.Equal(selection.GoEmbed, []string{config}) {
		t.Fatalf("exact-bytes files = %q, want the listed config", selection.GoEmbed)
	}

	named := []string{"src/*.txt", "putnami.json"}
	digests := func() (listed, decoded string) {
		t.Helper()
		listed, err := hashFiles(root, patterns, expander, ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err = hashFiles(root, named, nil, ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		return listed, decoded
	}
	listedBefore, decodedBefore := digests()
	gitInputWrite(t, root, "putnami.json", `{"name":"app","tasks":{"a":2}}`)
	listedAfter, decodedAfter := digests()
	if decodedBefore != decodedAfter {
		t.Fatal("a pattern that names a project config read its tasks block")
	}
	if listedBefore == listedAfter {
		t.Fatal("a listed project config was read through its decoded view")
	}
}

// Every way an expansion can fail is an invalid input selection, so the
// scheduler fails the task before it restores or runs anything.
func TestFailedExpansionIsAnInvalidInputSelection(t *testing.T) {
	root := t.TempDir()
	gitInputWrite(t, root, "main.go", "package main\n")
	patterns := []string{"**/*.go", "go-embed:build"}
	for _, tc := range []struct {
		name     string
		expander InputExpander
		message  string
	}{
		{name: "no expander", expander: nil, message: "no expander"},
		{name: "a plain error", expander: &recordingExpander{err: errors.New("extension exited")}, message: "extension exited"},
		{name: "a listed file that is missing", expander: &recordingExpander{files: []string{"missing.txt"}}, message: "missing.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := hashFiles(root, patterns, tc.expander, ProjectConfigScope{}); !errors.Is(err, ErrInputSelector) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("hash error = %v, want ErrInputSelector naming %q", err, tc.message)
			}
			if _, err := CollectKeyFileSelection(root, patterns, tc.expander); !errors.Is(err, ErrInputSelector) {
				t.Fatalf("selection error = %v, want ErrInputSelector", err)
			}
		})
	}
	key := &CacheKey{ProjectRoot: root, FilePatterns: patterns}
	if _, err := key.ComputeHashUsing(NewCacheManager(nil)); !errors.Is(err, ErrInputSelector) {
		t.Fatalf("a key without an expander = %v, want ErrInputSelector", err)
	}
	if _, err := hashFiles(root, []string{"**/*.go"}, nil, ProjectConfigScope{}); err != nil {
		t.Fatalf("a pattern set without a selector needs no expander: %v", err)
	}
}
