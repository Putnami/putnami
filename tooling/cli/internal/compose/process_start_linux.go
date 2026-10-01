//go:build linux

package compose

import (
	"os"
	"strconv"
	"strings"
)

// processStartTime returns the start time of process pid, in clock ticks since
// boot (field 22 of /proc/<pid>/stat). The value is fixed for the life of the
// process, so two reads of the same process agree and a process that reuses
// the pid reads differently. ok is false when the process is gone or the
// record cannot be parsed.
func processStartTime(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	// The command name (field 2) is parenthesized and may contain spaces and
	// parentheses: the fields after it start after the last ')'.
	stat := string(data)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", false
	}
	fields := strings.Fields(stat[end+1:])
	// fields[0] is field 3 (state); field 22 is fields[19].
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return "", false
	}
	return fields[startTimeIndex], true
}
