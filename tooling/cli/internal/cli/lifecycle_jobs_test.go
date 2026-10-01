package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

func TestRunWorkspaceJob_DefaultReportsOnlyCompletedProviderAction(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "compact-progress", "default-install-shows-only-completed-actions")
	wsRoot := t.TempDir()
	writeLifecycleFixture(t, wsRoot, "app")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	var actions []lifecycle.LifecycleAction
	_, stderr := captureLifecycleStreams(t, func() {
		result, err := RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
			WorkspaceRoot: wsRoot,
			Config:        wsproto.Load(wsRoot),
			Job:           "workspace-install",
			Out:           os.Stderr,
			Display:       lifecycle.LifecycleDisplay{Interactive: true},
			OnAction:      func(action lifecycle.LifecycleAction) { actions = append(actions, action) },
		})
		if err != nil || result.Outcome != lifecycle.WorkspaceJobOK {
			t.Fatalf("RunWorkspaceJob = %+v, %v", result, err)
		}
	})

	if stderr != "" {
		t.Fatalf("default lifecycle renderer emitted non-action chrome: %q", stderr)
	}
	if len(actions) != 1 || actions[0].Description != "Workspace setup completed (@putnami/installer)" {
		t.Fatalf("actions = %+v, want only the completed provider", actions)
	}
}

func TestRunWorkspaceJob_VerboseAndFailureKeepDiagnostics(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "compact-progress", "verbose-and-failures-retain-diagnostics")
	wsRoot := t.TempDir()
	writeLifecycleFixture(t, wsRoot, "app")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	_, verboseErr := captureLifecycleStreams(t, func() {
		_, _ = RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
			WorkspaceRoot: wsRoot,
			Config:        wsproto.Load(wsRoot),
			Job:           "workspace-install",
			Out:           os.Stderr,
			Display:       lifecycle.LifecycleDisplay{Verbose: true},
		})
	})
	if !strings.Contains(verboseErr, "starting") || !strings.Contains(verboseErr, "done") {
		t.Fatalf("verbose lifecycle output lost task detail: %q", verboseErr)
	}
	if strings.Contains(verboseErr, "\x1b[") {
		t.Fatalf("redirected verbose lifecycle output contains terminal escapes: %q", verboseErr)
	}

	if err := os.WriteFile(filepath.Join(wsRoot, "installer-extension", "install.sh"), []byte("#!/bin/sh\necho installer-broke >&2\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, failureErr := captureLifecycleStreams(t, func() {
		result, err := RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
			WorkspaceRoot: wsRoot,
			Config:        wsproto.Load(wsRoot),
			Job:           "workspace-install",
			Out:           os.Stderr,
		})
		if err != nil || result.Outcome != lifecycle.WorkspaceJobFailed {
			t.Fatalf("failed RunWorkspaceJob = %+v, %v", result, err)
		}
	})
	if !strings.Contains(failureErr, "FAIL") || !strings.Contains(failureErr, "installer-broke") {
		t.Fatalf("compact failure lost diagnostics: %q", failureErr)
	}
}

// Workspace-level lifecycle jobs run through
// Engine.Run instead of a private jobs.Plan + jobs.NewScheduler composition in
// internal/commands/deps.go. These tests pin what that has to mean and, more
// importantly, what it must NOT change:
//
//  1. The job runs once per PROVIDER, not once per selected project.
//  2. A lifecycle run leaves NO persisted run state — no session file, no
//     successful-run marker — so `putnami install` cannot become what
//     `putnami sessions` reports or what a later `--impacted` run trusts.
//  3. The three outcomes the callers render differently stay distinguishable:
//     ran / no provider / no match.
//  4. The engine's own no-op notices never reach the caller's stdout; the
//     lifecycle commands print their own, job-specific ones.
//  5. The cold first-use path still installs, and under a structured
//     output mode its chatter stays off stdout — which is the whole reason the
//     global os.Stdout swap could be deleted.
//
// The Observer seam is deliberately absent: the lifecycle adapter passes nil,
// which internal/engine's TestOnlyTheTerminalAdapterSuppliesAnObserver enforces
// module-wide (ADR 0001 §4).

