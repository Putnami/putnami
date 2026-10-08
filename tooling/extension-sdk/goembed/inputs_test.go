package goembed

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
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
	spectest.Proves(t, "tooling/extension-authoring", "go-embed-source-inputs", "directives-bind-build-and-test-assets")
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

func TestBuildConstraintHeaderDistinguishesCommentsAndLegacyPlacement(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "go-embed-source-inputs", "only-active-header-constraints-exclude-sources")
	for _, test := range []struct {
		name, src    string
		wantExcluded bool
	}{
		{"block comment", "/*\n//go:build ignore\n*/\npackage p\n", false},
		{"active go build", "//go:build ignore\n\npackage p\n", true},
		{"alternate tag", "//go:build ignore || windows\n\npackage p\n", false},
		{"legacy separated", "// +build ignore\n\npackage p\n", true},
		{"legacy package comment", "// +build ignore\npackage p\n", false},
		{"legacy in block", "/*\n// +build ignore\n*/\n\npackage p\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := excludedByConstraint([]byte(test.src)); got != test.wantExcluded {
				t.Fatalf("excludedByConstraint = %v, want %v", got, test.wantExcluded)
			}
		})
	}
}

func TestResolveIncludesBuildableSourcesInOrdinaryNamedDirectories(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "go-embed-source-inputs", "ordinary-package-directories-contribute-sources-and-assets")
	root := t.TempDir()
	for _, directory := range []string{"out", "dist", "node_modules"} {
		put(t, root, directory+"/p/embed.go", "/*\n//go:build ignore\n*/\npackage p\nimport _ \"embed\"\n//go:embed payload.txt\nvar payload string\n")
		put(t, root, directory+"/p/payload.txt", directory)
	}
	files, err := Resolve(root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"out", "dist", "node_modules"} {
		if !slices.Contains(files, filepath.Join(root, directory, "p", "payload.txt")) {
			t.Fatalf("%s source omitted its embedded payload: %v", directory, files)
		}
	}
	inputs, err := ResolveInputs(root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"out", "dist", "node_modules"} {
		for _, name := range []string{"embed.go", "payload.txt"} {
			if !slices.Contains(inputs, filepath.Join(root, directory, "p", name)) {
				t.Fatalf("%s source/input %s omitted from portable identity: %v", directory, name, inputs)
			}
		}
	}
}

func TestResolveInputsIncludesLexicalGoSourcesWithoutDirectives(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "go-embed-source-inputs", "lexical-go-sources-contribute-input-identity")
	root := t.TempDir()
	put(t, root, "out/p/source.go", "package p\nconst Value = 1\n")
	put(t, root, "out/p/source_test.go", "package p\n")
	inputs, err := ResolveInputs(root, false)
	if err != nil || !slices.Equal(inputs, []string{filepath.Join(root, "out/p/source.go")}) {
		t.Fatalf("lexical Go source inputs = %v, %v", inputs, err)
	}
	testInputs, err := ResolveInputs(root, true)
	if err != nil || !slices.Contains(testInputs, filepath.Join(root, "out/p/source_test.go")) {
		t.Fatalf("test selector omitted lexical test source: %v, %v", testInputs, err)
	}
	if err := os.Remove(filepath.Join(root, "out/p/source.go")); err != nil {
		t.Fatal(err)
	}
	if selected, err := SelectsPath(root, "out/p/source.go", false); err != nil || !selected {
		t.Fatalf("deleted lexical Go source selected = %v, %v", selected, err)
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

func TestResolveSourceFileSymlinkUsesLexicalPackagePath(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "go-embed-source-inputs", "direct-source-links-bind-referents-and-lexical-assets")
	root := t.TempDir()
	put(t, root, "p/.source/directives.txt", "package p\nimport _ \"embed\"\n//go:embed payload.sql\nvar payload string\n")
	put(t, root, "p/payload.sql", "SELECT 1;\n")
	link := filepath.Join(root, "p/embed.go")
	if err := os.Symlink(filepath.Join(".source", "directives.txt"), link); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	inputs, err := Resolve(root, false)
	if err != nil || !slices.Equal(inputs, []string{filepath.Join(root, "p/payload.sql")}) {
		t.Fatalf("source-link inputs = %v, %v", inputs, err)
	}
	allInputs, err := ResolveInputs(root, false)
	if err != nil || !slices.Equal(allInputs, []string{filepath.Join(root, "p/.source/directives.txt"), filepath.Join(root, "p/embed.go"), filepath.Join(root, "p/payload.sql")}) {
		t.Fatalf("source-link portable inputs = %v, %v", allInputs, err)
	}
	if selected, err := SelectsPath(root, "p/.source/directives.txt", false); err != nil || !selected {
		t.Fatalf("source-link target path selection = %v, %v", selected, err)
	}
	if selected, err := SelectsPath(root, "p/payload.sql", false); err != nil || !selected {
		t.Fatalf("source-link path selection = %v, %v", selected, err)
	}
	if err := os.Remove(filepath.Join(root, "p/payload.sql")); err != nil {
		t.Fatal(err)
	}
	if selected, err := SelectsPath(root, "p/payload.sql", false); err != nil || !selected {
		t.Fatalf("source-link deleted path selection = %v, %v", selected, err)
	}
	if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), "matched no files") {
		t.Fatalf("deleted source-link target was accepted: %v", err)
	}
}

