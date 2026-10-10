package cli

import (
	"os"
	"path/filepath"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
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

// TestTheCLIDeliversTheWinningCommandSurfaceUnderTheCamelCaseKey pins the
// CLI's half of the rule by which the Go extension's validate task reads its
// command-surface option (go/extension/internal/jobs/apicheck surfaceOptionIn):
// in the parameters the task receives, commandSurface holds the value of the
// project layer that wins, whatever spelling each layer used. The cases are
// the putnami.json files of the extension's TestBothSidesPickTheSamePathWhateverTheSpelling.
func TestTheCLIDeliversTheWinningCommandSurfaceUnderTheCamelCaseKey(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct{ name, config string }{
		{
			name:   "a lower kebab-case layer under a higher camelCase one",
			config: `{"name":"lib","options":{"validate":{"command-surface":"a.json"},"@putnami/go:validate":{"commandSurface":"b.json"}}}`,
		},
		{
			name:   "a lower camelCase layer under a higher kebab-case one",
			config: `{"name":"lib","options":{"validate":{"commandSurface":"a.json"},"@putnami/go:validate":{"command-surface":"b.json"}}}`,
		},
		{
			name:   "a lower kebab-case layer under a higher camelCase one, the extension's",
			config: `{"name":"lib","options":{"validate":{"command-surface":"a.json"},"@putnami/go":{"commandSurface":"b.json"}}}`,
		},
		{
			name:   "both spellings in the winning layer",
			config: `{"name":"lib","options":{"validate":{"commandSurface":"c.json"},"@putnami/go:validate":{"command-surface":"a.json","commandSurface":"b.json"}}}`,
		},
	} {
		config, diagnostics := wsproto.ParseProjectConfig([]byte(testCase.config))
		if len(diagnostics) > 0 {
			t.Fatalf("%s: %v", testCase.name, diagnostics)
		}
		job := &jobs.ScheduledJob{
			Project:   &workspace.Project{Name: "lib", Path: "lib", Config: config},
			Extension: &extension.ExtensionDescription{Name: "@putnami/go", Path: "go/extension"},
			JobDef:    &extension.JobDefinition{Name: "validate", ExtensionName: "@putnami/go"},
		}
		ctx := jobs.BuildJobContext(&workspace.Workspace{Name: "ws", Root: "/ws"}, job, nil, nil, nil)
		if got := ctx.Params["commandSurface"]; got != "b.json" {
			t.Errorf("%s: commandSurface = %v, want b.json", testCase.name, got)
		}
	}
}
