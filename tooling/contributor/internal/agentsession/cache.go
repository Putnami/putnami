package agentsession

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

const (
	sessionCacheVersion = 1
	// sessionSpoolMax bounds the spooled tool calls one holder folds in.
	sessionSpoolMax = 128
	// sessionSeenMax bounds the message ids a cache remembers as delivered.
	sessionSeenMax  = 256
	hookLogMaxBytes = 1 << 20
	// sessionCacheMaxBytes bounds one session cache file. Every value the
	// cache keeps is bounded, so a full cache stays below it; a write above it
	// is refused and the hook fails open.
	sessionCacheMaxBytes = 2 << 20
	// sessionPathMax bounds a path an engine payload names.
	sessionPathMax = 4096
	// repliedMarkerMaxAge and repliedMarkersMax bound the markers a
	// missions.reply call leaves: every cache write removes the markers older
	// than a day and keeps the newest repliedMarkersMax.
	repliedMarkerMaxAge = 24 * time.Hour
	repliedMarkersMax   = 256
)

// errSessionBusy reports that another hook holds the session lock.
var errSessionBusy = errors.New("the session cache is busy")

// sessionCache is the local state of one agent session. Only the hooks of the
// same session read and write it.
type sessionCache struct {
	Version    int    `json:"version"`
	Engine     string `json:"engine"`
	Session    string `json:"session"`
	Cwd        string `json:"cwd,omitempty"`
	Root       string `json:"root,omitempty"`
	Repository string `json:"repository,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Model      string `json:"model,omitempty"`
	Transcript string `json:"transcript,omitempty"`

	Mission sessionMissionState `json:"mission"`

	Plan       []planStep `json:"plan,omitempty"`
	PlanDigest string     `json:"plan_digest,omitempty"`
	// PlanSynced is the digest of the plan the last successful sync sent.
	PlanSynced string `json:"plan_synced,omitempty"`

	// Inbox holds the messages a sync answered and no hook delivered yet.
	Inbox []inboxMessage `json:"inbox,omitempty"`
	// Acks holds the ids of the delivered messages the next sync reports.
	Acks []string `json:"acks,omitempty"`
	// Prompts holds the delivered prompts that have no recorded reply.
	Prompts []pendingPrompt `json:"prompts,omitempty"`
	// Seen holds the ids of every message delivered, so a sync answer that
	// crossed a delivery never delivers a message twice.
	Seen []string `json:"seen,omitempty"`
	// Replies holds the `[ack <id>] <reply>` lines the next sync sends, and
	// Replied the messages that take no other reply: their reply is recorded
	// or was refused.
	Replies []messageReply `json:"replies,omitempty"`
	Replied []string       `json:"replied,omitempty"`
	// Decisions holds the decisions this session asked, to bind on the next
	// sync, and Asked the last ones it asked, kept after the sync bound them:
	// a Codex Stop waits only for one of those.
	Decisions []string `json:"decisions,omitempty"`
	Asked     []string `json:"asked,omitempty"`
	// ContinuedFor holds the messages a hook already continued a turn for:
	// no message continues a turn twice.
	ContinuedFor []string `json:"continued_for,omitempty"`

	State       collab.SessionState `json:"state,omitempty"`
	LastEvent   string              `json:"last_event,omitempty"`
	LastMessage string              `json:"last_message,omitempty"`
	// WaitUntil is the end of the wait while the state is waiting, and Error
	// the error while it is failed.
	WaitUntil time.Time `json:"wait_until"`
	Error     string    `json:"error,omitempty"`
	// Waiter names the one wait in progress. A newer wait replaces it, and a
	// wait that finds another name ends.
	Waiter string `json:"waiter,omitempty"`
	// ActiveAt is when the session last started, called a hooked tool or
	// stopped. A wait ends once the session is active again.
	ActiveAt time.Time `json:"active_at"`
	// StopDecidedAt is when the last Stop decided, StopContinued whether it
	// continued the turn, and StopKey which Stop event it was. The idle wake
	// reads all three.
	StopDecidedAt time.Time `json:"stop_decided_at"`
	StopContinued bool      `json:"stop_continued,omitempty"`
	StopKey       string    `json:"stop_key,omitempty"`

	LastSyncAt    time.Time `json:"last_sync_at"`
	MinIntervalMS int64     `json:"min_interval_ms,omitempty"`

	// Stage is the loop stage `agent stage` recorded last; every sync
	// carries it.
	Stage collab.LoopStage `json:"stage,omitempty"`
	// Usage is the token usage read from the session's transcripts; every
	// sync carries its totals.
	Usage *usageState `json:"usage,omitempty"`
}

// sessionMissionState is what the cache knows about the session's mission:
// the view the last sync answered for Key, and the checkpoint memory.mission
// answered at the last session start.
type sessionMissionState struct {
	Key string `json:"key,omitempty"`
	// Known reports that a sync answered a view of this mission.
	Known         bool                  `json:"known,omitempty"`
	Title         string                `json:"title,omitempty"`
	Plan          []planStep            `json:"plan,omitempty"`
	OpenDecisions []collab.OpenDecision `json:"open_decisions,omitempty"`
	Checkpoint    *missionCheckpoint    `json:"checkpoint,omitempty"`
	// SheetAt is when a sync last answered the view.
	SheetAt time.Time `json:"sheet_at"`
}

// missionCheckpoint is the part of a memory.mission record the sheet shows.
type missionCheckpoint struct {
	Revision  string `json:"revision,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Content   string `json:"content"`
}

// pendingPrompt is a delivered prompt that waits for its reply.
type pendingPrompt struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// sessionFiles locates the files of one session under the Putnami home, never
// in the checkout: hooks of every checkout share one session store.
type sessionFiles struct {
	dir, cache, lock, spoolDir string
}

