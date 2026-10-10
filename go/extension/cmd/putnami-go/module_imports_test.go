package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func writeModuleFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheImportScanReadsTestFilesAndIgnoresGeneratedTrees pins the walk rule:
// it is `go mod tidy`'s, so a requirement a `_test.go` alone imports is
// imported, and a directory the toolchain never loads from — `.gen`,
// `testdata`, a nested module — never changes the answer.
func TestTheImportScanReadsTestFilesAndIgnoresGeneratedTrees(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "main.go", "package app\n\nimport \"acme/used\"\n\nvar _ = used.X\n")
	writeModuleFile(t, dir, "app_test.go", "package app\n\nimport \"acme/testonly\"\n\nvar _ = testonly.Y\n")
	writeModuleFile(t, dir, ".gen/client/client.go", "package client\n\nimport \"acme/generated\"\n")
	writeModuleFile(t, dir, "testdata/fixture.go", "package fixture\n\nimport \"acme/fixture\"\n")
	writeModuleFile(t, dir, "nested/go.mod", "module acme/nested\n")
	writeModuleFile(t, dir, "nested/nested.go", "package nested\n\nimport \"acme/nestedonly\"\n")

	scan := scanModuleImports(dir, "acme/app", nil, []string{"acme/used", "acme/testonly", "acme/generated", "acme/fixture", "acme/nestedonly"})
	if !scan.complete {
		t.Fatalf("scan did not complete over a readable module")
	}
	for _, module := range []string{"acme/used", "acme/testonly"} {
		if !scan.importsModule(module) {
			t.Errorf("%s reads as unimported; a test file counts exactly like a non-test file", module)
		}
	}
	for _, module := range []string{"acme/generated", "acme/fixture", "acme/nestedonly"} {
		if scan.importsModule(module) {
			t.Errorf("%s reads as imported from a tree the go command never loads", module)
		}
	}
}

// TestTheImportScanSkipsTheDirectoriesTheGoModIgnores: `go mod tidy` reads
// nothing under a directory the go.mod's `ignore` directive names, so an
// import there never reads as imported.
func TestTheImportScanSkipsTheDirectoriesTheGoModIgnores(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "main.go", "package app\n\nimport \"acme/used\"\n")
	writeModuleFile(t, dir, "tools/gen.go", "package tools\n\nimport \"acme/rooted\"\n")
	writeModuleFile(t, dir, "tools/sub/gen.go", "package sub\n\nimport \"acme/rooted\"\n")
	writeModuleFile(t, dir, "pkg/static/assets.go", "package static\n\nimport \"acme/anywhere\"\n")
	writeModuleFile(t, dir, "pkg/tools/tools.go", "package tools\n\nimport \"acme/kept\"\n")

	scan := scanModuleImports(dir, "acme/app", []string{"./tools", "static"}, []string{"acme/used", "acme/rooted", "acme/anywhere", "acme/kept"})
	if !scan.complete {
		t.Fatalf("scan did not complete over a readable module")
	}
	for _, module := range []string{"acme/used", "acme/kept"} {
		if !scan.importsModule(module) {
			t.Errorf("%s reads as unimported from a directory the go.mod does not ignore", module)
		}
	}
	for _, module := range []string{"acme/rooted", "acme/anywhere"} {
		if scan.importsModule(module) {
			t.Errorf("%s reads as imported from a directory the go.mod ignores", module)
		}
	}
}

// TestAnIgnoredPackageTheModuleImportsIsRead: `ignore` takes a directory out
// of package patterns only. A package there that the module's own code
// imports is loaded, tests included, and so is every ignored package it
// imports in turn, so `go mod tidy` keeps what they import.
func TestAnIgnoredPackageTheModuleImportsIsRead(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "main.go", "package app\n\nimport (\n\t\"acme/app/tools/gen\"\n\t\"acme/app/tools/mod\"\n)\n")
	writeModuleFile(t, dir, "tools/gen/gen.go", "package gen\n\nimport (\n\t\"acme/core\"\n\t\"acme/app/tools/deeper\"\n)\n")
	writeModuleFile(t, dir, "tools/gen/gen_test.go", "package gen\n\nimport \"acme/gentest\"\n")
	writeModuleFile(t, dir, "tools/deeper/deeper.go", "package deeper\n\nimport \"acme/deep\"\n")
	writeModuleFile(t, dir, "tools/other/other.go", "package other\n\nimport \"acme/unreached\"\n")
	writeModuleFile(t, dir, "tools/mod/go.mod", "module acme/app/tools/mod\n")
	writeModuleFile(t, dir, "tools/mod/mod.go", "package mod\n\nimport \"acme/nestedonly\"\n")

	scan := scanModuleImports(dir, "acme/app", []string{"./tools"}, []string{"acme/core", "acme/gentest", "acme/deep", "acme/unreached", "acme/nestedonly"})
	if !scan.complete {
		t.Fatalf("scan did not complete over a readable module")
	}
	for _, module := range []string{"acme/core", "acme/gentest", "acme/deep"} {
		if !scan.importsModule(module) {
			t.Errorf("%s reads as unimported from an ignored package the module imports", module)
		}
	}
	for _, module := range []string{"acme/unreached", "acme/nestedonly"} {
		if scan.importsModule(module) {
			t.Errorf("%s reads as imported from a package the module never loads", module)
		}
	}
}

