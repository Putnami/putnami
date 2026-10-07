package store

import (
	"os"
	"path/filepath"
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
