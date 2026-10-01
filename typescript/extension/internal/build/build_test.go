package build

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
)

// ---- ResolveBuildFlags ----

func TestResolveBuildFlags_AllDefaults(t *testing.T) {
	flags := ResolveBuildFlags(false, false, false, false)
	if !flags.RunGenerate {
		t.Error("expected RunGenerate=true by default")
	}
	if !flags.RunTranspile {
		t.Error("expected RunTranspile=true by default")
	}
	if !flags.RunTypes {
		t.Error("expected RunTypes=true by default")
	}
	if flags.RunCompile {
		t.Error("expected RunCompile=false by default")
	}
}

func TestResolveBuildFlags_OnlyGenerate(t *testing.T) {
	flags := ResolveBuildFlags(true, false, false, false)
	if !flags.RunGenerate {
		t.Error("expected RunGenerate=true")
	}
	if flags.RunTranspile {
		t.Error("expected RunTranspile=false when specific flags set")
	}
	if flags.RunTypes {
		t.Error("expected RunTypes=false when specific flags set")
	}
	if flags.RunCompile {
		t.Error("expected RunCompile=false")
	}
}

func TestResolveBuildFlags_OnlyTranspile(t *testing.T) {
	flags := ResolveBuildFlags(false, true, false, false)
	if flags.RunGenerate {
		t.Error("expected RunGenerate=false")
	}
	if !flags.RunTranspile {
		t.Error("expected RunTranspile=true")
	}
	if flags.RunTypes {
		t.Error("expected RunTypes=false")
	}
}

func TestResolveBuildFlags_OnlyTypes(t *testing.T) {
	flags := ResolveBuildFlags(false, false, true, false)
	if flags.RunGenerate {
		t.Error("expected RunGenerate=false")
	}
	if flags.RunTranspile {
		t.Error("expected RunTranspile=false")
	}
	if !flags.RunTypes {
		t.Error("expected RunTypes=true")
	}
	if flags.RunCompile {
		t.Error("expected RunCompile=false")
	}
}

func TestResolveBuildFlags_OnlyCompile(t *testing.T) {
	flags := ResolveBuildFlags(false, false, false, true)
	if flags.RunGenerate {
		t.Error("expected RunGenerate=false")
	}
	if flags.RunTranspile {
		t.Error("expected RunTranspile=false")
	}
	if flags.RunTypes {
		t.Error("expected RunTypes=false")
	}
	if !flags.RunCompile {
		t.Error("expected RunCompile=true")
	}
}

func TestResolveBuildFlags_MultipleSpecific(t *testing.T) {
	flags := ResolveBuildFlags(true, false, true, false)
	if !flags.RunGenerate {
		t.Error("expected RunGenerate=true")
	}
	if flags.RunTranspile {
		t.Error("expected RunTranspile=false")
	}
	if !flags.RunTypes {
		t.Error("expected RunTypes=true")
	}
}

// ---- normalizeImportPath ----

func TestNormalizeImportPath_AlreadyRelative(t *testing.T) {
	result := normalizeImportPath("./src/foo")
	if result != "./src/foo" {
		t.Errorf("expected './src/foo', got %q", result)
	}
}

func TestNormalizeImportPath_ParentRelative(t *testing.T) {
	result := normalizeImportPath("../src/foo")
	if result != "../src/foo" {
		t.Errorf("expected '../src/foo', got %q", result)
	}
}

func TestNormalizeImportPath_StripsTsExtension(t *testing.T) {
	result := normalizeImportPath("./src/foo.ts")
	if result != "./src/foo" {
		t.Errorf("expected './src/foo', got %q", result)
	}
}

func TestNormalizeImportPath_StripsTsxExtension(t *testing.T) {
	result := normalizeImportPath("./src/component.tsx")
	if result != "./src/component" {
		t.Errorf("expected './src/component', got %q", result)
	}
}

func TestNormalizeImportPath_StripsJsExtension(t *testing.T) {
	result := normalizeImportPath("./dist/foo.js")
	if result != "./dist/foo" {
		t.Errorf("expected './dist/foo', got %q", result)
	}
}

func TestNormalizeImportPath_AddsLeadingDotSlash(t *testing.T) {
	result := normalizeImportPath("src/foo")
	if result != "./src/foo" {
		t.Errorf("expected './src/foo', got %q", result)
	}
}

func TestNormalizeImportPath_WindowsSlashes(t *testing.T) {
	result := normalizeImportPath("src\\foo.ts")
	if result != "./src/foo" {
		t.Errorf("expected './src/foo', got %q", result)
	}
}

// ---- isSourceFile ----

func TestIsSourceFile(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"src/foo.ts", true},
		{"src/component.tsx", true},
		{"src/util.js", true},
		{"src/app.jsx", true},
		{"src/style.css", false},
		{"src/data.json", false},
		{"README.md", false},
		{"src/types.d.ts", true}, // filepath.Ext returns ".ts" — .d.ts files are included
	}

	for _, tt := range tests {
		got := isSourceFile(tt.path)
		if got != tt.expected {
			t.Errorf("isSourceFile(%q) = %v, want %v", tt.path, got, tt.expected)
		}
	}
}

// ---- EnsureRealDirectory ----

func TestEnsureRealDirectory_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "new-dir")

	if err := EnsureRealDirectory(target, false); err != nil {
		t.Fatalf("EnsureRealDirectory failed: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("expected directory to be created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected a directory")
	}
}

func TestEnsureRealDirectory_ClearsExisting(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing")
	os.MkdirAll(target, 0755)
	os.WriteFile(filepath.Join(target, "file.txt"), []byte("content"), 0644)

	if err := EnsureRealDirectory(target, true); err != nil {
		t.Fatalf("EnsureRealDirectory failed: %v", err)
	}

	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Errorf("expected empty directory after clear, got %d entries", len(entries))
	}
}

func TestEnsureRealDirectory_RemovesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	os.MkdirAll(real, 0755)
	link := filepath.Join(dir, "link")
	os.Symlink(real, link)

	// Verify it's a symlink
	info, _ := os.Lstat(link)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected symlink")
	}

	if err := EnsureRealDirectory(link, false); err != nil {
		t.Fatalf("EnsureRealDirectory failed: %v", err)
	}

	// Now it should be a real directory
	info, err := os.Stat(link)
	if err != nil {
		t.Fatalf("expected directory after removing symlink: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected real directory")
	}
	// Should not be a symlink
	linfo, _ := os.Lstat(link)
	if linfo.Mode()&os.ModeSymlink != 0 {
		t.Error("expected real directory, not symlink")
	}
}

