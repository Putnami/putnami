package extension

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

func storeOnlyManifest(name string) string {
	return fmt.Sprintf(`{
	"name": %q,
	"version": "1.0.0",
	"cliContract": 4,
	"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
	"tasks": {"t": {"kind": "command", "command": "echo"}}
}`, name)
}

// storeOnlyFixture writes a workspace with four extensions: a workspace
// project (@acme/local), a registry extension present both in node_modules
// and, through its stable link, in the artifact store (@acme/tool), a
// directory committed where the stable link belongs (@acme/planted), and a
// devDependency in node_modules (@acme/dev).
func storeOnlyFixture(t *testing.T) (ws, storeRoot string, cfg *wsproto.Config) {
	t.Helper()
	ws, storeRoot = t.TempDir(), t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)

	writeExtensionManifest(t, filepath.Join(ws, "tools", "local"), storeOnlyManifest("@acme/local"))
	writeExtensionManifest(t, installedPackageDir(ws, "@acme/tool"), storeOnlyManifest("@acme/tool"))
	stored := filepath.Join(storeRoot, "extensions", "acme-tool@1.0.0")
	writeExtensionManifest(t, stored, storeOnlyManifest("@acme/tool"))
	link := layout.StableDir(ws, layout.Extensions, "@acme/tool")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stored, link); err != nil {
		t.Fatal(err)
	}
	writeExtensionManifest(t, layout.StableDir(ws, layout.Extensions, "@acme/planted"), storeOnlyManifest("@acme/planted"))
	writeExtensionManifest(t, installedPackageDir(ws, "@acme/dev"), storeOnlyManifest("@acme/dev"))
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte(`{"devDependencies": {"@acme/dev": "1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@acme/tool": "1.0.0", "@acme/planted": "1.0.0",
	}}}
	return ws, storeRoot, cfg
}

// A hosted run keeps only the extensions installed from the artifact store: a
// registry extension loads through its stable link even when node_modules
// holds a copy, and a workspace project, a directory committed where a link
// belongs and a devDependency are skipped with the reason. A run without the
// run credential discovers all four, as before.
func TestHostedDiscoveryKeepsOnlyStoreInstalledExtensions(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "hosted-run-runs-only-store-installed-extensions")
	ws, storeRoot, cfg := storeOnlyFixture(t)

	local, err := DiscoverExtensionsDetailed(ws, cfg, []string{"tools/local"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(local.Extensions); got != 4 {
		t.Fatalf("a local run discovered %d extensions, want 4", got)
	}
	if tool := FindExtensionByName(local.Extensions, "@acme/tool"); tool == nil || tool.Path != installedPackageDir(ws, "@acme/tool") {
		t.Fatalf("a local run loads @acme/tool from node_modules, got %+v", tool)
	}

	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	hosted, err := DiscoverExtensionsDetailed(ws, cfg, []string{"tools/local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosted.Extensions) != 1 || hosted.Extensions[0].Name != "@acme/tool" {
		t.Fatalf("a hosted run discovered %v, want only @acme/tool", extensionNames(hosted.Extensions))
	}
	resolved, _ := filepath.EvalSymlinks(hosted.Extensions[0].Path)
	if root, _ := filepath.EvalSymlinks(storeRoot); !within(root, resolved) {
		t.Errorf("@acme/tool loaded from %s, want the artifact store %s", resolved, root)
	}
	skipped := map[string]bool{}
	for _, skip := range hosted.Skipped {
		if errors.Is(skip.Reason, errNotFromTheStore) {
			skipped[skip.Name] = true
		}
	}
	for _, name := range []string{"@acme/local", "@acme/planted"} {
		if !skipped[name] {
			t.Errorf("a hosted run did not skip %s as not from the store: %+v", name, hosted.Skipped)
		}
	}
	if FindExtensionByName(hosted.Extensions, "@acme/dev") != nil {
		t.Error("a hosted run loaded the devDependency from node_modules")
	}
}

// InStoreRoot accepts an extension whose directory resolves inside the store,
// through a link included, unless it is a local source; InArtifactStore also
// refuses a store placed inside the workspace.
func TestInStoreRootAndInArtifactStore(t *testing.T) {
	ws, storeRoot := t.TempDir(), t.TempDir()
	stored := filepath.Join(storeRoot, "extensions", "acme-tool@1.0.0")
	if err := os.MkdirAll(stored, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "link")
	if err := os.Symlink(stored, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	outside := t.TempDir()
	for name, c := range map[string]struct {
		ext  *ExtensionDescription
		want bool
	}{
		"in the store":           {&ExtensionDescription{Path: stored}, true},
		"linked into the store":  {&ExtensionDescription{Path: link}, true},
		"a local source":         {&ExtensionDescription{Path: stored, LocalSource: true}, false},
		"outside the store":      {&ExtensionDescription{Path: outside}, false},
		"a path that is missing": {&ExtensionDescription{Path: filepath.Join(storeRoot, "missing")}, false},
		"no path":                {&ExtensionDescription{}, false},
		"no extension":           {nil, false},
	} {
		if got := InStoreRoot(storeRoot, c.ext); got != c.want {
			t.Errorf("%s: InStoreRoot = %v, want %v", name, got, c.want)
		}
		t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)
		if got := InArtifactStore(ws, c.ext); got != c.want {
			t.Errorf("%s: InArtifactStore = %v, want %v", name, got, c.want)
		}
	}

	// A store inside the workspace holds repository files.
	inside := filepath.Join(ws, "store")
	extDir := filepath.Join(inside, "extensions", "acme-tool@1.0.0")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_ARTIFACT_DIR", inside)
	ext := &ExtensionDescription{Path: extDir}
	if !InStoreRoot(inside, ext) || InArtifactStore(ws, ext) {
		t.Errorf("a store inside the workspace: InStoreRoot = %v, InArtifactStore = %v; want true, false",
			InStoreRoot(inside, ext), InArtifactStore(ws, ext))
	}
}

func extensionNames(extensions []*ExtensionDescription) []string {
	names := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		names = append(names, ext.Name)
	}
	return names
}
