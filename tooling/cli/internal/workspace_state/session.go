// Package workspace_state manages session lifecycle and recording.
// Sessions capture the execution of job runs, providing an audit trail
// with events, metadata, and plan snapshots.
package workspace_state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// Session represents a single execution session.
type Session struct {
	ID        string    `json:"id"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime,omitempty"`

	// Paths
	dir string // session directory
	// events is the session's one event stream (events.jsonl). The session is
	// its only producer; subscribers attach through Events.
	events *sessionstream.Log
	mu     sync.Mutex

	// tree is the worktree fingerprint captured by CaptureTree, or nil when the
	// caller did not capture one or git could not answer. It is held here rather
	// than passed to FinalizeV2 because it is a START-of-run measurement and
	// finalize runs after every task: the tree the session GATED is the tree it
	// opened on, not whatever `lint --fix` left behind.
	tree *cli.SessionTree
}

// SessionMetadata is the READER's view of a recorded session.json.
//
// It was also the v1 writer's document until a later change deleted that writer:
// recording is now the versioned cli.SessionFile. The shape survives because
// sessions already on disk are v1 and must keep opening, and because every
// consumer (gate metrics, inspect, list) reads this one view — the v2 document
// is projected onto it by internal/commands/sessions_helpers.go.
type SessionMetadata struct {
	ID        string          `json:"id"`
	StartTime string          `json:"startTime"`
	EndTime   string          `json:"endTime"`
	Duration  int64           `json:"durationMs"`
	Git       *SessionGitInfo `json:"git,omitempty"`
	Stats     *SessionStats   `json:"stats"`
	Commands  []string        `json:"commands"`
	// Jobs keeps task wall and process-start overhead in separate named fields.
	// SpawnToFirstEventMs is absent when no subprocess event was observed.
	Jobs []SessionJobEntry `json:"jobs,omitempty"`
	// Scheduler holds the machine-readable scheduler tuning report
	// (parallelism decision, ready wait, critical path). Omitted when the run
	// did not record one.
	Scheduler any `json:"scheduler,omitempty"`
	// Cache holds the machine-readable remote-cache summary, including the cache
	// provider's terminal byte totals. Omitted for local-only runs.
	Cache any `json:"cache,omitempty"`
}

// SessionGitInfo captures the git state at session start.
type SessionGitInfo struct {
	Branch   string `json:"branch,omitempty"`
	Baseline string `json:"baseline,omitempty"`
}

// SessionStats summarizes job execution results.
type SessionStats struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	// Canceled is work an abort killed mid-run. Kept apart from Skipped so a
	// recorded session cannot pass an interrupted run off as a fully-decided one.
	Canceled   int   `json:"canceled"`
	Skipped    int   `json:"skipped"`
	Cached     int   `json:"cached"`
	Coalesced  int   `json:"coalesced"`
	DurationMs int64 `json:"durationMs"`
}

// SessionJobEntry is the terminal economics row for one scheduled job.
type SessionJobEntry struct {
	Key                 string `json:"key"`
	Project             string `json:"project"`
	Job                 string `json:"job"`
	TaskKind            string `json:"taskKind"`
	Extension           string `json:"extension"`
	Status              string `json:"status"`
	Outcome             string `json:"outcome"`
	TaskWallMs          int64  `json:"taskWallMs"`
	SpawnToFirstEventMs *int64 `json:"spawnToFirstEventMs,omitempty"`
}

