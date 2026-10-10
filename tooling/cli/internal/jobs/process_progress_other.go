//go:build !darwin

package jobs

import "os"

// hostProcessProgress counts a process as running as soon as exec.Cmd.Start
// returned, so its deadline counts from then.
func hostProcessProgress(*os.Process) func() bool {
	return nil
}
