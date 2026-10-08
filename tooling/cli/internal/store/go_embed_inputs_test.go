package store

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGoEmbedSelectorHashesRawAssetOnlyEdits(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "embed.go"), []byte("package p\n//go:embed assets/putnami.json\nvar x []byte\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(root, "assets", "putnami.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(asset, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"tasks":{"a":1}}`)
	patterns := []string{"**/*.go", "go-embed:build"}
	first, err := hashFiles(root, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	write(`{"tasks":{"a":2}}`)
	second, err := hashFiles(root, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("embedded project config was normalized or omitted")
	}
	files, err := CollectKeyFiles(root, patterns)
	if err != nil || len(files) != 2 {
		t.Fatalf("portable files = %v, %v", files, err)
	}
	if err := os.Remove(asset); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFiles(root, patterns, ProjectConfigScope{}); err == nil {
		t.Fatal("missing embed input accepted")
	}
	if _, err := CollectKeyFiles(root, patterns); err == nil {
		t.Fatal("portable input silently dropped missing embed")
	}
}

func TestGoEmbedProvenanceSurvivesOverlappingGitSelection(t *testing.T) {
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, "embed.go", "package p\nimport _ \"embed\"\n//go:embed payload.txt\nvar payload string\n")
	gitInputWrite(t, root, "payload.txt", "A")
	selection, err := CollectKeyFileSelection(root, []string{"git:**", "go-embed:build"})
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "payload.txt")
	if !slices.Contains(selection.Files, payload) || !slices.Contains(selection.GoEmbed, payload) {
		t.Fatalf("overlapping Git selection lost semantic provenance: %+v", selection)
	}
}

func TestGoEmbedSelectorBindsReferentOfOrdinaryIgnoredGoAlias(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "_fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	referent := filepath.Join(root, ".source.txt")
	if err := os.WriteFile(referent, []byte("package p\nconst Value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "_fixtures", "source.go")
	if err := os.Symlink("../.source.txt", link); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	patterns := []string{"**/*.go", "go-embed:build"}
	selection, err := CollectKeyFileSelection(root, patterns)
	if err != nil || !slices.Contains(selection.Files, link) || !slices.Contains(selection.GoEmbed, referent) {
		t.Fatalf("ordinary Go alias referent selection = %+v, %v", selection, err)
	}
	before, err := hashFiles(root, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(referent, []byte("package p\nconst Value = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := hashFiles(root, patterns, ProjectConfigScope{})
	if err != nil || before == after {
		t.Fatalf("ordinary Go alias referent bytes did not move key: %q -> %q, %v", before, after, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectKeyFileSelection(root, patterns); err == nil {
		t.Fatal("ordinary Go alias escaping the project was accepted")
	}
	if _, err := CollectKeyFileSelection(root, []string{"**/*.go", "!_fixtures/source.go", "go-embed:build"}); err != nil {
		t.Fatalf("excluded Go alias was validated: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../missing.txt", link); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectKeyFileSelection(root, patterns); err == nil {
		t.Fatal("selected broken Go alias was silently dropped by os.Stat")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", link); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectKeyFileSelection(root, patterns); err == nil {
		t.Fatal("selected directory Go alias was silently dropped by os.Stat")
	}
}

func TestGoEmbedSelectorRejectsOrdinaryAliasAcrossLexicalModuleBoundary(t *testing.T) {
	root := t.TempDir()
	gitInputWrite(t, root, ".source.txt", "package p\n")
	gitInputWrite(t, root, "nested/go.mod", "module example.com/nested\n")
	if err := os.Symlink("../.source.txt", filepath.Join(root, "nested/source.go")); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	if _, err := CollectKeyFileSelection(root, []string{"**/*.go", "go-embed:build"}); err == nil || !strings.Contains(err.Error(), "nested module") {
		t.Fatalf("ordinary Go alias crossed its lexical module: %v", err)
	}
}

func TestGoEmbedSelectorOverridesVersionStampNormalization(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "embed.go"), []byte("package p\n//go:embed .gen/version.json\nvar x []byte\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(root, ".gen", "version.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(stamp, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"version":"1","buildTime":"A"}`)
	patterns := []string{".gen/version.json", "go-embed:build", "**/*.go"}
	first, err := hashFiles(root, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	write(`{"version":"1","buildTime":"B"}`)
	second, err := hashFiles(root, patterns, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("embedded version stamp was normalized despite raw selector")
	}
}