// writeLifecycleFixture creates a workspace with two projects and one local
// extension providing a workspace-activated "workspace-install" job. The job
// appends a line to marker so the test can count how many times it ran.
func writeLifecycleFixture(t *testing.T, wsRoot string, projects ...string) (marker string) {
	t.Helper()
	requireShell(t)

	includes := `"installer-extension"`
	for _, p := range projects {
		includes += `, "` + p + `"`
	}
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(wsRoot, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}

	write("putnami.workspace.json", `{"name":"lifecycle-ws","includes":[`+includes+`]}`, 0o644)
	write("installer-extension/putnami.json", `{"name":"@putnami/installer"}`, 0o644)
	for _, p := range projects {
		write(p+"/putnami.json", `{"name":"`+p+`"}`, 0o644)
	}

	marker = filepath.Join(wsRoot, "install-runs.txt")
	write("installer-extension/install.sh", "#!/bin/sh\nprintf 'ran\\n' >> "+marker+"\nexit 0\n", 0o755)
	write("installer-extension/putnami.extension.json", `{
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
      "timeoutMs": 10000
    }
  }
}`, 0o644)
	workspace.InvalidateLoadCache(wsRoot)
	return marker
}

// markerRuns counts how many times the fixture's install task executed.
func markerRuns(t *testing.T, marker string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "ran")
}

// A workspace-level job restores the workspace once per PROVIDER. With three
// projects and one installer extension, `putnami install` must run one install,
// not three — the property the deleted dedup-by-extension filter carried and
// that engine.Request.WorkspaceLifecycle carries now.
func TestRunWorkspaceJob_RunsOncePerProviderNotPerProject(t *testing.T) {
	wsRoot := t.TempDir()
	marker := writeLifecycleFixture(t, wsRoot, "app", "web", "api")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	var result lifecycle.WorkspaceJobResult
	captureLifecycleStreams(t, func() {
		var err error
		result, err = RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
			WorkspaceRoot: wsRoot,
			Config:        wsproto.Load(wsRoot),
			Job:           "workspace-install",
			Out:           os.Stderr,
		})
		if err != nil {
			t.Errorf("RunWorkspaceJob: %v", err)
		}
	})

	if result.Outcome != lifecycle.WorkspaceJobOK {
		t.Fatalf("outcome = %v, want WorkspaceJobOK", result.Outcome)
	}
	if runs := markerRuns(t, marker); runs != 1 {
		t.Errorf("workspace-install ran %d times over 3 projects, want exactly 1", runs)
	}
}

// A lifecycle run leaves no persisted run state. A session file would silently
// become what `putnami sessions show` reads as the latest run; a successful-run marker (keyed by branch+commands+params, no provenance)
// would let a later --impacted run treat HEAD as already built.
func TestRunWorkspaceJob_LeavesNoPersistedRunState(t *testing.T) {
	wsRoot := t.TempDir()
	writeLifecycleFixture(t, wsRoot, "app")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	captureLifecycleStreams(t, func() {
		if _, err := RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
			WorkspaceRoot: wsRoot,
			Config:        wsproto.Load(wsRoot),
			Job:           "workspace-install",
			Out:           os.Stderr,
		}); err != nil {
			t.Errorf("RunWorkspaceJob: %v", err)
		}
	})

	if entries, err := os.ReadDir(filepath.Join(wsRoot, ".putnami", "sessions")); err == nil && len(entries) > 0 {
		t.Errorf("lifecycle run wrote %d session entries, want none", len(entries))
	}
	store := workspace_state.NewSessionStore(wsRoot)
	if sha, err := store.LastBuildSHA("main", []string{"workspace-install"}, nil); err == nil && strings.TrimSpace(sha) != "" {
		t.Errorf("lifecycle run published a successful-run marker (%q), want none", sha)
	}
}

