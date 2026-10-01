package lifecycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// A hosted install leaves the committed lock byte for byte, even a CLI pin
// that a CLI of another version wrote without its protocol version, which an
// install without the run credential stamps. It pins no toolchain and writes
// no assistant content either.
func TestInstall_AHostedInstallWritesNoLock(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "cold-install-leaves-locks-unchanged", "hosted-install-writes-no-lock")
	hometest.Temp(t)
	origPin, origFill := pinExplicitToolchains, fillImplicitToolchainPins
	t.Cleanup(func() { pinExplicitToolchains, fillImplicitToolchainPins = origPin, origFill })
	var pinned int
	pinExplicitToolchains = func(context.Context, string) (bool, error) { pinned++; return true, nil }
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) { pinned++; return true, nil }

	var dir string
	install := func(bearer string) (before, after []byte) {
		t.Helper()
		dir = t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(`{"name":"install-ws"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		locked := lockfile.NewLockFile()
		locked.SetCLI(lockfile.LockEntry{Version: "4.2.1"})
		if err := lockfile.WriteLockFile(dir, locked); err != nil {
			t.Fatal(err)
		}
		before = readLock(t, dir)
		restore := runcredential.SetForTest(bearer)
		defer restore()
		var out strings.Builder
		env := installTestEnv(&out, WorkspaceJobOK)
		env.CLIVersion = "4.2.1"
		if _, err := captureStdout(t, func() error {
			return Install(context.Background(), dir, wsproto.Load(dir), nil, env)
		}); err != nil {
			t.Fatalf("Install: %v", err)
		}
		return before, readLock(t, dir)
	}

	if before, after := install("run-bearer"); !bytes.Equal(before, after) || pinned != 0 {
		t.Fatalf("a hosted install changed the lock (%d pin calls):\n%s\nto\n%s", pinned, before, after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		// .putnami holds the ignored engine state, not a committed file.
		if name := entry.Name(); name != wsproto.WorkspaceConfigFilename && name != lockfile.LockFilename && name != ".putnami" {
			t.Errorf("a hosted install wrote %s into the workspace", name)
		}
	}
	if before, after := install(""); bytes.Equal(before, after) || pinned == 0 {
		t.Fatalf("an install without the run credential left the lock unstamped (%d pin calls):\n%s", pinned, after)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf("an install without the run credential registered no MCP server: %v", err)
	}
}

// A hosted install runs every workspace-fetch before any onInstall hook and
// any installer, then BeforeRepositoryCode, then the hooks, then the
// installers, and it fetches once: the fetch receives the run credential, and
// no process started after repository code does. A fetch that fails runs no
// hook and no installer. An install without the run credential fetches
// nothing and runs its hooks with the extensions.
func TestInstall_AHostedInstallFetchesBeforeAnyHookOrInstaller(t *testing.T) {
	hometest.Temp(t)
	origHooks, origPin, origFill := runDeferredInstallHooks, pinExplicitToolchains, fillImplicitToolchainPins
	t.Cleanup(func() {
		runDeferredInstallHooks, pinExplicitToolchains, fillImplicitToolchainPins = origHooks, origPin, origFill
	})
	pinExplicitToolchains = func(context.Context, string) (bool, error) { return false, nil }
	fillImplicitToolchainPins = func(context.Context, string) (bool, error) { return false, nil }
	const hooksRan = "onInstall hooks"
	fetch, install := extensionproto.WorkspaceFetchCommand, "workspace-install"

	for name, c := range map[string]struct {
		bearer  string
		fetch   WorkspaceJobOutcome
		want    []string
		wantErr bool
	}{
		"a hosted install":          {bearer: "run-bearer", fetch: WorkspaceJobOK, want: []string{fetch, beforeRepositoryCode, hooksRan, install}},
		"a hosted fetch that fails": {bearer: "run-bearer", fetch: WorkspaceJobFailed, want: []string{fetch}, wantErr: true},
		"a local install":           {want: []string{install}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(`{"name":"install-ws"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			restore := runcredential.SetForTest(c.bearer)
			defer restore()
			var ran []string
			runDeferredInstallHooks = func(context.Context, string, *wsproto.Config, []string, io.Writer) error {
				ran = append(ran, hooksRan)
				return nil
			}
			var out strings.Builder
			env := LifecycleEnv{Out: &out, RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
				ran = append(ran, req.Job)
				if req.Job == fetch {
					return WorkspaceJobResult{Outcome: c.fetch}, nil
				}
				return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
			}, BeforeRepositoryCode: func(context.Context) error {
				ran = append(ran, beforeRepositoryCode)
				return nil
			}}
			_, err := captureStdout(t, func() error {
				return Install(context.Background(), dir, wsproto.Load(dir), nil, env)
			})
			if (err != nil) != c.wantErr {
				t.Errorf("Install = %v, want an error: %v", err, c.wantErr)
			}
			if !reflect.DeepEqual(ran, c.want) {
				t.Errorf("ran %q, want %q", ran, c.want)
			}
		})
	}
}

func readLock(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
