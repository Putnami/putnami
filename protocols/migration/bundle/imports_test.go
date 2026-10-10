package bundle

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPackageImportsOnlyTheStandardLibrary holds the package to the standard
// library, so a dependency-light publisher imports it without the parent
// protocol's dependencies.
func TestPackageImportsOnlyTheStandardLibrary(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if first, _, _ := strings.Cut(importPath, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q; the package imports only the standard library", name, importPath)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no package source file was checked")
	}
}
