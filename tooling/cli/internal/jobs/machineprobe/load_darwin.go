//go:build darwin

package machineprobe

import (
	"encoding/binary"
	"syscall"
)

// OneMinuteLoad is the machine's one-minute load average, read from the
// vm.loadavg sysctl without starting a process. It answers false when the host
// cannot report it.
func OneMinuteLoad() (float64, bool) {
	raw, err := syscall.Sysctl("vm.loadavg")
	if err != nil {
		return 0, false
	}
	return decodeLoadavg([]byte(raw))
}

// decodeLoadavg reads struct loadavg { fixpt_t ldavg[3]; long fscale; }: three
// little-endian uint32, then a 64-bit scale at offset 16. syscall.Sysctl drops
// one trailing NUL byte, so a short scale is padded back with zeros.
func decodeLoadavg(raw []byte) (float64, bool) {
	if len(raw) < 17 {
		return 0, false
	}
	var scaleBytes [8]byte
	copy(scaleBytes[:], raw[16:])
	scale := binary.LittleEndian.Uint64(scaleBytes[:])
	if scale == 0 {
		return 0, false
	}
	return float64(binary.LittleEndian.Uint32(raw[0:4])) / float64(scale), true
}
