// Package workspacejob drives a workspace-activated job through
// cli.RunWorkspaceJob. These tests set the environment and swap the process
// streams, so they run in their own test binary.
package workspacejob

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
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

// unloadableExtensionWorkspace writes a workspace that declares a registry
// extension the lock pins and the install layout holds, at a release that asks
// for a newer CLI contract. The installer keeps that release, since it reads
// the raw manifest, and discovery skips it, so no loaded extension provides
// workspace-install.
func unloadableExtensionWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"broken-ws","extensions":["@acme/broken"]}`)
	manifest := fmt.Sprintf(`{"name":"@acme/broken","version":"1.0.0","cliContract":%d,"commands":{}}`, protocolcli.LatestContract+1)
	clitest.WriteFile(t, filepath.Join(layout.StableDir(root, layout.Extensions, "@acme/broken"), "putnami.extension.json"), manifest)
	lock := lockfile.NewLockFile()
	lock.SetExtension("@acme/broken", lockfile.LockEntry{Version: "1.0.0"})
	if err := lockfile.WriteLockFile(root, lock); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	hometest.Temp(t)
	return root
}

// A declared extension that does not load may be the one that provides
// workspace-install, so the install fails and names it with its reason. It
// does not report that there is nothing to install.
func TestDepsInstall_ADeclaredExtensionThatDoesNotLoadFailsTheInstall(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "no-install-provider-nothing-to-install", "a-declared-extension-that-does-not-load-fails-the-install")
	root := unloadableExtensionWorkspace(t)

	code, output := clitest.RunGateArgs(t, root, "deps", "install")
	if code == 0 {
		t.Fatalf("deps install exit = 0, want a failure; output:\n%s", output)
	}
	if !strings.Contains(output, "extension @acme/broken did not load") || !strings.Contains(output, "requires a newer putnami") {
		t.Errorf("deps install output does not name the extension and why it did not load:\n%s", output)
	}
	if strings.Contains(output, "nothing to install") {
		t.Errorf("deps install output claims there is nothing to install:\n%s", output)
	}
}

// The runner fails, and does not report a missing provider, when an extension
// did not load. It still reports a missing provider when every extension the
// workspace needs loaded.
func TestRunWorkspaceJob_NamesTheExtensionsThatDidNotLoad(t *testing.T) {
	run := func(t *testing.T, root string) (lifecycle.WorkspaceJobResult, error) {
		t.Helper()
		var result lifecycle.WorkspaceJobResult
		var err error
		clitest.CaptureStreams(t, func() {
			result, err = cli.RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
				WorkspaceRoot: root,
				Config:        wsproto.Load(root),
				Job:           "workspace-install",
				Out:           io.Discard,
			})
		})
		return result, err
	}

	t.Run("an extension that is skipped", func(t *testing.T) {
		result, err := run(t, unloadableExtensionWorkspace(t))
		if result.Outcome != lifecycle.WorkspaceJobFailed {
			t.Errorf("outcome = %v, want WorkspaceJobFailed", result.Outcome)
		}
		if err == nil || !strings.Contains(err.Error(), "no loaded extension provides workspace-install") ||
			!strings.Contains(err.Error(), "extension @acme/broken did not load") ||
			!strings.Contains(err.Error(), "requires a newer putnami") {
			t.Errorf("error = %v, want the provider named missing and the extension named with its reason", err)
		}
	})

	t.Run("a declared extension that is not installed", func(t *testing.T) {
		root := t.TempDir()
		clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"absent-ws","extensions":["@acme/absent"]}`)
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
		hometest.Temp(t)
		result, err := run(t, root)
		if result.Outcome != lifecycle.WorkspaceJobFailed {
			t.Errorf("outcome = %v, want WorkspaceJobFailed", result.Outcome)
		}
		if err == nil || !strings.Contains(err.Error(), "extension @acme/absent is not installed") {
			t.Errorf("error = %v, want the extension named as not installed", err)
		}
	})

	t.Run("a workspace that declares no extension", func(t *testing.T) {
		root := t.TempDir()
		clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"empty-ws","extensions":[]}`)
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
		hometest.Temp(t)
		result, err := run(t, root)
		if err != nil || result.Outcome != lifecycle.WorkspaceJobMissing {
			t.Errorf("RunWorkspaceJob = %v, %v, want WorkspaceJobMissing and no error", result.Outcome, err)
		}
	})
}
