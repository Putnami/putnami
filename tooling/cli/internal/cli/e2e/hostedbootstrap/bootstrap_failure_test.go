package hostedbootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

const hostedBearer = "hosted-run-credential"

// failBootstrapInstall makes this process a hosted run when bearer is set,
// and makes every first-use install fail with install's error.
func failBootstrapInstall(t *testing.T, bearer string, install func(context.Context) error) {
	t.Helper()
	// The suite runs under a putnami that already bootstrapped its own
	// workspace; clear its markers so the fixture workspace bootstraps.
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
	t.Cleanup(runcredential.SetForTest(bearer))
	t.Cleanup(lifecycle.SetInstallRunnerForTest(func(ctx context.Context, _ string, _ *wsproto.Config, _ []string, _ lifecycle.LifecycleEnv) error {
		return install(ctx)
	}))
}

// A hosted run whose first-use install fails exits before the engine plans:
// its gate would otherwise run every task on a workspace without
// dependencies, and their failures would hide the install's. A local run
// keeps going, and its engine refuses build, which no extension of the empty
// workspace declares.
func TestAFailedBootstrapInstallStopsOnlyAHostedRun(t *testing.T) {
	const engineReached = "no extension of this workspace declares build"
	for name, tc := range map[string]struct {
		bearer string
		code   int
		hosted bool
	}{
		"hosted": {bearer: hostedBearer, code: cli.ExitError, hosted: true},
		"local":  {bearer: "", code: cli.ExitUsage},
	} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			clitest.WriteFile(t, filepath.Join(ws, wsproto.WorkspaceConfigFilename), `{"name":"ws"}`)
			// A lock and no install marker: the workspace is out of date.
			if err := lockfile.WriteLockFile(ws, lockfile.NewLockFile()); err != nil {
				t.Fatal(err)
			}
			failBootstrapInstall(t, tc.bearer, func(context.Context) error { return errors.New("registry unreachable") })

			code, output := clitest.RunGateArgs(t, ws, "build")

			if code != tc.code {
				t.Errorf("exit = %d, want %d\n%s", code, tc.code, output)
			}
			if !strings.Contains(output, "Workspace install did not complete: registry unreachable\n") {
				t.Errorf("output lacks the install failure line:\n%s", output)
			}
			if got := strings.Contains(output, engineReached); got == tc.hosted {
				t.Errorf("engine reached = %v, want %v:\n%s", got, !tc.hosted, output)
			}
			if got := strings.Contains(output, "Continuing;"); got == tc.hosted {
				t.Errorf("continue hint = %v, want %v:\n%s", got, !tc.hosted, output)
			}
		})
	}
}

// An interrupt that cancels a hosted run's install exits with the interrupt
// code, as it did when the engine ran on the canceled context.
func TestAnInterruptedHostedBootstrapExitsWithTheInterruptCode(t *testing.T) {
	ws := t.TempDir()
	clitest.WriteFile(t, filepath.Join(ws, wsproto.WorkspaceConfigFilename), `{"name":"ws"}`)
	if err := lockfile.WriteLockFile(ws, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failBootstrapInstall(t, hostedBearer, func(context.Context) error {
		cancel()
		return context.Canceled
	})

	code, output := clitest.RunGateArgsContext(t, ctx, ws, "build")

	if code != cli.ExitSignalReceived {
		t.Errorf("exit = %d, want %d\n%s", code, cli.ExitSignalReceived, output)
	}
}

// The executing engine of a bound request takes the same stop: a hosted
// request whose install fails records no session, and a local one runs.
func TestAFailedBootstrapInstallStopsAHostedBoundRequest(t *testing.T) {
	clitest.RequireShell(t)
	for name, tc := range map[string]struct {
		bearer string
		code   int
		hosted bool
	}{
		"hosted": {bearer: hostedBearer, code: cli.ExitError, hosted: true},
		"local":  {bearer: "", code: cli.ExitSuccess},
	} {
		t.Run(name, func(t *testing.T) {
			root, _ := clitest.PortableFixture(t, false, false)
			commit := strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
			request := filepath.Join(t.TempDir(), "request.json")
			clitest.WriteFile(t, request, string(clitest.CommitFixtureRequest(t, commit, "", runner.RequestedSelection{
				Mode: runner.SelectionModeProjects, Projects: []string{"app"},
			})))
			t.Setenv(runnerprovider.BoundRequestEnv, request)
			failBootstrapInstall(t, tc.bearer, func(context.Context) error { return errors.New("registry unreachable") })

			code, output := clitest.RunGateArgs(t, root)

			if code != tc.code {
				t.Errorf("exit = %d, want %d\n%s", code, tc.code, output)
			}
			if !strings.Contains(output, "Workspace install did not complete: registry unreachable\n") {
				t.Errorf("output lacks the install failure line:\n%s", output)
			}
			_, err := os.Lstat(filepath.Join(root, ".putnami", "sessions", "latest"))
			if ran := err == nil; ran == tc.hosted {
				t.Errorf("engine ran = %v, want %v:\n%s", ran, !tc.hosted, output)
			}
		})
	}
}
