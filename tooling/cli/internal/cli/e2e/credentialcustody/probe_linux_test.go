//go:build linux

package credentialcustody

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
// descriptor, and procguard denies the listing, so this finds nothing. A
// reader procguard does not deny, such as root, lists them and copies what
// the parent's pipes hold without taking it (readDescriptor).
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

// descriptorPollInterval is how often readDescriptor looks again at an empty
// pipe within descriptorReadWait.
const descriptorPollInterval = 5 * time.Millisecond

// maxPipeCopy is the most a pipe copy holds: the largest pipe an unprivileged
// process may ask for (/proc/sys/fs/pipe-max-size by default).
const maxPipeCopy = 1 << 20

// readDescriptor reads at most limit bytes of what the /proc descriptor link
// path opens: a regular file, or a copy of what a pipe holds (peekPipe). It
// reads nothing else.
func readDescriptor(path string, limit int) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mode := info.Mode()
	if mode&os.ModeNamedPipe != 0 {
		return peekPipe(path, limit)
	}
	if !mode.IsRegular() {
		return nil, fmt.Errorf("%s: not a file or a pipe", path)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)))
	if len(data) > 0 {
		return data, nil
	}
	return nil, err
}

// peekPipe copies at most limit bytes of what the pipe that path opens holds,
// waiting descriptorReadWait at most for data, and leaves every byte in it.
// The path opens the parent's own pipe, not a copy: a read would take the
// bytes from the process they are written for, such as the engine's
// credential provider session, which would then wait for an answer that
// never comes until its op timeout. tee(2) duplicates them instead. A pipe no
// writer fills, such as the parent's standard input, is never waited on
// longer than descriptorReadWait.
func peekPipe(path string, limit int) ([]byte, error) {
	source, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(source) }()
	var copied [2]int
	if err := unix.Pipe2(copied[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(copied[0]); _ = unix.Close(copied[1]) }()
	// The copy holds a default pipe's 64 KiB; a larger one, when the kernel
	// grants it, takes more of a source pipe that holds more.
	_, _ = unix.FcntlInt(uintptr(copied[1]), unix.F_SETPIPE_SZ, min(limit, maxPipeCopy))
	deadline := time.Now().Add(descriptorReadWait)
	for {
		n, err := unix.Tee(source, copied[1], limit, unix.SPLICE_F_NONBLOCK)
		switch {
		case n > 0:
			data := make([]byte, n)
			for read := 0; read < len(data); {
				got, err := unix.Read(copied[0], data[read:])
				if got <= 0 {
					return data[:read], err
				}
				read += got
			}
			return data, nil
		case err == nil:
			// Empty, and no writer is left.
			return nil, nil
		case !errors.Is(err, unix.EAGAIN):
			return nil, err
		case time.Now().After(deadline):
			return nil, nil
		}
		time.Sleep(descriptorPollInterval)
	}
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

// The descriptor probe copies what a pipe holds and leaves it there: the
// process the bytes are written for still reads every one of them. A probe
// that drained the engine's pipe to its credential provider would take an
// answer the engine then waits for until its op timeout.
func TestDescriptorProbeLeavesAPipeItsBytes(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	line := custodyBearer + "\n"
	if _, err := w.WriteString(line); err != nil {
		t.Fatal(err)
	}
	// SyscallConn, not Fd, keeps r non-blocking, so the read below ends at
	// its deadline when the probe took the bytes.
	raw, err := r.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fd uintptr
	if err := raw.Control(func(descriptor uintptr) { fd = descriptor }); err != nil {
		t.Fatal(err)
	}
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	path := procPath(os.Getpid(), "fd/"+strconv.Itoa(int(fd)))
	for range 2 {
		data, err := readDescriptor(path, procBound)
		if err != nil || string(data) != line {
			t.Fatalf("readDescriptor = %q, %v; want %q", data, err, line)
		}
	}
	got := make([]byte, len(line))
	if _, err := io.ReadFull(r, got); err != nil || string(got) != line {
		t.Fatalf("the pipe's reader read %q, %v after the probe; want %q", got, err, line)
	}
	// An empty pipe with a writer left is waited on for descriptorReadWait
	// at most.
	if data, err := readDescriptor(path, procBound); err != nil || len(data) != 0 {
		t.Fatalf("readDescriptor of an empty pipe = %q, %v; want nothing", data, err)
	}
}
