//go:build darwin

package jobs

import (
	"encoding/binary"
	"math"
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
// real started process move, and that the sample that sees it run reads its
// wait for a CPU. The program polls for its release file every 10 ms, so its
// counters move after the baseline whenever it was taken.
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
	sample := hostProcessProgress(cmd.Process)
	if sample == nil {
		t.Fatal("the counters of a live child could not be read")
	}
	current := sample()
	for bound := time.Now().Add(progressHangBound); current.held; current = sample() {
		if time.Now().After(bound) {
			t.Fatalf("the counters of a polling process did not move in %s", progressHangBound)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !current.measured {
		t.Error("the wait for a CPU of a live child could not be read")
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
	sample := hostProcessProgress(cmd.Process)
	if sample == nil {
		t.Fatal("the counters of a live child could not be read")
	}
	for range 5 {
		time.Sleep(10 * time.Millisecond)
		if !sample().held {
			t.Fatal("the counters of a stopped process moved")
		}
	}
}

// TestHostProcessProgressCountsAnUnreadableProcessAsRunning pins the fail-safe
// direction: a process whose counters cannot be read counts as running, at the
// baseline or at a later sample, so its deadline arms instead of waiting for
// the bound on time not counted.
func TestHostProcessProgressCountsAnUnreadableProcessAsRunning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	path := fixtureproc.Write(t, filepath.Join(dir, "program"), fixtureproc.Program{WaitFor: []string{release}})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sample := hostProcessProgress(cmd.Process)
	if sample == nil {
		t.Fatal("the counters of a live child could not be read")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("program: %v", err)
	}
	if sample().held {
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
	if _, err := readCPUWait(100000); err == nil {
		t.Error("the wait for a CPU of a process ID no process can have was read")
	}
	for _, size := range []int{0, procTaskInfoSize - 1, procTaskInfoSize + 1} {
		if _, err := decodeTaskProgress(make([]byte, size)); err == nil {
			t.Errorf("a %d-byte proc_taskinfo decoded", size)
		}
	}
}

// TestDecodeCPUWaitReadsTheRunnableTimeOffACPU pins how a struct
// rusage_info_v4 becomes a wait for a CPU: its runnable time less its user
// and system time, converted from mach time units.
func TestDecodeCPUWaitReadsTheRunnableTimeOffACPU(t *testing.T) {
	t.Parallel()
	info := func(user, system, runnable uint64) []byte {
		buf := make([]byte, rusageInfoV4Size)
		binary.NativeEndian.PutUint64(buf[rusageInfoUserTime:], user)
		binary.NativeEndian.PutUint64(buf[rusageInfoSystemTime:], system)
		binary.NativeEndian.PutUint64(buf[rusageInfoRunnableTime:], runnable)
		return buf
	}
	// 24 MHz is the frequency of Apple silicon: a unit is 125/3 ns.
	const frequency = 24_000_000
	cases := []struct {
		name                   string
		user, system, runnable uint64
		want                   time.Duration
	}{
		{"a wait", 10, 5, 45, 1250 * time.Nanosecond},
		{"no wait", 10, 5, 15, 0},
		{"a runnable time short of the time on a CPU", 10, 5, 12, 0},
		{"a time on a CPU past the counter", math.MaxUint64, 1, 1, 0},
		{"a wait past the longest duration", 0, 0, math.MaxUint64, math.MaxInt64},
	}
	for _, tc := range cases {
		got, err := decodeCPUWait(info(tc.user, tc.system, tc.runnable), frequency)
		if err != nil || got != tc.want {
			t.Errorf("%s: decodeCPUWait = %s, %v; want %s", tc.name, got, err, tc.want)
		}
	}
	for _, size := range []int{0, rusageInfoV4Size - 1, rusageInfoV4Size + 1} {
		if _, err := decodeCPUWait(make([]byte, size), frequency); err == nil {
			t.Errorf("a %d-byte rusage_info_v4 decoded", size)
		}
	}
	if _, err := decodeCPUWait(info(0, 0, 1), 0); err == nil {
		t.Error("a wait decoded with a mach timebase of zero units per second")
	}
	if got := machUnitsToDuration(3*frequency, frequency); got != 3*time.Second {
		t.Errorf("3 s of mach units = %s", got)
	}
}

// TestMachTimebaseFrequencyIsRead pins how a 64-bit sysctl value is decoded,
// with and without the zero last byte syscall.Sysctl drops, and that the host
// reports a frequency.
func TestMachTimebaseFrequencyIsRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value string
		want  uint64
	}{
		{"\x00\x36\x6e\x01\x00\x00\x00", 24_000_000},
		{"\x00\x36\x6e\x01\x00\x00\x00\x01", 1<<56 + 24_000_000},
		{"", 0},
	}
	for _, tc := range cases {
		if got, err := decodeSysctlUint64(tc.value); err != nil || got != tc.want {
			t.Errorf("decodeSysctlUint64(%q) = %d, %v; want %d", tc.value, got, err, tc.want)
		}
	}
	if _, err := decodeSysctlUint64("123456789"); err == nil {
		t.Error("a 9-byte sysctl value decoded")
	}
	if frequency, err := machTimebaseFrequency(); err != nil || frequency == 0 {
		t.Errorf("machTimebaseFrequency = %d, %v; want a frequency", frequency, err)
	}
}
