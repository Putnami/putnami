//go:build !windows && !linux

package runcredential

// Outside Linux the process keeps no dumpable flag to report.
const (
	dumpableAfterCapture = "n/a"
	dumpableUntouched    = "n/a"
)

func dumpableState() string {
	return "n/a"
}
