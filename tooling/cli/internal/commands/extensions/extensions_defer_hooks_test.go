package extensions

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// An install that defers its hooks installs the extensions and runs no
// onInstall hook; RunExtensionInstallHooksTo runs them afterwards. A hosted
// install runs its workspace-fetch between the two, because an onInstall hook
// is repository code and no process started after it receives the run
// credential.
func TestExtensionsInstallDefersOnInstallHooks(t *testing.T) {
	dir := t.TempDir()
	extDir := filepath.Join(dir, "extensions", "cloud")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	marker := filepath.Join(dir, "hook-ran.txt")
	hookCommand := writeInstallHookProgram(t, extDir, marker, fixtureproc.Program{})
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(`{
  "name": "@local/cloud",
  "hooks": {
    "onInstall": {
      "kind": "command",
      "command": "`+hookCommand+`",
      "args": ["{workspaceRoot}/hook-ran.txt"],
      "cwd": "{workspaceRoot}"
    }
  },
  "cliContract": 4,
  "commands": {
    "noop": {
      "visibility": "internal",
      "run": [{ "id": "noop", "task": "noop-exec" }]
    }
  },
  "tasks": {
    "noop-exec": {
      "kind": "command",
      "command": "true"
    }
  }
}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cfg := &wsproto.Config{
		Name:       "hook-ws",
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{"/extensions/cloud": ""}},
	}
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename),
		[]byte(`{"name":"hook-ws","extensions":["/extensions/cloud"]}`), 0o644); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}

	var out strings.Builder
	if err := ExtensionsInstallWithOptions(context.Background(), dir, cfg, nil, InstallOptions{Out: &out, DeferInstallHooks: true}); err != nil {
		t.Fatalf("ExtensionsInstallWithOptions: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the onInstall hook ran during an install that defers it: %v", err)
	}
	if strings.Contains(out.String(), "Running extension install hooks") {
		t.Errorf("output = %q, want no install hook phase", out.String())
	}

	if err := RunExtensionInstallHooksTo(context.Background(), dir, cfg, nil, io.Discard); err != nil {
		t.Fatalf("RunExtensionInstallHooksTo: %v", err)
	}
	wantHookRun(t, marker, extDir)
}