func TestResolveRejectsUnsafeSourceFileSymlinks(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "go-embed-source-inputs", "unsafe-source-links-fail-before-selection")
	outside := t.TempDir()
	put(t, outside, "directives.txt", "package p\n//go:embed payload.sql\nvar payload string\n")
	for _, test := range []struct {
		name, target, want string
	}{
		{name: "broken", target: "missing.txt", want: "unsafe referent"},
		{name: "absolute", target: filepath.Join(outside, "directives.txt"), want: "absolute target"},
		{name: "outside", target: filepath.Join("..", "..", "outside.txt"), want: "escapes project"},
		{name: "directory", target: "source-dir", want: "irregular file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "source-dir"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(test.target, filepath.Join(root, "embed.go")); err != nil {
				t.Skipf("source-file symlinks unavailable: %v", err)
			}
			if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe source link accepted: %v", err)
			}
		})
	}
	t.Run("nested module", func(t *testing.T) {
		root := t.TempDir()
		put(t, root, "nested/go.mod", "module nested\n")
		put(t, root, "nested/directives.txt", "package p\n//go:embed payload.sql\nvar payload string\n")
		if err := os.Symlink(filepath.Join("nested", "directives.txt"), filepath.Join(root, "embed.go")); err != nil {
			t.Skipf("source-file symlinks unavailable: %v", err)
		}
		if _, err := Resolve(root, false); err == nil || !strings.Contains(err.Error(), "nested module") {
			t.Fatalf("nested-module source link accepted: %v", err)
		}
	})
	t.Run("lexical nested module", func(t *testing.T) {
		root := t.TempDir()
		put(t, root, "nested/go.mod", "module nested\n")
		put(t, root, "directives.txt", "package p\n")
		link := filepath.Join(root, "nested", "embed.go")
		if err := os.Symlink("../directives.txt", link); err != nil {
			t.Skipf("source-file symlinks unavailable: %v", err)
		}
		if _, err := SourceReferent(root, link); err == nil || !strings.Contains(err.Error(), "nested module") {
			t.Fatalf("lexical nested-module source link accepted: %v", err)
		}
	})
	t.Run("chained link", func(t *testing.T) {
		root := t.TempDir()
		put(t, root, "directives.txt", "package p\n")
		if err := os.Symlink("directives.txt", filepath.Join(root, "middle.txt")); err != nil {
			t.Skipf("source-file symlinks unavailable: %v", err)
		}
		if err := os.Symlink("middle.txt", filepath.Join(root, "embed.go")); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveInputs(root, false); err == nil || !strings.Contains(err.Error(), "unsafe referent") {
			t.Fatalf("chained source link accepted without binding all links: %v", err)
		}
	})
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "directives.txt"), filepath.Join(root, "embed_test.go")); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	if _, err := Resolve(root, false); err != nil {
		t.Fatalf("excluded test source link blocked build inputs: %v", err)
	}
	if _, err := Resolve(root, true); err == nil || !strings.Contains(err.Error(), "absolute target") {
		t.Fatalf("test source link escaped project: %v", err)
	}
}
