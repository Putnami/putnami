package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

func TestHashParams_Empty(t *testing.T) {
	if got := hashParams(nil); got != "" {
		t.Errorf("hashParams(nil) = %q, want empty", got)
	}
	if got := hashParams(map[string]any{}); got != "" {
		t.Errorf("hashParams({}) = %q, want empty", got)
	}
}

func TestHashParams_HashesEveryProjectedParam(t *testing.T) {
	params := map[string]any{"output": "semantic-task-format"}
	if got := hashParams(params); got == "" {
		t.Error("hashParams(projected params) = empty, want semantic input hash")
	}
}

func TestHashParams_ProjectedParamIncluded(t *testing.T) {
	params := map[string]any{
		"target": "linux/amd64",
	}
	got := hashParams(params)
	if got == "" {
		t.Error("hashParams should include projected param 'target'")
	}
}

func TestHashParams_Deterministic(t *testing.T) {
	params := map[string]any{
		"target": "linux/amd64",
		"format": "binary",
	}
	h1 := hashParams(params)
	h2 := hashParams(params)
	if h1 != h2 {
		t.Errorf("hashParams should be deterministic: %q != %q", h1, h2)
	}
}

func TestHashParams_OrderIndependent(t *testing.T) {
	// Different insertion order should produce same hash (keys are sorted)
	p1 := map[string]any{"a": "1", "b": "2", "c": "3"}
	p2 := map[string]any{"c": "3", "a": "1", "b": "2"}
	if hashParams(p1) != hashParams(p2) {
		t.Error("hashParams should be order-independent")
	}
}

func TestHashParams_DifferentValues(t *testing.T) {
	p1 := map[string]any{"target": "linux/amd64"}
	p2 := map[string]any{"target": "darwin/arm64"}
	if hashParams(p1) == hashParams(p2) {
		t.Error("different values should produce different hashes")
	}
}

func TestHashEnvVars_Deterministic(t *testing.T) {
	t.Setenv("TEST_HASH_A", "valueA")
	t.Setenv("TEST_HASH_B", "valueB")

	h1 := hashEnvVars([]string{"TEST_HASH_A", "TEST_HASH_B"})
	h2 := hashEnvVars([]string{"TEST_HASH_B", "TEST_HASH_A"}) // reversed order
	if h1 != h2 {
		t.Error("hashEnvVars should be order-independent (sorted internally)")
	}
}

func TestHashEnvVars_ChangesWithValue(t *testing.T) {
	t.Setenv("TEST_HASH_X", "v1")
	h1 := hashEnvVars([]string{"TEST_HASH_X"})

	t.Setenv("TEST_HASH_X", "v2")
	h2 := hashEnvVars([]string{"TEST_HASH_X"})

	if h1 == h2 {
		t.Error("hashEnvVars should change when env var value changes")
	}
}

func TestCollectFiles_EmptyPatterns(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b"), 0o644)

	files, err := collectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d", len(files))
	}
}

func TestCollectFiles_SkipsHidden(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "visible.go"), []byte("ok"), 0o644)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("skip"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref"), 0o644)

	files, err := collectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 visible file, got %d", len(files))
	}
}

// A project whose own directory carries a skipped name, such as tools/dist,
// keys on its files: the walk skips such a directory only below its root, so
// a `**` pattern never yields an empty key for the whole project.
func TestCollectFiles_ARootNamedLikeASkippedDirectoryIsWalked(t *testing.T) {
	for _, name := range []string{"dist", "out", "vendor", "node_modules"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(filepath.Join(dir, "pkg", "dist"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "pkg", "api.go"), []byte("package pkg"), 0o644)
		os.WriteFile(filepath.Join(dir, "pkg", "dist", "built.go"), []byte("package dist"), 0o644)

		files, err := collectFiles(dir, []string{"**/*.go"})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Errorf("%s: expected the project's own file and nothing below its dist, got %d", name, len(files))
		}
	}
}

func TestCollectFiles_ARootNamedLikeASkippedDirectoryKeysWithNoPattern(t *testing.T) {
	for _, name := range []string{"out", "node_modules", ".putnami", ".tool"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(filepath.Join(dir, "pkg", "out"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "pkg", "api.go"), []byte("package pkg"), 0o644)
		os.WriteFile(filepath.Join(dir, "pkg", "out", "built.go"), []byte("package out"), 0o644)

		files, err := collectFiles(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Errorf("%s: expected the project's own file and nothing below its out, got %d", name, len(files))
		}
	}
}

func TestCollectFiles_SkipsNodeModules(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "src.ts"), []byte("ok"), 0o644)
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("skip"), 0o644)

	files, err := collectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 file (not node_modules), got %d", len(files))
	}
}

