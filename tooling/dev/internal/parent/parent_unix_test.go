//go:build unix

package parent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	// helperResultEnv names the file the helper reports to; set only for the
	// helper process.
	helperResultEnv = "PUTNAMI_PARENT_HELPER_RESULT"
	// lossBound is how long a real child may take to see its parent's loss.
	lossBound = 10 * time.Second
)

// A real child whose parent is killed sees the loss: its watched context ends
// with ErrGone, and a write to the stdout whose reader is gone fails instead of
// ending it.
func TestWatchSeesARealParentKilled(t *testing.T) {
	spectest.Proves(t, workloadFeature, parentRequirement, "a-killed-parent-cancels-a-real-command-that-outlives-a-closed-stdout")
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	result := filepath.Join(t.TempDir(), "result")
	// sh is the parent: it starts the helper in the background, then waits.
	parent := exec.Command("/bin/sh", "-c", `"$0" -test.run='^TestWatchHelper$' -test.timeout=60s & wait`, executable)
	parent.Env = append(os.Environ(), helperResultEnv+"="+result)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	parent.Stdout = writer
	if err := parent.Start(); err != nil {
		t.Fatalf("start the parent: %v", err)
	}
	_ = writer.Close()
	defer func() { _ = parent.Process.Kill() }()

	waitForResult(t, result, "watching")
	// Close the read end, so the helper's next write to stdout meets a closed
	// pipe, then kill the parent outright: it signals nothing.
	_ = reader.Close()
	if err := parent.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL the parent: %v", err)
	}
	_ = parent.Wait()
	got := waitForResult(t, result, "survived")
	if !strings.Contains(got, "gone: "+ErrGone.Error()) {
		t.Errorf("the helper reported %q, want its context ended by ErrGone", got)
	}
	if !strings.Contains(got, "write: ") || strings.Contains(got, "write: <nil>") {
		t.Errorf("the helper reported %q, want its write to the closed stdout to fail", got)
	}
}

// TestWatchHelper is the child half of TestWatchSeesARealParentKilled: it
// watches its parent, reports the end of the watch, writes to its stdout and
// reports that it survived the write.
func TestWatchHelper(t *testing.T) {
	result := os.Getenv(helperResultEnv)
	if result == "" {
		t.Skip("child-only helper; driven by TestWatchSeesARealParentKilled")
	}
	report := func(line string) {
		file, err := os.OpenFile(result, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(3)
		}
		_, _ = fmt.Fprintln(file, line)
		_ = file.Close()
	}
	ctx, stop := Watch(context.Background())
	defer stop()
	report("watching")
	select {
	case <-ctx.Done():
	case <-time.After(lossBound):
		report("timeout")
		return
	}
	report("gone: " + context.Cause(ctx).Error())
	// A pipe without a reader fails every write with EPIPE, which ends a Go
	// process writing to its stdout unless it ignores SIGPIPE.
	_, err := os.Stdout.WriteString("the reader is gone\n")
	report(fmt.Sprintf("write: %v", err))
	report("survived")
}

// waitForResult returns the helper's report once it holds want.
func waitForResult(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(lossBound + 5*time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return string(data)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read the helper's report: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper never reported %q; it reported %q", want, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
