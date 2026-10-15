package cloudcli

import (
	"os"
	"path/filepath"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// TestCommandsDeclareTheTraitsOfTheirVerb pins the traits each command named
// after serve, run, package, publish or deploy declares itself. The expected
// values are written here, not read from the core's verb table, so the plan and
// its cache keys keep them when that table no longer gives these verbs
// defaults. A command named after another verb is not checked.
func TestCommandsDeclareTheTraitsOfTheirVerb(t *testing.T) {
	verbTraits := map[string]extensionproto.CommandTraits{
		"serve":   {RequiresRunnable: true},
		"run":     {RequiresRunnable: true},
		"package": {InjectPublishChannels: true},
		"publish": {InjectPublishChannels: true, SideEffects: extensionproto.SideEffectsRegistry},
		"deploy":  {SideEffects: extensionproto.SideEffectsCloud},
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	manifest, diagnostics := extensionproto.ParseManifest(data)
	if len(diagnostics) != 0 {
		t.Fatalf("strict extension parse diagnostics = %+v", diagnostics)
	}
	for _, verb := range []string{"package", "publish"} {
		if _, ok := manifest.Commands[verb]; !ok {
			t.Errorf("the manifest declares no %s command", verb)
		}
	}
	for name, cmd := range manifest.Commands {
		want, ok := verbTraits[extensionproto.BaseCommandName(name)]
		switch {
		case !ok:
		case cmd.Traits == nil:
			t.Errorf("command %q declares no traits, so it depends on the core's verb table; declare %+v", name, want)
		case *cmd.Traits != want:
			t.Errorf("command %q declares traits %+v, want %+v", name, *cmd.Traits, want)
		}
	}
}
