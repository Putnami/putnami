//go:build !darwin && !linux

package jobs

import "os"

// hostProcessProgress counts a process as running as soon as exec.Cmd.Start
// returned, and does not measure how long it waits for a CPU, so its deadline
// counts every moment from then.
func hostProcessProgress(*os.Process) func() processSample {
	return nil
}
