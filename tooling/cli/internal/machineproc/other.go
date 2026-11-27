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

// LastLocal is why the last command meant for the machine process ran in its
// own process.
type LastLocal struct {
	Time   string `json:"time"`
	Reason string `json:"reason"`
	Dir    string `json:"dir"`
}

// ReadLastLocal answers nil on a platform without a machine process.
func ReadLastLocal() *LastLocal { return nil }

// ReadLastMemory answers nil on a platform without a machine process.
func ReadLastMemory() *MemoryDecision { return nil }

// RunMemory holds nothing on a platform without a machine process.
type RunMemory struct{}

// Take offers nothing.
func (RunMemory) Take(string) any { return nil }

// Keep holds nothing.
func (RunMemory) Keep(string, any, bool) {}