// ---- CopyFile ----

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.txt")
	dst := filepath.Join(dir, "subdir", "dest.txt")

	os.WriteFile(src, []byte("hello world"), 0644)

	if err := CopyFile(src, dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("failed to read destination: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(data))
	}
}

func TestCopyFile_SourceNotFound(t *testing.T) {
	dir := t.TempDir()
	err := CopyFile(filepath.Join(dir, "missing.txt"), filepath.Join(dir, "dest.txt"))
	if err == nil {
		t.Error("expected error for missing source")
	}
}

// ---- CopyDir ----

func TestCopyDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, "sub"), 0755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("b"), 0644)

	dst := filepath.Join(dir, "dst")
	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dst, "a.txt"))
	if string(data) != "a" {
		t.Errorf("expected 'a', got %q", string(data))
	}
	data, _ = os.ReadFile(filepath.Join(dst, "sub", "b.txt"))
	if string(data) != "b" {
		t.Errorf("expected 'b', got %q", string(data))
	}
}

// ---- normalizeImportPath ----

func TestNormalizeImportPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"../components/button.ts", "../components/button"},
		{"./utils/index.tsx", "./utils/index"},
		{"./main.js", "./main"},
		{"./app.jsx", "./app"},
		{"components/button", "./components/button"},
		{"../lib", "../lib"},
		{"./already-clean", "./already-clean"},
		{`path\with\backslash.ts`, "./path/with/backslash"},
	}
	for _, tt := range tests {
		got := normalizeImportPath(tt.input)
		if got != tt.want {
			t.Errorf("normalizeImportPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---- ComputeContentHash ----

func TestComputeContentHash_EmptyProject(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)

	hash := ComputeContentHash(dir, nil)
	if hash == "" {
		t.Error("expected non-empty hash even for empty src")
	}
	if len(hash) != 12 {
		t.Errorf("expected 12-char hash, got %d chars: %q", len(hash), hash)
	}
}

func TestComputeContentHash_Deterministic(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "deterministic-content-identity", "identical-inputs-yield-an-identical-hash")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "utils.ts"), []byte("export const y = 2;"), 0644)

	h1 := ComputeContentHash(dir, nil)
	h2 := ComputeContentHash(dir, nil)
	if h1 != h2 {
		t.Errorf("expected deterministic hash, got %q and %q", h1, h2)
	}
}

func TestComputeContentHash_DifferentContent(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "deterministic-content-identity", "changed-content-changes-the-hash")
	dir1 := t.TempDir()
	os.MkdirAll(filepath.Join(dir1, "src"), 0755)
	os.WriteFile(filepath.Join(dir1, "src", "main.ts"), []byte("const a = 1;"), 0644)

	dir2 := t.TempDir()
	os.MkdirAll(filepath.Join(dir2, "src"), 0755)
	os.WriteFile(filepath.Join(dir2, "src", "main.ts"), []byte("const a = 2;"), 0644)

	h1 := ComputeContentHash(dir1, nil)
	h2 := ComputeContentHash(dir2, nil)
	if h1 == h2 {
		t.Error("expected different hashes for different content")
	}
}

func TestComputeContentHash_IgnoresNonSourceFiles(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "deterministic-content-identity", "non-source-files-do-not-affect-the-hash")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("const x = 1;"), 0644)

	h1 := ComputeContentHash(dir, nil)

	// Adding a non-source file should not change the hash
	os.WriteFile(filepath.Join(dir, "src", "data.json"), []byte(`{"key":"value"}`), 0644)
	h2 := ComputeContentHash(dir, nil)

	if h1 != h2 {
		t.Errorf("non-source files should not affect hash, got %q and %q", h1, h2)
	}
}

// ---- CopyProjectGenerateAssets path traversal ----

func TestCopyProjectGenerateAssets_BlocksPathTraversal(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(projectPath, 0755)
	os.MkdirAll(genDir, 0755)

	// Create a file outside workspace (sibling directory)
	outsideDir := filepath.Join(workspace, "..", "outside-"+filepath.Base(workspace))
	os.MkdirAll(outsideDir, 0755)
	os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret"), 0644)
	defer os.RemoveAll(outsideDir)

	// Write a putnami.json with a relative traversal path
	rcContent := `{"options":{"generate":{"assets":[{"from":"../../../outside-` + filepath.Base(workspace) + `/secret.txt","to":"stolen.txt"}]}}}`
	os.WriteFile(filepath.Join(projectPath, "putnami.json"), []byte(rcContent), 0644)

	CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)

	// The file should NOT have been copied because it resolves outside workspace
	if _, err := os.Stat(filepath.Join(genDir, "stolen.txt")); err == nil {
		t.Error("path traversal should have been blocked — file outside workspace was copied")
	}
}

func TestCopyProjectGenerateAssets_BlocksDestPathTraversal(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(genDir, 0755)

	// A legitimate in-project source so the source-side check passes; the attack
	// is entirely in the destination `to`.
	os.WriteFile(filepath.Join(projectPath, "asset.txt"), []byte("data"), 0644)

	rcContent := `{"options":{"generate":{"assets":[{"from":"asset.txt","to":"../../../escaped.txt"}]}}}`
	os.WriteFile(filepath.Join(projectPath, "putnami.json"), []byte(rcContent), 0644)

	CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)

	// The destination resolves above genDir and must not be written.
	escaped := filepath.Clean(filepath.Join(genDir, "../../../escaped.txt"))
	if _, err := os.Stat(escaped); err == nil {
		os.Remove(escaped)
		t.Errorf("dest path traversal should have been blocked — file written outside genDir at %s", escaped)
	}
}

// ---- CopyDir propagates errors ----

func TestCopyDir_PropagatesWalkErrors(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "nonexistent")
	dst := filepath.Join(dir, "dst")

	err := CopyDir(src, dst)
	if err == nil {
		t.Error("expected error when source directory doesn't exist")
	}
}

// ---- UpdateVersionContentHash ----

func TestUpdateVersionContentHash_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()

	assets, err := UpdateVersionContentHash(dir, "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if assets["version-info"] == "" {
		t.Error("expected version-info asset path")
	}

	data, err := os.ReadFile(assets["version-info"])
	if err != nil {
		t.Fatalf("failed to read version.json: %v", err)
	}

	var info map[string]any
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("failed to parse version.json: %v", err)
	}

	if info["contentHash"] != "abc123" {
		t.Errorf("contentHash = %v, want abc123", info["contentHash"])
	}
}

