package sessionreporter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"go.putnami.dev/sdk/extension/filelock"
)

// Empty reports whether no capability started a run.
func (rs *Runs) Empty() bool {
	return rs == nil || len(rs.runs) == 0
}

// Stop ends every run's delivery at its durable checkpoint instead of
// draining it (ADR 0115), and records each run's evidence. A run keeps its
// acknowledged cursors and its exact pending frame, so a later replay
// (ReplaySelected) resumes where it stopped and resends that frame with the
// same identity and bytes. It reports whether any run left its delivery
// incomplete, which a replay then owes.
func (rs *Runs) Stop() bool {
	if rs == nil {
		return false
	}
	incomplete := make([]bool, len(rs.runs))
	var wg sync.WaitGroup
	for i, run := range rs.runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			incomplete[i] = run.stopAtCheckpoint()
		}()
	}
	wg.Wait()
	return slices.Contains(incomplete, true)
}

// stopAtCheckpoint cancels the worker, waits for it, and records the
// evidence. The worker saves its checkpoint before each frame leaves and
// after each acknowledgement, so canceling it at any point leaves a
// checkpoint a replay accepts. It reports whether the delivery is
// incomplete.
func (r *Run) stopAtCheckpoint() bool {
	r.cancel()
	<-r.done
	_ = r.recordEvidence()
	return !r.state.Complete
}

// PendingSessions returns, newest first, at most limit retained sessions
// under root other than exclude that a replay owes and that no process is
// delivering: the session is finalized, so a replay accepts it, the
// checkpoint of a capability the context selects, for the provider it
// selects, is incomplete, and that capability's lock is free. A session
// killed before it was finalized is never owed: no replay accepts it. It
// reads the checkpoints and the finalized records, and skips what it cannot
// read.
func PendingSessions(ctx context.Context, root, exclude string, limit int) []string {
	entries, err := os.ReadDir(root)
	if err != nil || limit <= 0 {
		return nil
	}
	selected := Selected(ctx)
	// Session ids start with their UTC start time: the newest sort last.
	slices.Reverse(entries)
	var pending []string
	for _, entry := range entries {
		// The latest pointer is a link to a session listed under its own id.
		if len(pending) == limit {
			break
		}
		if !entry.IsDir() || entry.Name() == exclude {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		owed := false
		for _, capability := range selected {
			if checkpointIncomplete(dir, entry.Name(), capability, capability.Provider(ctx)) && lockFree(dir, capability) {
				owed = true
				break
			}
		}
		if owed && finalizedSession(dir, entry.Name()) == nil {
			pending = append(pending, entry.Name())
		}
	}
	return pending
}

// finalizedSession is validateFinalizedSession; a unit test of PendingSessions
// replaces it rather than write whole session documents.
var finalizedSession = validateFinalizedSession

// lockFree reports whether no process holds capability's lock beside the
// session: no run and no replay is delivering it.
func lockFree(dir string, capability Capability) bool {
	lock, err := filelock.OpenFile(filepath.Join(dir, capability.LockFile), os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer func() { _ = lock.Close() }()
	if filelock.LockFile(lock, true, true) != nil {
		return false
	}
	_ = filelock.UnlockFile(lock)
	return true
}

// checkpointIncomplete reports whether dir holds capability's checkpoint of
// sessionID, for provider, and that checkpoint is incomplete.
func checkpointIncomplete(dir, sessionID string, capability Capability, provider string) bool {
	data, err := readBounded(filepath.Join(dir, capability.StateFile), checkpointBytes)
	if err != nil {
		return false
	}
	var saved struct {
		Provider  string `json:"provider"`
		SessionID string `json:"sessionId"`
		Complete  bool   `json:"complete"`
	}
	if json.Unmarshal(data, &saved) != nil {
		return false
	}
	return saved.Provider == provider && saved.SessionID == sessionID && !saved.Complete
}
