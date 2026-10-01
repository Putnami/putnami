package extension

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestResolveOptionOwnership pins who a bare option namespace belongs to.
//
// The answer comes from manifests, never from a convention: an extension
// declares what it reads, a command name stays everybody's, and a namespace two
// extensions declare is foreign to neither.
func TestResolveOptionOwnership(t *testing.T) {
	root := t.TempDir()
	write := func(dir, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "putnami.extension.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sdd", `{"name":"@putnami/sdd","optionNamespaces":["sdd","publish","shared"],"commands":{"validate":{"description":"v"}}}`)
	write("go", `{"name":"@putnami/go","optionNamespaces":["shared"],"commands":{"build":{"description":"b"},"publish":{"description":"p"}}}`)

	ownership := ResolveOptionOwnership(root, []string{"/sdd", "/go", "@putnami/registry-only", "/missing"})

	foreign := ownership.ForeignNamespaces("@putnami/go")
	if !slices.Contains(foreign, "sdd") {
		t.Errorf("foreign namespaces for @putnami/go = %v, want sdd among them", foreign)
	}
	if slices.Contains(foreign, "shared") {
		t.Errorf("foreign namespaces for @putnami/go = %v; a namespace it declares too is an input of both", foreign)
	}
	if slices.Contains(foreign, "publish") {
		t.Errorf("foreign namespaces for @putnami/go = %v; a command layer is merged into every provider's parameters",
			foreign)
	}
	if got := ownership.ForeignNamespaces("@putnami/sdd"); slices.Contains(got, "sdd") {
		t.Errorf("an extension's own declared namespace came back foreign to it: %v", got)
	}
	// A reference the workspace cannot resolve to a directory contributes
	// nothing, so its namespaces stay in every key.
	if _, declared := ownership.Namespaces["registry-only"]; declared {
		t.Error("a registry reference contributed a namespace; its install location is machine-local")
	}
}