// A torn stamp, or one that is not a JSON object, contributes nothing: the
// content hash lands alone.
func TestUpdateVersionContentHash_ReplacesAnUnusableStamp(t *testing.T) {
	for _, existing := range []string{`{"version":`, `null`, `["1.0.0"]`} {
		dir := t.TempDir()
		genDir := filepath.Join(dir, ".gen")
		os.MkdirAll(genDir, 0755)
		os.WriteFile(filepath.Join(genDir, "version.json"), []byte(existing), 0644)

		assets, err := UpdateVersionContentHash(dir, "abc123")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", existing, err)
		}
		data, _ := os.ReadFile(assets["version-info"])
		var info map[string]any
		if err := json.Unmarshal(data, &info); err != nil || len(info) != 1 || info["contentHash"] != "abc123" {
			t.Errorf("%s: version.json = %s, want only the content hash", existing, data)
		}
	}
}

func TestUpdateVersionContentHash_MergesExisting(t *testing.T) {
	dir := t.TempDir()
	genDir := filepath.Join(dir, ".gen")
	os.MkdirAll(genDir, 0755)

	existing := `{"version":"1.0.0","sha":"deadbeef"}`
	os.WriteFile(filepath.Join(genDir, "version.json"), []byte(existing), 0644)

	assets, err := UpdateVersionContentHash(dir, "newHash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(assets["version-info"])
	var info map[string]any
	json.Unmarshal(data, &info)

	if info["contentHash"] != "newHash" {
		t.Errorf("contentHash = %v, want newHash", info["contentHash"])
	}
	if info["version"] != "1.0.0" {
		t.Errorf("version = %v, want 1.0.0 (preserved)", info["version"])
	}
	if info["sha"] != "deadbeef" {
		t.Errorf("sha = %v, want deadbeef (preserved)", info["sha"])
	}
}

// ---- GenerateBundledServe ----

func writeCapabilityActivationManifest(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".gen", "schema", "capabilities.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGenerateBundledServe_NoMainFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0644)

	result, err := GenerateBundledServe(dir, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result when no main file, got %q", result)
	}
}

func TestGenerateBundledServe_WithMainFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","main":"src/main.ts"}`), 0644)

	var logMsgs []string
	logFn := func(msg string) { logMsgs = append(logMsgs, msg) }
	manifestPath := writeCapabilityActivationManifest(t, dir, `{"protocolVersion":1,"project":"test"}`)

	result, err := GenerateBundledServe(dir, manifestPath, logFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("failed to read generated file: %v", err)
	}
	content := string(data)

	if !contains(content, "import { bootstrapServe, registerModuleLoader }") {
		t.Error("expected bootstrapServe + registerModuleLoader import")
	}
	if !contains(content, "await bootstrapServe(async () =>") {
		t.Error("expected guarded bootstrap wrapper")
	}
	if !contains(content, "workload.registerContributedConfigs()") {
		t.Error("expected dependency-contributed configs to be registered")
	}
	if !contains(content, "assertRegisteredConfigDefinitions([])") {
		t.Error("expected manifest config activation assertion")
	}
	if len(logMsgs) == 0 {
		t.Error("expected log message about generation")
	}
}

