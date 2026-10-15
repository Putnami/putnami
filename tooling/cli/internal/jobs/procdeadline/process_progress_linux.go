//go:build linux

package procdeadline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// hostProcessProgress counts a process as running as soon as exec.Cmd.Start
// returned: Linux does not hold a started process before its first
// instruction. The returned function reads how long the threads of the
// process have waited for a CPU. A process whose threads cannot be read, such
// as on a kernel built without schedstats, has its wait not measured, so its
// deadline counts every moment of it.
func hostProcessProgress(process *os.Process) func() ProcessSample {
	if process == nil {
		return nil
	}
	tasks := filepath.Join("/proc", strconv.Itoa(process.Pid), "task")
	return func() ProcessSample {
		waited, err := readRunDelay(tasks)
		return ProcessSample{WaitedForCPU: waited, Measured: err == nil}
	}
}

// readRunDelay sums the run_delay of every thread listed under tasks, a
// /proc/<pid>/task directory: how long each has waited for a CPU since it
// started. It skips a thread whose schedstat cannot be read, which is one that
// exited since the listing, and fails when no thread's can.
//
// The kernel adds a wait to run_delay once the thread gets a CPU, so the wait
// of a thread still waiting is not in the sum yet.
func readRunDelay(tasks string) (time.Duration, error) {
	threads, err := os.ReadDir(tasks)
	if err != nil {
		return 0, err
	}
	var total time.Duration
	read := false
	for _, thread := range threads {
		schedstat, err := os.ReadFile(filepath.Join(tasks, thread.Name(), "schedstat"))
		if err != nil {
			continue
		}
		delay, err := parseRunDelay(schedstat)
		if err != nil {
			return 0, err
		}
		total += delay
		read = true
	}
	if !read {
		return 0, errors.New("no thread schedstat could be read under " + tasks)
	}
	return total, nil
}

// parseRunDelay reads run_delay, in nanoseconds, out of a thread's schedstat:
// the time on a CPU, the time waiting for one, and the number of times it got
// one.
func parseRunDelay(schedstat []byte) (time.Duration, error) {
	fields := strings.Fields(string(schedstat))
	if len(fields) != 3 {
		return 0, fmt.Errorf("schedstat %q has %d fields, want 3", schedstat, len(fields))
	}
	delay, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || delay < 0 {
		return 0, fmt.Errorf("schedstat %q has no run_delay", schedstat)
	}
	return time.Duration(delay), nil
}
