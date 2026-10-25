package scheduler

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestRunJobInteractiveWithStreams_UsesExplicitStreams(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	scriptPath := filepath.Join(wsRoot, "streams.sh")
	script := "#!/bin/sh\nread value\nprintf 'out:%s' \"$value\"\nprintf 'err:%s' \"$value\" >&2\n"
	// The job starts /bin/sh with the script as an argument instead of executing
	// the script: a parallel test's fork can inherit the descriptor that writes
	// the script, which Linux answers with ETXTBSY when the script itself is
	// executed. sh only reads it.
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "test-ws", Name: "test-ws", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "demo-streams",
			Kind:          "command",
			Command:       "/bin/sh",
			Args:          []string{scriptPath},
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	var stdout, stderr bytes.Buffer
	result, err := RunJobInteractiveWithStreams(
		context.Background(), ws, job, nil, nil, nil,
		strings.NewReader("fixture\n"), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("RunJobInteractiveWithStreams: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success (exit=%d, error=%v)", result.Status, result.ExitCode, result.Error)
	}
	if got := stdout.String(); got != "out:fixture" {
		t.Errorf("stdout = %q, want %q", got, "out:fixture")
	}
	if got := stderr.String(); got != "err:fixture" {
		t.Errorf("stderr = %q, want %q", got, "err:fixture")
	}
}

func TestRunJobInteractive_DirectAdapterCannotReceiveCloudCapability(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv(extensionproto.CloudTokenEnv, "interactive-adapter-must-not-receive-token")
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "lint,test,build,validate,validate-workspace")

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "test-ws", Name: "test-ws", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "custom-cloud-login",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
			Traits:        extension.CommandTraits{SideEffects: extensionproto.SideEffectsCloud},
		},
	}
	fixtureTask(t, job.JobDef, fixtureScript{
		{"exit-unless", "14", "unset:" + extensionproto.CloudTokenEnv},
		{"exit-unless", "15", "unset:" + extensionproto.CloudCapabilityAfterEnv},
	})

	result, err := RunJobInteractive(context.Background(), ws, job, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJobInteractive: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want direct interactive adapter to remain unarmed", result.Status)
	}
}

// TestRunJobInteractive_InheritsStdioAndSetsEnv runs a tiny shell script via
// RunJobInteractive and asserts that the environment carries PUTNAMI_INTERACTIVE,
// the context file is passed through, and the exit code is reported back.
func TestRunJobInteractive_InheritsStdioAndSetsEnv(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	// The script verifies that PUTNAMI_INTERACTIVE=1 is set, --putnamiContext
	// is passed, and that stdout/stderr are inherited (we don't assert on
	// captured stdout here because RunJobInteractive intentionally inherits
	// the parent stdout — the assertion is only that the script's exit code
	// flows through).
	//
	// The deadline check proves the interactive path routes the child
	// environment through applyTaskDeadline without carrying a budget the
	// test does not need: the job is unbounded, so the deadline this
	// process inherited from an outer putnami must NOT reach the child.
	t.Setenv(extensionproto.TaskDeadlineMsEnv, "5000")
	scriptPath := filepath.Join(wsRoot, "fixture.sh")
	script := `#!/bin/sh
test "$PUTNAMI_INTERACTIVE" = "1" || exit 11
test "${PUTNAMI_TASK_DEADLINE_MS+x}" != x || exit 13
case " $* " in
  *" --putnamiContext "*) ;;
  *) exit 12 ;;
esac
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}

	ws := &workspace.Workspace{
		Root: wsRoot,
		Name: "test-ws",
	}

	ext := &extension.ExtensionDescription{
		Name: "@putnami/test",
		Path: wsRoot,
	}
	jobDef := &extension.JobDefinition{
		ExtensionName: "@putnami/test",
		Name:          "demo-login",
		Kind:          "command",
		Command:       "/bin/sh",
		Args:          []string{scriptPath},
		TimeoutMs:     unboundedJobTimeoutMs,
	}

	job := &ScheduledJob{
		Project: &workspace.Project{
			ID:   "test-ws",
			Name: "test-ws",
			Path: ".",
		},
		Extension: ext,
		JobDef:    jobDef,
	}

	res, err := RunJobInteractive(t.Context(), ws, job, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJobInteractive: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status = %q, want success (exit=%d, error=%v)", res.Status, res.ExitCode, res.Error)
	}
}

// TestRunJobInteractive_PropagatesNonZeroExit confirms that a failing
// subprocess produces a "failed" result with the exit code preserved so the
// dispatcher can return it to the user.
func TestRunJobInteractive_PropagatesNonZeroExit(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	scriptPath := filepath.Join(wsRoot, "fail.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 7\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "test-ws", Name: "test-ws", Path: "."},
		Extension: ext,
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "demo-fail",
			Kind:          "command",
			Command:       "/bin/sh",
			Args:          []string{scriptPath},
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	res, err := RunJobInteractive(t.Context(), ws, job, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJobInteractive: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

// A kill that lands before the child started ends it as soon as it started,
// and counts as forced.
func TestInteractiveHold_AKillBeforeTheStartEndsTheChildOnceStarted(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"the-cli-holds-the-interactive-child-from-before-its-start")
	kills := 0
	h := holdInteractive(func() { kills++ })
	defer h.release()
	if !abort.InteractiveHeld() {
		t.Fatal("the hold does not record the child before its start")
	}
	h.killNow()
	if kills != 0 {
		t.Fatal("the hold killed a child that did not start")
	}
	if err := h.start(func() error { return nil }); err != nil {
		t.Fatalf("start: %v", err)
	}
	if kills != 1 || !h.forced() {
		t.Errorf("kills = %d, forced = %v; want the started child killed", kills, h.forced())
	}
}

// A kill that lands once the child runs ends it at once.
func TestInteractiveHold_AKillAfterTheStartEndsTheChild(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"the-cli-holds-the-interactive-child-from-before-its-start")
	kills := 0
	h := holdInteractive(func() { kills++ })
	defer h.release()
	if err := h.start(func() error { return nil }); err != nil {
		t.Fatalf("start: %v", err)
	}
	if kills != 0 || h.forced() {
		t.Fatalf("kills = %d, forced = %v before any kill", kills, h.forced())
	}
	h.killNow()
	if kills != 1 || !h.forced() {
		t.Errorf("kills = %d, forced = %v; want the child killed", kills, h.forced())
	}
}

// A child that failed to start is never killed, and its release leaves
// nothing held.
func TestInteractiveHold_AFailedStartReleasesTheHold(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "an-interactive-command-stops-on-its-own-terms",
		"the-cli-holds-the-interactive-child-from-before-its-start")
	kills := 0
	h := holdInteractive(func() { kills++ })
	h.killNow()
	startErr := errors.New("start failed")
	if err := h.start(func() error { return startErr }); !errors.Is(err, startErr) {
		t.Fatalf("start = %v, want %v", err, startErr)
	}
	h.release()
	h.release()
	if kills != 0 {
		t.Errorf("kills = %d, want none for a child that never started", kills)
	}
	if abort.InteractiveHeld() {
		t.Error("the hold still records a child that failed to start")
	}
}