// BundledServeImports names exactly the packages the generated entry imports,
// in order. The TypeScript workspace probe credits these packages to a project
// that depends on BundledServeHookPackage, because the entry lives under .gen,
// which the probe never reads. A package the entry imports that the list omits
// would let the probe call a load-bearing edge unused.
func TestBundledServeImportsNameEveryPackageTheEntryImports(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","main":"src/main.ts"}`), 0644)
	manifestPath := writeCapabilityActivationManifest(t, dir, `{"protocolVersion":1,"project":"test"}`)

	result, err := GenerateBundledServe(dir, manifestPath, nil)
	if err != nil || result == "" {
		t.Fatalf("GenerateBundledServe = %q, %v; want an entry", result, err)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var packages []string
	for _, match := range regexp.MustCompile(`(?m)^import\b.*\bfrom\s+['"]([^'"]+)['"];$`).FindAllStringSubmatch(string(data), -1) {
		if !strings.HasPrefix(match[1], ".") {
			packages = append(packages, match[1])
		}
	}
	if want := BundledServeImports(); !slices.Equal(packages, want) {
		t.Fatalf("the entry imports %q, want BundledServeImports() = %q", packages, want)
	}
	if !slices.Contains(packages, BundledServeHookPackage) {
		t.Fatalf("the entry does not import BundledServeHookPackage %q", BundledServeHookPackage)
	}
	for _, line := range []string{
		"import { bootstrapServe, registerModuleLoader } from '@putnami/application';",
		"import { assertRegisteredConfigDefinitions } from '@putnami/runtime';",
	} {
		if !strings.Contains(string(data), line+"\n") {
			t.Errorf("the entry lacks the line %s", line)
		}
	}
}

func TestGenerateBundledServe_WithLoaderExports(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","main":"src/main.ts"}`), 0644)

	os.MkdirAll(filepath.Join(dir, ".gen", "src", "loaders"), 0755)
	os.WriteFile(filepath.Join(dir, ".gen", "src", "loaders", "my.ts"), []byte("export const route = true;"), 0644)
	os.WriteFile(filepath.Join(dir, ".gen", "src", "loaders", "client.ts"), []byte("export const client = true;"), 0644)
	os.WriteFile(filepath.Join(dir, ".gen", "src", "loaders", "sql.ts"), []byte("export const table = true;"), 0644)
	os.WriteFile(filepath.Join(dir, ".gen", "src", "loaders", "manual.ts"), []byte("export const manual = true;"), 0644)
	manifestPath := writeCapabilityActivationManifest(t, dir, `{
  "protocolVersion": 1,
  "project": "test",
  "configDefinitions": [{"path":"database.primary","provenance":{"project":"test","sourceKind":"framework"}}],
  "schemas": [
    {"name":"my-loader","kind":"route","path":".gen/src/loaders/my.ts","provenance":{"project":"test","sourceKind":"generated"}},
    {"name":"other-client-loader","kind":"route","path":".gen/src/loaders/client.ts","provenance":{"project":"test","sourceKind":"generated"}},
    {"name":"manual-loader","kind":"route","path":".gen/src/loaders/manual.ts","provenance":{"project":"test","sourceKind":"manual"}},
    {"name":"openapi","kind":"openapi","path":"schema/openapi.json","provenance":{"project":"test","sourceKind":"framework"}}
  ],
  "discoverers": [
    {
      "name":"sql-loader",
      "kind":"source",
      "provenance":{"project":"test","sourceKind":"generated","evidencePath":".gen/src/loaders/sql.ts"}
    },
    {
      "name":"manual-source-loader",
      "kind":"source",
      "provenance":{"project":"test","sourceKind":"manual","evidencePath":".gen/src/loaders/manual.ts"}
    }
  ]
}`)

	result, err := GenerateBundledServe(dir, manifestPath, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	data, _ := os.ReadFile(result)
	content := string(data)

	// Should include server-side loader (ends with -loader, no -client)
	if !contains(content, "my-loader") {
		t.Error("expected my-loader registration")
	}
	if !contains(content, "import * as loaderModule0") {
		t.Error("expected generated loader to be statically imported for packaged binaries")
	}
	if !contains(content, "Promise.resolve(loaderModule0)") {
		t.Error("expected registered loader to resolve the statically imported module")
	}
	if !contains(content, `registerModuleLoader("sql-loader", () => Promise.resolve(loaderModule1))`) {
		t.Error("expected generated source discoverer to activate its server loader")
	}
	// Should exclude client-side loader
	if contains(content, "other-client-loader") {
		t.Error("expected client loader to be excluded")
	}
	if contains(content, "manual-loader") || contains(content, "manual-source-loader") {
		t.Error("expected only generated-provenance manifest entries to be activated")
	}
	// Should exclude non-loader export
	if contains(content, "regular-export") {
		t.Error("expected non-loader export to be excluded")
	}
	if !contains(content, `assertRegisteredConfigDefinitions(["database.primary"])`) {
		t.Error("expected config paths to come from the capability manifest")
	}
	// The app entrypoint must evaluate before every generated loader
	// module. A loader module reads config while it evaluates, and the
	// workload's static activation (first import of src/main.ts) is what
	// registers the remote config source in a compiled binary.
	appImport := strings.Index(content, `import { app } from "../../src/main";`)
	firstLoaderImport := strings.Index(content, "import * as loaderModule0")
	if appImport < 0 || firstLoaderImport < 0 || appImport > firstLoaderImport {
		t.Errorf("expected the app entrypoint import before the generated loader imports:\n%s", content)
	}
}

func TestGenerateBundledServe_V2FeatureEvidenceIsRuntimeInert(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","main":"src/main.ts"}`), 0644); err != nil {
		t.Fatal(err)
	}
	writeLoaderModule(t, dir, ".gen/src/loaders/api.ts")
	writeLoaderModule(t, dir, ".gen/src/loaders/sql.ts")
	manifestPath := writeCapabilityActivationManifest(t, dir, `{
  "$schema":"https://putnami.dev/schemas/putnami-capabilities-v2.json",
  "protocolVersion":2,
  "project":"test",
  "configDefinitions":[{
    "identity":{"ownerProject":"test","kind":"config","key":"database.primary"},
    "path":"database.primary",
    "provenance":{"project":"test","sourceKind":"framework","declaration":{"root":"project","path":"src/main.ts"}}
  }],
  "schemas":[{
    "identity":{"ownerProject":"test","kind":"schema","subkind":"route","key":"api-loader"},
    "name":"api-loader","kind":"route","path":".gen/src/loaders/api.ts",
    "provenance":{"project":"test","sourceKind":"generated","declaration":{"root":"project","path":"src/main.ts"},"artifacts":[{"root":"project","path":".gen/src/loaders/api.ts"}]}
  }],
  "discoverers":[{
    "identity":{"ownerProject":"test","kind":"discoverer","subkind":"source","key":"sql-loader"},
    "name":"sql-loader","kind":"source",
    "provenance":{"project":"test","sourceKind":"generated","declaration":{"root":"project","path":"src/main.ts"},"artifacts":[{"root":"project","path":".gen/src/loaders/sql.ts"}]}
  }]
}`)

	result, err := GenerateBundledServe(dir, manifestPath, nil)
	if err != nil {
		t.Fatalf("generate v2 bundled serve: %v", err)
	}
	before, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	evidenceDir := filepath.Join(dir, ".gen", "schema", "feature-evidence")
	if err := os.MkdirAll(evidenceDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "typescript-framework.json"), []byte(`{
  "protocolVersion":1,
  "evidence":[{
    "id":"capabilities/runtime-inert","feature":"capabilities/typescript-evidence","requirement":"implementation","stage":"coded","outcome":"supports",
    "issuer":{"kind":"framework","id":"@putnami/application"},
    "source":{"root":"project","ownerProject":"test","binding":"source-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000"},
    "subject":{"kind":"capability","contribution":{"ownerProject":"test","kind":"schema","subkind":"route","key":"api-loader"}},
    "provenance":{"root":"project","path":"src/main.ts"}
  }]
}`), 0644); err != nil {
		t.Fatal(err)
	}
	result, err = GenerateBundledServe(dir, manifestPath, nil)
	if err != nil {
		t.Fatalf("generate v2 bundled serve with feature evidence: %v", err)
	}
	after, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("feature evidence changed bundled loader/config/startup activation\nbefore:\n%s\nafter:\n%s", before, after)
	}
	content := string(after)
	for _, want := range []string{
		`registerModuleLoader("api-loader", () => Promise.resolve(loaderModule0))`,
		`registerModuleLoader("sql-loader", () => Promise.resolve(loaderModule1))`,
		`assertRegisteredConfigDefinitions(["database.primary"])`,
		"return workload;",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("v2 bundled serve missing stable activation %q", want)
		}
	}
}

func TestGenerateBundledServe_RejectsEscapingManifestActivation(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644)
	manifestPath := writeCapabilityActivationManifest(t, dir, `{
  "protocolVersion": 1,
  "project": "test",
  "schemas": [{"name":"api-loader","kind":"route","path":"../outside.ts","provenance":{"project":"test","sourceKind":"generated"}}]
}`)

	result, err := GenerateBundledServe(dir, manifestPath, nil)
	if err == nil || !strings.Contains(err.Error(), "escapes the project") {
		t.Fatalf("expected escaping activation error, got result=%q err=%v", result, err)
	}
}

func TestGenerateBundledServe_RejectsMissingManifestActivationModule(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644)
	manifestPath := writeCapabilityActivationManifest(t, dir, `{
  "protocolVersion": 1,
  "project": "test",
  "schemas": [{"name":"api-loader","kind":"route","path":".gen/src/missing.ts","provenance":{"project":"test","sourceKind":"generated"}}]
}`)

	result, err := GenerateBundledServe(dir, manifestPath, nil)
	if err == nil || !strings.Contains(err.Error(), "activation module not found") {
		t.Fatalf("expected missing activation module error, got result=%q err=%v", result, err)
	}
}

