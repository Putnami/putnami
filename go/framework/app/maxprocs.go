package app

import (
	"io/fs"
	"math"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Go 1.25+ sets GOMAXPROCS from the cgroup CPU limit but never below 2. A
// service limited to 1 CPU then runs two threads, plus the garbage collector,
// in one CPU's quota: the kernel throttles it in 100 ms slices and its tail
// latency climbs to the throttling period. Start lowers GOMAXPROCS to the
// limit, rounded up, once per process.
//
// Setting GOMAXPROCS turns off the runtime's own updates: a limit raised after
// Start is not followed. A valid GOMAXPROCS in the environment, or
// GODEBUG=containermaxprocs=0, keeps the runtime's choice.

var cpuQuotaOnce sync.Once

// limitProcs is the step Start runs; tests replace it to observe the call.
var limitProcs = limitProcsToCPUQuota

// limitProcsToCPUQuota lowers GOMAXPROCS to the process's cgroup v2 CPU limit
// when that limit is below it and GOMAXPROCS is not set in the environment.
// It runs once per process and reports the change, if any.
func limitProcsToCPUQuota() (from, to int, changed bool) {
	cpuQuotaOnce.Do(func() {
		from, to, changed = applyCPUQuota(os.DirFS("/"), os.Getenv, runtime.GOMAXPROCS)
	})
	return from, to, changed
}

// applyCPUQuota is limitProcsToCPUQuota over an injected file system,
// environment and runtime setter. gomaxprocs follows runtime.GOMAXPROCS: it
// sets a positive value and returns the previous one; 0 only reads it.
func applyCPUQuota(fsys fs.FS, getenv func(string) string, gomaxprocs func(int) int) (from, to int, changed bool) {
	if n, err := strconv.Atoi(getenv("GOMAXPROCS")); err == nil && n > 0 {
		return 0, 0, false
	}
	if godebugOff(getenv("GODEBUG"), "containermaxprocs") {
		return 0, 0, false
	}
	limit, ok := cgroupCPULimit(fsys)
	if !ok {
		return 0, 0, false
	}
	want := max(int(math.Ceil(limit)), 1)
	current := gomaxprocs(0)
	if want >= current {
		return 0, 0, false
	}
	gomaxprocs(want)
	return current, want, true
}

// godebugOff reports whether the GODEBUG list sets name to 0; the last
// setting wins, as in the runtime.
func godebugOff(godebug, name string) bool {
	off := false
	for setting := range strings.SplitSeq(godebug, ",") {
		if key, value, ok := strings.Cut(strings.TrimSpace(setting), "="); ok && key == name {
			off = value == "0"
		}
	}
	return off
}

// cgroupCPULimit returns the smallest CPU limit, in CPUs, of the process's
// cgroup v2 and its ancestors, from their cpu.max files. ok is false when no
// level sets a limit or the process runs outside cgroup v2.
func cgroupCPULimit(fsys fs.FS) (limit float64, ok bool) {
	data, err := fs.ReadFile(fsys, "proc/self/cgroup")
	if err != nil {
		return 0, false
	}
	var group string
	found := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, isV2 := strings.CutPrefix(line, "0::"); isV2 {
			group, found = rest, true
			break
		}
	}
	// A path above the cgroup namespace root ("/..") names a cgroup that is
	// not mounted here.
	if !found || strings.HasPrefix(group, "/..") {
		return 0, false
	}
	const root = "sys/fs/cgroup"
	dir := path.Join(root, path.Clean("/"+group))
	for {
		if quota, set := readCPUMax(fsys, path.Join(dir, "cpu.max")); set && (!ok || quota < limit) {
			limit, ok = quota, true
		}
		if dir == root {
			return limit, ok
		}
		dir = path.Dir(dir)
	}
}

// readCPUMax parses a cpu.max file: "<quota> <period>" in microseconds, or
// "max <period>" for no limit.
func readCPUMax(fsys fs.FS, name string) (float64, bool) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] == "max" {
		return 0, false
	}
	quota, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || quota <= 0 {
		return 0, false
	}
	period, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || period <= 0 {
		return 0, false
	}
	return quota / period, true
}
