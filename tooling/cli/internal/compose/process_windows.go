//go:build windows

package compose

import (
	"math"
	"strconv"

	"golang.org/x/sys/windows"
)

// processStartTime returns the creation time of process pid, in 100-nanosecond
// intervals since 1601 (GetProcessTimes). The value is fixed for the life of
// the process, so two reads of the same process agree and a process that
// reuses the pid reads differently. ok is false when the process is gone or
// cannot be read.
func processStartTime(pid int) (string, bool) {
	if pid <= 0 || uint64(pid) > math.MaxUint32 {
		return "", false
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(process) }()
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &creation, &exit, &kernel, &user); err != nil {
		return "", false
	}
	return strconv.FormatUint(uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime), 10), true
}