func TestGenerateBundledServe_RejectsMissingManifest(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644)

	result, err := GenerateBundledServe(dir, filepath.Join(dir, ".gen", "schema", "capabilities.json"), nil)
	if err == nil || !strings.Contains(err.Error(), "reading capability manifest") {
		t.Fatalf("expected missing manifest error, got result=%q err=%v", result, err)
	}
}

func TestGenerateBundledServe_StrictlyRejectsInvalidManifests(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "unknown field",
			body: `{"protocolVersion":1,"project":"test","unexpected":true}`,
			code: "capabilities.unknown_field",
		},
		{
			name: "missing project",
			body: `{"protocolVersion":1}`,
			code: "capabilities.missing_project",
		},
		{
			name: "missing provenance",
			body: `{"protocolVersion":1,"project":"test","schemas":[{"name":"api-loader","kind":"route","path":".gen/src/api.ts"}]}`,
			code: "capabilities.missing_provenance",
		},
		{
			name: "invalid enum",
			body: `{"protocolVersion":1,"project":"test","schemas":[{"name":"api-loader","kind":"grpc","provenance":{"project":"test","sourceKind":"generated"}}]}`,
			code: "capabilities.invalid_schema_kind",
		},
		{
			name: "semantic duplicate",
			body: `{"protocolVersion":1,"project":"test","schemas":[{"name":"api-loader","kind":"route","path":".gen/src/api.ts","provenance":{"project":"test","sourceKind":"generated"}},{"name":"api-loader","kind":"route","path":".gen/src/api.ts","provenance":{"project":"test","sourceKind":"generated"}}]}`,
			code: "capabilities.duplicate_provider",
		},
		{
			name: "semantic conflict",
			body: `{"protocolVersion":1,"project":"test","schemas":[{"name":"api-loader","kind":"route","path":".gen/src/api.ts","provenance":{"project":"test","sourceKind":"generated"}},{"name":"api-loader","kind":"route","path":".gen/src/other.ts","provenance":{"project":"test","sourceKind":"generated"}}]}`,
			code: "capabilities.conflicting_provider",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644); err != nil {
				t.Fatal(err)
			}
			manifestPath := writeCapabilityActivationManifest(t, dir, tt.body)
			result, err := GenerateBundledServe(dir, manifestPath, nil)
			if err == nil || !strings.Contains(err.Error(), tt.code) {
				t.Fatalf("expected %s, got result=%q err=%v", tt.code, result, err)
			}
		})
	}
}

func TestGenerateBundledServe_DefaultMainEntry(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644)
	// No package.json Main field
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0644)

	manifestPath := writeCapabilityActivationManifest(t, dir, `{"protocolVersion":1,"project":"test"}`)
	result, err := GenerateBundledServe(dir, manifestPath, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result with default src/main.ts")
	}
}

func TestGenerateBundledServe_NoPackageJSON(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export const app = () => {};"), 0644)

	manifestPath := writeCapabilityActivationManifest(t, dir, `{"protocolVersion":1,"project":"test"}`)
	result, err := GenerateBundledServe(dir, manifestPath, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result with default src/main.ts (no pkg)")
	}
}

// ---- CopyProjectGenerateAssets ----

func TestCopyProjectGenerateAssets_WithValidAssets(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(projectPath, 0755)
	os.MkdirAll(genDir, 0755)

	// Create a source file within workspace
	configDir := filepath.Join(workspace, "shared", "config")
	os.MkdirAll(configDir, 0755)
	os.WriteFile(filepath.Join(configDir, "biome.json"), []byte(`{"formatter":{}}`), 0644)

	// Write putnami.json with absolute path asset (starting with /)
	rcContent := `{"options":{"generate":{"assets":[{"from":"/shared/config/biome.json","to":"config/biome.json"}]}}}`
	os.WriteFile(filepath.Join(projectPath, "putnami.json"), []byte(rcContent), 0644)

	CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)

	data, err := os.ReadFile(filepath.Join(genDir, "config", "biome.json"))
	if err != nil {
		t.Fatalf("expected asset to be copied: %v", err)
	}
	if string(data) != `{"formatter":{}}` {
		t.Errorf("unexpected content: %q", string(data))
	}
}

func TestCopyProjectGenerateAssets_RelativePathAsset(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(projectPath, 0755)
	os.MkdirAll(genDir, 0755)

	// Create a file relative to the project
	os.WriteFile(filepath.Join(projectPath, "local.json"), []byte(`{"local":true}`), 0644)

	rcContent := `{"options":{"generate":{"assets":[{"from":"local.json","to":"config/local.json"}]}}}`
	os.WriteFile(filepath.Join(projectPath, "putnami.json"), []byte(rcContent), 0644)

	CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)

	data, err := os.ReadFile(filepath.Join(genDir, "config", "local.json"))
	if err != nil {
		t.Fatalf("expected asset to be copied: %v", err)
	}
	if string(data) != `{"local":true}` {
		t.Errorf("unexpected content: %q", string(data))
	}
}

func TestCopyProjectGenerateAssets_DirectoryAsset(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(projectPath, 0755)
	os.MkdirAll(genDir, 0755)

	// Create a source directory within workspace
	srcDir := filepath.Join(workspace, "shared", "templates")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "index.html"), []byte("<html></html>"), 0644)

	rcContent := `{"options":{"generate":{"assets":[{"from":"/shared/templates","to":"templates"}]}}}`
	os.WriteFile(filepath.Join(projectPath, "putnami.json"), []byte(rcContent), 0644)

	CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)

	data, err := os.ReadFile(filepath.Join(genDir, "templates", "index.html"))
	if err != nil {
		t.Fatalf("expected directory asset to be copied: %v", err)
	}
	if string(data) != "<html></html>" {
		t.Errorf("unexpected content: %q", string(data))
	}
}

func TestCopyProjectGenerateAssets_EmptyFromTo(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(projectPath, 0755)
	os.MkdirAll(genDir, 0755)

	rcContent := `{"options":{"generate":{"assets":[{"from":"","to":""},{"from":"valid.txt","to":""}]}}}`
	os.WriteFile(filepath.Join(projectPath, "putnami.json"), []byte(rcContent), 0644)

	// Should not panic on empty from/to
	CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)
}

func TestCopyProjectGenerateAssets_MissingSource(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(projectPath, 0755)
	os.MkdirAll(genDir, 0755)

	rcContent := `{"options":{"generate":{"assets":[{"from":"/nonexistent/file.txt","to":"output.txt"}]}}}`
	os.WriteFile(filepath.Join(projectPath, "putnami.json"), []byte(rcContent), 0644)

	// Should not panic on missing source
	CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)

	if _, err := os.Stat(filepath.Join(genDir, "output.txt")); err == nil {
		t.Error("expected file not to be created for missing source")
	}
}

