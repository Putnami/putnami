//go:build !linux && !darwin

package machineprobe

// OneMinuteLoad answers false: this platform reports no load average.
func OneMinuteLoad() (float64, bool) { return 0, false }
