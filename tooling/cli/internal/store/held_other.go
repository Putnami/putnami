//go:build !windows

package store

import "time"

// heldOpenBudget is zero: no operation here waits for another process's
// handle.
const heldOpenBudget time.Duration = 0

// hostHeldOpen reports false: a rename or an open here never fails because
// another process holds the path open.
func hostHeldOpen(error) bool {
	return false
}