func TestCollectFiles_WithPatterns(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("go"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.ts"), []byte("ts"), 0o644)

	files, err := collectFiles(dir, []string{"*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 .go file, got %d", len(files))
	}
}

func TestCollectFiles_DoubleStarRecurses(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("go"), 0o644)
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "foo.go"), []byte("go"), 0o644)
	os.WriteFile(filepath.Join(dir, "a", "b", "deep.go"), []byte("go"), 0o644)
	os.WriteFile(filepath.Join(dir, "a", "note.txt"), []byte("txt"), 0o644)

	files, err := collectFiles(dir, []string{"**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	// Recursive: root, one level, and two levels deep — but not the .txt.
	if len(files) != 3 {
		var names []string
		for _, f := range files {
			rel, _ := filepath.Rel(dir, f.path)
			names = append(names, rel)
		}
		t.Errorf("expected 3 .go files recursively, got %d: %v", len(files), names)
	}
}

func TestCollectFiles_ExcludePattern(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("go"), 0o644)
	os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("go"), 0o644)
	os.WriteFile(filepath.Join(dir, "pkg", "lib.go"), []byte("go"), 0o644)
	os.WriteFile(filepath.Join(dir, "pkg", "lib_test.go"), []byte("go"), 0o644)

	files, err := collectFiles(dir, []string{"**/*.go", "!**/*_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.path, "_test.go") {
			t.Errorf("test file should have been excluded: %s", f.path)
		}
	}
	if len(files) != 2 {
		t.Errorf("expected 2 non-test .go files, got %d", len(files))
	}
}

func TestCollectFiles_DoubleStarMatchesPathSuffix(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg", "generated"), 0o755)
	os.MkdirAll(filepath.Join(dir, "pkg", "manual"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "generated", "types.go"), []byte("go"), 0o644)
	os.WriteFile(filepath.Join(dir, "pkg", "manual", "types.go"), []byte("go"), 0o644)

	files, err := collectFiles(dir, []string{"**/generated/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one generated file, got %d", len(files))
	}
	if rel, _ := filepath.Rel(dir, files[0].path); filepath.ToSlash(rel) != "pkg/generated/types.go" {
		t.Fatalf("matched %q, want pkg/generated/types.go", rel)
	}
}

func TestHashFiles_ExcludedTestFileDoesNotChangeHash(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	testFile := filepath.Join(dir, "main_test.go")
	os.WriteFile(testFile, []byte("package main // v1"), 0o644)

	patterns := []string{"**/*.go", "!**/*_test.go"}
	h1, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}

	// Editing an excluded test file must not change the hash.
	os.WriteFile(testFile, []byte("package main // v2 changed"), 0o644)
	h2, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("editing an excluded *_test.go file should not change the hash")
	}

	// Editing a non-test source file must change the hash.
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // changed"), 0o644)
	h3, _ := hashFiles(dir, patterns, ProjectConfigScope{})
	if h1 == h3 {
		t.Error("editing a non-test source file should change the hash")
	}
}

func TestCollectFiles_Deduplicate(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("go"), 0o644)

	// Overlapping patterns matching same file
	files, err := collectFiles(dir, []string{"*.go", "main.*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("expected 1 file after dedup, got %d", len(files))
	}
}

// TestSelectsPath pins that the read-only projection answers for the same
// paths collectFiles selects, including the "../" form a pattern reaching
// outside the project root produces. A caller uses it to prove a file is a
// cache-key input, so a false positive would certify a hole rather than close
// one.
func TestSelectsPath(t *testing.T) {
	patterns := []string{
		"doc/**", "internal/cli/testdata/**", "**/*.go", "!**/*_test.go",
		"putnami.json", "../../**/putnami.json", "../../LICENSE.md", "../../.github/**",
	}
	for _, testCase := range []struct {
		rel  string
		want bool
	}{
		{"doc/reports/release-rehearsal.md", true},
		{"internal/cli/testdata/release-plan.json", true},
		{"internal/cli/app.go", true},
		{"internal/cli/app_test.go", false},
		{"putnami.json", true},
		{"../../typescript/framework/utils/putnami.json", true},
		{"../../LICENSE.md", true},
		{"../../GOVERNANCE.md", false},
		{"../../.github/workflows/ci.yml", true},
		{"scripts/install.sh", false},
		{"internal/cli/testdata", true},
	} {
		if got := SelectsPath(testCase.rel, patterns); got != testCase.want {
			t.Errorf("SelectsPath(%q) = %v, want %v", testCase.rel, got, testCase.want)
		}
	}
}

func TestHasMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gen", "version.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	matched, err := HasMatchingFiles(dir, []string{"**/*.ts", "**/*.json"})
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("TypeScript patterns unexpectedly matched Go-only project through hidden generated metadata")
	}

	matched, err = HasMatchingFiles(dir, []string{"**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("Go pattern did not match Go project")
	}
}

