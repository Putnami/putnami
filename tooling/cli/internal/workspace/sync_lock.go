package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// workspaceSyncLockFilename is deliberately adjacent to the index it guards.
// Synchronize holds it from the first snapshot read through the atomic publish,
// so a CLI command and a newly started MCP server cannot both decide that the
// same cold workspace needs probing.
const workspaceSyncLockFilename = "workspace-index.lock"

const (
	workspaceSyncLockMaxWait     = 60 * time.Second
	workspaceSyncLockNoticeAfter = time.Second
	workspaceSyncLockRetryDelay  = 10 * time.Millisecond
	workspaceSyncLockWaitMessage = "waiting for another putnami process to prepare the workspace index"
)

// workspaceSyncGates supplies the in-process half of the lock. A context
// waiting on a blocking mutex cannot be canceled, so a one-slot channel orders
// the callers of one process and keeps their wait cancelable.
var workspaceSyncGates sync.Map // map[canonical root]chan struct{}

type workspaceSyncLockOptions struct {
	maxWait     time.Duration
	noticeAfter time.Duration
	onWait      func(string)
}

func acquireWorkspaceSyncLock(ctx context.Context, root string, onWait func(string)) (func(), error) {
	return acquireWorkspaceSyncLockWithOptions(ctx, root, workspaceSyncLockOptions{
		maxWait: workspaceSyncLockMaxWait, noticeAfter: workspaceSyncLockNoticeAfter, onWait: onWait,
	})
}

func acquireWorkspaceSyncLockWithOptions(
	ctx context.Context,
	root string,
	options workspaceSyncLockOptions,
) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, options.maxWait)
	defer cancel()
	waitStarted := time.Now()

	var notice <-chan time.Time
	if options.onWait != nil {
		timer := time.NewTimer(options.noticeAfter)
		defer timer.Stop()
		notice = timer.C
	}
	notifyWait := func() {
		options.onWait(workspaceSyncLockWaitMessage)
		notice = nil
	}
	waitError := func() error {
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			elapsed := time.Since(waitStarted).Round(time.Millisecond)
			return fmt.Errorf("timed out %s after %s; retry after the other process finishes: %w",
				workspaceSyncLockWaitMessage, elapsed, waitCtx.Err())
		}
		return fmt.Errorf("canceled while %s: %w", workspaceSyncLockWaitMessage, waitCtx.Err())
	}
	if err := waitCtx.Err(); err != nil {
		return nil, waitError()
	}

	key := loadKey(root)
	actual, _ := workspaceSyncGates.LoadOrStore(key, make(chan struct{}, 1))
	gate, ok := actual.(chan struct{})
	if !ok {
		return nil, errors.New("workspace index preparation gate has an invalid type")
	}
	gateAcquired := false
	for !gateAcquired {
		select {
		case gate <- struct{}{}:
			gateAcquired = true
		case <-waitCtx.Done():
			return nil, waitError()
		case <-notice:
			notifyWait()
		}
	}

	releaseGate := func() { <-gate }
	if err := waitCtx.Err(); err != nil {
		releaseGate()
		return nil, waitError()
	}
	dir := filepath.Join(root, ".putnami")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		releaseGate()
		if waitCtx.Err() != nil {
			return nil, waitError()
		}
		return nil, fmt.Errorf("create workspace index directory: %w", err)
	}
	lockPath := filepath.Join(dir, workspaceSyncLockFilename)

	retry := time.NewTicker(workspaceSyncLockRetryDelay)
	defer retry.Stop()
	for {
		if err := waitCtx.Err(); err != nil {
			releaseGate()
			return nil, waitError()
		}
		lock, err := flock.Acquire(lockPath, true, true)
		if contextErr := waitCtx.Err(); contextErr != nil {
			_ = lock.Release()
			releaseGate()
			return nil, waitError()
		}
		switch {
		case err == nil:
			return func() {
				_ = lock.Release()
				releaseGate()
			}, nil
		case !errors.Is(err, flock.ErrBusy):
			releaseGate()
			return nil, fmt.Errorf("lock workspace index preparation: %w", err)
		}

		select {
		case <-waitCtx.Done():
			releaseGate()
			return nil, waitError()
		case <-notice:
			notifyWait()
		case <-retry.C:
		}
	}
}
