//go:build !linux && !darwin

package store

import (
	"errors"
	"os"
)

// exchangePaths has no one-step form on this platform, so every directory
// swap takes the two-rename path. Windows is such a platform.
func exchangePaths(a, b string) error {
	return &os.LinkError{Op: "exchange", Old: a, New: b, Err: errors.ErrUnsupported}
}
