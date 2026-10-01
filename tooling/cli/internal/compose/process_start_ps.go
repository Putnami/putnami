//go:build unix && !linux

package compose

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// processStartTime returns the start time of process pid as ps(1) prints it,
// in the C locale and UTC so every invocation formats it identically. The
// value is fixed for the life of the process, so two reads of the same process
// agree and a process that reuses the pid reads differently. ok is false when
// the process is gone or ps is unavailable.
func processStartTime(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)) //nolint:gosec // a fixed program; the only argument is a formatted integer
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	start := strings.Join(strings.Fields(string(out)), " ")
	return start, start != ""
}
