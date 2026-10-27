package extension

import (
	"fmt"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// changedFilesManifest is a manifest whose race step gates on and binds the
// run's changed files, stamped with the given contract.
const changedFilesManifest = `{
  "name": "@acme/race",
  "version": "1.0.0",
  "cliContract": %d,
  "commands": {
    "test": {
      "run": [
        { "id": "test", "task": "t" },
        {
          "id": "test-race",
          "task": "t",
          "activation": { "changedFiles": ["**/*.go"] },
          "with": { "changed-files": { "from": "context", "path": "selection.changedFiles" } }
        }
      ]
    }
  },
  "tasks": { "t": { "kind": "command", "command": "echo" } }
}`

// Stamped 16, a manifest with a change-gated step loads with its gate and
// binding; stamped 15, discovery skips it with the reason and the remedy,
// because a reader without the rung would plan the step on every run and key
// it without the files.
func TestLoadExtension_ChangeGateRequiresItsContract(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-step-reads-the-changed-files",
		"a-manifest-with-a-change-gate-requires-contract-16")

	dir := t.TempDir()
	writeExtensionManifest(t, dir, fmt.Sprintf(changedFilesManifest, protocolcli.ChangedFilesContract))
	ext, skip := tryLoadExtension(dir, "@acme/race", false)
	if ext == nil || skip != nil {
		t.Fatalf("a manifest stamped 16 did not load: skip=%v", skip)
	}
	steps := ext.Jobs["test"].PipelineSteps
	if len(steps) != 2 || steps[1].Activation == nil || len(steps[1].Activation.ChangedFiles) != 1 ||
		steps[1].With["changed-files"].Path != "selection.changedFiles" {
		t.Fatalf("loaded race step = %+v", steps)
	}

	dir = t.TempDir()
	writeExtensionManifest(t, dir, fmt.Sprintf(changedFilesManifest, protocolcli.ChangedFilesContract-1))
	ext, skip = tryLoadExtension(dir, "@acme/race", false)
	if ext != nil || skip == nil {
		t.Fatalf("a manifest stamped 15 loaded: ext=%v skip=%v", ext, skip)
	}
	for _, want := range []string{"requires 16 for a step that reads the run's changed files", "putnami extensions update"} {
		if !strings.Contains(skip.Reason.Error(), want) {
			t.Errorf("skip reason %q does not say %q", skip.Reason, want)
		}
	}
}