func TestGoEmbedBatchProbeDefersSemanticReadToKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nimport _ \"embed\"\n//go:embed missing.txt\nvar text string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patterns := []string{"**/*.go", "go-embed:build"}
	if matched, err := HasMatchingFiles(dir, patterns); err != nil || !matched {
		t.Fatalf("ordinary Go input should admit a live batch candidate: matched=%v err=%v", matched, err)
	}
	if _, err := hashFiles(dir, patterns, ProjectConfigScope{}); !errors.Is(err, ErrGoEmbedInput) {
		t.Fatalf("actual key must reject missing embedded input: %v", err)
	}
	if matched, err := HasMatchingFiles(dir, []string{"go-embed:build"}); !errors.Is(err, ErrGoEmbedInput) || matched {
		t.Fatalf("selector-only batch must resolve inputs: matched=%v err=%v", matched, err)
	}
	if matched, err := HasMatchingFiles(dir, []string{"**/*.go", "go-embed:unknown"}); !errors.Is(err, ErrGoEmbedInput) || matched {
		t.Fatalf("unsupported selector must fail before ordinary probe: matched=%v err=%v", matched, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "missing.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "missing.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil || first == second {
		t.Fatalf("embedded bytes must still change key: first=%q second=%q err=%v", first, second, err)
	}
}

func TestMixedGitGoInputRequiresAvailableCandidateInventory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patterns := []string{"git:**", "**/*.go", "go-embed:build"}
	if _, err := hashFiles(dir, patterns, ProjectConfigScope{}); !errors.Is(err, ErrGoEmbedInput) || !strings.Contains(err.Error(), "go input selection unavailable") {
		t.Fatalf("mixed selection with unknown Git candidates must fail closed: %v", err)
	}
	if matched, err := HasMatchingFiles(dir, patterns); !errors.Is(err, ErrGoEmbedInput) || matched {
		t.Fatalf("mixed batch selection with unknown Git candidates: matched=%v err=%v", matched, err)
	}
	if _, err := hashFiles(dir, []string{"git:**"}, ProjectConfigScope{}); err == nil || errors.Is(err, ErrGoEmbedInput) {
		t.Fatalf("Git-only error must keep its generic category: %v", err)
	}
}

