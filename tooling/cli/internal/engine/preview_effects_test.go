package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	workspacepb "go.putnami.dev/protocol/workspace"
)

// --plan and a plan-only --dry-run plan the selected job and start nothing:
// the job's process never runs and no session, last-build record or other
// session state is written. A build of the same workspace runs the job, so the
// fixture observes a run when one happens.
func TestRun_PlanOnlyPreviewsExecuteNothing(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "preview-no-effects", "plan-only-and-dry-runs-execute-nothing")
	if runtime.GOOS == "windows" {
		t.Skip("the fixture task is a POSIX shell script")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("the fixture task runs through /bin/sh")
	}
	for name, global := range map[string]GlobalFlags{
		"--plan":    {Plan: true},
		"--dry-run": {DryRun: true},
	} {
		t.Run(name, func(t *testing.T) {
			root, runs := shippingWorkspace(t, false, false)
			var result SessionResult
			var err error
			stderr := captureStderr(t, func() {
				result, err = New().Run(context.Background(), Request{
					WorkspaceRoot: root, Config: workspacepb.Load(root), Commands: []string{"build"},
					Global: global, Stdout: io.Discard,
				}, discardEvents{})
			})
			if err != nil || result.ExitCode != ExitSuccess {
				t.Fatalf("%s = exit %d, %v (stderr %q), want success", name, result.ExitCode, err, stderr)
			}
			if len(result.Plan) != 1 {
				t.Fatalf("%s planned %d jobs, want the one build job", name, len(result.Plan))
			}
			if _, err := os.Stat(runs); !os.IsNotExist(err) {
				t.Errorf("%s ran the job: %v", name, err)
			}
			if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
				t.Errorf("%s wrote session state: %v", name, err)
			}
		})
	}

	root, runs := shippingWorkspace(t, false, false)
	var result SessionResult
	stderr := captureStderr(t, func() {
		result, _ = New().Run(context.Background(), Request{
			WorkspaceRoot: root, Config: workspacepb.Load(root), Commands: []string{"build"}, Stdout: io.Discard,
		}, discardEvents{})
	})
	if result.ExitCode != ExitSuccess {
		t.Fatalf("build = exit %d (stderr %q), want success", result.ExitCode, stderr)
	}
	if data, _ := os.ReadFile(runs); string(data) != "run\n" {
		t.Fatalf("build task runs = %q, want one run", data)
	}
}
