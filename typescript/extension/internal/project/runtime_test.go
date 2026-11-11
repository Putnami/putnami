package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/typescript/extension/internal/errs"
)

const toolchainFeature = "typescript/typescript-project-toolchain"

func TestTheRuntimeDefaultsToBun(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-workload-runtime", "the-runtime-defaults-to-bun")
	for value, want := range map[string]string{"": RuntimeBun, "bun": RuntimeBun, "node": RuntimeNode} {
		got, err := ResolveRuntime(value)
		if err != nil || got != want {
			t.Errorf("ResolveRuntime(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
}

func TestAnUnknownRuntimeFailsNamingBunAndNode(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-workload-runtime", "an-unknown-runtime-fails-naming-bun-and-node")
	_, err := ResolveRuntime("deno")
	if err == nil {
		t.Fatal("ResolveRuntime(deno) succeeded")
	}
	if errs.CodeOf(err) != errs.CodeInvalidRuntime {
		t.Errorf("code = %q, want %q", errs.CodeOf(err), errs.CodeInvalidRuntime)
	}
	for _, word := range []string{`"deno"`, `"bun"`, `"node"`, `options["@putnami/typescript"].runtime`} {
		if !strings.Contains(err.Error(), word) {
			t.Errorf("error %q does not name %s", err, word)
		}
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNodeExternalsLeaveInstalledPackagesAndBundleWorkspaceLinks(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app/package.json":                     `{"dependencies":{"express":"4","@acme/lib":"workspace:*","missing":"1"},"optionalDependencies":{"fsevents":"2"},"devDependencies":{"typescript":"5"}}`,
		"node_modules/express/package.json":    `{"name":"express"}`,
		"libs/lib/package.json":                `{"name":"@acme/lib","dependencies":{"pg":"8"}}`,
		"node_modules/pg/package.json":         `{"name":"pg"}`,
		"node_modules/typescript/package.json": `{"name":"typescript"}`,
	})
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "libs", "lib"), filepath.Join(root, "node_modules", "@acme", "lib")); err != nil {
		t.Fatal(err)
	}
	got := NodeExternals(filepath.Join(root, "app"))
	want := []string{"express", "fsevents", "missing", "pg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NodeExternals = %v, want %v", got, want)
	}
}

func TestResolvePackageDirWalksUpLikeNode(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"node_modules/a/package.json":                `{}`,
		"node_modules/b/package.json":                `{"version":"2"}`,
		"node_modules/a/node_modules/b/package.json": `{"version":"1"}`,
	})
	real := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if dir, ok := ResolvePackageDir(filepath.Join(root, "node_modules", "a"), "b"); !ok || dir != real(filepath.Join(root, "node_modules", "a", "node_modules", "b")) {
		t.Errorf("b from a = %q, %v; want a's own b", dir, ok)
	}
	if dir, ok := ResolvePackageDir(root, "b"); !ok || dir != real(filepath.Join(root, "node_modules", "b")) {
		t.Errorf("b from the root = %q, %v; want the hoisted b", dir, ok)
	}
	if _, ok := ResolvePackageDir(root, "c"); ok {
		t.Error("an absent package resolved")
	}
}
