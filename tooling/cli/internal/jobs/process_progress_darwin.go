//go:build darwin

package jobs

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// proc_info with PROC_INFO_CALL_PIDINFO and the PROC_PIDTASKINFO flavor fills
// a struct proc_taskinfo (<sys/proc_info.h>) for one process. syscall names
// the system call, but not these values.
const (
	procInfoCallPIDInfo = 2
	procPIDTaskInfo     = 4
	// procTaskInfoSize is sizeof(struct proc_taskinfo). A read of any other
	// size is not that struct.
	procTaskInfoSize = 96
	// Offsets of pti_total_user (uint64), pti_syscalls_mach (int32) and
	// pti_syscalls_unix (int32).
	procTaskInfoTotalUser    = 16
	procTaskInfoSyscallsMach = 72
	procTaskInfoSyscallsUnix = 76
)

// taskProgress is the part of a task's counters that moves only when the task
// runs its own code. Between exec.Cmd.Start and the first instruction of a
// freshly written executable, the host holds the new process while it checks
// the file, and all three stay frozen. dyld moves them as soon as the host
// lets the process run. System time is not part of it: it moves a little when
// the host releases the process.
type taskProgress struct {
	userTime     uint64
	machSyscalls int32
	unixSyscalls int32
}

// hostProcessProgress samples the task counters of process as its baseline.
// The returned function reports whether they moved since. A process whose
// counters cannot be read counts as running, at the baseline or at any later
// sample.
func hostProcessProgress(process *os.Process) func() bool {
	if process == nil {
		return nil
	}
	return watchTaskProgress(process.Pid)
}

func watchTaskProgress(pid int) func() bool {
	baseline, err := readTaskProgress(pid)
	if err != nil {
		return nil
	}
	return func() bool {
		current, err := readTaskProgress(pid)
		return err != nil || current != baseline
	}
}

// readTaskProgress reads the task counters of the process pid.
func readTaskProgress(pid int) (taskProgress, error) {
	var info [procTaskInfoSize]byte
	n, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDTaskInfo, 0,
		uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	if errno != 0 {
		return taskProgress{}, errno
	}
	if n > uintptr(len(info)) {
		return taskProgress{}, fmt.Errorf("proc_info reports %d bytes in a %d-byte buffer", n, len(info))
	}
	return decodeTaskProgress(info[:n])
}

// decodeTaskProgress reads the counters out of a struct proc_taskinfo.
func decodeTaskProgress(info []byte) (taskProgress, error) {
	if len(info) != procTaskInfoSize {
		return taskProgress{}, fmt.Errorf("proc_taskinfo is %d bytes, want %d", len(info), procTaskInfoSize)
	}
	return taskProgress{
		userTime:     binary.NativeEndian.Uint64(info[procTaskInfoTotalUser:]),
		machSyscalls: int32(binary.NativeEndian.Uint32(info[procTaskInfoSyscallsMach:])),
		unixSyscalls: int32(binary.NativeEndian.Uint32(info[procTaskInfoSyscallsUnix:])),
	}, nil
}
