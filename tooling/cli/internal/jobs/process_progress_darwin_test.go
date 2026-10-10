//go:build darwin

package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// progressHangBound is a hang detector, not a latency budget: a process whose
// counters did not do what the test waits for within it never will.
const progressHangBound = time.Minute

// TestHostProcessProgressSeesARealProcessRun pins that the task counters of a
// real started process move. The program polls for its release file every
// 10 ms, so its counters move after the baseline whenever it was taken.
func TestHostProcessProgressSeesARealProcessRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	path := fixtureproc.Write(t, filepath.Join(dir, "program"), fixtureproc.Program{WaitFor: []string{release}})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Error(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Errorf("program: %v", err)
		}
	}()
	moved := hostProcessProgress(cmd.Process)
	if moved == nil {
		t.Fatal("the counters of a live child could not be read")
	}
	for bound := time.Now().Add(progressHangBound); !moved(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(bound) {
			t.Fatalf("the counters of a polling process did not move in %s", progressHangBound)
		}
	}
}

// TestHostProcessProgressSeesAStoppedProcessStill pins that the counters of a
// process that runs no code do not move: a process the host holds before its
// first instruction reads the same way. The test stops the process and waits
// until the kernel reports it stopped, so no timing decides the outcome.
func TestHostProcessProgressSeesAStoppedProcessStill(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/sleep", "3600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WUNTRACED, nil); err != nil {
		t.Fatal(err)
	}
	// syscall's BSD WaitStatus reads a stop by SIGSTOP as Continued, not
	// Stopped, so the test reads the bits <sys/wait.h> defines: 0177 in the
	// low byte for a stopped process, the stop signal in the next one.
	if status&0xff != 0o177 || syscall.Signal(status>>8) != syscall.SIGSTOP {
		t.Fatalf("wait status %#x, want stopped by SIGSTOP", uint32(status))
	}
	moved := hostProcessProgress(cmd.Process)
	if moved == nil {
		t.Fatal("the counters of a live child could not be read")
	}
	for range 5 {
		time.Sleep(10 * time.Millisecond)
		if moved() {
			t.Fatal("the counters of a stopped process moved")
		}
	}
}

// TestHostProcessProgressCountsAnUnreadableProcessAsRunning pins the fail-safe
// direction: a process whose counters cannot be read counts as running, at the
// baseline or at a later sample, so its deadline arms instead of waiting for
// the admission bound.
func TestHostProcessProgressCountsAnUnreadableProcessAsRunning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	path := fixtureproc.Write(t, filepath.Join(dir, "program"), fixtureproc.Program{WaitFor: []string{release}})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	moved := hostProcessProgress(cmd.Process)
	if moved == nil {
		t.Fatal("the counters of a live child could not be read")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("program: %v", err)
	}
	if !moved() {
		t.Error("a sample of a reaped process reads as held")
	}
	if hostProcessProgress(cmd.Process) != nil {
		t.Error("the baseline of a reaped process reads as held")
	}
	if hostProcessProgress(nil) != nil {
		t.Error("no process reads as held")
	}
	// A darwin process ID is at most 99999.
	if watchTaskProgress(100000) != nil {
		t.Error("the baseline of a process ID no process can have reads as held")
	}
	for _, size := range []int{0, procTaskInfoSize - 1, procTaskInfoSize + 1} {
		if _, err := decodeTaskProgress(make([]byte, size)); err == nil {
			t.Errorf("a %d-byte proc_taskinfo decoded", size)
		}
	}
}
