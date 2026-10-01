//go:build windows

package versioncmd

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// systemSwitch replaces binaries on the real filesystem. Its renames retry the
// refusals a scanner that holds a file open causes.
var systemSwitch = asideSwitch{ops: fileOps{
	rename: retryingRename(os.Rename, transientRenameError, renameAttempts, renameRetryDelay, time.Sleep),
	remove: os.Remove,
}}

// transientRenameError reports whether err is a refusal Windows returns while
// another process, such as an antivirus scanner, holds the file open: it
// clears once that process closes the file.
func transientRenameError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// installBinary installs src at dst. dst may be the running CLI, which
// Windows neither deletes nor overwrites, so it moves aside first.
func installBinary(src, dst string) error {
	return systemSwitch.install(src, dst)
}

// activateCLI makes link, putnami.exe, a copy of targetName, a binary in the
// same directory. A symbolic link needs Developer Mode or an administrator,
// and a copy leaves the versioned binary free to be replaced while putnami.exe
// runs.
func activateCLI(targetName, link string) error {
	return systemSwitch.activate(targetName, link)
}

// activeCLIName names the binary in binDir that putnami.exe is a copy of.
func activeCLIName(binDir string) string {
	return sameContentName(binDir, CLIPath(binDir))
}
