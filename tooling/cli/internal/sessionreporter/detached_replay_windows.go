//go:build windows

package sessionreporter

import "os/exec"

// detachSession leaves cmd as it is: proctree.StartDetached already starts it
// outside the run's job and console.
func detachSession(*exec.Cmd) {}