// The three outcomes the callers render differently must stay distinguishable
// once the run is an engine run: "no extension provides this job" is an error
// with the available names, while "the job matched no project" is a no-op.
func TestRunWorkspaceJob_DistinguishesMissingProviderFromNoMatches(t *testing.T) {
	t.Run("missing provider", func(t *testing.T) {
		wsRoot := t.TempDir()
		writeLifecycleFixture(t, wsRoot, "app")
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

		var result lifecycle.WorkspaceJobResult
		captureLifecycleStreams(t, func() {
			var err error
			result, err = RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
				WorkspaceRoot: wsRoot,
				Config:        wsproto.Load(wsRoot),
				Job:           "deps-upgrade", // the fixture extension provides only workspace-install
				Out:           os.Stderr,
			})
			if err != nil {
				t.Errorf("RunWorkspaceJob: %v", err)
			}
		})

		if result.Outcome != lifecycle.WorkspaceJobMissing {
			t.Fatalf("outcome = %v, want WorkspaceJobMissing", result.Outcome)
		}
		if !strings.Contains(result.AvailableJobs, "workspace-install") {
			t.Errorf("available jobs = %q, want it to name workspace-install", result.AvailableJobs)
		}
	})

	t.Run("no matching project", func(t *testing.T) {
		wsRoot := t.TempDir()
		marker := writeLifecycleFixture(t, wsRoot, "app")
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

		var result lifecycle.WorkspaceJobResult
		captureLifecycleStreams(t, func() {
			var err error
			result, err = RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
				WorkspaceRoot: wsRoot,
				Config:        wsproto.Load(wsRoot),
				Job:           "workspace-install",
				FilterTag:     "no-project-carries-this-tag",
				Out:           os.Stderr,
			})
			if err != nil {
				t.Errorf("RunWorkspaceJob: %v", err)
			}
		})

		if result.Outcome != lifecycle.WorkspaceJobNoMatches {
			t.Fatalf("outcome = %v, want WorkspaceJobNoMatches", result.Outcome)
		}
		if runs := markerRuns(t, marker); runs != 0 {
			t.Errorf("a fully filtered selection ran the job %d times, want 0", runs)
		}
	})
}

// DepsInstall's own no-op message is the one the user sees; the engine's generic
// "No jobs matched. Nothing to do." must not double it up (Request.Stdout is
// io.Discard for this adapter).
func TestDepsInstall_NoMatchesPrintsOnlyItsOwnNotice(t *testing.T) {
	wsRoot := t.TempDir()
	writeLifecycleFixture(t, wsRoot, "app")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	var out strings.Builder
	var err error
	stdout, _ := captureLifecycleStreams(t, func() {
		err = lifecycle.DepsInstall(context.Background(), wsRoot, wsproto.Load(wsRoot),
			"no-project-carries-this-tag", "", lifecycle.LifecycleEnv{Out: &out, RunJob: RunWorkspaceJob})
	})
	if err != nil {
		t.Fatalf("DepsInstall over an empty selection must be a no-op, got %v", err)
	}
	if !strings.Contains(out.String(), "No jobs matched for deps install.") {
		t.Errorf("deps install notice = %q, want its own no-op message", out.String())
	}
	if strings.Contains(out.String(), "Nothing to do") {
		t.Errorf("the engine's generic notice leaked into the caller's stream: %q", out.String())
	}
	if strings.Contains(stdout, "Nothing to do") {
		t.Errorf("the engine's generic notice leaked to the process stdout: %q", stdout)
	}
}

// The COLD path: a fresh checkout that never ran `putnami install`
// bootstraps through the same engine adapter, actually runs the workspace
// installers, and records the marker so the next command is a fast no-op. A
// mistake here is invisible in a warm workspace, which is why this drives the
// whole chain — EnsureWorkspaceBootstrap → Install → DepsInstall →
// RunWorkspaceJob → Engine.Run → the provider's task.
func TestEnsureWorkspaceBootstrap_ColdCheckoutInstallsThroughTheEngine(t *testing.T) {
	wsRoot := t.TempDir()
	marker := writeLifecycleFixture(t, wsRoot, "app")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	hometest.Temp(t) // keep AI-context writes off the developer's home
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
	// A lock file is what makes the workspace "installable"; with no recorded
	// install state beside it, this is exactly a fresh checkout.
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.lock.json"), []byte(`{"version": 2}`), 0o644); err != nil {
		t.Fatal(err)
	}

	captureLifecycleStreams(t, func() {
		lifecycle.EnsureWorkspaceBootstrap(context.Background(), wsRoot, wsproto.Load(wsRoot), lifecycle.BootstrapOptions{
			Command: "build",
			RunJob:  RunWorkspaceJob,
		})
	})

	if runs := markerRuns(t, marker); runs != 1 {
		t.Fatalf("cold bootstrap ran the workspace installer %d times, want 1", runs)
	}
	if _, err := os.Stat(filepath.Join(wsRoot, ".putnami", installStateFile)); err != nil {
		t.Fatalf("cold bootstrap did not record the install state marker: %v", err)
	}

	// Warm path: the same call is now a no-op, so a second command does not
	// reinstall. (The per-process dedup is bypassed by a fresh workspace root in
	// every test, so this really is the marker doing the work.)
	captureLifecycleStreams(t, func() {
		lifecycle.EnsureWorkspaceBootstrap(context.Background(), wsRoot, wsproto.Load(wsRoot), lifecycle.BootstrapOptions{
			Command: "build",
			RunJob:  RunWorkspaceJob,
		})
	})
	if runs := markerRuns(t, marker); runs != 1 {
		t.Errorf("a bootstrapped workspace reinstalled: %d runs, want 1", runs)
	}
}

