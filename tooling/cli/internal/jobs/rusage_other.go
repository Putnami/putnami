//go:build !darwin && !linux

package jobs

import "os"

// processRusage is the graceful degradation for a platform whose ProcessState
// carries no rusage this package knows how to read.
//
// It returns zeros, and the projection omits every zero counter, so an
// unsupported platform records an execution with its wall and CPU times and
// simply says nothing about memory or IO. Reporting a fabricated zero as a
// measurement, or failing the run over a counter the kernel does not expose,
// are the two things a measurement foundation must not do.
func processRusage(_ *os.ProcessState) (maxRSSBytes, inBlocks, outBlocks int64) {
	return 0, 0, 0
}
