package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	protocolcache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
)

func TestCacheKey_ComputeHash_Deterministic(t *testing.T) {
	key := &CacheKey{
		Extension:        "@putnami/typescript",
		Task:             "build~transpile",
		Project:          "my-project",
		WorkspaceVersion: "1.0.0",
		Params:           map[string]any{"target": "es2022"},
	}

	h1, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}

	h2, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}

	if h1 != h2 {
		t.Errorf("hash not deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64", len(h1))
	}
}

func TestCacheKeyFormatVersion(t *testing.T) {
	const want = "v8"
	if cacheKeyVersion != want {
		t.Fatalf("cache key format version = %q, want %q", cacheKeyVersion, want)
	}
}

func TestCacheKey_ComputeHash_DifferentInputs(t *testing.T) {
	key1 := &CacheKey{
		Extension: "ext", Task: "build", Project: "a",
		Params: map[string]any{"target": "es2022"},
	}
	key2 := &CacheKey{
		Extension: "ext", Task: "build", Project: "a",
		Params: map[string]any{"target": "es2020"},
	}

	h1, _ := key1.ComputeHashUsing(NewCacheManager(nil))
	h2, _ := key2.ComputeHashUsing(NewCacheManager(nil))

	if h1 == h2 {
		t.Error("different params should produce different hashes")
	}
}

func TestCacheKey_ComputeHash_WithFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.ts"), []byte("const a = 1;"), 0o644)

	key := &CacheKey{
		Extension:   "ext",
		Task:        "build",
		Project:     "pkg",
		ProjectRoot: dir,
	}

	h1, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}

	// Modify file → different hash
	os.WriteFile(filepath.Join(dir, "a.ts"), []byte("const a = 99;"), 0o644)

	h2, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}

	if h1 == h2 {
		t.Error("different file content should produce different hashes")
	}
}

func TestCacheKey_ComputeHash_WorkspaceFiles(t *testing.T) {
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "packages", "app")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(workspaceRoot, "bun.lock")
	if err := os.WriteFile(lockPath, []byte("lock-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := &CacheKey{
		Extension:             "@putnami/typescript",
		Task:                  "build~types",
		Project:               "app",
		ProjectRoot:           projectRoot,
		FilePatterns:          []string{"src/**/*.ts"},
		WorkspaceRoot:         workspaceRoot,
		WorkspaceFilePatterns: []string{"bun.lock"},
	}
	h1, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("lock-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("workspace lockfile change must invalidate the cache key")
	}
}

func TestCacheKey_EveryProjectedParamIsSemantic(t *testing.T) {
	key1 := &CacheKey{
		Extension: "ext", Task: "build", Project: "pkg",
		Params: map[string]any{"target": "es2022", "verbose": true, "noCache": true},
	}
	key2 := &CacheKey{
		Extension: "ext", Task: "build", Project: "pkg",
		Params: map[string]any{"target": "es2022", "verbose": false, "noCache": false},
	}

	h1, _ := key1.ComputeHashUsing(NewCacheManager(nil))
	h2, _ := key2.ComputeHashUsing(NewCacheManager(nil))

	if h1 == h2 {
		t.Error("different task-projected params must produce different hashes")
	}
}

