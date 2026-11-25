//go:build linux

package machineprobe

import "os"

// OneMinuteLoad is the machine's one-minute load average, read from
// /proc/loadavg without starting a process. It answers false when the host
// cannot report it.
func OneMinuteLoad() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	return parseLoadAverage(string(data))
}
