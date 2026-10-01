//go:build windows

package robustio

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// retryBudget bounds how long Rename and Remove keep retrying: cmd/go's
// robustio waits the same two seconds for the same reason.
const retryBudget = 2 * time.Second

func rename(oldpath, newpath string) error {
	return retry(func() error { return os.Rename(oldpath, newpath) }, transient, retryBudget)
}

func remove(path string) error {
	return retry(func() error { return os.Remove(path) }, transient, retryBudget)
}

// transient reports whether err is what a rename or a removal answers while
// another process holds a handle on the file.
func transient(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
