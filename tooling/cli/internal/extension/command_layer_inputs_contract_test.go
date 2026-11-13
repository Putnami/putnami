package extension

import (
	"fmt"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// commandLayerManifest is a manifest whose generate step reads no command
// option layer, stamped with the given contract.
const commandLayerManifest = `{
  "name": "@acme/layers",
  "version": "1.0.0",
  "cliContract": %d,
  "commands": {
    "test": {
      "run": [
        { "id": "generate", "task": "t", "commandLayerInputs": false },
        { "id": "test", "task": "t", "dependsOn": ["generate"] }
      ]
    }
  },
  "tasks": { "t": { "kind": "command", "command": "echo" } }
}`

// Stamped 23, a manifest whose step declares commandLayerInputs loads, and
// the expanded step job carries the opt-out; stamped 22, discovery skips it
// with the reason and the remedy.
func TestLoadExtension_CommandLayerInputsRequiresItsContract(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-step-may-read-no-command-option-layer",
		"a-manifest-with-command-layer-inputs-requires-contract-23")

	dir := t.TempDir()
	writeExtensionManifest(t, dir, fmt.Sprintf(commandLayerManifest, protocolcli.CommandLayerInputsContract))
	ext, skip := tryLoadExtension(dir, "@acme/layers", false)
	if ext == nil || skip != nil {
		t.Fatalf("a manifest stamped 23 did not load: skip=%v", skip)
	}
	steps := ext.Jobs["test"].PipelineSteps
	if len(steps) != 2 || steps[0].ReadsCommandLayer() || !steps[1].ReadsCommandLayer() {
		t.Fatalf("loaded steps = %+v, want generate to read no command layer and test to read it", steps)
	}
	expanded, err := ExpandPipeline("test", ext.Jobs["test"], steps, ext, nil)
	if err != nil || len(expanded) != 2 {
		t.Fatalf("expand test: %d steps, err %v", len(expanded), err)
	}
	for _, step := range expanded {
		if want := step.Step.ID == "generate"; step.JobDef.NoCommandLayerInputs != want {
			t.Errorf("%s: NoCommandLayerInputs = %v, want %v", step.JobDef.Name, step.JobDef.NoCommandLayerInputs, want)
		}
	}

	dir = t.TempDir()
	writeExtensionManifest(t, dir, fmt.Sprintf(commandLayerManifest, protocolcli.CommandLayerInputsContract-1))
	ext, skip = tryLoadExtension(dir, "@acme/layers", false)
	if ext != nil || skip == nil {
		t.Fatalf("a manifest stamped 22 loaded: ext=%v skip=%v", ext, skip)
	}
	for _, want := range []string{"requires 23 for a step that reads no command option layer", "putnami extensions update"} {
		if !strings.Contains(skip.Reason.Error(), want) {
			t.Errorf("skip reason %q does not say %q", skip.Reason, want)
		}
	}
}
