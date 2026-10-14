package cloudcli

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// TestCommandsDeclareTheTraitsOfTheirVerb pins that every command named after a
// well-known verb with default traits declares those traits itself, with the
// same values. The effective traits, and with them the plan and its cache keys,
// do not depend on the core's verb table.
func TestCommandsDeclareTheTraitsOfTheirVerb(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	manifest, diagnostics := extensionproto.ParseManifest(data)
	if len(diagnostics) != 0 {
		t.Fatalf("strict extension parse diagnostics = %+v", diagnostics)
	}
	var checked []string
	for name, cmd := range manifest.Commands {
		defaults := extensionproto.WellKnownVerbs[extensionproto.BaseCommandName(name)].Traits
		if defaults == (extensionproto.CommandTraits{}) {
			continue
		}
		checked = append(checked, name)
		if cmd.Traits == nil {
			t.Errorf("command %q inherits its traits from the core verb table; declare %+v", name, defaults)
		} else if *cmd.Traits != defaults {
			t.Errorf("command %q declares traits %+v, want the verb defaults %+v", name, *cmd.Traits, defaults)
		}
	}
	sort.Strings(checked)
	if want := []string{"package", "publish"}; !slices.Equal(checked, want) {
		t.Errorf("commands with default traits = %v, want %v", checked, want)
	}
}
