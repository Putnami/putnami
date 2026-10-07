package goembed

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func put(t *testing.T, root, rel, contents string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSelectorsAndDeletedPaths(t *testing.T) {
	root := t.TempDir()
	put(t, root, "p/embed.go", "package p\nimport _ \"embed\"\n//go:embed \"assets/report one.json\" all:assets/config\nvar x []byte\n")
	put(t, root, "p/embed_test.go", "package p\nimport _ \"embed\"\n//go:embed assets/test.txt\nvar y []byte\n")
	put(t, root, "p/ignored.go", "//go:build ignore\npackage p\n//go:embed missing.txt\nvar z []byte\n")
	put(t, root, "p/literal.go", "package p\nvar comment = \"//go:embed absent.txt\"\n")
	put(t, root, "p/assets/report one.json", "one")
	put(t, root, "p/assets/config/.hidden", "secret")
	put(t, root, "p/assets/config/visible", "v")
	put(t, root, "p/assets/test.txt", "test")
	build, err := Resolve(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(build) != 3 || !slices.Contains(build, filepath.Join(root, "p/assets/config/.hidden")) || slices.Contains(build, filepath.Join(root, "p/assets/test.txt")) {
		t.Fatalf("build assets = %v", build)
	}
	test, err := Resolve(root, true)
	if err != nil || len(test) != 4 {
		t.Fatalf("test assets = %v, %v", test, err)
	}
	if err := os.Remove(filepath.Join(root, "p/assets/report one.json")); err != nil {
		t.Fatal(err)
	}
	selected, err := SelectsPath(root, "p/assets/report one.json", false)
	if err != nil || !selected {
		t.Fatalf("deleted path selection = %v, %v", selected, err)
	}
	if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("missing target was accepted: %v", err)
	}
}

func TestResolveRejectsUnsafeAndIgnoresGeneratedSource(t *testing.T) {
	root := t.TempDir()
	put(t, root, ".gen/bad.go", "package p\n//go:embed missing\nvar x []byte\n")
	put(t, root, "p/embed.go", "package p\n//go:embed assets/link\nvar x []byte\n")
	put(t, root, "p/assets/real", "real")
	if err := os.Symlink("real", filepath.Join(root, "p/assets/link")); err != nil {
		t.Skip(err)
	}
	if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink accepted: %v", err)
	}
	put(t, root, "p/embed.go", "package p\n//go:embed ../escape\nvar x []byte\n")
	if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("escape accepted: %v", err)
	}
}

func TestResolveRequiresAlternatePlatformTargetsAndRejectsNestedModules(t *testing.T) {
	root := t.TempDir()
	put(t, root, "platform_windows.go", "//go:build windows\npackage p\n//go:embed absent.txt\nvar x []byte\n")
	if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("alternate-platform missing target accepted: %v", err)
	}
	put(t, root, "absent.txt", "ok")
	if _, err := Resolve(root, false); err != nil {
		t.Fatal(err)
	}
	put(t, root, "platform_windows.go", "package p\n//go:embed nested/payload.txt\nvar x []byte\n")
	put(t, root, "nested/go.mod", "module nested\n")
	put(t, root, "nested/payload.txt", "nested")
	if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), "nested module") {
		t.Fatalf("nested-module target accepted: %v", err)
	}
}

func TestResolveRejectsAncestorSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	put(t, outside, "payload.txt", "outside")
	put(t, root, "source.go", "package p\n//go:embed assets/payload.txt\nvar x []byte\n")
	if err := os.Symlink(outside, filepath.Join(root, "assets")); err != nil {
		t.Skip(err)
	}
	if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ancestor symlink accepted: %v", err)
	}
}
