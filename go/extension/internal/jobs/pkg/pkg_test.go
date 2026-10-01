package pkg

import (
	"os"
	"path/filepath"
	"testing"
)

// --- stripReplaceDirectives ---

func TestStripReplaceDirectives_SingleLine(t *testing.T) {
	input := `module example.com/foo

go 1.21

require example.com/bar v1.0.0

replace example.com/bar => ../bar
`
	result := stripReplaceDirectives(input)
	if contains(result, "replace ") {
		t.Errorf("result still contains replace directive:\n%s", result)
	}
	if !contains(result, "require example.com/bar") {
		t.Errorf("result missing require directive:\n%s", result)
	}
}

func TestStripReplaceDirectives_BlockReplace(t *testing.T) {
	input := `module example.com/foo

go 1.21

require (
	example.com/bar v1.0.0
	example.com/baz v2.0.0
)

replace (
	example.com/bar => ../bar
	example.com/baz => ../baz
)
`
	result := stripReplaceDirectives(input)
	if contains(result, "replace") {
		t.Errorf("result still contains replace block:\n%s", result)
	}
	if !contains(result, "require") {
		t.Errorf("result missing require block:\n%s", result)
	}
}

func TestStripReplaceDirectives_NoReplace(t *testing.T) {
	input := `module example.com/foo

go 1.21

require example.com/bar v1.0.0
`
	result := stripReplaceDirectives(input)
	if result != input {
		t.Errorf("result changed unexpectedly:\ngot:  %q\nwant: %q", result, input)
	}
}

func TestStripReplaceDirectives_MultipleReplaceSingleLine(t *testing.T) {
	input := `module example.com/foo

go 1.21

replace example.com/a => ../a
replace example.com/b => ../b
`
	result := stripReplaceDirectives(input)
	if contains(result, "replace") {
		t.Errorf("result still contains replace:\n%s", result)
	}
}

func TestStripReplaceDirectives_EndsWithNewline(t *testing.T) {
	input := `module example.com/foo

go 1.21

replace example.com/bar => ../bar
`
	result := stripReplaceDirectives(input)
	if len(result) == 0 || result[len(result)-1] != '\n' {
		t.Errorf("result should end with newline, got: %q", result)
	}
}

func TestStripReplaceDirectives_EmptyInput(t *testing.T) {
	result := stripReplaceDirectives("")
	// Should return a newline per the function contract
	if result != "\n" {
		t.Errorf("empty input result = %q, want %q", result, "\n")
	}
}

// --- toResolverArtifactName ---

func TestToResolverArtifactName_ScopedPackage(t *testing.T) {
	got, err := toResolverArtifactName("@putnami/go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "putnami-go" {
		t.Errorf("toResolverArtifactName = %q, want %q", got, "putnami-go")
	}
}

func TestToResolverArtifactName_ScopedWithSubpackage(t *testing.T) {
	got, err := toResolverArtifactName("@putnami/ci")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "putnami-ci" {
		t.Errorf("toResolverArtifactName = %q, want %q", got, "putnami-ci")
	}
}

func TestToResolverArtifactName_UnsccopedPackage(t *testing.T) {
	got, err := toResolverArtifactName("mypackage")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "mypackage" {
		t.Errorf("toResolverArtifactName = %q, want %q", got, "mypackage")
	}
}

func TestToResolverArtifactName_WithSlash(t *testing.T) {
	got, err := toResolverArtifactName("org/pkg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "org-pkg" {
		t.Errorf("toResolverArtifactName = %q, want %q", got, "org-pkg")
	}
}

func TestToResolverArtifactName_SpecialCharsNormalized(t *testing.T) {
	got, err := toResolverArtifactName("@my-scope/my.package_1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Special chars aside from A-Za-z0-9._- become dashes
	if got == "" {
		t.Errorf("expected non-empty artifact name")
	}
}

func TestToResolverArtifactName_InvalidEmpty(t *testing.T) {
	_, err := toResolverArtifactName("@")
	if err == nil {
		t.Error("expected error for name that reduces to empty, got nil")
	}
}

func TestToResolverArtifactName_MultiDashCollapse(t *testing.T) {
	got, err := toResolverArtifactName("@a///b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Multiple slashes become multiple dashes, then collapsed
	if contains(got, "--") {
		t.Errorf("toResolverArtifactName result %q contains consecutive dashes", got)
	}
}

// --- safeName ---

func TestSafeName_WithColon(t *testing.T) {
	got := safeName("foo:bar")
	if got != "foo-bar" {
		t.Errorf("safeName = %q, want %q", got, "foo-bar")
	}
}

func TestSafeName_WithSlash(t *testing.T) {
	got := safeName("@putnami/go")
	if got != "@putnami-go" {
		t.Errorf("safeName = %q, want %q", got, "@putnami-go")
	}
}

func TestSafeName_WithBackslash(t *testing.T) {
	got := safeName("a\\b")
	if got != "a-b" {
		t.Errorf("safeName = %q, want %q", got, "a-b")
	}
}

func TestSafeName_NoSpecialChars(t *testing.T) {
	got := safeName("simple-name")
	if got != "simple-name" {
		t.Errorf("safeName = %q, want %q", got, "simple-name")
	}
}

func TestSafeName_Empty(t *testing.T) {
	got := safeName("")
	if got != "" {
		t.Errorf("safeName empty = %q, want %q", got, "")
	}
}

// --- discoverWorkspaceGoModules ---

func TestDiscoverWorkspaceGoModules_ValidGoWork(t *testing.T) {
	dir := t.TempDir()

	// Create module A
	modA := filepath.Join(dir, "modA")
	os.MkdirAll(modA, 0o755)
	os.WriteFile(filepath.Join(modA, "go.mod"), []byte("module example.com/a\n\ngo 1.21\n"), 0o644)

	// Create module B
	modB := filepath.Join(dir, "modB")
	os.MkdirAll(modB, 0o755)
	os.WriteFile(filepath.Join(modB, "go.mod"), []byte("module example.com/b\n\ngo 1.21\n"), 0o644)

	// Create go.work
	goWork := "go 1.21\n\nuse (\n\t./modA\n\t./modB\n)\n"
	os.WriteFile(filepath.Join(dir, "go.work"), []byte(goWork), 0o644)

	modules := discoverWorkspaceGoModules(dir)
	if len(modules) != 2 {
		t.Fatalf("expected 2 modules, got %d: %v", len(modules), modules)
	}

	found := map[string]bool{}
	for _, m := range modules {
		found[m] = true
	}
	if !found["example.com/a"] {
		t.Errorf("expected example.com/a in modules, got %v", modules)
	}
	if !found["example.com/b"] {
		t.Errorf("expected example.com/b in modules, got %v", modules)
	}
}

func TestDiscoverWorkspaceGoModules_NoGoWork(t *testing.T) {
	dir := t.TempDir()
	modules := discoverWorkspaceGoModules(dir)
	if modules != nil {
		t.Errorf("expected nil for missing go.work, got %v", modules)
	}
}

func TestDiscoverWorkspaceGoModules_EmptyGoWork(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.work"), []byte("go 1.21\n"), 0o644)
	modules := discoverWorkspaceGoModules(dir)
	if len(modules) != 0 {
		t.Errorf("expected empty modules for go.work with no use block, got %v", modules)
	}
}

// --- helpers ---

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