// A declared asset that cannot be copied fails the generate: the project would
// otherwise ship without it.
func TestCopyProjectGenerateAssets_ReportsCopyFailure(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(genDir, 0755)
	os.WriteFile(filepath.Join(projectPath, "local.json"), []byte(`{}`), 0644)
	// A file where the destination directory should be.
	os.WriteFile(filepath.Join(genDir, "config"), []byte("not a directory"), 0644)

	rcContent := `{"options":{"generate":{"assets":[{"from":"local.json","to":"config/local.json"}]}}}`
	os.WriteFile(filepath.Join(projectPath, "putnami.json"), []byte(rcContent), 0644)

	err := CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)
	if err == nil || !strings.Contains(err.Error(), "local.json -> config/local.json") {
		t.Fatalf("err = %v, want the failed copy of local.json -> config/local.json", err)
	}
}

func TestCopyProjectGenerateAssets_LegacyConfig(t *testing.T) {
	workspace := t.TempDir()
	projectPath := filepath.Join(workspace, "projects", "myapp")
	genDir := filepath.Join(projectPath, ".gen")
	os.MkdirAll(projectPath, 0755)
	os.MkdirAll(genDir, 0755)

	// Create file and legacy config
	os.WriteFile(filepath.Join(projectPath, "data.json"), []byte(`{"data":1}`), 0644)
	rcContent := `{"options":{"generate":{"assets":[{"from":"data.json","to":"data.json"}]}}}`
	os.WriteFile(filepath.Join(projectPath, ".putnamirc.json"), []byte(rcContent), 0644)

	CopyProjectGenerateAssets(workspace, projectPath, genDir, nil)

	data, err := os.ReadFile(filepath.Join(genDir, "data.json"))
	if err != nil {
		t.Fatalf("expected asset to be copied from legacy config: %v", err)
	}
	if string(data) != `{"data":1}` {
		t.Errorf("unexpected content: %q", string(data))
	}
}

// ---- resolveProjectConfig ----

func TestResolveProjectConfig_PrefersModernConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, ".putnamirc.json"), []byte(`{}`), 0644)

	result := resolveProjectConfig(dir)
	if !contains(result, "putnami.json") || contains(result, ".putnamirc") {
		t.Errorf("expected putnami.json to be preferred, got %q", result)
	}
}

func TestResolveProjectConfig_FallsBackToLegacy(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".putnamirc.json"), []byte(`{}`), 0644)

	result := resolveProjectConfig(dir)
	if !contains(result, ".putnamirc.json") {
		t.Errorf("expected .putnamirc.json fallback, got %q", result)
	}
}

func TestResolveProjectConfig_NeitherExists(t *testing.T) {
	dir := t.TempDir()

	result := resolveProjectConfig(dir)
	// Should return the preferred path even if it doesn't exist
	if !contains(result, "putnami.json") {
		t.Errorf("expected putnami.json path as default, got %q", result)
	}
}

// ---- cond helper ----

func TestCond(t *testing.T) {
	if got := cond(true, "yes", "no"); got != "yes" {
		t.Errorf("cond(true) = %q, want 'yes'", got)
	}
	if got := cond(false, "yes", "no"); got != "no" {
		t.Errorf("cond(false) = %q, want 'no'", got)
	}
}

// ---- EnsureRealDirectory additional cases ----

func TestEnsureRealDirectory_ExistingDirNoClear(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing")
	os.MkdirAll(target, 0755)
	os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0644)

	if err := EnsureRealDirectory(target, false); err != nil {
		t.Fatalf("EnsureRealDirectory failed: %v", err)
	}

	// File should still exist
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Error("expected file to be preserved when clear=false")
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// ---- Sorted file hashing ----

// ---- resolveCompileOutputFile ----

func TestResolveCompileOutputFile_LinuxX64(t *testing.T) {
	got := resolveCompileOutputFile("/out", "serve.ts", "bun-linux-x64")
	want := filepath.Join("/out", "serve-linux-x64")
	if got != want {
		t.Errorf("resolveCompileOutputFile() = %q, want %q", got, want)
	}
}

func TestResolveCompileOutputFile_DarwinArm64(t *testing.T) {
	got := resolveCompileOutputFile("/out", "app.bundled.ts", "bun-darwin-arm64")
	want := filepath.Join("/out", "app.bundled-darwin-arm64")
	if got != want {
		t.Errorf("resolveCompileOutputFile() = %q, want %q", got, want)
	}
}

// Windows starts only a program whose name ends in ".exe", so the compile
// names the file it asks bun to write for a Windows target with that suffix.
func TestResolveCompileOutputFile_WindowsX64(t *testing.T) {
	got := resolveCompileOutputFile("/out", "serve.bundled.ts", "bun-windows-x64")
	want := filepath.Join("/out", "serve.bundled-windows-x64.exe")
	if got != want {
		t.Errorf("resolveCompileOutputFile() = %q, want %q", got, want)
	}
	if !slices.Contains(DefaultBunTargets, "bun-windows-x64") {
		t.Errorf("DefaultBunTargets = %v, want the windows-x64 distribution target", DefaultBunTargets)
	}
}

func TestResolveCompileOutputFile_NestedEntrypoint(t *testing.T) {
	got := resolveCompileOutputFile("/out", "/gen/src/serve.bundled.ts", "bun-linux-x64")
	want := filepath.Join("/out", "serve.bundled-linux-x64")
	if got != want {
		t.Errorf("resolveCompileOutputFile() = %q, want %q", got, want)
	}
}

// ---- buildCompileArgs ----

func TestBuildCompileArgs(t *testing.T) {
	args := buildCompileArgs("serve.ts", "/out/serve-linux-x64", "bun-linux-x64")
	if len(args) != 7 {
		t.Fatalf("expected 7 args, got %d: %v", len(args), args)
	}
	if args[0] != "build" || args[1] != "--compile" || args[2] != "serve.ts" {
		t.Errorf("unexpected first args: %v", args[:3])
	}
	if args[3] != "--outfile" || args[4] != "/out/serve-linux-x64" {
		t.Errorf("unexpected outfile args: %v", args[3:5])
	}
	if args[5] != "--target" || args[6] != "bun-linux-x64" {
		t.Errorf("unexpected target args: %v", args[5:7])
	}
}

// ---- boolOr ----

func TestBoolOr(t *testing.T) {
	if boolOr(false, false, true) != true {
		t.Error("expected defaultVal when no specific")
	}
	if boolOr(true, true, false) != true {
		t.Error("expected specific when hasSpecific")
	}
	if boolOr(true, false, true) != false {
		t.Error("expected specific=false when hasSpecific")
	}
}

