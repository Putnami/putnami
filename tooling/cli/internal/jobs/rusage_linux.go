//go:build linux

package jobs

import (
	"os"
	"syscall"
)

// processRusage reads peak RSS and the block-IO counters off a finished
// process's rusage.
//
// UNIT, stated once: linux's ru_maxrss is in KILOBYTES (getrusage(2)), where
// darwin reports bytes. The ledger's MaxRSSBytes is bytes on every platform, so
// the ×1024 belongs here — at the one place that knows which kernel answered —
// rather than in a consumer that would have to guess. CI runs linux and
// developers run darwin, so a ledger that mixed the two units would silently
// compare a gigabyte against a megabyte.
//
// ru_inblock/ru_oublock count block-device input and output operations the
// process actually caused; work served entirely from page cache reports 0.
//
// The members are read without conversion: every architecture this CLI is built
// for is 64-bit (internal/commands.supportedPlatform), where linux's rusage
// members are already int64. A 32-bit target would stop compiling here rather
// than silently truncating a peak, which is the failure mode worth having.
func processRusage(ps *os.ProcessState) (maxRSSBytes, inBlocks, outBlocks int64) {
	rusage, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || rusage == nil {
		return 0, 0, 0
	}
	return max(rusage.Maxrss, 0) * 1024, max(rusage.Inblock, 0), max(rusage.Oublock, 0)
}