// The A5a deliverable, end to end: under a structured output mode the cold
// bootstrap's install — including the scheduler's rendering of the
// workspace-install job — writes NOTHING to the process stdout, which is the
// invoking command's machine contract. Before A5a this held only because
// bootstrap reassigned the process-global os.Stdout for the duration.
func TestEnsureWorkspaceBootstrap_StructuredColdBootstrapKeepsStdoutClean(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "compact-progress", "implicit-install-preserves-machine-stdout")
	wsRoot := t.TempDir()
	marker := writeLifecycleFixture(t, wsRoot, "app")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	hometest.Temp(t)
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.lock.json"), []byte(`{"version": 2}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := captureLifecycleStreams(t, func() {
		lifecycle.EnsureWorkspaceBootstrap(context.Background(), wsRoot, wsproto.Load(wsRoot), lifecycle.BootstrapOptions{
			Command: "build",
			Output:  "json",
			RunJob:  RunWorkspaceJob,
		})
	})

	if runs := markerRuns(t, marker); runs != 1 {
		t.Fatalf("structured cold bootstrap ran the workspace installer %d times, want 1", runs)
	}
	if stdout != "" {
		t.Errorf("structured cold bootstrap wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "Workspace setup completed (@putnami/installer)") {
		t.Errorf("structured cold bootstrap lost its completed action; stderr = %q", stderr)
	}
	lockBytes, err := os.ReadFile(filepath.Join(wsRoot, "putnami.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(lockBytes); got != `{"version": 2}` {
		t.Errorf("implicit bootstrap rewrote committed lock: %q", got)
	}
}

// installStateFile is the bootstrap marker's filename, spelled here because it
// is unexported in internal/commands/lifecycle.
const installStateFile = "install-state.json"

// captureLifecycleStreams runs fn with the process stdout and stderr replaced by
// pipes and returns them SEPARATELY — which the shared captureStdoutStderr
// helper does not, and this file's whole point is telling the two apart.
func captureLifecycleStreams(t *testing.T, fn func()) (string, string) {
	t.Helper()

	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		_ = wOut.Close()
		_ = rOut.Close()
		t.Fatalf("os.Pipe stderr: %v", err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	restored := false
	restore := func() {
		if restored {
			return
		}
		os.Stdout, os.Stderr = origOut, origErr
		restored = true
	}
	defer func() {
		_ = wOut.Close()
		_ = wErr.Close()
		_ = rOut.Close()
		_ = rErr.Close()
	}()
	defer restore()
	outDone := drainCapturedStream(rOut)
	errDone := drainCapturedStream(rErr)

	fn()

	if err := wOut.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := wErr.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	restore()

	out := <-outDone
	if out.err != nil {
		t.Fatalf("read stdout: %v", out.err)
	}
	errOut := <-errDone
	if errOut.err != nil {
		t.Fatalf("read stderr: %v", errOut.err)
	}
	return string(out.data), string(errOut.data)
}

func TestCaptureLifecycleStreams_DrainsBothStreamsBeyondPipeCapacity(t *testing.T) {
	wantOut := strings.Repeat("o", 256*1024)
	wantErr := strings.Repeat("e", 256*1024)
	gotOut, gotErr := captureLifecycleStreams(t, func() {
		if _, err := io.WriteString(os.Stdout, wantOut); err != nil {
			t.Fatalf("write stdout capacity probe: %v", err)
		}
		if _, err := io.WriteString(os.Stderr, wantErr); err != nil {
			t.Fatalf("write stderr capacity probe: %v", err)
		}
	})
	if gotOut != wantOut {
		t.Fatalf("captured %d stdout bytes, want %d", len(gotOut), len(wantOut))
	}
	if gotErr != wantErr {
		t.Fatalf("captured %d stderr bytes, want %d", len(gotErr), len(wantErr))
	}
}