// sessionFilesFor names the files of the session key <engine>:<session id>.
// The digest keeps an engine-supplied id out of every path.
func sessionFilesFor(dir, key string) sessionFiles {
	sum := sha256.Sum256([]byte(key))
	return sessionFilesNamed(dir, hex.EncodeToString(sum[:]))
}

// sessionFilesNamed names the files of the session whose key digests to name.
func sessionFilesNamed(dir, name string) sessionFiles {
	return sessionFiles{
		dir:      dir,
		cache:    filepath.Join(dir, name+".json"),
		lock:     filepath.Join(dir, name+".lock"),
		spoolDir: filepath.Join(dir, name+".spool"),
	}
}

// read returns the cache, or nil when the session has none yet. Every write
// is a rename, so a reader without the lock sees a whole file.
func (f sessionFiles) read() (*sessionCache, error) {
	data, err := os.ReadFile(f.cache)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cache sessionCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("read the session cache: %w", err)
	}
	if cache.Version != sessionCacheVersion {
		return nil, nil
	}
	return &cache, nil
}

// write replaces the cache atomically. The caller holds the lock.
func (f sessionFiles) write(cache *sessionCache) error {
	cache.Version = sessionCacheVersion
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if len(data) > sessionCacheMaxBytes {
		return fmt.Errorf("the session cache would hold %d bytes, more than %d", len(data), sessionCacheMaxBytes)
	}
	if err := writeFileAtomic(f.cache, data); err != nil {
		return err
	}
	pruneRepliedMarkers(filepath.Join(f.dir, repliedMarkersDir), time.Now())
	return nil
}

// writeFileAtomic writes data to a temporary file beside path, then renames
// it over path, so a reader sees the old file or the new one, never a part.
// The temporary name does not end in .json, so a spool reader skips it.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// pruneRepliedMarkers removes the replied markers older than
// repliedMarkerMaxAge and keeps the newest repliedMarkersMax. The Stop of the
// session that holds a prompt reads its marker; a marker no Stop read within
// a day belongs to a prompt no hooked session holds.
func pruneRepliedMarkers(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type marker struct {
		path string
		at   time.Time
	}
	kept := make([]marker, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if now.Sub(info.ModTime()) > repliedMarkerMaxAge {
			_ = os.Remove(path)
			continue
		}
		kept = append(kept, marker{path: path, at: info.ModTime()})
	}
	if len(kept) <= repliedMarkersMax {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].at.After(kept[j].at) })
	for _, old := range kept[repliedMarkersMax:] {
		_ = os.Remove(old.path)
	}
}

// sessionLock is one held session lock: an open lock file the kernel locks
// for this process. The kernel releases it when the process exits, however it
// exits, so a holder the engine killed never blocks the next hook.
type sessionLock struct {
	file *os.File
}

// tryLock takes the session lock without waiting. It answers errSessionBusy
// when another hook holds it.
func (f sessionFiles) tryLock() (*sessionLock, error) {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return nil, err
	}
	return lockSessionFile(f.lock)
}

// lockWithin takes the session lock, waiting at most wait. A tool call never
// waits for it: only SessionStart, Stop, StopFailure, a wait and a sync do.
func (f sessionFiles) lockWithin(wait time.Duration) (*sessionLock, error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := f.tryLock()
		if !errors.Is(err, errSessionBusy) || !time.Now().Before(deadline) {
			return lock, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// release unlocks and closes the lock file. The lock file stays: removing it
// would let a hook that opened it before the removal lock a file no other
// hook opens.
func (l *sessionLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
}

// spooledToolUse is one tool call a PostToolUse hook could not apply because
// another hook held the lock. The next holder applies it.
type spooledToolUse struct {
	At           int64           `json:"at"`
	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse json.RawMessage `json:"tool_response,omitempty"`
}

// spool writes one tool call to its own file, so it needs no lock. The name
// orders the files by time; the process id keeps two hooks apart.
func (f sessionFiles) spool(event spooledToolUse) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%d.json", event.At, os.Getpid())
	return writeFileAtomic(filepath.Join(f.spoolDir, name), data)
}

// readSpool returns the spooled tool calls in the order they happened, with
// the files to remove once the cache that holds them is written.
func (f sessionFiles) readSpool() ([]spooledToolUse, []string) {
	entries, err := os.ReadDir(f.spoolDir)
	if err != nil {
		return nil, nil
	}
	var events []spooledToolUse
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if len(paths) == sessionSpoolMax {
			break
		}
		path := filepath.Join(f.spoolDir, name)
		paths = append(paths, path)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var event spooledToolUse
		if json.Unmarshal(data, &event) == nil {
			events = append(events, event)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At < events[j].At })
	return events, paths
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

// appendBounded appends value unless present and keeps the newest limit
// values.
func appendBounded(values []string, value string, limit int) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	values = append(values, value)
	if len(values) > limit {
		values = append([]string(nil), values[len(values)-limit:]...)
	}
	return values
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// logHookFailure appends one line to hook.log. The hook never reports a
// failure to the engine, so this file is the only trace. It never holds a
// message text or a credential.
func logHookFailure(dir string, now time.Time, engine, event string, failure error) {
	if failure == nil {
		return
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, hookLogName)
	if info, err := os.Stat(path); err == nil && info.Size() > hookLogMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	entry := map[string]string{"at": now.UTC().Format(time.RFC3339Nano), "engine": engine, "event": event, "error": failure.Error()}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(line, '\n'))
	_ = file.Close()
}

// hookLogName is the log of the hook's failures, in the session store.
const hookLogName = "hook.log"
