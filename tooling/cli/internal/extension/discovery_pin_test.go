package extension

import (
	"fmt"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/layout"
)

const pinFixtureManifest = `{
	"name": "@acme/tool",
	"version": %q,
	"cliContract": 4,
	"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
	"tasks": {"t": {"kind": "command", "command": "echo"}}
}`

// pinFixture writes a workspace project tools/acme whose manifest names
// @acme/tool at version 0.1.0 and, when installed is true, the installed
// build of the same name at version 2.0.0.
func pinFixture(t *testing.T, installed bool) string {
	t.Helper()
	dir := t.TempDir()
	writeExtensionManifest(t, filepath.Join(dir, "tools", "acme"), sprintfManifest("0.1.0"))
	if installed {
		writeExtensionManifest(t, layout.StableDir(dir, layout.Extensions, "@acme/tool"), sprintfManifest("2.0.0"))
	}
	return dir
}

func sprintfManifest(version string) string {
	return fmt.Sprintf(pinFixtureManifest, version)
}

func discoverPinFixture(t *testing.T, dir string, entries map[string]string) *ExtensionDescription {
	t.Helper()
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: entries}}
	result, err := DiscoverExtensionsDetailed(dir, cfg, []string{"tools/acme"})
	if err != nil {
		t.Fatalf("DiscoverExtensionsDetailed: %v", err)
	}
	if len(result.Extensions) != 1 {
		t.Fatalf("expected one @acme/tool, got %d extensions", len(result.Extensions))
	}
	return result.Extensions[0]
}

func TestDiscoverExtensions_RegistryPinWinsOverSameNameProjectManifest(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "registry-pin-wins", "a-pin-by-name-loads-the-published-build")

	ext := discoverPinFixture(t, pinFixture(t, true), map[string]string{"@acme/tool": "2.0.0"})
	if ext.Version != "2.0.0" {
		t.Errorf("Version = %q, want the pinned 2.0.0", ext.Version)
	}
	if ext.LocalSource {
		t.Error("the pinned build must not be marked as a workspace source")
	}
	want := filepath.Join("tools", "acme")
	if ext.PinnedOver != want {
		t.Errorf("PinnedOver = %q, want %q", ext.PinnedOver, want)
	}
	for name, job := range ext.Jobs {
		if job.ExtensionPath != want {
			t.Errorf("job %s ExtensionPath = %q, want the replaced project %q", name, job.ExtensionPath, want)
		}
	}
}

func TestDiscoverExtensions_ExplicitPathKeepsTheProjectManifest(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "registry-pin-wins", "an-explicit-path-keeps-the-project-manifest")

	ext := discoverPinFixture(t, pinFixture(t, true), map[string]string{"/tools/acme": "", "@acme/tool": "2.0.0"})
	if ext.Version != "0.1.0" || !ext.LocalSource {
		t.Errorf("got version %q local %v, want the project manifest 0.1.0", ext.Version, ext.LocalSource)
	}
	if ext.PinnedOver != "" {
		t.Errorf("PinnedOver = %q, want none for the project manifest", ext.PinnedOver)
	}
}

func TestDiscoverExtensions_UnloadedPinFallsBackToTheProjectManifest(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "registry-pin-wins", "an-unloaded-pin-keeps-the-project-manifest")

	ext := discoverPinFixture(t, pinFixture(t, false), map[string]string{"@acme/tool": "2.0.0"})
	if ext.Version != "0.1.0" || !ext.LocalSource {
		t.Fatalf("got version %q local %v, want the project manifest 0.1.0", ext.Version, ext.LocalSource)
	}
	if ext.RelPath != filepath.Join("tools", "acme") {
		t.Errorf("RelPath = %q, want tools/acme", ext.RelPath)
	}
	for name, job := range ext.Jobs {
		if job.ExtensionPath != ext.RelPath {
			t.Errorf("job %s ExtensionPath = %q, want %q", name, job.ExtensionPath, ext.RelPath)
		}
	}
}

func TestPinnedByName(t *testing.T) {
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@acme/tool": "", "acme": ""}}}
	cases := []struct {
		name, project string
		want          bool
	}{
		{"@acme/tool", "tools/acme", true},
		{"@other/tool", "tools/other", false},
		{"acme", "acme", false},
	}
	for _, c := range cases {
		if got := pinnedByName(cfg, c.name, filepath.FromSlash(c.project)); got != c.want {
			t.Errorf("pinnedByName(%q, %q) = %v, want %v", c.name, c.project, got, c.want)
		}
	}
	if pinnedByName(nil, "@acme/tool", "tools/acme") {
		t.Error("a nil config pins nothing")
	}
}