func TestMixedGitGoInputWithAvailableCandidatesHashesPayload(t *testing.T) {
	dir := gitInputPhysicalRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nimport _ \"embed\"\n//go:embed payload.txt\nvar payload string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(asset, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	patterns := []string{"git:**", "**/*.go", "go-embed:build"}
	first, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil || first == second {
		t.Fatalf("available mixed selection lost payload identity: %q %q %v", first, second, err)
	}
}

func TestGoEmbedBatchProbeObservesCreationDeletionAndExclusion(t *testing.T) {
	dir := t.TempDir()
	patterns := []string{"file.txt", "go-embed:build"}
	for i := 0; i < 2; i++ {
		if matched, err := HasMatchingFiles(dir, patterns); err != nil || matched {
			t.Fatalf("empty probe %d: matched=%v err=%v", i, matched, err)
		}
	}
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if matched, err := HasMatchingFiles(dir, patterns); err != nil || !matched {
			t.Fatalf("created probe %d: matched=%v err=%v", i, matched, err)
		}
	}
	if matched, err := HasMatchingFiles(dir, []string{"file.txt", "!file.txt", "go-embed:build"}); err != nil || matched {
		t.Fatalf("excluded ordinary input: matched=%v err=%v", matched, err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if matched, err := HasMatchingFiles(dir, patterns); err != nil || matched {
		t.Fatalf("deleted probe: matched=%v err=%v", matched, err)
	}
}

func TestHashFiles_Deterministic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b"), 0o644)

	h1, err := hashFiles(dir, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := hashFiles(dir, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("hashFiles should be deterministic")
	}
}

// TestHashFiles_ManyFilesDeterministic exercises the concurrent worker pool
// with enough files to span multiple workers, and asserts the digest-of-digests
// fold stays deterministic regardless of the order workers finish in. A single
// content change anywhere must still move the hash.
func TestHashFiles_ManyFilesDeterministic(t *testing.T) {
	dir := t.TempDir()
	const n = 200
	for i := 0; i < n; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%03d.go", i))
		if err := os.WriteFile(name, []byte(fmt.Sprintf("package p // %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	first, err := hashFiles(dir, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got, err := hashFiles(dir, nil, ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("hashFiles is not deterministic across runs: %q != %q", got, first)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "f100.go"), []byte("package p // changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _ := hashFiles(dir, nil, ProjectConfigScope{}); changed == first {
		t.Error("changing one file among many should change the hash")
	}
}

func TestHashFiles_ChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	os.WriteFile(path, []byte("v1"), 0o644)

	h1, _ := hashFiles(dir, nil, ProjectConfigScope{})

	os.WriteFile(path, []byte("v2"), 0o644)
	h2, _ := hashFiles(dir, nil, ProjectConfigScope{})

	if h1 == h2 {
		t.Error("hashFiles should change when file content changes")
	}
}

// makeUnreadable strips every permission bit from p and reports whether the
// running user is genuinely locked out. Root ignores mode bits, and so do some
// filesystems, so a caller that cannot produce a real read failure skips rather
// than asserting something the platform will not do.
func makeUnreadable(t *testing.T, p string) bool {
	t.Helper()
	if err := os.Chmod(p, 0o000); err != nil {
		t.Skipf("chmod 0o000 unsupported here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	file, err := os.Open(p)
	if err == nil {
		_ = file.Close()
		return false
	}
	return true
}

// The fold layout of hashFiles is a cache-key ABI: every task cache key in every
// developer's workspace and on CI derives from it, so moving it is a one-time
// total cache miss for everyone. This pins the exact bytes a fully readable tree
// contributes — relative path, NUL, content digest, NUL, in sorted-path order —
// against both an independently constructed expectation and a literal golden
// computed outside Go, so no refactor (including the unreadable-file
// sentinel) can silently move a happy-path key.
func TestHashFiles_ReadableTreeEncodingIsPinned(t *testing.T) {
	dir := t.TempDir()
	files := []struct{ rel, content string }{
		{"a.go", "package a"},
		{"b.go", "package b"},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.rel), []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// collectFiles returns files sorted by path, so the fold order is a.go, b.go.
	expected := sha256.New()
	for _, f := range files {
		expected.Write([]byte(f.rel))
		expected.Write([]byte{0})
		digest := sha256.Sum256([]byte(f.content))
		expected.Write(digest[:])
		expected.Write([]byte{0})
	}
	structural := hex.EncodeToString(expected.Sum(nil))

	const golden = "00f1d0a5ab4b348c8abeeb596065f3b066a2148c4f523bd678f95ee0d2e33fa8"
	if structural != golden {
		t.Fatalf("the structural expectation drifted from the golden: %s != %s", structural, golden)
	}

	got, err := hashFiles(dir, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if got != golden {
		t.Fatalf(
			"hashFiles moved the cache key of a fully readable tree: got %s, want %s\n"+
				"every task cache entry keyed on this layout is invalidated by that change",
			got, golden,
		)
	}
}

// A file that is selected but cannot be read must hash differently from the same
// tree without it. Skipping it made hash(a, b-unreadable) == hash(a), so
// an entry legitimately built while b was deleted could be restored as a hit for
// a tree that has b — a transient open failure (editor atomic-save rename, a
// concurrent generate, watch mode recomputing keys mid-write) is enough, and an
// unreadable-by-permission source file reproduces it on every run forever.
func TestHashFiles_UnreadableFileIsNeitherAbsentNorPresent(t *testing.T) {
	dir := t.TempDir()
	unreadablePath := filepath.Join(dir, "b.go")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unreadablePath, []byte("package b"), 0o644); err != nil {
		t.Fatal(err)
	}

	readable, err := hashFiles(dir, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}

	if !makeUnreadable(t, unreadablePath) {
		t.Skip("the running user can still read a 0o000 file (root?); the skip cannot be exercised")
	}
	unreadable, err := hashFiles(dir, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if unreadable == readable {
		t.Error("an unreadable file hashed the same as its readable content")
	}

	if err := os.Chmod(unreadablePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(unreadablePath); err != nil {
		t.Fatal(err)
	}
	absent, err := hashFiles(dir, nil, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if unreadable == absent {
		t.Error(
			"an unreadable file hashed identically to an absent one: an entry built without " +
				"the file would be served as a hit for a tree that has it")
	}
	if absent == readable {
		t.Fatal("removing a keyed file did not change the hash; the test is not exercising what it claims")
	}
}

// The .gen/version.json stamp is the second source of a nil digest
// (versionStampDigest returns nil when os.ReadFile fails). It reaches the same
// fold as any other file, so the sentinel covers it without a second code path —
// this pins that.
func TestHashFiles_UnreadableVersionStampIsNotAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(dir, ".gen", "version.json")
	content := `{"name":"proj","version":"0.1.0","sha":"abc1234","buildTime":"2026-07-26T16:10:39Z"}`
	if err := os.WriteFile(stamp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	patterns := []string{"**/*.json"}
	readable, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}

	if !makeUnreadable(t, stamp) {
		t.Skip("the running user can still read a 0o000 file (root?); the skip cannot be exercised")
	}
	unreadable, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if unreadable == readable {
		t.Error("an unreadable build stamp hashed the same as its readable content")
	}

	if err := os.Chmod(stamp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stamp); err != nil {
		t.Fatal(err)
	}
	absent, err := hashFiles(dir, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if unreadable == absent {
		t.Error("an unreadable build stamp hashed identically to an absent one")
	}
}

func TestHashFiles_ProjectConfigIgnoresTaskTuning(t *testing.T) {
	for _, filename := range []string{wsproto.ConfigFilename, wsproto.LegacyConfigFilename} {
		t.Run(filename, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, filename)
			write := func(body string) string {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				hash, err := hashFiles(dir, []string{filename}, ProjectConfigScope{})
				if err != nil {
					t.Fatal(err)
				}
				return hash
			}

			base := write(`{"name":"proj","tags":["go"]}`)
			for _, body := range []string{
				`{"name":"proj","tags":["go"],"tasks":{"lint":{"cpuWeight":4}}}`,
				`{"name":"proj","tags":["go"],"tasks":{"lint":{"timeoutMs":900000}}}`,
				`{"name":"proj","tags":["go"],"tasks":{"lint":{"cpuWeight":4,"timeoutMs":900000},"build":{"timeoutMs":1}}}`,
			} {
				if got := write(body); got != base {
					t.Errorf("execution-only task tuning moved the file digest: %s != %s", got, base)
				}
			}
			if got := write(`{"name":"renamed","tags":["go"]}`); got == base {
				t.Fatal("a non-tuning project config edit did not move the digest")
			}
		})
	}
}