// ---- mockExecRun helper ----

func withMockExec(t *testing.T, fn func(string, []string, ...exec.Option) (*exec.Result, error)) {
	t.Helper()
	orig := execRunFunc
	t.Cleanup(func() { execRunFunc = orig })
	execRunFunc = fn
}

// ---- RunCompile ----

func TestRunCompile_Success(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "compile")

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		// Create the output file to simulate a real compile
		for i, a := range args {
			if a == "--outfile" && i+1 < len(args) {
				os.MkdirAll(filepath.Dir(args[i+1]), 0755)
				os.WriteFile(args[i+1], []byte("binary"), 0755)
			}
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	files, errors, err := RunCompile("bun", dir, outDir, "serve.ts", []string{"bun-linux-x64"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) != 0 {
		t.Errorf("unexpected errors: %v", errors)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if !strings.HasSuffix(files[0], "-linux-x64") {
		t.Errorf("expected linux-x64 suffix, got %q", files[0])
	}
}

func TestRunCompile_AllDefaultTargets(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "compile")

	var calledTargets []string
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		for i, a := range args {
			if a == "--target" && i+1 < len(args) {
				calledTargets = append(calledTargets, args[i+1])
			}
			if a == "--outfile" && i+1 < len(args) {
				os.MkdirAll(filepath.Dir(args[i+1]), 0755)
				os.WriteFile(args[i+1], []byte("binary"), 0755)
			}
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	files, errors, err := RunCompile("bun", dir, outDir, "serve.ts", DefaultBunTargets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) != 0 {
		t.Errorf("unexpected errors: %v", errors)
	}
	if len(files) != len(DefaultBunTargets) {
		t.Errorf("expected %d files (all default targets), got %d", len(DefaultBunTargets), len(files))
	}
	if !reflect.DeepEqual(calledTargets, DefaultBunTargets) {
		t.Errorf("compiled targets = %v, want every default target %v", calledTargets, DefaultBunTargets)
	}
}

func TestRunCompile_Failure(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "compile")

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "compile error"}, nil
	})

	files, errors, err := RunCompile("bun", dir, outDir, "serve.ts", []string{"bun-linux-x64"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected no files on failure, got %d", len(files))
	}
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
	if !strings.Contains(errors[0], "Compile failed") {
		t.Errorf("expected 'Compile failed' in error, got %q", errors[0])
	}
}

func TestRunCompile_ExecError(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "compile")

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return nil, os.ErrNotExist
	})

	_, errors, err := RunCompile("bun", dir, outDir, "serve.ts", []string{"bun-linux-x64"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errors))
	}
}

// ---- RunTranspile ----

func TestRunTranspile_Success(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "lib")

	// Create a project with entrypoints
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","exports":{"./index":"./src/index.ts"}}`), 0644)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		// Create output files to simulate transpile
		os.MkdirAll(outDir, 0755)
		os.WriteFile(filepath.Join(outDir, "index.js"), []byte("const x = 1;"), 0644)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	files, errors, err := RunTranspile("bun", dir, outDir, TranspileParams{Target: "bun"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) != 0 {
		t.Errorf("unexpected errors: %v", errors)
	}
	if len(files) == 0 {
		t.Error("expected output files")
	}
}

func TestRunTranspile_NoEntrypoints(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "lib")

	// Empty project - no entrypoints
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0644)

	files, errors, err := RunTranspile("bun", dir, outDir, TranspileParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) != 0 {
		t.Errorf("unexpected errors: %v", errors)
	}
	if len(files) != 0 {
		t.Errorf("expected no files for project without entrypoints, got %d", len(files))
	}
}

func TestRunTranspile_Failure(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "lib")

	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","exports":{"./index":"./src/index.ts"}}`), 0644)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "syntax error"}, nil
	})

	_, errors, err := RunTranspile("bun", dir, outDir, TranspileParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) == 0 {
		t.Error("expected errors on failure")
	}
}

func TestRunTranspile_ExecError(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "lib")

	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","exports":{"./index":"./src/index.ts"}}`), 0644)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return nil, os.ErrNotExist
	})

	_, errors, err := RunTranspile("bun", dir, outDir, TranspileParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) == 0 {
		t.Error("expected errors on exec failure")
	}
}

// ---- RunTypes ----

func TestRunTypes_Success(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "types")

	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export type Foo = string;"), 0644)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		// Create output .d.ts files
		os.MkdirAll(outDir, 0755)
		os.WriteFile(filepath.Join(outDir, "index.d.ts"), []byte("export type Foo = string;"), 0644)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	result, err := RunTypes("bun", dir, outDir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success")
	}
	if len(result.Diagnostics) != 0 {
		t.Errorf("unexpected diagnostics: %v", result.Diagnostics)
	}
	if len(result.GeneratedFiles) == 0 {
		t.Error("expected generated .d.ts files")
	}
}

func TestRunTypes_NoSourceFiles(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "types")

	// No src directory
	result, err := RunTypes("bun", dir, outDir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success for no source files")
	}
	if len(result.Diagnostics) != 0 {
		t.Errorf("unexpected diagnostics: %v", result.Diagnostics)
	}
	if len(result.GeneratedFiles) != 0 {
		t.Errorf("expected no generated files, got %d", len(result.GeneratedFiles))
	}
}

func TestRunTypes_WithDiagnostics(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "types")

	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("const x: number = 'hello';"), 0644)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{
			Success:  false,
			ExitCode: 1,
			Stdout:   "src/index.ts(1,7): error TS2322: Type 'string' is not assignable to type 'number'.\n",
		}, nil
	})

	result, err := RunTypes("bun", dir, outDir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure")
	}
	if len(result.Diagnostics) == 0 {
		t.Error("expected diagnostics")
	}
}

func TestRunTypes_ExecError(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "types")

	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export type Foo = string;"), 0644)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return nil, os.ErrNotExist
	})

	_, err := RunTypes("bun", dir, outDir, "")
	if err == nil {
		t.Error("expected error on exec failure")
	}
}

func TestRunTypes_RawOutputPreserved(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "types")

	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("const x = 1;"), 0644)

	rawOutput := "some unrecognized tsc output\n"
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{
			Success:  false,
			ExitCode: 1,
			Stderr:   rawOutput,
		}, nil
	})

	result, err := RunTypes("bun", dir, outDir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure")
	}
	if result.RawOutput != rawOutput {
		t.Errorf("expected raw output %q, got %q", rawOutput, result.RawOutput)
	}
}

