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

// Refused records nothing.
func (RunMemory) Refused(string, string) {}

// TakeRun offers nothing.
func (RunMemory) TakeRun(string) any { return nil }

// Witness witnesses nothing.
func (RunMemory) Witness(string) string { return "" }

// KeepRun holds nothing.
func (RunMemory) KeepRun(string, any, bool) {}

// PeekRun offers nothing.
func (RunMemory) PeekRun(string) any { return nil }

// Reused records nothing.
func (RunMemory) Reused(string, string, bool) {}

// TakeDispatch offers nothing.
func TakeDispatch(string) any { return nil }

// KeepDispatch holds nothing.
func KeepDispatch(string, any, bool) {}

// DispatchMoved withdraws nothing.
func DispatchMoved(string, string) {}

// Witness takes no stat on a platform without a machine process.
func Witness(string) string { return "" }
