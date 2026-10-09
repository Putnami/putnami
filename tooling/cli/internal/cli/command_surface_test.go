package cli

import (
	"os"
	"path/filepath"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// commandSurfacePath is the CLI's committed command surface, relative to this
// package. The Go extension's validate task compares it with the one at the
// last tag of the CLI's line (putnami.json, option command-surface of
// @putnami/go:validate).
var commandSurfacePath = filepath.Join("..", "..", "command-surface.json")

// TestGoldenCommandSurfaceDocument pins the committed command surface to the
// catalog's rendering, so a change to the commands or flags users type always
// shows in the reviewed diff, and the compatibility check reads what the
// catalog declares. PUTNAMI_UPDATE_SURFACE_GOLDENS=1 rewrites it, with the
// other goldens.
func TestGoldenCommandSurfaceDocument(t *testing.T) {
	t.Parallel()
	surface, err := commandmeta.Surface()
	if err != nil {
		t.Fatalf("derive the command surface from the catalog: %v", err)
	}
	got, err := protocolcli.MarshalCommandSurface(surface)
	if err != nil {
		t.Fatalf("render the command surface: %v", err)
	}
	if *updateSurfaceGoldens {
		if err := os.WriteFile(commandSurfacePath, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", commandSurfacePath, err)
		}
		return
	}
	want, err := os.ReadFile(commandSurfacePath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with: PUTNAMI_UPDATE_SURFACE_GOLDENS=1 ./putnamiw test --projects @putnami/cli --run Golden --no-cache --no-enforce-coverage)", commandSurfacePath, err)
	}
	if string(want) != string(got) {
		t.Errorf("command-surface.json drifted from the command catalog.\n%s\nRegenerate with:\n  PUTNAMI_UPDATE_SURFACE_GOLDENS=1 ./putnamiw test --projects @putnami/cli --run Golden --no-cache --no-enforce-coverage",
			firstGoldenDiff(string(want), string(got)))
	}
	if _, err := protocolcli.ParseCommandSurface(want); err != nil {
		t.Errorf("the committed command surface does not parse: %v", err)
	}
}
