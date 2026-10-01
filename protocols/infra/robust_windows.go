//go:build windows

package infra

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// retryBudget bounds the retries: cmd/go's robustio waits the same two seconds
// for the same reason.
const retryBudget = 2 * time.Second

// errorSharingViolation is ERROR_SHARING_VIOLATION, which package syscall does
// not declare.
const errorSharingViolation syscall.Errno = 32

func renameFile(oldpath, newpath string) error {
	return retry(func() error { return os.Rename(oldpath, newpath) }, transient, retryBudget)
}

func removeFile(path string) error {
	return retry(func() error { return os.Remove(path) }, transient, retryBudget)
}

func readFile(path string) ([]byte, error) {
	var data []byte
	err := retry(func() error {
		var readErr error
		data, readErr = os.ReadFile(path)
		return readErr
	}, transient, retryBudget)
	return data, err
}

// transient reports whether err is what a rename, a removal or an open
// answers while another process holds the file: ERROR_ACCESS_DENIED or
// ERROR_SHARING_VIOLATION.
func transient(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errorSharingViolation)
}
