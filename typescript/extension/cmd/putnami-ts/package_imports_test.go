package main

import (
	"os"
	"path/filepath"
	"slices"
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

// TestThePackageScanReadsResolvesTypesAndConfigExtends checks the reads an
// import statement does not spell: require.resolve and import.meta.resolve, a
// type import, a triple-slash types reference, a config file that imports a
// shared configuration, and a tsconfig `extends`. Each one reads the package,
// so none of them may leave its edge reported as a declaration.
func TestThePackageScanReadsResolvesTypesAndConfigExtends(t *testing.T) {
	dir := t.TempDir()
	writePackageFile(t, dir, "src/mcp/apps.ts", "const root = path.dirname(require.resolve(\"@acme/mcp-apps/package.json\"));\n")
	writePackageFile(t, dir, "src/meta.ts", "const url = import.meta.resolve('@acme/meta-resolved');\n")
	writePackageFile(t, dir, "src/lastRunView.ts", "import type { RunReport } from '@acme/types';\n")
	writePackageFile(t, dir, "src/ambient.d.ts", "/// <reference types=\"@acme/ambient\" />\n")
	writePackageFile(t, dir, "eslint.config.mts", "import baseConfig from \"@acme/eslint-config/base\";\nexport default [...baseConfig];\n")
	writePackageFile(t, dir, "tsconfig.json", "{\n  \"extends\": \"@acme/tsconfig/base.json\",\n  \"include\": [\"src\"]\n}\n")
	writePackageFile(t, dir, "tsconfig.build.json", `{"extends": ["./tsconfig.json", "@acme/tsconfig-strict/strict.json"]}`)
	writePackageFile(t, dir, "src/data.json", `{"extends": "@acme/not-a-config"}`)

	read := []string{
		"@acme/mcp-apps", "@acme/meta-resolved", "@acme/types", "@acme/ambient",
		"@acme/eslint-config", "@acme/tsconfig", "@acme/tsconfig-strict",
	}
	scan := scanPackageImports(dir, append(slices.Clone(read), "@acme/not-a-config"))
	if !scan.complete {
		t.Fatalf("scan did not complete over a readable package")
	}
	for _, name := range read {
		if !scan.importsPackage(name) {
			t.Errorf("%s reads as unimported", name)
		}
	}
	if scan.importsPackage("@acme/not-a-config") {
		t.Errorf("an `extends` in a JSON file that is no tsconfig reads as an import")
	}
}
