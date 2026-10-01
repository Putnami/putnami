//go:build windows

package jobs

import (
	"syscall"
	"unsafe"
)

// memoryStatusEx is MEMORYSTATUSEX, which syscall does not declare.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// procGlobalMemoryStatusEx is GlobalMemoryStatusEx, which syscall does not
// declare. syscall loads kernel32.dll, a system DLL, from the system directory
// only.
var procGlobalMemoryStatusEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// platformPhysicalMemoryBytes is the physical memory Windows makes available,
// the counterpart of MemTotal in /proc/meminfo, or 0 when it cannot be read.
func platformPhysicalMemoryBytes() uint64 {
	if procGlobalMemoryStatusEx.Find() != nil {
		return 0
	}
	status := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if ok, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status))); ok == 0 {
		return 0
	}
	return status.TotalPhys
}