func TestCacheManager_LookupAndSave(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)
	cm := NewCacheManager(s)

	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Lookup miss
	entry, err := cm.Lookup(hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if entry != nil {
		t.Error("Lookup should return nil for miss")
	}

	// Save
	result := &EntryResult{Status: "success", Data: map[string]any{"output": "/dist"}}
	meta := &EntryMetadata{Extension: "ext", Task: "build", Project: "pkg"}
	if err := cm.Save(hash, result, meta, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Lookup hit
	entry, err = cm.Lookup(hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if entry == nil {
		t.Fatal("Lookup should return non-nil for hit")
	}
	if entry.Result.Status != "success" {
		t.Errorf("Result.Status = %q, want success", entry.Result.Status)
	}
}

func TestCacheManager_SaveWithFiles(t *testing.T) {
	storeDir := t.TempDir()
	s := NewLocalStore(storeDir)
	cm := NewCacheManager(s)

	// Create output files
	outputDir := filepath.Join(t.TempDir(), "out")
	os.MkdirAll(filepath.Join(outputDir, "dist"), 0o755)
	os.WriteFile(filepath.Join(outputDir, "dist", "main.js"), []byte("hello"), 0o644)

	hash := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

	result := &EntryResult{Status: "success"}
	meta := &EntryMetadata{Extension: "ext", Task: "build", Project: "pkg"}
	if err := cm.Save(hash, result, meta, outputDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Restore files to a new location
	entry, _ := cm.Lookup(hash)
	if entry == nil {
		t.Fatal("entry not found after Save")
	}

	restoreDir := filepath.Join(t.TempDir(), "restored")
	restored, err := cm.RestoreFiles(entry, restoreDir)
	if err != nil {
		t.Fatalf("RestoreFiles: %v", err)
	}
	if !restored {
		t.Error("RestoreFiles returned false, want true")
	}

	// Verify restored file
	data, err := os.ReadFile(filepath.Join(restoreDir, "dist", "main.js"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("restored content = %q, want hello", string(data))
	}
}

func TestCacheManager_RestoreFiles_NoFiles(t *testing.T) {
	storeDir := t.TempDir()
	s := NewLocalStore(storeDir)
	cm := NewCacheManager(s)

	hash := "aabbccdd1234567890abcdef1234567890abcdef1234567890abcdef12345678"

	result := &EntryResult{Status: "success"}
	meta := &EntryMetadata{Extension: "ext", Task: "lint", Project: "pkg"}
	cm.Save(hash, result, meta, "")

	entry, _ := cm.Lookup(hash)
	restored, err := cm.RestoreFiles(entry, t.TempDir())
	if err != nil {
		t.Fatalf("RestoreFiles: %v", err)
	}
	if restored {
		t.Error("RestoreFiles returned true for entry with no files")
	}
}

// RestoreDir must make the target byte-identical to the stored snapshot, not an
// overlay onto whatever is there — so a generate cache hit cannot leave a stale
// file from a previous tree behind. This is the store-level guarantee behind a
// generate cache hit rematerializing .gen identically to a fresh run.
func TestCacheManager_RestoreDir_ReplacesStaleTree(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	cm := NewCacheManager(s)

	// Snapshot a .gen-like tree into the cache.
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "public", "docs"), 0o755)
	os.WriteFile(filepath.Join(src, "public", "docs", "index.html"), []byte("fresh"), 0o644)
	os.WriteFile(filepath.Join(src, "generate-result.json"), []byte("{}"), 0o644)

	hash := "11bbccdd1234567890abcdef1234567890abcdef1234567890abcdef12345678"
	if err := cm.Save(hash, &EntryResult{Status: "success"}, &EntryMetadata{}, src); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entry, _ := cm.Lookup(hash)

	// Target holds a stale tree that must not survive the restore.
	target := filepath.Join(t.TempDir(), ".gen")
	os.MkdirAll(filepath.Join(target, "public", "docs"), 0o755)
	os.WriteFile(filepath.Join(target, "public", "docs", "stale.html"), []byte("stale"), 0o644)
	os.WriteFile(filepath.Join(target, "public", "docs", "index.html"), []byte("old"), 0o644)

	restored, err := cm.RestoreDir(entry, target)
	if err != nil || !restored {
		t.Fatalf("RestoreDir = (%v, %v), want (true, nil)", restored, err)
	}

	if _, err := os.Stat(filepath.Join(target, "public", "docs", "stale.html")); !os.IsNotExist(err) {
		t.Error("stale file survived RestoreDir (it overlaid instead of replacing)")
	}
	if b, _ := os.ReadFile(filepath.Join(target, "public", "docs", "index.html")); string(b) != "fresh" {
		t.Errorf("index.html = %q, want %q", b, "fresh")
	}
	if _, err := os.Stat(filepath.Join(target, "generate-result.json")); err != nil {
		t.Errorf("generate-result.json not restored: %v", err)
	}
	if _, err := os.Stat(target + ".tmp-restore"); !os.IsNotExist(err) {
		t.Error("RestoreDir left its staging dir behind")
	}
}

func TestCacheManager_RestoreDir_NoFiles(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	cm := NewCacheManager(s)

	hash := "22bbccdd1234567890abcdef1234567890abcdef1234567890abcdef12345678"
	cm.Save(hash, &EntryResult{Status: "success"}, &EntryMetadata{}, "")

	entry, _ := cm.Lookup(hash)
	restored, err := cm.RestoreDir(entry, filepath.Join(t.TempDir(), ".gen"))
	if err != nil {
		t.Fatalf("RestoreDir: %v", err)
	}
	if restored {
		t.Error("RestoreDir returned true for an entry with no files")
	}
}

func TestCacheManager_RestoreDir_UnchangedTreeIsNotRewritten(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	cm := NewCacheManager(s)

	target := t.TempDir()
	file := filepath.Join(target, "version.json")
	if err := os.WriteFile(file, []byte(`{"version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}

	hash := "22ccddaa1234567890abcdef1234567890abcdef1234567890abcdef12345678"
	if err := cm.Save(hash, &EntryResult{Status: "success"}, &EntryMetadata{}, target); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entry, err := cm.Lookup(hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	restored, err := cm.RestoreDir(entry, target)
	if err != nil || !restored {
		t.Fatalf("RestoreDir = (%v, %v), want (true, nil)", restored, err)
	}
	after, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("unchanged manifest-matching tree was rewritten instead of using the no-op fast path")
	}
}

func TestCacheManager_RestoreDirPreservingKeepsPostCaptureSidecar(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	cm := NewCacheManager(s)
	target := t.TempDir()
	versionPath := filepath.Join(target, "version.json")
	if err := os.WriteFile(versionPath, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash := "23ccddaa1234567890abcdef1234567890abcdef1234567890abcdef12345678"
	if err := cm.Save(hash, &EntryResult{Status: "success"}, &EntryMetadata{}, target); err != nil {
		t.Fatal(err)
	}
	entry, err := cm.Lookup(hash)
	if err != nil {
		t.Fatal(err)
	}

	runtimePath := filepath.Join(target, "infra", "runtime.json")
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath, []byte("runtime\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	preserveRuntime := func(rel string) bool { return rel == "infra/runtime.json" }

	if restored, err := cm.RestoreDirPreserving(entry, target, preserveRuntime); err != nil || !restored {
		t.Fatalf("matching RestoreDirPreserving = (%v, %v), want (true, nil)", restored, err)
	}
	after, err := os.Stat(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("post-capture sidecar forced a matching generated tree to be rewritten")
	}

	if err := os.WriteFile(versionPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if restored, err := cm.RestoreDirPreserving(entry, target, preserveRuntime); err != nil || !restored {
		t.Fatalf("mismatching RestoreDirPreserving = (%v, %v), want (true, nil)", restored, err)
	}
	if b, err := os.ReadFile(versionPath); err != nil || string(b) != "fresh\n" {
		t.Fatalf("cached file was not restored: content=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(runtimePath); err != nil || string(b) != "runtime\n" {
		t.Fatalf("post-capture sidecar was not preserved: content=%q err=%v", b, err)
	}
}

func TestCacheManager_RestoreDirIgnoresUncacheableArtifacts(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	cm := NewCacheManager(s)
	target := t.TempDir()
	outputPath := filepath.Join(target, "coverage", "lcov.info")
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("coverage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash := "24ccddaa1234567890abcdef1234567890abcdef1234567890abcdef12345678"
	if err := cm.Save(hash, &EntryResult{Status: "success"}, &EntryMetadata{}, target); err != nil {
		t.Fatal(err)
	}
	entry, err := cm.Lookup(hash)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(target, "coverage", "lcov.info.3.tmp")
	if err := os.WriteFile(tmpPath, []byte("throwaway\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if restored, err := cm.RestoreDir(entry, target); err != nil || !restored {
		t.Fatalf("RestoreDir = (%v, %v), want (true, nil)", restored, err)
	}
	after, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("excluded throwaway artifact forced a matching tree to be rewritten")
	}
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("throwaway artifact should be ignored by manifest matching: %v", err)
	}
}

// SaveDirs captures several sibling output trees into one entry under per-resource
// subpaths; RestoreSubdir rematerializes each independently and byte-identically
// (stale files removed). This is the store-level guarantee behind a single
// generate cache hit restoring both .gen and the generated clients/ tree.
func TestCacheManager_SaveDirs_RoundTripAndRestoreSubdir(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	cm := NewCacheManager(s)

	gen := t.TempDir()
	os.MkdirAll(filepath.Join(gen, "clientgen"), 0o755)
	os.WriteFile(filepath.Join(gen, "clientgen", "config.json"), []byte(`{"targets":["ts"]}`), 0o644)

	clients := t.TempDir()
	os.MkdirAll(filepath.Join(clients, "ts", "src"), 0o755)
	os.WriteFile(filepath.Join(clients, "ts", "src", "index.ts"), []byte("export class C {}\n"), 0o644)

	hash := "33bbccdd1234567890abcdef1234567890abcdef1234567890abcdef12345678"
	if err := cm.SaveDirs(hash, &EntryResult{Status: "success"}, &EntryMetadata{}, map[string]string{
		"gen":     gen,
		"clients": clients,
	}); err != nil {
		t.Fatalf("SaveDirs: %v", err)
	}

	entry, _ := cm.Lookup(hash)
	if entry == nil || entry.FilesDir == "" {
		t.Fatalf("entry has no files after SaveDirs")
	}

	// Restore clients/ over a stale tree: the stale file must not survive.
	clientTarget := filepath.Join(t.TempDir(), "clients")
	os.MkdirAll(filepath.Join(clientTarget, "ts", "src"), 0o755)
	os.WriteFile(filepath.Join(clientTarget, "ts", "src", "stale.ts"), []byte("stale"), 0o644)

	restored, err := cm.RestoreSubdir(entry, "clients", clientTarget)
	if err != nil || !restored {
		t.Fatalf("RestoreSubdir(clients) = (%v, %v), want (true, nil)", restored, err)
	}
	if b, _ := os.ReadFile(filepath.Join(clientTarget, "ts", "src", "index.ts")); string(b) != "export class C {}\n" {
		t.Errorf("restored client index.ts = %q", b)
	}
	if _, err := os.Stat(filepath.Join(clientTarget, "ts", "src", "stale.ts")); !os.IsNotExist(err) {
		t.Error("stale file survived RestoreSubdir (it overlaid instead of replacing)")
	}

	// gen restores independently to its own target.
	genTarget := filepath.Join(t.TempDir(), ".gen")
	if restored, err := cm.RestoreSubdir(entry, "gen", genTarget); err != nil || !restored {
		t.Fatalf("RestoreSubdir(gen) = (%v, %v), want (true, nil)", restored, err)
	}
	if _, err := os.Stat(filepath.Join(genTarget, "clientgen", "config.json")); err != nil {
		t.Errorf("gen subtree not restored: %v", err)
	}

	// A subpath the producing run never captured is a no-op, not an error.
	if restored, err := cm.RestoreSubdir(entry, "go", filepath.Join(t.TempDir(), "go")); err != nil || restored {
		t.Errorf("RestoreSubdir(go) = (%v, %v), want (false, nil) for an uncaptured subpath", restored, err)
	}
}

// SaveDirs skips sources that are absent or empty, so a generate run that
// produced no client captures only the trees it actually wrote.
func TestCacheManager_SaveDirs_SkipsAbsentAndEmpty(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	cm := NewCacheManager(s)

	gen := t.TempDir()
	os.WriteFile(filepath.Join(gen, "version.json"), []byte("{}"), 0o644)

	empty := t.TempDir() // exists but has no files → skipped

	hash := "44bbccdd1234567890abcdef1234567890abcdef1234567890abcdef12345678"
	if err := cm.SaveDirs(hash, &EntryResult{Status: "success"}, &EntryMetadata{}, map[string]string{
		"gen":     gen,
		"clients": empty,
		"missing": filepath.Join(t.TempDir(), "does-not-exist"),
	}); err != nil {
		t.Fatalf("SaveDirs: %v", err)
	}
	entry, _ := cm.Lookup(hash)
	if entry == nil || entry.FilesDir == "" {
		t.Fatalf("gen should have been captured")
	}
	if restored, _ := cm.RestoreSubdir(entry, "clients", filepath.Join(t.TempDir(), "c")); restored {
		t.Error("empty clients source should not have been captured")
	}
	if restored, _ := cm.RestoreSubdir(entry, "gen", filepath.Join(t.TempDir(), "g")); !restored {
		t.Error("gen should have been captured and restorable")
	}
}

func TestBuildCacheKey(t *testing.T) {
	key := BuildCacheKey(
		"@putnami/typescript",
		"2.3.4",
		"extension-implementation-digest",
		"go1.25.7",
		"build~transpile",
		"tc1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"my-project",
		"wsid1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"1.0.0",
		"",
		[]string{"/pkg"},
		map[string]any{"target": "es2022"},
		"/workspace/packages/my-project",
		"/workspace",
		CacheKeyPolicy{
			Files:          []string{"src/**/*.ts"},
			WorkspaceFiles: []string{"bun.lock"},
			Env:            []string{"NODE_ENV"},
		},
		[]string{"upstream-hash-1"},
	)

	if key.Extension != "@putnami/typescript" {
		t.Errorf("Extension = %q", key.Extension)
	}
	if key.ExtensionVersion != "2.3.4" {
		t.Errorf("ExtensionVersion = %q", key.ExtensionVersion)
	}
	if key.ExtensionImplementationDigest != "extension-implementation-digest" {
		t.Errorf("ExtensionImplementationDigest = %q", key.ExtensionImplementationDigest)
	}
	if key.ToolchainVersion != "go1.25.7" {
		t.Errorf("ToolchainVersion = %q", key.ToolchainVersion)
	}
	if key.Task != "build~transpile" {
		t.Errorf("Task = %q", key.Task)
	}
	if key.TaskContractDigest != "tc1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("TaskContractDigest = %q", key.TaskContractDigest)
	}
	if key.ProjectMetadataDigest != "wsid1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("ProjectMetadataDigest = %q", key.ProjectMetadataDigest)
	}
	if len(key.FilePatterns) != 1 || key.FilePatterns[0] != "src/**/*.ts" {
		t.Errorf("FilePatterns = %v", key.FilePatterns)
	}
	if key.WorkspaceRoot != "/workspace" || len(key.WorkspaceFilePatterns) != 1 || key.WorkspaceFilePatterns[0] != "bun.lock" {
		t.Errorf("workspace files = %q %v", key.WorkspaceRoot, key.WorkspaceFilePatterns)
	}
	if len(key.EnvVars) != 1 || key.EnvVars[0] != "NODE_ENV" {
		t.Errorf("EnvVars = %v", key.EnvVars)
	}
	if len(key.UpstreamHashes) != 1 || key.UpstreamHashes[0] != "upstream-hash-1" {
		t.Errorf("UpstreamHashes = %v", key.UpstreamHashes)
	}
	if len(key.SelectedProjects) != 1 || key.SelectedProjects[0] != "/pkg" {
		t.Errorf("SelectedProjects = %v", key.SelectedProjects)
	}
}

func TestCacheKey_ComputeHash_TaskContractDigestMoves(t *testing.T) {
	// v5's new dimension (binding invariant 6): a contract change is a key
	// change. Absent vs present and two different
	// digests must all produce distinct keys.
	hash := func(digest string) string {
		key := &CacheKey{Extension: "ext", Task: "build", TaskContractDigest: digest, Project: "p"}
		h, err := key.ComputeHashUsing(NewCacheManager(nil))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	none, a, b := hash(""), hash("tc1:aa"), hash("tc1:bb")
	if none == a || a == b || none == b {
		t.Errorf("task-contract digest does not move the key: %q %q %q", none, a, b)
	}
}

func TestCacheKey_ComputeHash_ExtensionImplementationDigestMoves(t *testing.T) {
	hash := func(digest string) string {
		key := &CacheKey{Extension: "ext", Task: "build", ExtensionImplementationDigest: digest, Project: "p"}
		h, err := key.ComputeHashUsing(NewCacheManager(nil))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	none, a, b := hash(""), hash("ed1:aa"), hash("ed1:bb")
	if none == a || a == b || none == b {
		t.Errorf("extension implementation digest does not move the key: %q %q %q", none, a, b)
	}
}

func TestCacheKey_HashFormatIsPinned(t *testing.T) {
	// Golden pin of the v8 key format: a fixed key must hash to a fixed value.
	// If this moves, the KEY FORMAT changed and every existing cache entry
	// becomes a miss — that is sometimes the intent (a version bump like
	// v4→v5), but it must be a reviewed, deliberate event, never a side
	// effect. Update the pinned value only alongside a cacheKeyVersion bump or
	// an explicitly justified field change.
	key := pinnedFormatKey()
	got, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatal(err)
	}
	const want = "0c6e523e28d04888c42e9d19ae36a3c4aeba1ab429b661eff9d3d639bdf0c3c6"
	if got != want {
		t.Errorf("v8 key format moved: got %s, pinned %s", got, want)
	}
}

// pinnedFormatKey is the fully populated key both the golden pin and the
// per-field movement checks below use, so a field that stops contributing
// cannot hide behind a key that never carried it.
func pinnedFormatKey() *CacheKey {
	return &CacheKey{
		Extension:                     "ext",
		ExtensionVersion:              "1.2.3",
		ExtensionImplementationDigest: "ed1:0000000000000000000000000000000000000000000000000000000000000000",
		ToolchainVersion:              "go1.25.7",
		Task:                          "build~transpile",
		TaskContractDigest:            "tc1:0000000000000000000000000000000000000000000000000000000000000000",
		Project:                       "proj",
		ProjectMetadataDigest:         "wsid1:0000000000000000000000000000000000000000000000000000000000000000",
		WorkspaceVersion:              "1.0.0",
		EmbeddedVersion:               "1.0.0-abc",
		SelectedProjects:              []string{"a", "b"},
		UpstreamHashes:                []string{"up1", "up2"},
	}
}

// The project metadata digest must be a key input, not decoration:
// a project whose type/tags/dependency edges moved has a different digest and
// must therefore miss. Absent (empty) is itself a stable, distinct value —
// jobs whose workspace never computed one keep a single consistent key.
func TestCacheKey_ProjectMetadataDigestMovesTheKey(t *testing.T) {
	cm := NewCacheManager(nil)
	hash := func(digest string) string {
		key := pinnedFormatKey()
		key.ProjectMetadataDigest = digest
		h, err := key.ComputeHashUsing(cm)
		if err != nil {
			t.Fatalf("ComputeHash: %v", err)
		}
		return h
	}
	none, a, b := hash(""), hash("wsid1:aa"), hash("wsid1:bb")
	if none == a || a == b || none == b {
		t.Errorf("project metadata digest does not move the key: %q %q %q", none, a, b)
	}
	if hash("wsid1:aa") != a {
		t.Error("project metadata digest is not deterministic")
	}
}

func TestCacheKey_ComputeHash_WithSelectedProjects(t *testing.T) {
	base := &CacheKey{
		Extension: "ext",
		Task:      "deploy",
		Project:   "workspace",
	}
	selectedA := &CacheKey{
		Extension:        "ext",
		Task:             "deploy",
		Project:          "workspace",
		SelectedProjects: []string{"/A"},
	}
	selectedB := &CacheKey{
		Extension:        "ext",
		Task:             "deploy",
		Project:          "workspace",
		SelectedProjects: []string{"/B"},
	}
	selectedAReordered := &CacheKey{
		Extension:        "ext",
		Task:             "deploy",
		Project:          "workspace",
		SelectedProjects: []string{"/B", "/A"},
	}
	selectedAB := &CacheKey{
		Extension:        "ext",
		Task:             "deploy",
		Project:          "workspace",
		SelectedProjects: []string{"/A", "/B"},
	}

	cm := NewCacheManager(nil)
	baseHash, err := base.ComputeHashUsing(cm)
	if err != nil {
		t.Fatalf("base hash: %v", err)
	}
	aHash, err := selectedA.ComputeHashUsing(cm)
	if err != nil {
		t.Fatalf("selected A hash: %v", err)
	}
	bHash, err := selectedB.ComputeHashUsing(cm)
	if err != nil {
		t.Fatalf("selected B hash: %v", err)
	}
	abHash, err := selectedAB.ComputeHashUsing(cm)
	if err != nil {
		t.Fatalf("selected AB hash: %v", err)
	}
	baHash, err := selectedAReordered.ComputeHashUsing(cm)
	if err != nil {
		t.Fatalf("selected BA hash: %v", err)
	}

	if baseHash == aHash {
		t.Fatal("selection must change aggregate cache hash")
	}
	if aHash == bHash {
		t.Fatal("different selections must produce different hashes")
	}
	if abHash != baHash {
		t.Fatal("selection hash should be order-independent")
	}
}

func TestCacheKey_ComputeHash_WithEnvVars(t *testing.T) {
	os.Setenv("TEST_CACHE_VAR", "value1")
	defer os.Unsetenv("TEST_CACHE_VAR")

	key1 := &CacheKey{
		Extension: "ext", Task: "build", Project: "pkg",
		EnvVars: []string{"TEST_CACHE_VAR"},
	}

	h1, err := key1.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}

	// Change env var → different hash
	os.Setenv("TEST_CACHE_VAR", "value2")

	h2, err := key1.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}

	if h1 == h2 {
		t.Error("different env var values should produce different hashes")
	}
}

func TestCacheKey_ComputeHash_EmbeddedVersionVariesHash(t *testing.T) {
	base := CacheKey{
		Extension:        "ext",
		Task:             "build",
		Project:          "pkg",
		WorkspaceVersion: "1.0.0",
	}

	keyA := base
	keyA.EmbeddedVersion = "1.0.0-abc1234"
	keyB := base
	keyB.EmbeddedVersion = "1.0.0-def5678"
	keyEmpty := base
	keyEmpty.EmbeddedVersion = ""

	hA, _ := keyA.ComputeHashUsing(NewCacheManager(nil))
	hB, _ := keyB.ComputeHashUsing(NewCacheManager(nil))
	hEmpty, _ := keyEmpty.ComputeHashUsing(NewCacheManager(nil))

	if hA == hB {
		t.Error("different EmbeddedVersion values should produce different hashes")
	}
	if hA == hEmpty || hB == hEmpty {
		t.Error("set EmbeddedVersion should differ from empty")
	}
}

func TestCacheKey_ComputeHash_ExtensionVersionVariesHash(t *testing.T) {
	// Without an implementation digest the version is the only identity of the
	// extension's implementation, and an extension upgrade can change a task's
	// output for byte-identical sources, so the version must move the hash.
	base := CacheKey{Extension: "ext", Task: "test", Project: "pkg"}

	keyA := base
	keyA.ExtensionVersion = "1.0.0"
	keyB := base
	keyB.ExtensionVersion = "1.1.0"

	hA, _ := keyA.ComputeHashUsing(NewCacheManager(nil))
	hB, _ := keyB.ComputeHashUsing(NewCacheManager(nil))
	if hA == hB {
		t.Error("different ExtensionVersion values should produce different hashes")
	}
}

// A digest identifies the implementation itself, so the version is not key
// material beside it: two builds of an unchanged extension that differ in
// version alone share their entries, and a different digest still misses.
func TestCacheKey_ComputeHash_ImplementationDigestReplacesTheVersion(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-digest-replaces-the-version-in-the-key")
	cm := NewCacheManager(nil)
	hash := func(version, digest string) string {
		t.Helper()
		key := pinnedFormatKey()
		key.ExtensionVersion = version
		key.ExtensionImplementationDigest = digest
		h, err := key.ComputeHashUsing(cm)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	canary := hash("0.3.1-20261003152506-4da6833", "ied1:aa")
	if next := hash("0.3.1-20261004094924-a389c95", "ied1:aa"); next != canary {
		t.Errorf("a version moved the key of an unchanged implementation: %s -> %s", canary, next)
	}
	if changed := hash("0.3.1-20261003152506-4da6833", "ied1:bb"); changed == canary {
		t.Error("a changed implementation digest kept the key at one version")
	}
	if a, b := hash("1.0.0", ""), hash("1.1.0", ""); a == b {
		t.Error("without an implementation digest, the version no longer moves the key")
	}
	// The two shapes of the (version, digest) pair never meet: a version
	// spelled like a digest, with no digest, is not the key of that digest.
	if hash("ied1:aa", "") == hash("", "ied1:aa") {
		t.Error("a key with only a version equals a key with only a digest")
	}
}

func TestCacheKey_ComputeHash_ToolchainVersionVariesHash(t *testing.T) {
	// Coverage instrumentation and compiler output differ across toolchain
	// versions; a result cached under one toolchain must not be served under
	// another (the stale-coverage bug this guards against).
	base := CacheKey{Extension: "ext", Task: "test", Project: "pkg"}

	keyA := base
	keyA.ToolchainVersion = "go1.24.0"
	keyB := base
	keyB.ToolchainVersion = "go1.25.7"

	hA, _ := keyA.ComputeHashUsing(NewCacheManager(nil))
	hB, _ := keyB.ComputeHashUsing(NewCacheManager(nil))
	if hA == hB {
		t.Error("different ToolchainVersion values should produce different hashes")
	}
}

func TestCacheKey_ComputeHash_WithUpstreamHashes(t *testing.T) {
	key1 := &CacheKey{
		Extension: "ext", Task: "build", Project: "pkg",
		UpstreamHashes: []string{"hash-a"},
	}
	key2 := &CacheKey{
		Extension: "ext", Task: "build", Project: "pkg",
		UpstreamHashes: []string{"hash-b"},
	}

	h1, _ := key1.ComputeHashUsing(NewCacheManager(nil))
	h2, _ := key2.ComputeHashUsing(NewCacheManager(nil))

	if h1 == h2 {
		t.Error("different upstream hashes should produce different cache keys")
	}
}

// The memoized extra-files hash must be byte-identical to the inline
// hashExtraFiles it replaced — otherwise every existing cache key that carries
// ExtraFiles would silently change and invalidate the whole cache.
func TestCacheManager_LookupExtraFilesHash_MatchesInlineHash(t *testing.T) {
	dir := t.TempDir()
	docDir := filepath.Join(dir, "doc")
	if err := os.MkdirAll(filepath.Join(docDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(docDir, "a.md"), []byte("one"), 0o644)
	os.WriteFile(filepath.Join(docDir, "sub", "b.md"), []byte("two"), 0o644)
	file := filepath.Join(dir, "asset.txt")
	os.WriteFile(file, []byte("data"), 0o644)
	missing := filepath.Join(dir, "does-not-exist")

	paths := []string{docDir, file, missing}
	want := hashExtraFiles(paths)
	if got := NewCacheManager(nil).lookupExtraFilesHash(paths); got != want {
		t.Errorf("memoized extra-files hash %q != inline hashExtraFiles %q", got, want)
	}
}

// Each ExtraFiles set must be walked and read once per CacheManager session:
// repeat lookups — including a reordered path set, since hashExtraFiles is
// order-independent — serve the memo entry, while a fresh manager (a new CLI
// invocation) re-reads and observes changed content.
func TestCacheManager_LookupExtraFilesHash_MemoizedPerSession(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	os.WriteFile(a, []byte("aaa"), 0o644)
	os.WriteFile(b, []byte("bbb"), 0o644)

	cm := NewCacheManager(nil)
	first := cm.lookupExtraFilesHash([]string{a, b})

	// Mutate an input: the same manager must keep serving the memoized value.
	os.WriteFile(a, []byte("changed"), 0o644)
	if again := cm.lookupExtraFilesHash([]string{a, b}); again != first {
		t.Errorf("same manager recomputed the extra-files hash: %q != %q", again, first)
	}
	if reordered := cm.lookupExtraFilesHash([]string{b, a}); reordered != first {
		t.Errorf("reordered path set missed the memo entry: %q != %q", reordered, first)
	}

	if fresh := NewCacheManager(nil).lookupExtraFilesHash([]string{a, b}); fresh == first {
		t.Error("fresh manager should observe changed extra-file content")
	}
}

// Distinct ExtraFiles sets must never share a memo entry (the memo key uses a
// NUL joiner, which cannot occur in a path, so no two sets can collide).
func TestCacheManager_LookupExtraFilesHash_DistinctSetsDistinctEntries(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	os.WriteFile(a, []byte("aaa"), 0o644)
	os.WriteFile(b, []byte("bbb"), 0o644)

	cm := NewCacheManager(nil)
	hA := cm.lookupExtraFilesHash([]string{a})
	hB := cm.lookupExtraFilesHash([]string{b})
	hAB := cm.lookupExtraFilesHash([]string{a, b})
	if hA == hB || hA == hAB || hB == hAB {
		t.Errorf("distinct extra-file sets collided: a=%q b=%q ab=%q", hA, hB, hAB)
	}
}

// End-to-end through ComputeHashUsing: ExtraFiles content still moves the
// cache key across invocations (memoization must not break invalidation), and
// unchanged inputs still produce the same key.
func TestCacheKey_ComputeHash_ExtraFilesVariesHash(t *testing.T) {
	dir := t.TempDir()
	asset := filepath.Join(dir, "asset.txt")
	os.WriteFile(asset, []byte("v1"), 0o644)

	key := &CacheKey{
		Extension:  "ext",
		Task:       "generate",
		Project:    "pkg",
		ExtraFiles: []string{asset},
	}

	h1, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}
	h1Again, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}
	if h1 != h1Again {
		t.Errorf("unchanged ExtraFiles produced different keys: %q != %q", h1, h1Again)
	}

	os.WriteFile(asset, []byte("v2"), 0o644)
	h2, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHash: %v", err)
	}
	if h1 == h2 {
		t.Error("changed extra-file content should produce a different cache key")
	}
}

func TestCacheManager_Store(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)
	cm := NewCacheManager(s)

	if cm.Store() != s {
		t.Error("Store() should return the underlying store")
	}
}

func TestEntry_MarshalRoundtrip(t *testing.T) {
	entry := &Entry{
		Result: &EntryResult{
			Status: "failed",
			Data:   map[string]any{"output": "/dist"},
			Error:  &EntryError{Message: "build failed", Code: "BUILD_ERR"},
			Events: []protocolcache.ActionEvent{
				{Version: 1, Type: "metric", Data: map[string]any{"name": "coverage", "value": float64(84), "unit": "percent"}},
				{Version: 1, Type: "summary", Data: map[string]any{"message": "51/51 passed, 84.0% coverage"}},
			},
		},
		Metadata: &EntryMetadata{
			Hash:        "abc123",
			CreatedAt:   time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
			Extension:   "@putnami/typescript",
			Task:        "build~transpile",
			Project:     "my-project",
			DurationMs:  1500,
			Size:        4096,
			OutputFiles: []string{"dist/index.js", "dist/index.d.ts"},
		},
	}

	// Marshal result
	resultJSON, err := json.Marshal(entry.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	// Unmarshal result
	result, err := UnmarshalResult(resultJSON)
	if err != nil {
		t.Fatalf("UnmarshalResult: %v", err)
	}
	if result.Status != "failed" {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if result.Error == nil || result.Error.Message != "build failed" {
		t.Errorf("Error.Message = %v, want build failed", result.Error)
	}
	if result.Error.Code != "BUILD_ERR" {
		t.Errorf("Error.Code = %q, want BUILD_ERR", result.Error.Code)
	}
	if result.Data["output"] != "/dist" {
		t.Errorf("Data[output] = %v, want /dist", result.Data["output"])
	}
	if len(result.Events) != 2 || result.Events[0].Type != "metric" {
		t.Errorf("Events = %+v, want cached result events preserved", result.Events)
	}

	// Marshal metadata
	metaJSON, err := json.Marshal(entry.Metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	// Unmarshal metadata
	meta, err := UnmarshalMetadata(metaJSON)
	if err != nil {
		t.Fatalf("UnmarshalMetadata: %v", err)
	}
	if meta.Hash != "abc123" {
		t.Errorf("Hash = %q, want abc123", meta.Hash)
	}
	if meta.Extension != "@putnami/typescript" {
		t.Errorf("Extension = %q", meta.Extension)
	}
	if meta.Task != "build~transpile" {
		t.Errorf("Task = %q", meta.Task)
	}
	if meta.Project != "my-project" {
		t.Errorf("Project = %q", meta.Project)
	}
	if meta.DurationMs != 1500 {
		t.Errorf("DurationMs = %d, want 1500", meta.DurationMs)
	}
	if meta.Size != 4096 {
		t.Errorf("Size = %d, want 4096", meta.Size)
	}
	if len(meta.OutputFiles) != 2 {
		t.Errorf("OutputFiles = %v, want 2 items", meta.OutputFiles)
	}
}

func TestUnmarshalResult_InvalidJSON(t *testing.T) {
	_, err := UnmarshalResult([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestUnmarshalMetadata_InvalidJSON(t *testing.T) {
	_, err := UnmarshalMetadata([]byte("{invalid"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestEntry_MarshalResult_SuccessNoError(t *testing.T) {
	entry := &Entry{
		Result: &EntryResult{Status: "success"},
	}

	data, err := json.Marshal(entry.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	// Error field should be omitted
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if _, exists := raw["error"]; exists {
		t.Error("error field should be omitted when nil")
	}
}

func TestCacheKey_ComputeHash_EmptyProjectRoot(t *testing.T) {
	// No ProjectRoot → file hashing is skipped, should not error
	key := &CacheKey{
		Extension: "ext", Task: "build", Project: "pkg",
	}

	h, err := key.ComputeHashUsing(NewCacheManager(nil))
	if err != nil {
		t.Fatalf("ComputeHashUsing with empty ProjectRoot: %v", err)
	}
	if len(h) != 64 {
		t.Errorf("hash length = %d, want 64", len(h))
	}
}
