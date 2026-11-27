//go:build !linux && !darwin

package machineproc

import (
	"context"
	"errors"
)

// RunFunc runs one command in this process.
type RunFunc func(ctx context.Context, args []string) int

// ErrNotRunning says no supervisor answers.
var ErrNotRunning = errors.New("no machine process is running")

// Serve never serves a run on a platform without a machine process.
func Serve(context.Context, []string) (int, bool) { return 0, false }

// Query answers that no machine process runs.
func Query(string) (*Reply, error) { return nil, ErrNotRunning }

// StartIfWanted does nothing on a platform without a machine process.
func StartIfWanted() {}

// Supervise refuses on a platform without a machine process.
func Supervise() int { return 1 }

// Work refuses on a platform without a machine process.
func Work(RunFunc) int { return 1 }
