package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestRunJobWithProcessEnv_ReachesTheProcessAndNothingElse pins the seam
// compose injects CONFIG_DATA through: the value is in the subprocess
// environment, it is absent from the job context file every task reads its
// params from, and the spawned process group is reported to the observer so a
// supervisor can record it.
func TestRunJobWithProcessEnv_ReachesTheProcessAndNothingElse(t *testing.T) {
	wsRoot := t.TempDir()
	envOut := filepath.Join(wsRoot, "env.out")
	contextOut := filepath.Join(wsRoot, "context.out")
	pidOut := filepath.Join(wsRoot, "pid.out")
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/app", Name: "app", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/widget", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/widget",
			Name:          "serve",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	fixtureTask(t, job.JobDef, fixtureScript{
		{"write-env", "CONFIG_DATA", envOut},
		{"write-pid", pidOut},
		{"copy-flag-file", "--putnamiContext", contextOut},
		{"print", `{"v":2,"type":"result","data":{"status":"success"}}`},
	})

	const secret = `{"database":{"password":"compose-secret-value"}}`
	var observed []int
	ctx := WithProcessGroupObserver(context.Background(), func(pgid int) { observed = append(observed, pgid) })
	result, err := RunJobWithProcessEnv(ctx, ws, job, map[string]any{"port": 0}, nil, nil,
		[]string{"CONFIG_DATA=" + secret}, nil)
	if err != nil {
		t.Fatalf("RunJobWithProcessEnv: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}

	env, err := os.ReadFile(envOut)
	if err != nil || string(env) != secret {
		t.Fatalf("CONFIG_DATA seen by the process = %q (%v), want the injected value", env, err)
	}
	contextFile, err := os.ReadFile(contextOut)
	if err != nil {
		t.Fatalf("read copied context file: %v", err)
	}
	if strings.Contains(string(contextFile), "compose-secret-value") || strings.Contains(string(contextFile), "CONFIG_DATA") {
		t.Errorf("the process environment leaked into the job context file:\n%s", contextFile)
	}

	pid, err := os.ReadFile(pidOut)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	if len(observed) != 1 || strconv.Itoa(observed[0]) != string(pid) {
		t.Errorf("observed process groups = %v, want exactly the spawned pid %s", observed, pid)
	}
}

// TestRunJob_WithoutAnObserverReportsNothing keeps the observer opt-in: the
// scheduler's own spawns carry no callback and must not require one.
func TestRunJob_WithoutAnObserverReportsNothing(t *testing.T) {
	observeProcessGroup(context.Background(), 42)
	if ctx := WithProcessGroupObserver(context.Background(), nil); ctx.Value(processGroupObserverKey{}) != nil {
		t.Error("a nil observer was attached to the context")
	}
}
