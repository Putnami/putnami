//go:build windows

package store

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// heldOpenBudget bounds how long an operation waits for another process to
// close its handle on the operation's path: the same two seconds robustio
// waits before a rename.
const heldOpenBudget = 2 * time.Second

// hostHeldOpen reports whether err is what Windows answers while another
// process holds a handle on the path, or on a file inside a directory: a
// reader of the file, an antivirus scanner, the search indexer. Go opens files
// and directories without FILE_SHARE_DELETE, so any reader counts. The error
// clears when that handle closes.
func hostHeldOpen(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
