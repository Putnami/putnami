package store

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

const (
	scratchLockName           = "cache.lock"
	scratchGenerationName     = "cache-generation"
	scratchGenerationLifetime = 30 * 24 * time.Hour
)

// acquireScratchLease takes the scratch lease. Tests replace it to produce the
// probe outcomes a real filesystem cannot be asked for on demand.
var acquireScratchLease = flock.Acquire

// AcquireScratch protects the mutable workspace cache for a complete consumer
// lifetime. Call Close after its last filesystem access, including task capture.
// Before joining, an idle consumer may reap an expired generation. It NEVER
// waits for an exclusive lock: nested CLI calls must coexist with their parent.
// A shared-lock failure fails the consumer closed; reclamation errors only warn.
// An active consumer is the only silent reason to skip reclamation; a probe
// that fails for any other reason is reported and still admits the consumer.
// Reclamation runs only where the lease passes to child processes
// (flock.Inheritable): elsewhere a child that outlives its CLI would hold no
// lease, so the consumer takes the shared lease and never reaps.
func AcquireScratch(workspaceRoot string) (*flock.Lock, error) {
	parent := filepath.Join(workspaceRoot, putnamiDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("prepare scratch lease: %w", err)
	}
	lockPath := filepath.Join(parent, scratchLockName)
	if flock.Inheritable {
		exclusive, err := acquireScratchLease(lockPath, true, true)
		switch {
		case err == nil:
			if err := reapScratch(workspaceRoot, time.Now()); err != nil {
				slog.Warn("scratch reclamation deferred", "error", err)
			}
			_ = exclusive.Close()
		case !errors.Is(err, flock.ErrBusy):
			slog.Warn("scratch reclamation skipped", "error", err)
		}
	}
	lease, err := acquireScratchLease(lockPath, false, false)
	if err != nil {
		return nil, fmt.Errorf("acquire scratch lease: %w", err)
	}
	return lease, nil
}

// AttachScratch protects a subprocess even if the spawning CLI exits or is
// killed. Where flock.Inheritable, the child receives one explicitly inherited
// descriptor; unrelated descriptors keep Go's close-on-exec default. Close the
// returned parent copy after Start (or its failure), never Release it. Descendants that retain the
// descriptor also retain protection; an active consumer never expires by age.
// A descendant's descriptors close asynchronously with respect to the wait that
// reaps it, so the lease alone — never a child's exit status — states that the
// generation is idle.
func AttachScratch(cmd *exec.Cmd, workspaceRoot string) (*flock.Lock, error) {
	lease, err := AcquireScratch(workspaceRoot)
	if err != nil {
		return nil, err
	}
	if flock.Inheritable {
		cmd.ExtraFiles = append(cmd.ExtraFiles, lease.File())
	}
	return lease, nil
}

// reapScratch runs only under the exclusive lease. A generation is a fixed
// retention window, not an access-time estimate: stable compiler outputs need
// not change mtime when read. An old unmarked root starts its first window now.
// Only successful deletion advances the stamp, so partial failures are retried.
func reapScratch(workspaceRoot string, now time.Time) error {
	ws, err := os.OpenRoot(workspaceRoot)
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close() }()
	info, err := ws.Lstat(putnamiDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("scratch parent is not a real directory")
	}
	root, err := ws.OpenRoot(putnamiDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	stamp, err := root.Lstat(scratchGenerationName)
	switch {
	case os.IsNotExist(err):
		file, err := root.OpenFile(scratchGenerationName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		return file.Close()
	case err != nil:
		return err
	case !stamp.Mode().IsRegular():
		return errors.New("scratch generation stamp is not a regular file")
	case now.Sub(stamp.ModTime()) < scratchGenerationLifetime:
		return nil
	}
	// Root.RemoveAll never follows symlinks and confines traversal to this
	// opened root, even if a path is replaced during deletion. It uses bounded
	// directory batches: no per-build inventory, byte scan, or in-memory tree.
	if err := root.RemoveAll(scratchDirName); err != nil {
		return err
	}
	return root.Chtimes(scratchGenerationName, now, now)
}
