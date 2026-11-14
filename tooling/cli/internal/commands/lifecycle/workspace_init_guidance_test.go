package lifecycle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// syncedTestBuildExtension declares test and build and no flag, as
// @putnami/bazel does.
const syncedTestBuildExtension = `{
  "name": "@example/synced",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "test": { "description": "Test fixture.", "run": [{ "id": "test", "task": "ok" }] },
    "build": { "description": "Build fixture.", "run": [{ "id": "build", "task": "ok" }] }
  },
  "tasks": {
    "ok": { "kind": "command", "command": "/bin/sh", "args": ["-c", "exit 0"], "cache": false }
  }
}`

// An extension the sync declares after the first context generation still
// decides the gate init writes: the guidance block and the Next: hint name
// the tasks and flags it declares, not the default the workspace had before.
func TestInitFromWritesTheGateOfTheExtensionsTheSyncDeclares(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "gate-runs-as-written", "the-init-hint-names-the-gate")
	dir := t.TempDir()
	stubWorkspaceInitStages(t, "", 0)
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "MODULE.bazel"), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncInitProjects = func(_ context.Context, root string, _ *wsproto.Config, _ LifecycleEnv) error {
		extDir := filepath.Join(root, "synced")
		if err := os.MkdirAll(extDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(syncedTestBuildExtension), 0o644); err != nil {
			return err
		}
		path := filepath.Join(root, wsproto.WorkspaceConfigFilename)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var config map[string]json.RawMessage
		if err := json.Unmarshal(data, &config); err != nil {
			return err
		}
		var extensions []string
		if err := json.Unmarshal(config["extensions"], &extensions); err != nil {
			return err
		}
		if config["extensions"], err = json.Marshal(append(extensions, "/synced")); err != nil {
			return err
		}
		data, err = json.MarshalIndent(config, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(data, '\n'), 0o644)
	}

	out, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", []string{"--from", "bazel", "--workspace", "shop"}, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("WorkspaceInit: %v\n%s", err, out)
	}
	guidance, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "run `putnami test,build --impacted` once"; !strings.Contains(string(guidance), want) {
		t.Errorf("AGENTS.md does not name the synced extension's gate %q:\n%s", want, guidance)
	}
	if want := "putnami test,build --impacted\n"; !strings.Contains(out, want) {
		t.Errorf("the summary does not name the gate %q:\n%s", want, out)
	}
}
