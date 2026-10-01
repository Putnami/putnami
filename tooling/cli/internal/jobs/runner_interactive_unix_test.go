//go:build unix

package jobs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestRunJobInteractive_SharesParentProcessGroup is the regression test:
// an interactive extension inherits the real terminal via os.Stdin, and
// only the controlling terminal's foreground process group may read from it.
// The CLI is that foreground group, so the child must stay in it. Placing the
// child in its own group (SysProcAttr.Setpgid) makes it a background group:
// the line discipline still echoes typed characters, but the child's read() on
// stdin blocks forever and prompts never receive their answer.
//
// Faithful TTY foreground semantics can't be exercised without a PTY, but the
// invariant the fix establishes — child PGID == parent (CLI) PGID — is directly
// observable and fails under the old Setpgid:true code, where the child would
// become the leader of its own group.
func TestRunJobInteractive_SharesParentProcessGroup(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	pgidFile := filepath.Join(wsRoot, "pgid")
	scriptPath := filepath.Join(wsRoot, "pgid.sh")
	// ps -o pgid= prints the process group id of the running shell ($$),
	// without a header. Supported on both Linux and macOS.
	script := "#!/bin/sh\nps -o pgid= -p $$ > " + pgidFile + "\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "test-ws", Name: "test-ws", Path: "."},
		Extension: ext,
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "demo-pgid",
			Kind:          "command",
			Command:       scriptPath,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	res, err := RunJobInteractive(t.Context(), ws, job, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJobInteractive: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status = %q, want success (exit=%d, error=%v)", res.Status, res.ExitCode, res.Error)
	}

	raw, err := os.ReadFile(pgidFile)
	if err != nil {
		t.Fatalf("read pgid file: %v", err)
	}
	// `ps -o pgid= -p $$` prints one line on procps-ng, but an implementation
	// that ignores -p (busybox, and the image CI runs on) prints one line per
	// process in the group instead. Every line carries the same pgid either
	// way, so take the first and let the comparison below do the real work —
	// TrimSpace only trims the ends, so the multi-line form reached Atoi whole
	// and failed to parse a value the test had actually captured correctly.
	trimmed := strings.TrimSpace(string(raw))
	first, _, _ := strings.Cut(trimmed, "\n")
	childPgid, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		t.Fatalf("parse child pgid %q: %v", trimmed, err)
	}
	parentPgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("get parent pgid: %v", err)
	}
	if childPgid != parentPgid {
		t.Errorf("child pgid = %d, want parent pgid %d: an interactive child placed in its own process group cannot read the controlling TTY", childPgid, parentPgid)
	}
}
