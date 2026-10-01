package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writePackageFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestThePackageScanReadsSourcesAndIgnoresBuiltTrees pins the walk rule: every
// committed source form counts, and what an install or a build writes never
// does — a warm checkout and a cold clone answer the same thing.
func TestThePackageScanReadsSourcesAndIgnoresBuiltTrees(t *testing.T) {
	dir := t.TempDir()
	writePackageFile(t, dir, "src/index.ts", "import { a } from '@acme/used';\nexport const x = a;\n")
	writePackageFile(t, dir, "test/index.test.ts", "import '@acme/testonly';\n")
	writePackageFile(t, dir, "src/lazy.ts", "const m = await import(\"@acme/lazy/sub\");\n")
	writePackageFile(t, dir, "src/legacy.cjs", "const k = require('@acme/legacy');\n")
	writePackageFile(t, dir, "dist/index.js", "import '@acme/built';\n")
	writePackageFile(t, dir, "node_modules/@acme/vendored/index.js", "import '@acme/vendored-dep';\n")
	writePackageFile(t, dir, ".gen/routes.ts", "import '@acme/generated';\n")
	writePackageFile(t, dir, "nested/package.json", `{"name":"@acme/nested"}`)
	writePackageFile(t, dir, "nested/src/index.ts", "import '@acme/nestedonly';\n")

	wanted := []string{
		"@acme/used", "@acme/testonly", "@acme/lazy", "@acme/legacy",
		"@acme/built", "@acme/vendored-dep", "@acme/generated", "@acme/nestedonly",
	}
	scan := scanPackageImports(dir, wanted)
	if !scan.complete {
		t.Fatalf("scan did not complete over a readable package")
	}
	for _, name := range []string{"@acme/used", "@acme/testonly", "@acme/lazy", "@acme/legacy"} {
		if !scan.importsPackage(name) {
			t.Errorf("%s reads as unimported", name)
		}
	}
	for _, name := range []string{"@acme/built", "@acme/vendored-dep", "@acme/generated", "@acme/nestedonly"} {
		if scan.importsPackage(name) {
			t.Errorf("%s reads as imported from a tree a build or an install wrote", name)
		}
	}
}

// TestAPackageWithNothingToAttributeIsNotWalked: no workspace edge, no walk.
func TestAPackageWithNothingToAttributeIsNotWalked(t *testing.T) {
	scan := scanPackageImports(filepath.Join(t.TempDir(), "absent"), nil)
	if !scan.complete || len(scan.imported) != 0 {
		t.Fatalf("scan = %+v, want a complete empty answer without touching the tree", scan)
	}
}

// TestAnUnreadablePackageAttributesNothing: an unreadable source file leaves
// every asked-about package attributed to its manifest, never to a phantom.
func TestAnUnreadablePackageAttributesNothing(t *testing.T) {
	dir := t.TempDir()
	writePackageFile(t, dir, "src/index.ts", "export const x = 1;\n")
	if err := os.Chmod(filepath.Join(dir, "src", "index.ts"), 0o000); err != nil {
		t.Skipf("cannot make a file unreadable here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "src", "index.ts"), 0o600) })
	if _, err := os.ReadFile(filepath.Join(dir, "src", "index.ts")); err == nil {
		t.Skip("this user reads a file with no read permission; the unreadable case cannot be staged here")
	}

	scan := scanPackageImports(dir, []string{"@acme/never"})
	if scan.complete {
		t.Fatalf("an unreadable file left the scan complete")
	}
	if !scan.importsPackage("@acme/never") {
		t.Errorf("an incomplete scan reported a phantom edge")
	}
}