// TestTheIgnoreRuleIsTheGoCommands pins the go command's reading of an
// `ignore` path: `./` anchors it at the module root, any other path matches at
// any depth, and only whole directory names match.
func TestTheIgnoreRuleIsTheGoCommands(t *testing.T) {
	for _, c := range []struct {
		ignore string
		dir    string
		want   bool
	}{
		{"./tools", "tools", true},
		{"./tools", "tools/sub", true},
		{"./tools/", "tools", true},
		{"./tools", "pkg/tools", false},
		{"./tools", "toolsx", false},
		{"./a/b", "a/b/c", true},
		{"./a/b", "a", false},
		{"static", "static", true},
		{"static", "pkg/static", true},
		{"static", "pkg/static/css", true},
		{"static", "pkg/nonstatic", false},
		{"content/html", "site/content/html", true},
		{"content/html", "site/content", false},
	} {
		if got := newGoIgnoreRule([]string{c.ignore}).ignores(filepath.FromSlash(c.dir)); got != c.want {
			t.Errorf("ignore %q matches %q = %v, want %v", c.ignore, c.dir, got, c.want)
		}
	}
}

// TestTheImportScanCreditsTheLongestModulePath: a module nested under another's
// path is credited alone for its own packages, so the parent is not held
// imported by a package it does not provide.
func TestTheImportScanCreditsTheLongestModulePath(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "main.go", "package app\n\nimport \"acme/cli/model/extension\"\n")

	scan := scanModuleImports(dir, "acme/app", nil, []string{"acme/cli", "acme/cli/model"})
	if !scan.importsModule("acme/cli/model") {
		t.Errorf("the providing module reads as unimported")
	}
	if scan.importsModule("acme/cli") {
		t.Errorf("the parent module reads as imported by a package it does not provide")
	}
}

// TestAnUnreadableModuleAttributesNothing: a scan that could not read the whole
// module answers "imported" for every module asked about. An unparsable file
// must never turn a real import into a phantom edge.
func TestAnUnreadableModuleAttributesNothing(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "broken.go", "package app\n\nimport \"acme/used\n")

	scan := scanModuleImports(dir, "acme/app", nil, []string{"acme/used", "acme/never"})
	if scan.complete {
		t.Fatalf("an unparsable file left the scan complete")
	}
	if !scan.importsModule("acme/never") {
		t.Errorf("an incomplete scan reported a phantom edge")
	}
}

// TestAModuleWithNothingToAttributeIsNotWalked: no module to ask about, no walk.
func TestAModuleWithNothingToAttributeIsNotWalked(t *testing.T) {
	scan := scanModuleImports(filepath.Join(t.TempDir(), "absent"), "acme/app", nil, nil)
	if !scan.complete || len(scan.imported) != 0 {
		t.Fatalf("scan = %+v, want a complete empty answer without touching the tree", scan)
	}
}

// TestTidyIgnoresOnlyWhatTheIgnoreTagExcludes pins tidy's constraint rule:
// `ignore` is false and every other tag satisfies either polarity.
func TestTidyIgnoresOnlyWhatTheIgnoreTagExcludes(t *testing.T) {
	for _, c := range []struct {
		header string
		want   bool
	}{
		{"", false},
		{"//go:build ignore\n\n", true},
		{"//go:build !ignore\n\n", false},
		{"//go:build linux && !linux\n\n", false},
		{"//go:build windows && ignore\n\n", true},
		{"// +build ignore\n\n", true},
		{"//go:build linux\n// +build ignore\n\n", false},
		{"//go:build linux &&\n\n", true},
		{"//go:build linux\n//go:build darwin\n\n", true},
		{"// +build ignore\n", false},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "f.go", c.header+"package app\n", parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if got := tidyIgnores(fset, file); got != c.want {
			t.Errorf("tidyIgnores(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}
