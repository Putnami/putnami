package machineprobe

import (
	"strconv"
	"strings"
)

// parseLoadAverage reads the first field of a load-average text, such as the
// content of /proc/loadavg. It answers false for anything that is not a
// non-negative number.
func parseLoadAverage(raw string) (float64, bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0, false
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || load < 0 {
		return 0, false
	}
	return load, true
}
