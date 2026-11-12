package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// clientsWorkspace writes a stale workspace (a lock and no install marker)
// whose one project declares a generated client when client is set.
func clientsWorkspace(t *testing.T, client bool) string {
	t.Helper()
	root := t.TempDir()
	project := `{"name":"items-client"}`
	if client {
		project = `{"name":"items-client","generatedFrom":{"service":"catalog.items","language":"go"}}`
	}
	for rel, content := range map[string]string{
		wsproto.WorkspaceConfigFilename: `{"name":"clients-ws","includes":["clients/items"]}`,
		"clients/items/putnami.json":    project,
		"putnami.lock.json":             `{}`,
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace.InvalidateLoadCache(root)
	return root
}

// spyRestore records each client restore the bootstrap asks for.
func spyRestore(ret error) (ClientRestorer, *[]ClientRestoreRequest) {
	var calls []ClientRestoreRequest
	return func(_ context.Context, req ClientRestoreRequest) error {
		calls = append(calls, req)
		return ret
	}, &calls
}

// After the implicit install of a fresh worktree, the first-run pass runs
// client-generate once, and only then.
func TestEnsureWorkspaceBootstrap_RestoresGeneratedClientsAfterTheInstall(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "the-first-run-pass-runs-client-generate-cached")
	resetBootstrapEnv(t)
	t.Cleanup(runcredential.SetForTest(""))
	installs := spyInstall(t, nil)
	restore, calls := spyRestore(nil)
	root := clientsWorkspace(t, true)
	cfg := &wsproto.Config{Name: "clients-ws"}

	for range 2 {
		if err := EnsureWorkspaceBootstrap(context.Background(), root, cfg, BootstrapOptions{Command: "build", RestoreClients: restore}); err != nil {
			t.Fatal(err)
		}
	}
	if *installs != 1 || len(*calls) != 1 {
		t.Fatalf("installs = %d, restores = %d; want one of each per process", *installs, len(*calls))
	}
	got := (*calls)[0]
	if got.WorkspaceRoot != root || got.Config != cfg || got.Out != os.Stdout {
		t.Fatalf("restore request = %+v, want the workspace, its config and stdout", got)
	}
}

// Machine output keeps stdout for the command: the restore's progress goes to
// stderr, as the install's does.
func TestEnsureWorkspaceBootstrap_RoutesStructuredClientRestoreToStderr(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "the-first-run-pass-runs-client-generate-cached")
	resetBootstrapEnv(t)
	t.Cleanup(runcredential.SetForTest(""))
	spyInstall(t, nil)
	restore, calls := spyRestore(nil)
	root := clientsWorkspace(t, true)

	opts := BootstrapOptions{Command: "build", Output: string(protocolcli.OutputJSON), RestoreClients: restore}
	if err := EnsureWorkspaceBootstrap(context.Background(), root, &wsproto.Config{}, opts); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].Out != os.Stderr {
		t.Fatalf("restores = %+v, want one on stderr", *calls)
	}
}

// The pass runs nothing when the install did not run or failed, when the
// command is client-generate itself, on a hosted run, and in a workspace that
// declares no client.
func TestEnsureWorkspaceBootstrap_SkipsTheClientRestore(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "the-first-run-pass-runs-client-generate-cached")
	for _, tc := range []struct {
		name       string
		client     bool
		command    string
		installErr error
		hosted     bool
		fresh      bool
	}{
		{name: "no client", command: "build"},
		{name: "the command is client-generate", client: true, command: "client-generate"},
		{name: "the install failed", client: true, command: "build", installErr: errors.New("network down")},
		{name: "a hosted run", client: true, command: "build", hosted: true},
		{name: "the install is current", client: true, command: "build", fresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetBootstrapEnv(t)
			credential := ""
			if tc.hosted {
				credential = "hosted-run-credential"
			}
			t.Cleanup(runcredential.SetForTest(credential))
			spyInstall(t, tc.installErr)
			restore, calls := spyRestore(nil)
			root := clientsWorkspace(t, tc.client)
			if tc.fresh {
				if err := writeInstallState(root); err != nil {
					t.Fatal(err)
				}
			}
			captureProcessOutput(t, func() {
				_ = EnsureWorkspaceBootstrap(context.Background(), root, &wsproto.Config{}, BootstrapOptions{Command: tc.command, RestoreClients: restore})
			})
			if len(*calls) != 0 {
				t.Fatalf("restores = %+v, want none", *calls)
			}
		})
	}
}

// A failed restore prints one line, and the command still runs.
func TestEnsureWorkspaceBootstrap_ClientRestoreIsBestEffort(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-fresh-worktree-restores-generated-clients", "the-first-run-pass-runs-client-generate-cached")
	resetBootstrapEnv(t)
	t.Cleanup(runcredential.SetForTest(""))
	spyInstall(t, nil)
	restore, calls := spyRestore(errors.New("client-generate exited with code 1"))
	root := clientsWorkspace(t, true)

	var err error
	_, stderr := captureProcessOutput(t, func() {
		err = EnsureWorkspaceBootstrap(context.Background(), root, &wsproto.Config{}, BootstrapOptions{Command: "build", RestoreClients: restore})
	})
	if err != nil || len(*calls) != 1 {
		t.Fatalf("err = %v, restores = %d; want the command to continue after one restore", err, len(*calls))
	}
	if want := "  Generated clients were not restored: client-generate exited with code 1\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}