func TestHashExtraFiles_MissingFile(t *testing.T) {
	// Missing files should produce a deterministic hash (not panic)
	h := hashExtraFiles("", []string{"/nonexistent/file.txt"})
	if h == "" {
		t.Error("hashExtraFiles should return a hash even for missing files")
	}
}

func TestHashExtraFiles_Deterministic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asset.txt")
	os.WriteFile(path, []byte("data"), 0o644)

	h1 := hashExtraFiles("", []string{path})
	h2 := hashExtraFiles("", []string{path})
	if h1 != h2 {
		t.Error("hashExtraFiles should be deterministic")
	}
}

func TestHashExtraFiles_OrderIndependent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	os.WriteFile(a, []byte("aaa"), 0o644)
	os.WriteFile(b, []byte("bbb"), 0o644)

	h1 := hashExtraFiles("", []string{a, b})
	h2 := hashExtraFiles("", []string{b, a})
	if h1 != h2 {
		t.Error("hashExtraFiles should be order-independent")
	}
}

// A generate asset declared as a directory (e.g. a docs tree pulled into .gen)
// must fold in its files' content recursively. The prior implementation io.Copy'd
// a directory fd, reading zero bytes, so edits under the directory never changed
// the key — the latent cause of generate cache "hits" that shipped stale docs.
func TestHashExtraFiles_DirectoryContentInvalidatesKey(t *testing.T) {
	dir := t.TempDir()
	docDir := filepath.Join(dir, "doc")
	if err := os.MkdirAll(filepath.Join(docDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(docDir, "a.md"), []byte("one"), 0o644)
	os.WriteFile(filepath.Join(docDir, "sub", "b.md"), []byte("two"), 0o644)

	before := hashExtraFiles("", []string{docDir})

	// Editing a nested file must flip the key.
	os.WriteFile(filepath.Join(docDir, "sub", "b.md"), []byte("two-edited"), 0o644)
	afterEdit := hashExtraFiles("", []string{docDir})
	if before == afterEdit {
		t.Error("editing a file under a directory asset did not change the hash")
	}

	// Adding a file must flip the key too (path is folded in, not just content).
	os.WriteFile(filepath.Join(docDir, "c.md"), []byte("three"), 0o644)
	afterAdd := hashExtraFiles("", []string{docDir})
	if afterEdit == afterAdd {
		t.Error("adding a file under a directory asset did not change the hash")
	}
}

func TestHashExtraFiles_DirectoryDeterministic(t *testing.T) {
	dir := t.TempDir()
	docDir := filepath.Join(dir, "doc")
	os.MkdirAll(filepath.Join(docDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(docDir, "a.md"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(docDir, "sub", "b.md"), []byte("b"), 0o644)

	h1 := hashExtraFiles("", []string{docDir})
	h2 := hashExtraFiles("", []string{docDir})
	if h1 != h2 {
		t.Error("directory hashing should be deterministic")
	}
}

// A path under the workspace root is named by its workspace-relative slash
// form: the checkout directory does not reach the hash, and the relative path
// and the content both do. A path outside the root, and every path when no root
// is known, is named as given.
func TestHashExtraFiles_NamesAPathUnderTheRootRelativeToIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "tree-keyed-task-cache",
		"a-generate-asset-keys-by-its-workspace-relative-path")

	// The two checkouts sit beside a directory whose name extends the first
	// root's, so a prefix match on the root string would claim it.
	parent := t.TempDir()
	rootA := filepath.Join(parent, "ws")
	rootB := filepath.Join(t.TempDir(), "elsewhere")
	beside := filepath.Join(parent, "ws-other")
	write := func(path, content string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	assets := func(root string) []string {
		t.Helper()
		write(filepath.Join(root, "docs", "guide", "intro.md"), "intro")
		write(filepath.Join(root, "docs", "index.md"), "index")
		return []string{filepath.Join(root, "docs"), write(filepath.Join(root, "README.md"), "readme")}
	}
	pathsA, pathsB := assets(rootA), assets(rootB)

	inA := hashExtraFiles(rootA, pathsA)
	if inB := hashExtraFiles(rootB, pathsB); inB != inA {
		t.Errorf("one asset tree under two workspace roots hashed differently: %s != %s", inA, inB)
	}
	if reordered := hashExtraFiles(rootA, []string{pathsA[1], pathsA[0]}); reordered != inA {
		t.Error("the order of the paths reached the hash")
	}
	if hashExtraFiles("", pathsA) == hashExtraFiles("", pathsB) {
		t.Error("without a workspace root, two absolute paths hashed under one name")
	}

	t.Run("the relative path reaches the hash", func(t *testing.T) {
		moved := []string{pathsB[0], write(filepath.Join(rootB, "notes", "README.md"), "readme")}
		if hashExtraFiles(rootB, moved) == inA {
			t.Error("an asset with the same content at another workspace-relative path kept the hash")
		}
	})

	t.Run("the content reaches the hash", func(t *testing.T) {
		write(filepath.Join(rootB, "docs", "guide", "intro.md"), "intro, edited")
		if hashExtraFiles(rootB, pathsB) == inA {
			t.Error("an edit under a directory asset kept the hash")
		}
	})

	t.Run("a path outside the root is named as given", func(t *testing.T) {
		outside := write(filepath.Join(beside, "asset.txt"), "data")
		asGiven := hashExtraFiles("", []string{outside})
		if got := hashExtraFiles(rootA, []string{outside}); got != asGiven {
			t.Errorf("a path beside the workspace root was renamed against it: %s != %s", got, asGiven)
		}
		if got := hashExtraFiles(rootB, []string{outside}); got != asGiven {
			t.Errorf("a path outside the workspace root was renamed against it: %s != %s", got, asGiven)
		}
	})
}

// versionStampTestDocument is a build stamp as its writers leave it: the
// scheduler's fields, the contentHash a TypeScript build merges in, and the
// publication fields a Docker publish merges in.
type versionStampTestDocument struct {
	Name               string          `json:"name"`
	Version            string          `json:"version"`
	Suffix             string          `json:"suffix,omitempty"`
	SHA                string          `json:"sha"`
	Branch             string          `json:"branch"`
	IsDirty            bool            `json:"isDirty"`
	BuildTime          string          `json:"buildTime"`
	CapabilityRoot     string          `json:"capabilityRoot"`
	CapabilityPackages json.RawMessage `json:"capabilityPackages"`
	ContentHash        string          `json:"contentHash"`
	Image              string          `json:"image,omitempty"`
	ImageDigest        string          `json:"image_digest,omitempty"`
	Publish            json.RawMessage `json:"publish,omitempty"`
}

// versionStampTestPublished is the stamp of a tree after a run and a publish at
// one pull request commit.
var versionStampTestPublished = versionStampTestDocument{
	Name:               "proj",
	Version:            "0.3.1-20261003152506-4da6833",
	Suffix:             "20261003152506-4da6833",
	SHA:                "4da6833",
	Branch:             "feature/x",
	IsDirty:            true,
	BuildTime:          "2026-10-03T15:25:06Z",
	CapabilityRoot:     "../..",
	CapabilityPackages: json.RawMessage(`[{"package":"proj","version":"0.3.1"}]`),
	ContentHash:        "0123456789abcdef",
	Image:              "registry.example/proj:0.3.1-20261003152506-4da6833",
	ImageDigest:        "sha256:4da68334da68334da68334da68334da68334da68334da68334da68334da68334",
	Publish:            json.RawMessage(`{"version":"0.3.1-20261003152506-4da6833"}`),
}

// versionStampTestChanges edit each top-level field of versionStampTestPublished
// once, to the value another commit, run or tree gives it.
var versionStampTestChanges = []struct {
	field string
	apply func(*versionStampTestDocument)
}{
	{"name", func(d *versionStampTestDocument) { d.Name = "renamed" }},
	{"version", func(d *versionStampTestDocument) { d.Version = "0.3.1-20261004094924-a389c95" }},
	{"suffix", func(d *versionStampTestDocument) { d.Suffix = "20261004094924-a389c95" }},
	{"sha", func(d *versionStampTestDocument) { d.SHA = "a389c95" }},
	{"branch", func(d *versionStampTestDocument) { d.Branch = "main" }},
	{"isDirty", func(d *versionStampTestDocument) { d.IsDirty = false }},
	{"buildTime", func(d *versionStampTestDocument) { d.BuildTime = "2026-10-04T09:49:24Z" }},
	{"capabilityRoot", func(d *versionStampTestDocument) { d.CapabilityRoot = ".." }},
	{"capabilityPackages", func(d *versionStampTestDocument) {
		d.CapabilityPackages = json.RawMessage(`[{"package":"proj","version":"0.3.1"},{"package":"dep","version":"0.3.1"}]`)
	}},
	{"contentHash", func(d *versionStampTestDocument) { d.ContentHash = "fedcba9876543210" }},
	{"image", func(d *versionStampTestDocument) { d.Image = "registry.example/proj:0.3.1-20261004094924-a389c95" }},
	{"image_digest", func(d *versionStampTestDocument) {
		d.ImageDigest = "sha256:a389c95a389c95a389c95a389c95a389c95a389c95a389c95a389c95a389c9"
	}},
	{"publish", func(d *versionStampTestDocument) {
		d.Publish = json.RawMessage(`{"version":"0.3.1-20261004094924-a389c95"}`)
	}},
}

// The .gen/version.json stamp describes the tree to a file-pattern input. Its
// build time names the invocation, its commit fields name the commit, and its
// publication fields name a publication of that commit, so two stamps that
// differ only in those fields give one digest: a task whose globs reach the
// stamp keys the same at two commits that share one tree. Every other field
// describes the tree and still moves the digest.
func TestHashFiles_VersionStampKeysTheTreeNotTheCommit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "tree-keyed-task-cache",
		"the-build-stamp-keys-without-its-commit-fields")

	// The change table and the excluded set both cover every field of
	// versionStampTestPublished, so a field added to that document fails here
	// until it is classified.
	var fields map[string]json.RawMessage
	encoded, err := json.Marshal(versionStampTestPublished)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(versionStampTestChanges) != len(fields) {
		t.Fatalf("the change table edits %d fields, the stamp has %d", len(versionStampTestChanges), len(fields))
	}
	for _, change := range versionStampTestChanges {
		if _, ok := fields[change.field]; !ok {
			t.Fatalf("the change table edits %q, which the stamp does not carry", change.field)
		}
	}
	for _, field := range versionStampNonTreeFields {
		if _, ok := fields[field]; !ok {
			t.Errorf("the excluded field %q is not a field any stamp writer leaves", field)
		}
	}

	dir := t.TempDir()
	stamp := filepath.Join(dir, ".gen", "version.json")
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	// hash writes the stamp and returns the digest of a pattern that reaches it.
	hash := func(document versionStampTestDocument) string {
		t.Helper()
		content, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stamp, content, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := hashFiles(dir, []string{"**/*.json"}, ProjectConfigScope{})
		if err != nil {
			t.Fatalf("hashFiles: %v", err)
		}
		return got
	}
	base := hash(versionStampTestPublished)

	for _, change := range versionStampTestChanges {
		changed := versionStampTestPublished
		change.apply(&changed)
		kept := hash(changed) == base
		if excluded := slices.Contains(versionStampNonTreeFields, change.field); kept != excluded {
			t.Errorf("a change to the stamp field %q kept the digest = %t, want %t", change.field, kept, excluded)
		}
	}

	// A fresh checkout of the squash commit stamps the same tree at another
	// commit and carries no publication fields.
	squashMerge := versionStampTestPublished
	squashMerge.Version = "0.3.1-20261004094924-a389c95"
	squashMerge.Suffix = "20261004094924-a389c95"
	squashMerge.SHA = "a389c95"
	squashMerge.Branch = "main"
	squashMerge.IsDirty = false
	squashMerge.BuildTime = "2026-10-04T09:49:24Z"
	squashMerge.Image, squashMerge.ImageDigest, squashMerge.Publish = "", "", nil
	if got := hash(squashMerge); got != base {
		t.Errorf("two commits on one tree gave two stamp digests: %s != %s", base, got)
	}

	// A release stamp carries no suffix at all; the field's absence is a commit
	// difference like any of its values.
	release := squashMerge
	release.Version = "0.3.1"
	release.Suffix = ""
	if got := hash(release); got != base {
		t.Errorf("a stamp without a suffix moved the digest: %s != %s", base, got)
	}
}

