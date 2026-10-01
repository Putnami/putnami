package hooks

import (
	"fmt"
	"runtime"
)

// shellNotFound is the error a workspace hook returns when `sh` is not on
// PATH. Hooks run with `sh -c` on every platform (decision D-W3); on Windows
// that shell comes from Git for Windows, so the error names it as the
// prerequisite.
func shellNotFound(err error) error {
	return shellNotFoundOn(runtime.GOOS, err)
}

// shellNotFoundOn is shellNotFound for the host operating system goos.
func shellNotFoundOn(goos string, err error) error {
	if goos == "windows" {
		return fmt.Errorf("shell not found: workspace hooks run with `sh`, which Git for Windows provides; "+
			"install Git for Windows (https://gitforwindows.org) and put its `usr\\bin` directory on PATH: %w", err)
	}
	return fmt.Errorf("shell not found: %w", err)
}
