//go:build linux

package credentialcustody

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The Linux-only reach a repository process of the same user has into another
// process: its /proc environ, memory and descriptors, and a ptrace attach. On
// macOS none of these exist for a same-user process — there is no /proc and
// task_for_pid is restricted — so probe_other_test.go contributes no probes and
// the test asserts only the environment and file probes there (spec nonGoal:
// "Denying inspection on macOS").
//
// Each probe targets the engine, this process's parent, which holds the run
// credential and is non-dumpable (procguard.DenyInspection). The kernel then
// owns its /proc files by root and refuses this same-user reader, and refuses a
// ptrace attach: every probe must record a check and find no secret.

// procBound caps the bytes a memory or descriptor probe reads from the parent.
const procBound = 16 << 20

// platformSearchProbes names the Linux probes that must find no secret.
func platformSearchProbes() []string {
	return []string{"env-ancestors", "proc-environ-parent", "proc-mem-parent", "proc-fd-parent", "ptrace-parent"}
}

// platformProbes runs the Linux probes against the parent (the engine).
func platformProbes(role string) []finding {
	parent := os.Getppid()
	return []finding{
		probeEnvAncestors(role),
		probeProcEnviron(role, parent),
		probeProcMem(role, parent),
		probeProcFD(role, parent),
		probePtrace(role, parent),
	}
}

// probeEnvAncestors walks the parent chain and reads each ancestor's
// /proc/<pid>/environ. The engine's environ holds the two tokens (they entered
// on its stack and os.Unsetenv does not rewrite /proc), so a readable engine
// environ is a leak; procguard must make the read fail.
func probeEnvAncestors(role string) finding {
	f := finding{Role: role, Probe: "env-ancestors", Checked: true}
	foundSet := map[string]bool{}
	pid := os.Getppid()
	levels, denied := 0, 0
	for pid > 1 && levels < 64 {
		levels++
		data, err := os.ReadFile(procPath(pid, "environ"))
		if err != nil {
			denied++
		} else {
			for _, s := range scanBytes(data) {
				foundSet[s] = true
			}
		}
		next, ok := parentPID(pid)
		if !ok {
			break
		}
		pid = next
	}
	f.Found = collectFound(foundSet)
	f.Detail = fmt.Sprintf("levels=%d denied=%d", levels, denied)
	return f
}

// probeProcEnviron reads the parent's /proc/<pid>/environ directly.
func probeProcEnviron(role string, pid int) finding {
	f := finding{Role: role, Probe: "proc-environ-parent", Checked: true}
	data, err := os.ReadFile(procPath(pid, "environ"))
	if err != nil {
		f.Detail = "denied: " + err.Error()
		return f
	}
	f.Found = scanBytes(data)
	f.Detail = fmt.Sprintf("read=%d", len(data))
	return f
}

// probeProcMem opens the parent's /proc/<pid>/mem and, if it can, reads its
// readable regions (from /proc/<pid>/maps) up to procBound and scans them.
func probeProcMem(role string, pid int) finding {
	f := finding{Role: role, Probe: "proc-mem-parent", Checked: true}
	mem, err := os.Open(procPath(pid, "mem"))
	if err != nil {
		f.Detail = "open-denied: " + err.Error()
		return f
	}
	defer func() { _ = mem.Close() }()

	maps, err := os.ReadFile(procPath(pid, "maps"))
	if err != nil {
		f.Detail = "maps-denied: " + err.Error()
		return f
	}
	foundSet := map[string]bool{}
	read := 0
	for _, line := range strings.Split(string(maps), "\n") {
		if read >= procBound {
			break
		}
		start, size, ok := readableRegion(line)
		if !ok {
			continue
		}
		if size > procBound-read {
			size = procBound - read
		}
		buf := make([]byte, size)
		n, _ := mem.ReadAt(buf, int64(start))
		if n <= 0 {
			continue
		}
		read += n
		for _, s := range scanBytes(buf[:n]) {
			foundSet[s] = true
		}
	}
	f.Found = collectFound(foundSet)
	f.Detail = fmt.Sprintf("read=%d", read)
	return f
}

