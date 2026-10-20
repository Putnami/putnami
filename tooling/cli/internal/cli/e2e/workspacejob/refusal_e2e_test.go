// Package workspacejob drives a workspace-activated job through
// cli.RunWorkspaceJob. These tests set the environment and swap the process
// streams, so they run in their own test binary.
package workspacejob

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/git"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// installFixture writes a workspace whose one local extension declares a
// workspace-activated "workspace-install" job.
func installFixture(t *testing.T) string {
	t.Helper()
	clitest.RequireShell(t)
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"lifecycle-ws","includes":["installer-extension","app"]}`)
	clitest.WriteFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app"}`)
	clitest.WriteFile(t, filepath.Join(root, "installer-extension", "putnami.json"), `{"name":"@putnami/installer"}`)
	clitest.WriteFile(t, filepath.Join(root, "installer-extension", "putnami.extension.json"), `{
  "name": "@putnami/installer",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    }
  },
  "tasks": {
    "install-task": {
      "kind": "command",
      "command": "{extensionRoot}/install.sh",
      "cache": false,
      "timeoutMs": -1
    }
  }
}`)
	script := filepath.Join(root, "installer-extension", "install.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRunWorkspaceJob_ReportsAnEngineRefusalOnce holds the run to returning an
// engine refusal without printing it: the caller reports the returned error, so
// a copy on stderr would be a second report.
func TestRunWorkspaceJob_ReportsAnEngineRefusalOnce(t *testing.T) {
	root := installFixture(t)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv(git.SourceRevisionEnv, "not-a-commit")

	var err error
	_, stderr := clitest.CaptureStreams(t, func() {
		_, err = cli.RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
			WorkspaceRoot: root,
			Config:        wsproto.Load(root),
			Job:           "workspace-install",
			Out:           io.Discard,
		})
	})
	if err == nil || !strings.Contains(err.Error(), git.SourceRevisionEnv) {
		t.Fatalf("RunWorkspaceJob error = %v, want the refusal of the source revision", err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want the refusal returned and not printed", stderr)
	}
}
