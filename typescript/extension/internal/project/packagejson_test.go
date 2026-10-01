package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- StripPreReleaseSuffix ----

func TestStripPreReleaseSuffix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"1.2.3", "1.2.3"},
		{"1.2.3-alpha.1", "1.2.3"},
		{"1.2.3-beta", "1.2.3"},
		{"1.2.3-rc.0+build.123", "1.2.3"},
		{"", ""},
		{"0.0.1-dev", "0.0.1"},
	}

	for _, tt := range tests {
		got := StripPreReleaseSuffix(tt.input)
		if got != tt.expected {
			t.Errorf("StripPreReleaseSuffix(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// ---- RelativePath ----

func TestRelativePath(t *testing.T) {
	tests := []struct {
		base, target, expected string
	}{
		{"/a/b", "/a/b/c/d.ts", "c/d.ts"},
		{"/a/b/c", "/a/b/d.ts", "../d.ts"},
		{"/a/b", "/a/b", "."},
	}

	for _, tt := range tests {
		// RelativePath answers in the host's form, as filepath.Rel does.
		got := RelativePath(filepath.FromSlash(tt.base), filepath.FromSlash(tt.target))
		if got != filepath.FromSlash(tt.expected) {
			t.Errorf("RelativePath(%q, %q) = %q, want %q", tt.base, tt.target, got, tt.expected)
		}
	}
}

// ---- ResolveExportPath ----

func TestResolveExportPath_Nil(t *testing.T) {
	result := ResolveExportPath(nil, "./serve")
	if result != "" {
		t.Errorf("expected empty string for nil exports, got %q", result)
	}
}

func TestResolveExportPath_InvalidJSON(t *testing.T) {
	result := ResolveExportPath(json.RawMessage("not json"), "./serve")
	if result != "" {
		t.Errorf("expected empty string for invalid JSON, got %q", result)
	}
}

func TestResolveExportPath_MissingKey(t *testing.T) {
	exports := json.RawMessage(`{"./index": "./dist/index.js"}`)
	result := ResolveExportPath(exports, "./serve")
	if result != "" {
		t.Errorf("expected empty string for missing key, got %q", result)
	}
}

func TestResolveExportPath_StringValue(t *testing.T) {
	exports := json.RawMessage(`{"./serve": "./dist/serve.js"}`)
	result := ResolveExportPath(exports, "./serve")
	if result != "./dist/serve.js" {
		t.Errorf("expected './dist/serve.js', got %q", result)
	}
}

func TestResolveExportPath_ObjectWithDefault(t *testing.T) {
	exports := json.RawMessage(`{"./serve": {"default": "./dist/serve.js", "bun": "./dist/serve.bun.js"}}`)
	result := ResolveExportPath(exports, "./serve")
	if result != "./dist/serve.js" {
		t.Errorf("expected './dist/serve.js' (default wins), got %q", result)
	}
}

func TestResolveExportPath_ObjectWithBunOnly(t *testing.T) {
	exports := json.RawMessage(`{"./serve": {"bun": "./dist/serve.bun.js", "node": "./dist/serve.node.js"}}`)
	result := ResolveExportPath(exports, "./serve")
	if result != "./dist/serve.bun.js" {
		t.Errorf("expected './dist/serve.bun.js' (bun priority), got %q", result)
	}
}

func TestResolveExportPath_ObjectWithNodeOnly(t *testing.T) {
	exports := json.RawMessage(`{"./serve": {"node": "./dist/serve.node.js"}}`)
	result := ResolveExportPath(exports, "./serve")
	if result != "./dist/serve.node.js" {
		t.Errorf("expected './dist/serve.node.js', got %q", result)
	}
}

// ---- PackageJSON GetBinMap / GetBinString ----

func TestGetBinMap_StringBin(t *testing.T) {
	pkg := &PackageJSON{Bin: json.RawMessage(`"./bin/cli.js"`)}
	m := pkg.GetBinMap()
	if m != nil {
		t.Errorf("expected nil for string bin, got %v", m)
	}
}

func TestGetBinMap_ObjectBin(t *testing.T) {
	pkg := &PackageJSON{Bin: json.RawMessage(`{"mycli": "./bin/cli.js"}`)}
	m := pkg.GetBinMap()
	if m == nil {
		t.Fatal("expected non-nil map for object bin")
	}
	if m["mycli"] != "./bin/cli.js" {
		t.Errorf("expected './bin/cli.js', got %q", m["mycli"])
	}
}

func TestGetBinMap_NilBin(t *testing.T) {
	pkg := &PackageJSON{}
	if pkg.GetBinMap() != nil {
		t.Error("expected nil for nil bin")
	}
}

func TestGetBinString_StringBin(t *testing.T) {
	pkg := &PackageJSON{Bin: json.RawMessage(`"./bin/cli.js"`)}
	s := pkg.GetBinString()
	if s != "./bin/cli.js" {
		t.Errorf("expected './bin/cli.js', got %q", s)
	}
}

func TestGetBinString_ObjectBin(t *testing.T) {
	pkg := &PackageJSON{Bin: json.RawMessage(`{"cli": "./bin/cli.js"}`)}
	s := pkg.GetBinString()
	if s != "" {
		t.Errorf("expected empty string for object bin, got %q", s)
	}
}

func TestGetBinString_NilBin(t *testing.T) {
	pkg := &PackageJSON{}
	if pkg.GetBinString() != "" {
		t.Error("expected empty string for nil bin")
	}
}

// ---- PackageJSON GetExportsMap ----

func TestGetExportsMap_Nil(t *testing.T) {
	pkg := &PackageJSON{}
	if pkg.GetExportsMap() != nil {
		t.Error("expected nil for nil exports")
	}
}

func TestGetExportsMap_ValidObject(t *testing.T) {
	pkg := &PackageJSON{Exports: json.RawMessage(`{"./index": "./dist/index.js"}`)}
	m := pkg.GetExportsMap()
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if _, ok := m["./index"]; !ok {
		t.Error("expected './index' key")
	}
}

func TestGetExportsMap_String(t *testing.T) {
	pkg := &PackageJSON{Exports: json.RawMessage(`"./dist/index.js"`)}
	if pkg.GetExportsMap() != nil {
		t.Error("expected nil for string exports")
	}
}

// ---- readPackageJSON / WritePackageJSON round-trip ----

func TestReadPackageJSON_NotFound(t *testing.T) {
	_, err := readPackageJSON("/nonexistent/package.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestReadPackageJSON_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	os.WriteFile(path, []byte("not json"), 0644)

	_, err := readPackageJSON(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestReadPackageJSONSafe_NotFound(t *testing.T) {
	pkg := ReadPackageJSONSafe("/nonexistent/package.json")
	if pkg != nil {
		t.Error("expected nil for missing file")
	}
}

func TestReadWritePackageJSON_RoundTrip(t *testing.T) {
	content := `{
  "name": "@test/pkg",
  "version": "1.2.3",
  "main": "./dist/index.js",
  "customField": "preserved",
  "dependencies": {
    "foo": "^1.0.0"
  }
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	os.WriteFile(path, []byte(content), 0644)

	pkg, err := readPackageJSON(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pkg.Name != "@test/pkg" {
		t.Errorf("expected name '@test/pkg', got %q", pkg.Name)
	}
	if pkg.Version != "1.2.3" {
		t.Errorf("expected version '1.2.3', got %q", pkg.Version)
	}

	// Modify and write back
	pkg.Version = "2.0.0"
	outPath := filepath.Join(dir, "out.json")
	if err := WritePackageJSON(outPath, pkg); err != nil {
		t.Fatalf("write error: %v", err)
	}

	// Re-read and check
	pkg2, err := readPackageJSON(outPath)
	if err != nil {
		t.Fatalf("re-read error: %v", err)
	}
	if pkg2.Version != "2.0.0" {
		t.Errorf("expected updated version '2.0.0', got %q", pkg2.Version)
	}
	if pkg2.Name != "@test/pkg" {
		t.Errorf("expected preserved name '@test/pkg', got %q", pkg2.Name)
	}

	// Check that custom field was preserved
	if _, ok := pkg2.Raw["customField"]; !ok {
		t.Error("expected customField to be preserved in raw")
	}
}

// ---- WritePackageJSON additional cases ----

func TestWritePackageJSON_WithAllTypedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")

	pkg := &PackageJSON{
		Name:    "test-pkg",
		Version: "1.0.0",
		Main:    "./dist/index.js",
		Types:   "./dist/index.d.ts",
		Bin:     json.RawMessage(`{"cli":"./bin/cli.js"}`),
		Exports: json.RawMessage(`{"./index":"./dist/index.js"}`),
		Dependencies: map[string]string{
			"react": "^18.0.0",
		},
		DevDependencies: map[string]string{
			"typescript": "^5.0.0",
		},
		PeerDependencies: map[string]string{
			"react-dom": "^18.0.0",
		},
		OptionalDependencies: map[string]string{
			"fsevents": "^2.0.0",
		},
		Raw: map[string]json.RawMessage{
			"custom": json.RawMessage(`"preserved"`),
		},
	}

	if err := WritePackageJSON(path, pkg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pkg2, err := readPackageJSON(path)
	if err != nil {
		t.Fatalf("re-read error: %v", err)
	}

	if pkg2.Name != "test-pkg" {
		t.Errorf("name = %q, want test-pkg", pkg2.Name)
	}
	if pkg2.Types != "./dist/index.d.ts" {
		t.Errorf("types = %q, want ./dist/index.d.ts", pkg2.Types)
	}
	if pkg2.Dependencies["react"] != "^18.0.0" {
		t.Error("expected dependencies to be preserved")
	}
	if pkg2.PeerDependencies["react-dom"] != "^18.0.0" {
		t.Error("expected peerDependencies to be preserved")
	}
	if pkg2.OptionalDependencies["fsevents"] != "^2.0.0" {
		t.Error("expected optionalDependencies to be preserved")
	}
}

// TestWritePackageJSON_PreservesExportsKeyOrder pins the invariant that the
// exports field's internal key ordering survives serialization. TypeScript
// requires the "types" condition to appear FIRST; json.MarshalIndent would
// otherwise re-sort keys alphabetically (placing "types" last). The written
// bytes must therefore preserve the raw exports ordering verbatim.
func TestWritePackageJSON_PreservesExportsKeyOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")

	pkg := &PackageJSON{
		Name:    "test-pkg",
		Version: "1.0.0",
		// "types" deliberately placed first; "browser"/"default" follow.
		// Alphabetical sorting would reorder to browser < default < types.
		Exports: json.RawMessage(`{".":{"types":"./index.d.ts","browser":"./index.browser.js","default":"./index.js"}}`),
	}

	if err := WritePackageJSON(path, pkg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(data)

	tIdx := strings.Index(s, `"types"`)
	bIdx := strings.Index(s, `"browser"`)
	dIdx := strings.Index(s, `"default"`)
	if tIdx < 0 || bIdx < 0 || dIdx < 0 {
		t.Fatalf("missing expected condition keys in output:\n%s", s)
	}
	if tIdx >= bIdx || bIdx >= dIdx {
		t.Errorf("exports key order not preserved (want types < browser < default):\n%s", s)
	}

	// The placeholder must never leak into the written file.
	if strings.Contains(s, "putnami-exports-placeholder") {
		t.Errorf("exports placeholder leaked into output:\n%s", s)
	}

	// Re-read to confirm the output is still valid JSON with intact conditions.
	pkg2, err := readPackageJSON(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got := ResolveExportPath(pkg2.Exports, "."); got != "./index.js" {
		t.Errorf("resolved '.' default = %q, want ./index.js", got)
	}
}

func TestReadPackageJSONSafe_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	os.WriteFile(path, []byte(`{"name":"test","version":"1.0.0"}`), 0644)

	pkg := ReadPackageJSONSafe(path)
	if pkg == nil {
		t.Fatal("expected non-nil for valid file")
	}
	if pkg.Name != "test" {
		t.Errorf("name = %q, want test", pkg.Name)
	}
}

func TestReadPackageJSONSafe_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	os.WriteFile(path, []byte("not json"), 0644)

	pkg := ReadPackageJSONSafe(path)
	if pkg != nil {
		t.Error("expected nil for invalid JSON")
	}
}

// ---- ResolveExportPath additional cases ----

func TestResolveExportPath_EmptyConditions(t *testing.T) {
	exports := json.RawMessage(`{"./serve": {}}`)
	result := ResolveExportPath(exports, "./serve")
	if result != "" {
		t.Errorf("expected empty string for empty conditions, got %q", result)
	}
}

// ---- FileExists ----

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	os.WriteFile(existing, []byte("hi"), 0644)

	if !FileExists(existing) {
		t.Error("expected FileExists to return true for existing file")
	}
	if FileExists(filepath.Join(dir, "missing.txt")) {
		t.Error("expected FileExists to return false for missing file")
	}
}