// probeProcFD lists the parent's /proc/<pid>/fd and reads what it can of each
// descriptor, in case one still holds the credential pipe. Capture closed that
// descriptor, and procguard denies the listing, so this finds nothing.
func probeProcFD(role string, pid int) finding {
	f := finding{Role: role, Probe: "proc-fd-parent", Checked: true}
	entries, err := os.ReadDir(procPath(pid, "fd"))
	if err != nil {
		f.Detail = "list-denied: " + err.Error()
		return f
	}
	foundSet := map[string]bool{}
	read := 0
	for _, entry := range entries {
		if read >= procBound {
			break
		}
		data, err := readDescriptor(procPath(pid, "fd/"+entry.Name()), procBound-read)
		if err != nil {
			continue
		}
		read += len(data)
		for _, s := range scanBytes(data) {
			foundSet[s] = true
		}
	}
	f.Found = collectFound(foundSet)
	f.Detail = fmt.Sprintf("fds=%d read=%d", len(entries), read)
	return f
}

// probePtrace attaches to the parent with ptrace. A non-dumpable same-user
// target refuses it (EPERM), and the default yama scope refuses a non-descendant
// too. A successful attach would let the probe read the parent's registers and
// memory, so it is itself the leak: the probe reads memory after it and records
// what it found before detaching.
func probePtrace(role string, pid int) finding {
	f := finding{Role: role, Probe: "ptrace-parent", Checked: true}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := syscall.PtraceAttach(pid); err != nil {
		f.Detail = "attach-denied: " + err.Error()
		return f
	}
	// Unexpected: we attached to the credential holder. Read its memory so the
	// finding names the leak, then detach so the parent runs on.
	var status syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &status, 0, nil)
	if mem, err := os.Open(procPath(pid, "mem")); err == nil {
		buf := make([]byte, procBound)
		foundSet := map[string]bool{}
		if maps, err := os.ReadFile(procPath(pid, "maps")); err == nil {
			read := 0
			for _, line := range strings.Split(string(maps), "\n") {
				if read >= procBound {
					break
				}
				start, size, ok := readableRegion(line)
				if !ok {
					continue
				}
				if size > len(buf) {
					size = len(buf)
				}
				n, _ := mem.ReadAt(buf[:size], int64(start))
				if n <= 0 {
					continue
				}
				read += n
				for _, s := range scanBytes(buf[:n]) {
					foundSet[s] = true
				}
			}
		}
		f.Found = collectFound(foundSet)
		_ = mem.Close()
	}
	_ = syscall.PtraceDetach(pid)
	f.Detail = "attached"
	return f
}

// descriptorReadWait bounds how long readDescriptor waits on a pipe that holds
// no data yet.
const descriptorReadWait = 100 * time.Millisecond

// readDescriptor reads at most limit bytes of what the /proc descriptor link
// path opens: a regular file, or a pipe without blocking. A pipe of a readable
// parent that no writer fills, such as its standard input, would otherwise
// hold the probe, and the parent with it, forever. It reads nothing else.
func readDescriptor(path string, limit int) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mode := info.Mode()
	if !mode.IsRegular() && mode&os.ModeNamedPipe == 0 {
		return nil, fmt.Errorf("%s: not a file or a pipe", path)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if mode&os.ModeNamedPipe != 0 {
		_ = file.SetReadDeadline(time.Now().Add(descriptorReadWait))
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)))
	if len(data) > 0 {
		return data, nil
	}
	return nil, err
}

// procPath returns /proc/<pid>/<sub>.
func procPath(pid int, sub string) string {
	return "/proc/" + strconv.Itoa(pid) + "/" + sub
}

// parentPID reads the parent pid of pid from /proc/<pid>/stat. The comm field
// is parenthesized and may hold spaces, so the fields after the last ')' are
// state then ppid.
func parentPID(pid int) (int, bool) {
	data, err := os.ReadFile(procPath(pid, "stat"))
	if err != nil {
		return 0, false
	}
	stat := string(data)
	close := strings.LastIndex(stat, ")")
	if close < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[close+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// readableRegion parses one /proc/<pid>/maps line and returns the start address
// and byte length of a readable region, or ok false for a line that is not
// readable or not parseable.
func readableRegion(line string) (start uint64, size int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || len(fields[1]) < 1 || fields[1][0] != 'r' {
		return 0, 0, false
	}
	bounds := strings.SplitN(fields[0], "-", 2)
	if len(bounds) != 2 {
		return 0, 0, false
	}
	lo, err := strconv.ParseUint(bounds[0], 16, 64)
	if err != nil {
		return 0, 0, false
	}
	hi, err := strconv.ParseUint(bounds[1], 16, 64)
	if err != nil || hi <= lo {
		return 0, 0, false
	}
	return lo, int(hi - lo), true
}