// A generate asset copies its files into the task's output, so a build stamp
// inside an asset directory is hashed as the output carries it: only its build
// time stays out of the key, and its commit and publication fields move it. A
// hit then never restores a copy that names another commit.
func TestHashExtraFiles_AStampInsideAnAssetKeepsItsCommit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "tree-keyed-task-cache",
		"a-stamp-inside-a-generate-asset-keeps-its-commit")

	root := t.TempDir()
	asset := filepath.Join(root, "app")
	stamp := filepath.Join(asset, ".gen", "version.json")
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(asset, "index.html"), []byte("<p>app</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// hash writes the stamp and returns the digest of the directory asset.
	hash := func(document versionStampTestDocument) string {
		t.Helper()
		content, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stamp, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return hashExtraFiles(root, []string{asset})
	}
	base := hash(versionStampTestPublished)

	for _, change := range versionStampTestChanges {
		changed := versionStampTestPublished
		change.apply(&changed)
		kept := hash(changed) == base
		if want := change.field == versionStampFieldBuildTime; kept != want {
			t.Errorf("a change to the asset stamp field %q kept the digest = %t, want %t", change.field, kept, want)
		}
	}

	squashMerge := versionStampTestPublished
	squashMerge.SHA = "a389c95"
	squashMerge.Image, squashMerge.ImageDigest, squashMerge.Publish = "", "", nil
	if hash(squashMerge) == base {
		t.Error("an asset stamp that names another commit kept the digest")
	}
}

