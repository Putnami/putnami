package workspace_state

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/flock"
)

// DefaultMaxSessions is the default number of sessions to retain.
const DefaultMaxSessions = 20

// SessionStore manages session persistence under .putnami/sessions/.
type SessionStore struct {
	root        string // .putnami/sessions
	maxSessions int
}

// NewSessionStore creates a SessionStore for the given workspace root, keeping
// DefaultMaxSessions records. Every read-only caller uses this form: retention
// is only consulted by Prune, which one writer calls.
func NewSessionStore(workspaceRoot string) *SessionStore {
	return NewSessionStoreWithRetention(workspaceRoot, DefaultMaxSessions)
}

// NewSessionStoreWithRetention creates a SessionStore that prunes down to keep
// records instead of DefaultMaxSessions.
//
// A non-positive keep means "unset" and restores the default. That is the
// fail-safe reading, and it is why the workspace schema's minimum is 1: a 0 on
// the wire could be read as "unlimited" or as "delete everything", and a store
// of durable measurement records may never guess between those.
func NewSessionStoreWithRetention(workspaceRoot string, keep int) *SessionStore {
	if keep <= 0 {
		keep = DefaultMaxSessions
	}
	return &SessionStore{
		root:        filepath.Join(workspaceRoot, ".putnami", "sessions"),
		maxSessions: keep,
	}
}

// RetentionFromWorkspace resolves how many sessions to retain from the merged
// workspace config: `sessions.keep` when it is authored and positive, otherwise
// DefaultMaxSessions.
//
// There is deliberately no environment override, unlike the machine-global
// build store's GC settings: session records are per-worktree, and a committed
// workspace config already reaches every worktree that writes them.
func RetentionFromWorkspace(cfg *wsproto.Config) int {
	if cfg == nil || cfg.Sessions == nil || cfg.Sessions.Keep == nil || *cfg.Sessions.Keep <= 0 {
		return DefaultMaxSessions
	}
	return *cfg.Sessions.Keep
}

// Root returns the sessions root directory.
func (ss *SessionStore) Root() string {
	return ss.root
}

// Create starts a new session and returns it.
func (ss *SessionStore) Create() (*Session, error) {
	return NewSession(ss.root)
}

// UpdateLatest creates or updates the "latest" symlink (a directory junction on
// Windows) to point at the given session.
func (ss *SessionStore) UpdateLatest(session *Session) error {
	latestLink := filepath.Join(ss.root, "latest")
	return dirlink.Replace(session.Dir(), latestLink, latestLink+".tmp")
}

// Prune removes old sessions exceeding the retention limit.
// Sessions are sorted by name (which includes timestamp), oldest first. The
// session targeted by "latest" is always retained so pruning an ephemeral run
// cannot leave the measurement pointer dangling.
func (ss *SessionStore) Prune() error {
	entries, err := os.ReadDir(ss.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Collect session directories (skip "latest" symlink)
	var sessions []string
	for _, e := range entries {
		if e.Name() == "latest" {
			continue
		}
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			sessions = append(sessions, e.Name())
		}
	}

	if len(sessions) <= ss.maxSessions {
		return nil
	}

	// Sort alphabetically (timestamp-based names sort chronologically)
	sort.Strings(sessions)

	// Remove the oldest non-latest sessions until the store is back within the
	// limit. "latest" can intentionally point at an older user run while newer
	// ephemeral adapter sessions remain listable.
	latestID := ss.LatestID()
	excess := len(sessions) - ss.maxSessions
	// Prefer pending reporting over ordinary history, but an outage cannot
	// exempt it from sessions.keep indefinitely. Active reporters hold a flock
	// across their whole delivery/replay and may temporarily exceed that bound.
	for _, expirePending := range []bool{false, true} {
		for _, name := range sessions {
			if excess == 0 {
				break
			}
			if name == latestID {
				continue
			}
			removed, err := pruneRecordedSession(filepath.Join(ss.root, name), expirePending)
			if err != nil {
				return err
			}
			if removed {
				excess--
				if expirePending {
					_, _ = fmt.Fprintf(os.Stderr, "putnami: expired unacknowledged reporting session %s (sessions.keep=%d); replay is no longer available\n", name, ss.maxSessions)
				}
			}
		}
	}

	return nil
}

// reportingCheckpoints are the checkpoint and advisory lock of every native
// reporting capability (internal/sessionreporter.Capabilities): the session
// reporter's, then the log reporter's.
var reportingCheckpoints = [...]struct{ state, lock string }{
	{"reporting.json", "reporting.lock"},
	{"log-reporting.json", "log-reporting.lock"},
}

func pruneRecordedSession(dir string, expirePending bool) (bool, error) {
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return false, nil
	}
	// Lock even before the first checkpoint exists: a reporter acquires its lock
	// before publishing its checkpoint, including first-time replay. Every
	// capability's lock is held until the directory is gone.
	var locks []*os.File
	release := func() error {
		for _, lock := range locks {
			_ = flock.UnlockFile(lock)
			_ = lock.Close()
		}
		locks = nil
		return nil
	}
	pending := false
	lockNames := make([]string, 0, len(reportingCheckpoints))
	for _, checkpoint := range reportingCheckpoints {
		lock, err := flock.OpenFile(filepath.Join(dir, checkpoint.lock), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			_ = release()
			return false, nil
		}
		if flock.LockFile(lock, true, true) != nil {
			_ = lock.Close()
			_ = release()
			return false, nil
		}
		locks = append(locks, lock)
		lockNames = append(lockNames, checkpoint.lock)
		pending = pending || reportingPending(filepath.Join(dir, checkpoint.state))
	}
	if pending != expirePending {
		_ = release()
		return false, nil
	}
	// The locks are released before their files go: Windows before version
	// 1809 cannot remove a directory that holds an open file.
	if err := flock.RemoveDirLocks(dir, lockNames, release); err != nil {
		return false, err
	}
	return true, nil
}

// reportingPending reports whether a capability's checkpoint records delivery
// that has not completed. A missing checkpoint is not pending; an unreadable
// one is.
func reportingPending(path string) bool {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 192*1024+1))
	_ = f.Close()
	var reporting struct {
		Complete bool `json:"complete"`
	}
	if readErr != nil || len(data) > 192*1024 || json.Unmarshal(data, &reporting) != nil {
		return true
	}
	return !reporting.Complete
}

// List returns all session IDs, sorted newest first.
func (ss *SessionStore) List() ([]string, error) {
	entries, err := os.ReadDir(ss.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []string
	for _, e := range entries {
		if e.Name() == "latest" {
			continue
		}
		if e.IsDir() {
			sessions = append(sessions, e.Name())
		}
	}

	// Sort newest first
	sort.Sort(sort.Reverse(sort.StringSlice(sessions)))
	return sessions, nil
}

// LatestID returns the ID of the most recent session, or "" if none.
func (ss *SessionStore) LatestID() string {
	latestLink := filepath.Join(ss.root, "latest")
	target, err := os.Readlink(latestLink)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// LinkLatest points "latest" at an already recorded session directory by id,
// for a session this store did not create itself (an imported one). It
// refuses an id that does not name an existing session, so the measurement
// pointer can never dangle on a name that was never recorded here.
func (ss *SessionStore) LinkLatest(sessionID string) error {
	dir := filepath.Join(ss.root, sessionID)
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("session %s is not a recorded session directory", sessionID)
	}
	return ss.UpdateLatest(&Session{ID: sessionID, dir: dir})
}
