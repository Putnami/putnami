//go:build !linux

package machineproc

import (
	"errors"
	"time"
)

// errNoWatcher says this platform has no watcher: macOS's FSEvents needs cgo,
// which the CLI is built without, so every path is taken by stat there
// (CLI ADR 0116 §7).
var errNoWatcher = errors.New("machine: no file watcher on this platform")

// Watcher is absent on this platform.
type Watcher struct{}

// FileSig is what a stat says of a path.
type FileSig struct {
	Dev, Ino, Mode, Uid, Gid uint64
	Size, ModNano, CtimeNano int64
}

// NewWatcher answers that this platform has no watcher.
func NewWatcher(int) (*Watcher, error) { return nil, errNoWatcher }

// Close does nothing.
func (*Watcher) Close() error { return nil }

// Watch refuses.
func (*Watcher) Watch(string) error { return errNoWatcher }

// Since breaks the whole worktree: every path is taken by stat.
func (*Watcher) Since(root string, _ uint64) Journal { return Journal{Broken: []string{root}} }

// Sync refuses.
func (*Watcher) Sync(string, time.Duration) error { return errNoWatcher }
