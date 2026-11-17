package cli

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/launch"
)

// A command that runs in a workspace records its repository, the only kind
// the host-hook relay serves; a run in CI records nothing.
func TestACommandRecordsItsRepositoryForTheRelay(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "host-hook-relay", "the-relay-serves-only-a-recorded-repository")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	home := hometest.Temp(t)
	t.Setenv("CI", "true")
	recordHostHookRepository(root)
	if launch.HostHookRepositoryRecorded(home, root) {
		t.Fatal("a run in CI recorded the repository")
	}

	t.Setenv("CI", "")
	recordHostHookRepository(root)
	recordHostHookRepository("")
	if !launch.HostHookRepositoryRecorded(home, root) {
		t.Fatal("a command did not record its repository")
	}
	for value, want := range map[string]bool{"1": true, "TRUE": true, "yes": true, "on": true, "false": false, "": false, "0": false} {
		if got := hostHookCI(value); got != want {
			t.Errorf("hostHookCI(%q) = %v, want %v", value, got, want)
		}
	}
}
