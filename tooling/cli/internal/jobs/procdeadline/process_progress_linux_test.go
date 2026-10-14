//go:build linux

package procdeadline

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestParseRunDelayReadsTheSecondField pins how a thread's schedstat becomes
// its wait for a CPU, and that any other text is refused.
func TestParseRunDelayReadsTheSecondField(t *testing.T) {
	t.Parallel()
	if got, err := parseRunDelay([]byte("1032857 4167 12\n")); err != nil || got != 4167*time.Nanosecond {
		t.Errorf("parseRunDelay = %s, %v; want 4.167µs", got, err)
	}
	for _, schedstat := range []string{"", "1032857 4167\n", "1032857 4167 12 7\n", "1032857 -4167 12\n", "1 x 2\n"} {
		if got, err := parseRunDelay([]byte(schedstat)); err == nil {
			t.Errorf("parseRunDelay(%q) = %s, want an error", schedstat, got)
		}
	}
}

// TestReadRunDelaySumsTheThreads pins that the wait of a process is the sum
// of its threads' waits, that a thread that exited since the listing is
// skipped, and that a process with no readable thread has no wait.
func TestReadRunDelaySumsTheThreads(t *testing.T) {
	t.Parallel()
	tasks := t.TempDir()
	for thread, schedstat := range map[string]string{"100": "5 300 2\n", "101": "7 45 1\n"} {
		if err := os.MkdirAll(filepath.Join(tasks, thread), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tasks, thread, "schedstat"), []byte(schedstat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A thread that exited since the listing has no schedstat left.
	if err := os.MkdirAll(filepath.Join(tasks, "102"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := readRunDelay(tasks); err != nil || got != 345*time.Nanosecond {
		t.Errorf("readRunDelay = %s, %v; want 345ns", got, err)
	}
	if _, err := readRunDelay(filepath.Join(tasks, "102")); err == nil {
		t.Error("a process with no thread has a wait")
	}
	if _, err := readRunDelay(filepath.Join(tasks, "missing")); err == nil {
		t.Error("a process that does not exist has a wait")
	}
}

// TestHostProcessProgressReadsThisProcess pins a real read of the threads of
// a live process, and that no process reads as running at once.
func TestHostProcessProgressReadsThisProcess(t *testing.T) {
	t.Parallel()
	if hostProcessProgress(nil) != nil {
		t.Error("no process has a sampler")
	}
	if _, err := os.Stat("/proc/self/schedstat"); errors.Is(err, fs.ErrNotExist) {
		t.Skip("this kernel keeps no schedstat")
	}
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	sample := hostProcessProgress(process)
	if sample == nil {
		t.Fatal("a live process has no sampler")
	}
	if current := sample(); current.Held || !current.Measured || current.WaitedForCPU < 0 {
		t.Errorf("sample = %+v, want a running process whose wait is read", current)
	}
}
