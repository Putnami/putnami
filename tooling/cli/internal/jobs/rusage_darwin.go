//go:build darwin

package jobs

import (
	"os"
	"syscall"
)

// processRusage reads peak RSS and the block-IO counters off a finished
// process's rusage.
//
// UNIT, stated once: darwin's ru_maxrss is in BYTES (xnu fills it from the
// task's resident size), where linux reports KILOBYTES. The ledger's
// MaxRSSBytes is bytes on every platform, so the normalization belongs here —
// at the one place that knows which kernel answered — rather than in a
// consumer that would have to guess.
//
// ru_inblock/ru_oublock are accounted only for processes doing block-device IO
// darwin attributes; most processes report 0. That is an ABSENT counter, and
// the projection omits a zero rather than publishing it as a measurement.
//
// The members are read without conversion: every architecture this CLI is built
// for is 64-bit (internal/commands.supportedPlatform), where darwin's rusage
// members are already int64. A 32-bit target would stop compiling here rather
// than silently truncating a peak, which is the failure mode worth having.
func processRusage(ps *os.ProcessState) (maxRSSBytes, inBlocks, outBlocks int64) {
	rusage, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || rusage == nil {
		return 0, 0, 0
	}
	return max(rusage.Maxrss, 0), max(rusage.Inblock, 0), max(rusage.Oublock, 0)
}