// Only a real .gen/version.json is normalized: a same-named file elsewhere, and
// a stamp that is not the JSON object the CLI writes, are hashed verbatim.
func TestHashFiles_VersionStampNormalizationIsNarrow(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".gen"), 0o755)

	sibling := filepath.Join(dir, "src", "version.json")
	os.WriteFile(sibling, []byte(`{"buildTime":"A"}`), 0o644)
	before, err := hashFiles(dir, []string{"**/*.json"}, ProjectConfigScope{})
	if err != nil {
		t.Fatalf("hashFiles: %v", err)
	}
	os.WriteFile(sibling, []byte(`{"buildTime":"B"}`), 0o644)
	after, err := hashFiles(dir, []string{"**/*.json"}, ProjectConfigScope{})
	if err != nil {
		t.Fatalf("hashFiles: %v", err)
	}
	if after == before {
		t.Error("a version.json outside .gen must be hashed verbatim")
	}

	stamp := filepath.Join(dir, ".gen", "version.json")
	os.WriteFile(stamp, []byte("not json"), 0o644)
	malformed, err := hashFiles(dir, []string{"**/*.json"}, ProjectConfigScope{})
	if err != nil {
		t.Fatalf("hashFiles: %v", err)
	}
	os.WriteFile(stamp, []byte("still not json"), 0o644)
	malformedEdited, err := hashFiles(dir, []string{"**/*.json"}, ProjectConfigScope{})
	if err != nil {
		t.Fatalf("hashFiles: %v", err)
	}
	if malformed == malformedEdited {
		t.Error("an unparseable stamp must be hashed verbatim")
	}
}

