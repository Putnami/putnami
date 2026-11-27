//go:build linux || darwin

package machineproc

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// fingerprintLimits are the resource limits a run's jobs inherit.
var fingerprintLimits = []struct {
	name     string
	resource int
}{
	{"nofile", unix.RLIMIT_NOFILE}, {"nproc", unix.RLIMIT_NPROC}, {"core", unix.RLIMIT_CORE},
	{"stack", unix.RLIMIT_STACK}, {"as", unix.RLIMIT_AS}, {"data", unix.RLIMIT_DATA},
	{"fsize", unix.RLIMIT_FSIZE}, {"memlock", unix.RLIMIT_MEMLOCK},
}

// processFingerprint is what a process passes to the processes it starts
// beyond its environment, working directory, umask and descriptors: its
// identity, groups, limits, priority and control group. A worker serves a
// client only when both have the same, so a run's jobs start as they would
// from the client (CLI ADR 0116 §4).
func processFingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "uid=%d gid=%d euid=%d egid=%d", os.Getuid(), os.Getgid(), os.Geteuid(), os.Getegid())
	if groups, err := unix.Getgroups(); err == nil {
		sort.Ints(groups)
		fmt.Fprintf(&b, " groups=%v", groups)
	}
	for _, l := range fingerprintLimits {
		var lim unix.Rlimit
		if unix.Getrlimit(l.resource, &lim) == nil {
			fmt.Fprintf(&b, " %s=%d/%d", l.name, lim.Cur, lim.Max)
		}
	}
	if prio, err := unix.Getpriority(unix.PRIO_PROCESS, 0); err == nil {
		fmt.Fprintf(&b, " priority=%d", prio)
	}
	if runtime.GOOS == "linux" {
		if cg, err := os.ReadFile("/proc/self/cgroup"); err == nil {
			fmt.Fprintf(&b, " cgroup=%q", strings.TrimSpace(string(cg)))
		}
	}
	return b.String()
}

// startupVariables are read once, when a Go process starts, by its runtime
// or its time package: a worker that started without a client's values cannot
// take them for a run. Variables a tool reads when it runs (GOFLAGS, GOPATH,
// GOPROXY…) reach the run's jobs with the client's environment.
var startupVariables = map[string]bool{
	"GOMAXPROCS": true, "GOGC": true, "GOMEMLIMIT": true, "GODEBUG": true, "GOTRACEBACK": true, "TZ": true,
}

// runtimeEnv is the part of env that startupVariables name.
func runtimeEnv(env []string) string {
	var kept []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if startupVariables[name] {
			kept = append(kept, kv)
		}
	}
	sort.Strings(kept)
	return strings.Join(kept, "\x00")
}

// hasTerminal reports whether this process has a controlling terminal. A
// worker has none: a prompt that opens /dev/tty, a terminal's Ctrl-C and
// Ctrl-Z, and its size would not reach a job, so such a command runs in its
// own process.
func hasTerminal() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// selfImage is the file this process runs from: on Linux the running image
// itself, even when its path was replaced since it started.
func selfImage(exe string) string {
	if runtime.GOOS == "linux" {
		return "/proc/self/exe"
	}
	return exe
}