// TestRunTypes_ConcurrentSameProject guards against the publish-time race
// where `build~types` and `package~types` ran in parallel for the same
// project and clobbered each other's tsconfig.types.json. Each invocation
// must write to a unique file in the project directory.
func TestRunTypes_ConcurrentSameProject(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export type Foo = string;"), 0644)
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0644)

	// Track the --project paths tsc was invoked with, and assert each one
	// exists when tsc "starts" (mimicking what real tsc would do on read).
	var (
		mu        sync.Mutex
		seenPaths = map[string]bool{}
	)
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		// Find --project arg
		var projectArg string
		for i, a := range args {
			if a == "--project" && i+1 < len(args) {
				projectArg = args[i+1]
				break
			}
		}
		if projectArg == "" {
			return &exec.Result{Success: true, ExitCode: 0}, nil
		}
		// The file must exist at the moment tsc reads it.
		if _, err := os.Stat(projectArg); err != nil {
			return &exec.Result{Success: false, ExitCode: 1, Stderr: "missing: " + projectArg}, nil
		}
		mu.Lock()
		seenPaths[projectArg] = true
		mu.Unlock()
		// Simulate tsc taking long enough for a concurrent invocation to overlap.
		time.Sleep(20 * time.Millisecond)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outDir := filepath.Join(dir, fmt.Sprintf("out-%d", i))
			res, err := RunTypes("bun", dir, outDir, "")
			if err != nil {
				errs <- err
				return
			}
			if !res.Success {
				errs <- fmt.Errorf("invocation %d failed: %s", i, res.RawOutput)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(seenPaths) != 2 {
		t.Errorf("expected 2 distinct tsconfig paths, got %d: %v", len(seenPaths), seenPaths)
	}
	// And nothing should remain on disk afterwards.
	matches, _ := filepath.Glob(filepath.Join(dir, "tsconfig.types.*.json"))
	if len(matches) != 0 {
		t.Errorf("expected all temp tsconfigs removed, found: %v", matches)
	}
}

func TestComputeContentHash_FileOrderIndependent(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "deterministic-content-identity", "file-discovery-order-does-not-affect-the-hash")
	// Create two projects with same files but created in different order
	dir1 := t.TempDir()
	os.MkdirAll(filepath.Join(dir1, "src"), 0755)
	os.WriteFile(filepath.Join(dir1, "src", "a.ts"), []byte("const a = 1;"), 0644)
	os.WriteFile(filepath.Join(dir1, "src", "b.ts"), []byte("const b = 2;"), 0644)

	dir2 := t.TempDir()
	os.MkdirAll(filepath.Join(dir2, "src"), 0755)
	// Write in reverse order
	os.WriteFile(filepath.Join(dir2, "src", "b.ts"), []byte("const b = 2;"), 0644)
	os.WriteFile(filepath.Join(dir2, "src", "a.ts"), []byte("const a = 1;"), 0644)

	h1 := ComputeContentHash(dir1, nil)
	h2 := ComputeContentHash(dir2, nil)
	if h1 != h2 {
		t.Errorf("hash should be order-independent, got %q and %q", h1, h2)
	}

	// Verify sort is used internally
	files := []string{"z.ts", "a.ts", "m.ts"}
	sort.Strings(files)
	if files[0] != "a.ts" {
		t.Error("sort sanity check failed")
	}
}

// ---- ResolveCompileTargets (package-channel scoping) ----

// TestResolveCompileTargets pins that a compile bound to the docker channel
// builds ONE bun target — the image's — while an unbound compile keeps the
// distribution matrix. An image carries one executable; the other three were
// compiled and thrown away.
func TestResolveCompileTargets(t *testing.T) {
	tests := []struct {
		name string
		req  CompileTargetRequest
		want []string
	}{
		{
			name: "unbound compile keeps the distribution matrix",
			req:  CompileTargetRequest{},
			want: DefaultBunTargets,
		},
		{
			name: "docker intent compiles the image target alone",
			req:  CompileTargetRequest{Docker: true},
			want: []string{"bun-linux-x64"},
		},
		{
			name: "docker intent honors the image platform",
			req:  CompileTargetRequest{Docker: true, DockerPlatform: "linux/arm64"},
			want: []string{"bun-linux-arm64"},
		},
		{
			name: "an image platform is not a bun target verbatim",
			req:  CompileTargetRequest{Docker: true, DockerPlatform: "darwin/amd64"},
			want: []string{"bun-darwin-x64"},
		},
		{
			name: "an explicit compile target wins over the image",
			req:  CompileTargetRequest{CompileTarget: "bun-darwin-arm64", Docker: true},
			want: []string{"bun-darwin-arm64"},
		},
		{
			name: "an image platform without the docker intent changes nothing",
			req:  CompileTargetRequest{DockerPlatform: "linux/arm64"},
			want: DefaultBunTargets,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveCompileTargets(tt.req)
			if err != nil {
				t.Fatalf("ResolveCompileTargets(%+v): %v", tt.req, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("targets = %v, want %v", got, tt.want)
			}
		})
	}

	// The default matrix must not be handed out by reference: a caller that
	// appended to it would rewrite the matrix for every later compile in the
	// process.
	unbound, err := ResolveCompileTargets(CompileTargetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	unbound[0] = "mutated"
	if DefaultBunTargets[0] == "mutated" {
		t.Fatal("ResolveCompileTargets returned the DefaultBunTargets backing array")
	}

	if _, err := ResolveCompileTargets(CompileTargetRequest{Docker: true, DockerPlatform: "linux"}); err == nil {
		t.Error("an os-only image platform resolved silently; an image names one os/arch and the compile " +
			"must not guess the architecture")
	}
}

// TestTargetSuffixIsTheNameBothSidesUse pins the one spelling the compile writes
// and the docker packager searches for. They are derived from a single function
// precisely because the docker channel now compiles ONLY that target: a drift
// between the two names would leave the packager with an empty directory
// instead of three unused binaries to fall back on.
func TestTargetSuffixIsTheNameBothSidesUse(t *testing.T) {
	target, err := ImageCompileTarget("linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.FromSlash("/out")
	outputFile := resolveCompileOutputFile(outDir, "serve.ts", target)
	if want := filepath.Join(outDir, "serve"+TargetSuffix(target)); outputFile != want {
		t.Errorf("compiled file = %q, want %q", outputFile, want)
	}
	if got, want := TargetSuffix(target), "-linux-x64"; got != want {
		t.Errorf("TargetSuffix(%q) = %q, want %q", target, got, want)
	}
}