// TestCacheKeyVariesWithHostPlatform locks the machine half of a cache key.
//
// A host-platform compile is a function of the MACHINE as well as the sources,
// and nothing else in this key says which machine: ToolchainVersion is
// runtime.Version(), the same "go1.x" string on darwin and linux, and
// ExtensionImplementationDigest is populated only for workspace-local
// extensions — so the fixture below pins it EMPTY, which is what a consumer
// workspace on a published @putnami/go actually has. Entries are shared between
// developer machines and CI by design, so the concrete failure this forbids is:
// a darwin laptop compiles and uploads, a linux runner restores at identical
// sources, and the app's binary is Mach-O while a library's compile-check
// verdict silently skips every `//go:build linux` file.
func TestCacheKeyVariesWithHostPlatform(t *testing.T) {
	cm := NewCacheManager(NewLocalStore(t.TempDir()))

	keyFor := func(identity ...string) string {
		t.Helper()
		key := BuildCacheKey(
			"@putnami/go", "1.4.2",
			"", // published extension: no implementation digest exists to carry the platform
			"go1.25.7",
			"build~compile",
			"tc1:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			"go.putnami.dev/cache",
			"wsid1:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			"",
			nil,
			nil, // no params: the point is that the MACHINE moves the key on its own
			"", "",
			CacheKeyPolicy{RuntimeIdentity: identity},
			nil,
		)
		hash, err := key.ComputeHashUsing(cm)
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		return hash
	}

	darwin := keyFor("hostPlatform=darwin/arm64")
	linux := keyFor("hostPlatform=linux/amd64")
	if darwin == linux {
		t.Error("a darwin-built entry and a linux-built entry share one cache key at identical sources; " +
			"the darwin verdict would be served to CI, so linux-only compile errors pass")
	}

	// Stability: the same host twice is the same address, or nothing would ever hit.
	if darwin != keyFor("hostPlatform=darwin/arm64") {
		t.Error("the same host platform produced two different keys")
	}

	// Declaring NO runtime input must leave a key exactly where it was, so this
	// mechanism cannot invalidate the whole cache for every task that does not
	// depend on the machine. Two no-identity keys agree, and they differ from
	// every keyed variant.
	none := keyFor()
	if none != keyFor() {
		t.Error("a key with no runtime identity is unstable")
	}
	if none == darwin || none == linux {
		t.Error("a key with no runtime identity collides with a host-keyed one; the marker is not written")
	}
}

// The portable admission asks the store WHICH files a key hashes; the two
// exported collectors must answer with exactly the set the hashing walks.
func TestCollectKeyFilesAndExtraKeyFilesAgreeWithHashing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, content := range map[string]string{"a.go": "a", "b_test.go": "b", "sub/c.go": "c", ".hidden/d.go": "d", "node_modules/e.go": "e", "conf/local.txt": "l"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rel := func(paths []string) string {
		out := make([]string, 0, len(paths))
		for _, p := range paths {
			r, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(r))
		}
		return strings.Join(out, ",")
	}
	whole, err := CollectKeyFiles(dir, nil)
	if err != nil || rel(whole) != "a.go,b_test.go,conf/local.txt,sub/c.go" {
		t.Fatalf("empty patterns = %s (%v); expected the whole non-hidden tree", rel(whole), err)
	}
	// A pattern walk skips node_modules but not hidden directories: that is
	// the key's own rule, and the admission must see what the key sees.
	patterned, err := CollectKeyFiles(dir, []string{"**/*.go", "!**/*_test.go", "conf/*.txt"})
	if err != nil || rel(patterned) != ".hidden/d.go,a.go,conf/local.txt,sub/c.go" {
		t.Fatalf("patterns = %s (%v)", rel(patterned), err)
	}
	// Same selection, same file set: the collector the key hashes through.
	files, err := collectFiles(dir, []string{"**/*.go", "!**/*_test.go", "conf/*.txt"})
	if err != nil || len(files) != len(patterned) {
		t.Fatalf("CollectKeyFiles diverged from collectFiles: %d vs %d", len(patterned), len(files))
	}
	extra := ExtraKeyFiles([]string{filepath.Join(dir, "sub"), filepath.Join(dir, "a.go"), filepath.Join(dir, "missing")})
	if rel(extra) != "a.go,sub/c.go" {
		t.Fatalf("extra files = %s; a directory contributes every file beneath it, a missing path nothing", rel(extra))
	}
}