// SessionEvent is a single event recorded during a session.
//
// Data is the on-disk payload and is written verbatim; its keys are a persisted
// wire (internal/commands/sessions/sessions_helpers.go reads them back).
type SessionEvent struct {
	// Version-2 machine/session stream. New writers use only these members.
	ProtocolVersion int                       `json:"protocolVersion,omitempty"`
	Record          string                    `json:"record,omitempty"`
	Time            string                    `json:"time"`
	Identity        *cli.TaskIdentity         `json:"identity,omitempty"`
	Event           map[string]any            `json:"event,omitempty"`
	Task            *cli.TaskRecord           `json:"task,omitempty"`
	TestCase        *cli.TestCase             `json:"testCase,omitempty"`
	Run             *cli.StreamRunSummary     `json:"run,omitempty"`
	MachineOutput   *cli.MachineOutputSummary `json:"machineOutput,omitempty"`

	// Version-1 recorded stream. Readers retain these fields for sessions
	// already on disk; AppendEvent is the compatibility-fixture writer only.
	Type   string         `json:"type,omitempty"`
	JobKey string         `json:"jobKey,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

// PlanSnapshot captures the execution plan at the start of a session.
type PlanSnapshot struct {
	SessionID string         `json:"sessionId"`
	Commands  []string       `json:"commands"`
	Jobs      []PlanJobEntry `json:"jobs"`
}

// PlanJobEntry is a single job in the plan snapshot.
type PlanJobEntry struct {
	Key       string   `json:"key"`
	Project   string   `json:"project"`
	Job       string   `json:"job"`
	Extension string   `json:"extension"`
	DependsOn []string `json:"dependsOn,omitempty"`
	After     []string `json:"after,omitempty"`
	Cache     bool     `json:"cache"`
}

// NewSession creates a new session with a generated ID and directory.
// Format: YYYYMMDD-HHMMSS-{random}
func NewSession(sessionsRoot string) (*Session, error) {
	id := generateSessionID()
	dir := filepath.Join(sessionsRoot, id)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	// Open the event stream for incremental append (crash-safe)
	events, err := sessionstream.Create(dir, id)
	if err != nil {
		return nil, fmt.Errorf("open events file: %w", err)
	}

	return &Session{
		ID:        id,
		StartTime: time.Now(),
		dir:       dir,
		events:    events,
	}, nil
}

// CaptureTree records which worktree this session runs against, by content.
//
// It must be called BEFORE the first task runs. The recorded fingerprint is a
// statement about the tree the session's work was planned and executed from, and
// a capture taken later would describe the tree that work PRODUCED — which is a
// different claim, and the wrong one for anybody asking "was this gate run on
// the code I am looking at?". A gate whose `lint --fix` rewrote a file is
// exactly the case the distinction exists for: the record keeps saying what went
// in, and a reader comparing it against the tree on disk learns that the gate
// mutated it.
//
// Failure is silent and absent, never a zero value: a session that cannot
// determine its tree (outside a worktree, a repository with no commit, git
// missing) says nothing rather than claiming a clean one. The capture is
// synchronous on purpose — reading the tree concurrently with running tasks
// would digest a state that never existed.
func (s *Session) CaptureTree(repoRoot string) {
	if repoRoot == "" {
		return
	}
	fingerprint, err := git.FingerprintTree(repoRoot)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tree = &cli.SessionTree{
		Fingerprint: fingerprint.Fingerprint,
		Dirty:       fingerprint.Dirty,
		HeadSHA:     fingerprint.HeadSHA,
	}
}

// AppendEvent writes a single event to events.jsonl incrementally.
func (s *Session) AppendEvent(event SessionEvent) error {
	if event.Time == "" {
		event.Time = time.Now().Format(time.RFC3339Nano)
	}

	line, err := json.Marshal(event)
	if err != nil || s.events == nil {
		return err
	}
	return s.events.Append(line)
}

// AppendMachineOutputLine appends one already-sanitized compact v2 session
// stream record to events.jsonl. The caller supplies the record without its LF
// so the exact bytes measured for the live machine-output budget are the bytes
// persisted here. Recording is incremental and goes through the event stream's
// single producer lock, so concurrent task callbacks cannot interleave JSON
// documents, and every subscriber is woken without the recorder waiting.
func (s *Session) AppendMachineOutputLine(line []byte) error {
	if s.events == nil {
		return nil
	}
	return s.events.Append(line)
}

// Events is the session's event stream, for declaring a subscriber. The session
// stays its only producer.
func (s *Session) Events() *sessionstream.Log {
	return s.events
}

// WritePlanV2 writes plan.json as the version-2 sessionPlanFile document. It is
// the ONLY plan writer since a later change deleted the v1 WritePlan: a recorded
// plan says which contract it speaks through its protocolVersion, and the
// readers keep a v1 fallback for snapshots already on disk
// (internal/commands/sessions_helpers.go).
func (s *Session) WritePlanV2(snapshot *cli.SessionPlanFile) error {
	if snapshot == nil {
		return nil
	}
	snapshot.SessionID = s.ID
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "plan.json"), data, 0o644)
}

// FinalizeV2 writes session.json as the version-2 sessionFile document, then
// closes the events file. It is the ONLY session writer since a later change
// deleted the v1 Finalize.
//
// The caller supplies the run: this stamps only what the session itself owns
// and the caller cannot know — the id, the start and end times, the run's wall
// duration (end minus start) and the git block. v2 carries no top-level
// duration member, so the run summary's durationMs is the one the recorded
// session reports. Anything the caller derived FROM that duration is re-derived
// here for the same reason: see the CPU balance below.
//
// The recorded event log is written incrementally by the renderer as the
// complete sanitized v2 task stream. Finalize closes it before publishing the
// summary; session readers retain the legacy event-shape fallback for history
// written by earlier CLI builds.
func (s *Session) FinalizeV2(meta *cli.SessionFile, wsRoot string, baseline string) error {
	s.EndTime = time.Now()

	// The stream's final marker precedes session.json, as the closed events file
	// always did: no record can follow the summary that counts them.
	if s.events != nil {
		_ = s.events.Close()
	}

	meta.ProtocolVersion = cli.ResultProtocolVersion
	meta.SessionID = s.ID
	meta.StartTime = s.StartTime.Format(time.RFC3339Nano)
	meta.EndTime = s.EndTime.Format(time.RFC3339Nano)
	meta.Run.DurationMs = max(s.EndTime.Sub(s.StartTime).Milliseconds(), 0)

	// The CPU balance is stated against the durationMs this summary reports, and
	// the wall just re-stamped is WIDER than the scheduler wall the producer
	// divided by (it spans the whole session, not just job execution). Re-derive
	// the allocation over the final wall so the two keep dividing: leaving the
	// producer's figure would publish an allocation measured over a different
	// denominator, which the contract rejects (cli.result.count_mismatch at
	// run.cpu.allocatedMs).
	if meta.Run.CPU != nil {
		meta.Run.CPU.AllocatedMs = meta.Run.DurationMs * int64(meta.Run.CPU.AllocatedMillicores) / 1000
	}

	if wsRoot != "" {
		branch, err := git.CurrentBranch(wsRoot)
		if err == nil {
			meta.Git = &cli.SessionGit{Branch: branch, Baseline: baseline}
		}
	}

	// The tree block is stamped from the START-of-run capture, never recomputed
	// here: see CaptureTree. A caller that already supplied one keeps it.
	s.mu.Lock()
	if meta.Tree == nil && s.tree != nil {
		meta.Tree = s.tree
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSessionFile(filepath.Join(s.dir, "session.json"), data, 0o644)
}

// Dir returns the session directory path.
func (s *Session) Dir() string {
	return s.dir
}

// Close cleans up session resources without finalizing.
func (s *Session) Close() {
	if s.events != nil {
		_ = s.events.Close()
	}
}

// generateSessionID generates a session ID: YYYYMMDD-HHMMSS-{6 hex chars}
func generateSessionID() string {
	now := time.Now()
	b := make([]byte, 3) // 3 bytes = 6 hex chars
	rand.Read(b)
	return fmt.Sprintf("%s-%s", now.Format("20060102-150405"), hex.EncodeToString(b))
}
