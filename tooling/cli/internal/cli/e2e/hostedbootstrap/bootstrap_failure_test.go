package hostedbootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// A hosted run whose first-use install fails exits before the engine plans:
// its gate would otherwise run every task on a workspace without
// dependencies, and their failures would hide the install's. A local run
// keeps going, and its engine reports the empty workspace.
func TestAFailedBootstrapInstallStopsOnlyAHostedRun(t *testing.T) {
	const engineReached = "no project matched the default selection"
	for name, tc := range map[string]struct {
		bearer string
		code   int
		hosted bool
	}{
		"hosted": {bearer: "hosted-run-credential", code: cli.ExitError, hosted: true},
		"local":  {bearer: "", code: cli.ExitUsage},
	} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			clitest.WriteFile(t, filepath.Join(ws, wsproto.WorkspaceConfigFilename), `{"name":"ws"}`)
			// A lock and no install marker: the workspace is out of date.
			if err := lockfile.WriteLockFile(ws, lockfile.NewLockFile()); err != nil {
				t.Fatal(err)
			}
			// The suite runs under a putnami that already bootstrapped its own
			// workspace; clear its markers so this workspace bootstraps.
			t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
			t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
			t.Cleanup(runcredential.SetForTest(tc.bearer))
			t.Cleanup(lifecycle.SetInstallRunnerForTest(func(context.Context, string, *wsproto.Config, []string, lifecycle.LifecycleEnv) error {
				return errors.New("registry unreachable")
			}))

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
