package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A file an expansion lists is hashed as its exact bytes: a project config
// keeps its tasks block, and a missing one fails the key and the portable
// file list alike.
func TestListedInputHashesItsExactBytes(t *testing.T) {
	root := t.TempDir()
	gitInputWrite(t, root, "main.go", "package p\n")
	gitInputWrite(t, root, "assets/putnami.json", `{"tasks":{"a":1}}`)
	expander := listExpander{"sel:build": {"assets/putnami.json"}}
	patterns := []string{"**/*.go", "sel:build"}
	first, err := hashFiles(root, patterns, expander, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	gitInputWrite(t, root, "assets/putnami.json", `{"tasks":{"a":2}}`)
	second, err := hashFiles(root, patterns, expander, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("a listed project config was normalized or omitted")
	}
	files, err := CollectKeyFiles(root, patterns, expander)
	if err != nil || len(files) != 2 {
		t.Fatalf("portable files = %v, %v", files, err)
	}
	if err := os.Remove(filepath.Join(root, "assets", "putnami.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFiles(root, patterns, expander, ProjectConfigScope{}); !errors.Is(err, ErrInputSelector) {
		t.Fatalf("missing listed input = %v, want ErrInputSelector", err)
	}
	if _, err := CollectKeyFiles(root, patterns, expander); !errors.Is(err, ErrInputSelector) {
		t.Fatalf("portable files dropped a missing listed input: %v", err)
	}
}

// A file both a Git candidate pattern and an expansion select keeps its
// listing: the portable admission still knows a selector requires it.
func TestListedInputSurvivesOverlappingGitSelection(t *testing.T) {
	root := gitInputPhysicalRoot(t)
	gitInputWrite(t, root, "main.go", "package p\n")
	gitInputWrite(t, root, "payload.txt", "A")
	expander := listExpander{"sel:build": {"payload.txt"}}
	selection, err := CollectKeyFileSelection(root, []string{"git:**", "sel:build"}, expander)
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "payload.txt")
	if !slices.Contains(selection.Files, payload) || !slices.Contains(selection.Selected, payload) {
		t.Fatalf("overlapping Git selection lost the listing: %+v", selection)
	}
}

// A version stamp an expansion lists is read raw, so its build time keys,
// where an ordinary pattern reads the stamp normalized.
func TestListedInputOverridesVersionStampNormalization(t *testing.T) {
	root := t.TempDir()
	gitInputWrite(t, root, "main.go", "package p\n")
	write := func(s string) {
		t.Helper()
		gitInputWrite(t, root, ".gen/version.json", s)
	}
	write(`{"version":"1","buildTime":"A"}`)
	expander := listExpander{"sel:build": {".gen/version.json"}}
	patterns := []string{".gen/version.json", "sel:build", "**/*.go"}
	first, err := hashFiles(root, patterns, expander, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	write(`{"version":"1","buildTime":"B"}`)
	second, err := hashFiles(root, patterns, expander, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("a listed version stamp was normalized")
	}
}
