package main

import (
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

	scan := scanModuleImports(dir, []string{"acme/used", "acme/testonly", "acme/generated", "acme/fixture", "acme/nestedonly"})
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

// TestTheImportScanCreditsTheLongestModulePath: a module nested under another's
// path is credited alone for its own packages, so the parent is not held
// imported by a package it does not provide.
func TestTheImportScanCreditsTheLongestModulePath(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "main.go", "package app\n\nimport \"acme/cli/model/extension\"\n")

	scan := scanModuleImports(dir, []string{"acme/cli", "acme/cli/model"})
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

	scan := scanModuleImports(dir, []string{"acme/used", "acme/never"})
	if scan.complete {
		t.Fatalf("an unparsable file left the scan complete")
	}
	if !scan.importsModule("acme/never") {
		t.Errorf("an incomplete scan reported a phantom edge")
	}
}

// TestAModuleWithNothingToAttributeIsNotWalked: no workspace edge, no walk.
func TestAModuleWithNothingToAttributeIsNotWalked(t *testing.T) {
	scan := scanModuleImports(filepath.Join(t.TempDir(), "absent"), nil)
	if !scan.complete || len(scan.imported) != 0 {
		t.Fatalf("scan = %+v, want a complete empty answer without touching the tree", scan)
	}
}
