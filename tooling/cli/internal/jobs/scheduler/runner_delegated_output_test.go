package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A delegated step writes the tool's output on the extension's stderr, then
// the extension reports the step failure and a FAILED result with no error
// message of its own, and exits 1. The job's error is the tail of that output,
// so the task:end that carries it holds the tool's error, the last line
// included.
func TestRunJob_DelegatedStepFailureCarriesTheToolOutputTail(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	scriptPath := filepath.Join(wsRoot, "delegated-step.sh")
	writeExecutable(t, scriptPath, "#!/bin/sh\n"+
		"echo '21:31:22.03 [INFO] Initialization options changed: reinitializing scheduler...' >&2\n"+
		"i=0; while [ $i -lt 80 ]; do echo \"21:31:23.00 [INFO] step $i\" >&2; i=$((i+1)); done\n"+
		"echo '21:31:40.52 [ERROR] ServiceMissingError: mongodb is not reachable' >&2\n"+
		`echo '{"v":2,"type":"diagnostic","message":"pants test x:: : exit status 1","severity":"error","code":"stacks.step_failed"}'`+"\n"+
		`echo '{"v":2,"type":"log","level":"error","message":"stacks.step_failed"}'`+"\n"+
		`echo '{"v":2,"type":"result","level":"info","message":"Job FAILED","data":{"status":"FAILED"}}'`+"\n"+
		"exit 1\n")

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/pkg", Name: "pkg", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/pants", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/pants",
			Name:          "test~test",
			Kind:          "command",
			Command:       scriptPath,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "failed" || result.Error == nil {
		t.Fatalf("result = %+v, want failed with an error", result)
	}
	if !strings.HasSuffix(result.Error.Message, "[ERROR] ServiceMissingError: mongodb is not reachable") {
		t.Fatalf("error message does not end with the tool's error:\n%s", result.Error.Message)
	}
	if strings.Contains(result.Error.Message, "reinitializing scheduler") {
		t.Fatalf("error message holds the head of the output, not its tail:\n%s", result.Error.Message)
	}
}
