//go:build unix

package procstat

import (
	"syscall"
	"time"
)

// SelfCPU is the CPU, user plus system, this process spent itself, without its
// children. ok is false when the platform does not measure it.
func SelfCPU() (cpu time.Duration, ok bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano()), true
}

// ChildrenCPU is the CPU, user plus system, of every child this process waited
// for, with their own waited-for descendants. ok is false when the platform
// does not measure it.
func ChildrenCPU() (cpu time.Duration, ok bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &usage); err != nil {
		return 0, false
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano()), true
}
