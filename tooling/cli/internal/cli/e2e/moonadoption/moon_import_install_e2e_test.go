package moonadoption

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// failingInstallExtension writes at dir a stand-in for a language extension
// whose workspace install fails, as a package manager that rejects the
// manifests the import wrote does.
func failingInstallExtension(t *testing.T, root, dir, name string) {
	t.Helper()
	ext := filepath.Join(root, dir)
	clitest.WriteFile(t, filepath.Join(ext, "putnami.extension.json"), `{
  "name": "`+name+`",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "workspace-install": {"description": "install", "quiet": true, "run": [{"id": "workspace-install", "task": "workspace-install-exec"}]}
  },
  "tasks": {
    "workspace-install-exec": {"kind": "command", "command": "{extensionRoot}/install.sh", "cwd": "{workspaceRoot}", "cache": false, "timeoutMs": -1}
  }
}`)
	script := filepath.Join(ext, "install.sh")
	clitest.WriteFile(t, script, "#!/bin/sh\necho 'ERR_PNPM_TRUST_DOWNGRADE High-risk trust downgrade for vite@4.5.14'\nexit 1\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
}

// The install the import's projects sync runs fails init: the import is not
// done, and a script that runs init must not go on.
func TestAnImportWhoseInstallFailsFailsInit(t *testing.T) {
	spectest.Proves(t, "cli/init-from-a-monorepo-tool", "init-rerun-shows-a-diff", "init-from-apply-fails-when-the-workspace-install-fails")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	demo := readDemo(t)
	root := moonWorkspace(t, demo, buildRuntime(t), "")
	stubMoon(t)
	failingInstallExtension(t, root, "typescript-extension", "@putnami/typescript")
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "shop", "extensions": ["/moon-extension", "/typescript-extension"], "includes": []}`)

	code, out := clitest.RunGateArgs(t, root, "init", "--from", "moon", "--mode", "import", "--apply")
	if code == 0 || !strings.Contains(out, "workspace-install~workspace-install FAIL") || !strings.Contains(out, "synchronize the moon projects: workspace install: deps install failed") {
		t.Fatalf("init exit %d after a failed install:\n%s", code, out)
	}
}
