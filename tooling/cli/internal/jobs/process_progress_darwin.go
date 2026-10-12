//go:build darwin

package jobs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"sync"
	"syscall"
	"time"
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

// proc_info with PROC_INFO_CALL_PIDINFO and the PROC_PIDREGIONINFO flavor
// fills a struct proc_regioninfo (<sys/proc_info.h>) for the first memory
// region of one process at or past an address.
const (
	procPIDRegionInfo = 7
	// procRegionInfoSize is sizeof(struct proc_regioninfo).
	procRegionInfoSize = 96
	// Offsets of pri_user_tag (uint32), pri_address and pri_size (uint64).
	procRegionInfoUserTag    = 32
	procRegionInfoAddress    = 80
	procRegionInfoRegionSize = 88
)

// proc_info with PROC_INFO_CALL_PIDRUSAGE and the RUSAGE_INFO_V4 flavor fills
// a struct rusage_info_v4 (<sys/resource.h>) for one process: what
// proc_pid_rusage(3) returns. The host keeps it for a process that exited
// until the process is reaped.
const (
	procInfoCallPIDRusage = 9
	rusageInfoV4          = 4
	// rusageInfoV4Size is sizeof(struct rusage_info_v4).
	rusageInfoV4Size = 296
	// Offsets of ri_user_time, ri_system_time and ri_runnable_time, each a
	// uint64 count of mach time units. ri_runnable_time counts the time the
	// threads of the process were ready to run, on a CPU or waiting for one.
	rusageInfoUserTime     = 16
	rusageInfoSystemTime   = 24
	rusageInfoRunnableTime = 288
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

// hostProcessProgress reads the task counters of process as the baseline
// that tells a held process. The returned function reads the process held
// while they have not moved since, unless the baseline found the process
// already ran its own code. Once they have, it reads how long the process has
// waited for a CPU. A process whose counters cannot be read counts as
// running, at the baseline or at any later sample, and a wait that cannot be
// read is not measured.
func hostProcessProgress(process *os.Process) func() processSample {
	if process == nil {
		return nil
	}
	return watchTaskProgress(process.Pid)
}

func watchTaskProgress(pid int) func() processSample {
	baseline, err := readTaskProgress(pid)
	if err != nil {
		return nil
	}
	// A process that ran and then blocked before the baseline keeps still
	// counters too: only its memory tells it from a held one.
	ran := hasRunItsOwnCode(pid)
	return func() processSample {
		if !ran {
			if current, err := readTaskProgress(pid); err == nil && current == baseline {
				return processSample{held: true}
			}
		}
		waited, err := readCPUWait(pid)
		return processSample{waitedForCPU: waited, measured: err == nil}
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

// hasRunItsOwnCode reports that the process pid holds memory that only code
// running in it allocates: a region tagged by malloc and the other user-space
// allocators, by dyld or by the once-only allocator of libSystem. exec gives the new process a fresh address
// space whose only tagged region is its stack, so a process the host holds
// before its first instruction has none. dyld and the libSystem initializer
// create them before main, whatever the parent and the arguments. A process
// whose regions cannot be read, or that blocked inside dyld before any of
// them, is not proven to have run.
func hasRunItsOwnCode(pid int) bool {
	var address uint64
	for {
		tag, next, err := readRegion(pid, address)
		if err != nil || next <= address {
			return false
		}
		if allocatedByOwnCode(tag) {
			return true
		}
		address = next
	}
}

// allocatedByOwnCode reports that tag, a region's user tag
// (<mach/vm_statistics.h>), names an allocator that runs in the process: the
// tags from VM_MEMORY_MALLOC to VM_MEMORY_MALLOC_PROB_GUARD, which also
// include VM_MEMORY_SBRK and VM_MEMORY_ANALYSIS_TOOL, VM_MEMORY_DYLD,
// VM_MEMORY_DYLD_MALLOC and VM_MEMORY_OS_ALLOC_ONCE.
func allocatedByOwnCode(tag uint32) bool {
	return tag >= 1 && tag <= 13 || tag == 60 || tag == 61 || tag == 73
}

// readRegion reads the user tag of the first memory region of the process pid
// at or past address, and the address that region ends at.
func readRegion(pid int, address uint64) (tag uint32, end uint64, err error) {
	var info [procRegionInfoSize]byte
	n, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDRegionInfo,
		uintptr(address), uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	if errno != 0 {
		return 0, 0, errno
	}
	if n > uintptr(len(info)) {
		return 0, 0, fmt.Errorf("proc_info reports %d bytes in a %d-byte buffer", n, len(info))
	}
	return decodeRegion(info[:n])
}

// decodeRegion reads the user tag and the end address out of a struct
// proc_regioninfo. An end past the address space reads as 0.
func decodeRegion(info []byte) (tag uint32, end uint64, err error) {
	if len(info) != procRegionInfoSize {
		return 0, 0, fmt.Errorf("proc_regioninfo is %d bytes, want %d", len(info), procRegionInfoSize)
	}
	end, carry := bits.Add64(binary.NativeEndian.Uint64(info[procRegionInfoAddress:]),
		binary.NativeEndian.Uint64(info[procRegionInfoRegionSize:]), 0)
	if carry != 0 {
		end = 0
	}
	return binary.NativeEndian.Uint32(info[procRegionInfoUserTag:]), end, nil
}

// machTimebaseFrequency is how many mach time units the host counts per
// second.
var machTimebaseFrequency = sync.OnceValues(func() (uint64, error) {
	value, err := syscall.Sysctl("hw.tbfrequency")
	if err != nil {
		return 0, err
	}
	return decodeSysctlUint64(value)
})

// decodeSysctlUint64 reads a 64-bit sysctl value out of what syscall.Sysctl
// returns: its bytes, less a last byte that is zero. Every darwin host is
// little-endian, so that byte is the most significant one, and zeros put
// back in its place restore the value.
func decodeSysctlUint64(value string) (uint64, error) {
	var buf [8]byte
	if len(value) > len(buf) {
		return 0, fmt.Errorf("sysctl value is %d bytes, want at most %d", len(value), len(buf))
	}
	copy(buf[:], value)
	return binary.LittleEndian.Uint64(buf[:]), nil
}

// readCPUWait reads how long the threads of the process pid have waited for a
// CPU, in total since it started.
func readCPUWait(pid int) (time.Duration, error) {
	var info [rusageInfoV4Size]byte
	// The kernel sizes the copy by the flavor, so the call takes no buffer
	// size.
	_, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDRusage, uintptr(pid), rusageInfoV4, 0,
		uintptr(unsafe.Pointer(&info[0])), 0)
	if errno != 0 {
		return 0, errno
	}
	frequency, err := machTimebaseFrequency()
	if err != nil {
		return 0, err
	}
	return decodeCPUWait(info[:], frequency)
}

// decodeCPUWait reads, out of a struct rusage_info_v4, the time the process
// was ready to run and not on a CPU: its runnable time less its user and
// system time. frequency is the mach time units per second.
func decodeCPUWait(info []byte, frequency uint64) (time.Duration, error) {
	if len(info) != rusageInfoV4Size {
		return 0, fmt.Errorf("rusage_info_v4 is %d bytes, want %d", len(info), rusageInfoV4Size)
	}
	if frequency == 0 {
		return 0, errors.New("mach timebase frequency is 0")
	}
	runnable := binary.NativeEndian.Uint64(info[rusageInfoRunnableTime:])
	onCPU, carry := bits.Add64(binary.NativeEndian.Uint64(info[rusageInfoUserTime:]),
		binary.NativeEndian.Uint64(info[rusageInfoSystemTime:]), 0)
	if carry != 0 || runnable <= onCPU {
		return 0, nil
	}
	return machUnitsToDuration(runnable-onCPU, frequency), nil
}

// machUnitsToDuration converts a count of mach time units, at frequency units
// per second, to a duration, capped at the longest duration.
func machUnitsToDuration(units, frequency uint64) time.Duration {
	hi, lo := bits.Mul64(units, uint64(time.Second))
	if hi >= frequency {
		return math.MaxInt64
	}
	nanoseconds, _ := bits.Div64(hi, lo, frequency)
	if nanoseconds > math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(nanoseconds)
}
