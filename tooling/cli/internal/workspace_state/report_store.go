package workspace_state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/protocol/cli"
)

// ReportsDirName is the directory reports are recorded under, beside the
// sessions they synthesize.
const ReportsDirName = "reports"

// The newest-first lookup (Latest) deliberately carries NO scan cap. Its
// origin filter only learns whether a file matches by reading it, so any fixed
// window can be filled by a burst of non-matching reports — a busy agent's MCP
// runs — and turn "no run report recorded" into a wrong answer while the
// matching report sits just past it. The store's size is bounded where it
// belongs, by Prune: steady state is the count cap plus the cadence-exempt
// tail, and the only unbounded residue is whatever landed inside the prune's
// one-hour grace window — KB files, one directory, a cost worth paying for a
// lookup that is never falsely empty.

// ReportStore persists run reports under .putnami/reports/.
//
// A report is the run's contracted bilan (protocols/cli
// doc/03-report.md): the small, closed document a longitudinal consumer files,
// while the session directory next to it stays the debug payload. It is written
// per session and named by the session id, so binding a report to its session
// needs no set-difference over a directory listing — the guess cloud's CI runner
// had to make before this document existed.
//
// The store is deliberately separate from SessionStore. A session is an open
// stream the run appends to as it goes; a report is written ONCE, from a settled
// run, and never edited — so it needs no lifecycle, only an atomic write.
type ReportStore struct {
	root string // .putnami/reports
}

// NewReportStore creates a ReportStore for the given workspace root.
func NewReportStore(workspaceRoot string) *ReportStore {
	return &ReportStore{root: filepath.Join(workspaceRoot, ".putnami", ReportsDirName)}
}

// Root returns the reports root directory.
func (rs *ReportStore) Root() string {
	return rs.root
}

// Path is where the report for a session id is recorded.
func (rs *ReportStore) Path(sessionID string) string {
	return filepath.Join(rs.root, sessionID+".json")
}

// Write records one report atomically: a temp file in the destination directory
// followed by a rename, so a reader either sees the previous bytes or the
// complete new ones and never a half-written document.
//
// The report's own sessionId names the file. It is validated as a plain name
// rather than trusted as a path: the id comes from the session that produced it,
// and a document is not a place to accept a path from.
func (rs *ReportStore) Write(report *cli.ReportFile) error {
	if report == nil {
		return nil
	}
	if !plainFileName(report.SessionID) {
		return fmt.Errorf("write report: invalid session id %q", report.SessionID)
	}
	if err := os.MkdirAll(rs.root, 0o755); err != nil {
		return fmt.Errorf("create reports dir: %w", err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(rs.root, "report-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create report temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	// os.CreateTemp opens at 0600, and a report is a record meant to be COLLECTED
	// — by a CI step, an ingestion agent, another account on a shared runner. It
	// is recorded at the same 0644 the session documents beside it use.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if err := os.Rename(tmpPath, rs.Path(report.SessionID)); err != nil {
		return fmt.Errorf("replace report: %w", err)
	}
	return nil
}

// The retention policy. Reports are the durable time-series — KB each,
// against the sessions' hundreds of KB — so the policy keeps far more of them
// than SessionStore.Prune keeps sessions, and it keeps the enforcing cadence
// longest: a validation report replays a coverage figure and is a longitudinal
// consumer's file under a commit, so it is exempt from the count cap until it is
// simply old. Constants, not config surface, for v1.
const (
	// reportRetainCount is how many of the newest reports the count cap keeps.
	reportRetainCount = 200
	// reportValidationRetainDays exempts an enforceCoverage report from the
	// count cap while it is younger than this; past it, it is count-pruned like
	// the rest.
	reportValidationRetainDays = 365
	// reportPruneGraceWindow spares any file modified this recently, whatever
	// its name sorts as: the prune rides a write, and it must never race a
	// concurrent writer's temp file or a clock-skewed sibling's fresh report.
	reportPruneGraceWindow = time.Hour
)

// Prune bounds the store: the newest reportRetainCount reports are kept, and
// past the cap a report is deleted unless it is a validation-cadence report
// still inside its reportValidationRetainDays.
//
// It is best-effort at every step, for the same reason the write it rides on
// is: retention is housekeeping, and a file that could not be examined or
// removed is a file kept, never a failed run. Reading the cadence marker costs
// one small-file parse per candidate BEYOND the cap, so a store inside its
// budget pays only the directory listing.
func (rs *ReportStore) Prune() {
	ids := rs.List()
	if len(ids) <= reportRetainCount {
		return
	}
	now := time.Now()
	for _, id := range ids[reportRetainCount:] {
		path := rs.Path(id)
		info, err := os.Stat(path)
		if err != nil || now.Sub(info.ModTime()) < reportPruneGraceWindow {
			continue
		}
		// The exemption is for the USER's validation history — the figure the
		// replay display and a longitudinal consumer file under a commit. An
		// agent's MCP-origin run is neither, so it is count-pruned like the rest
		// rather than accumulating exempt for a year.
		if recorded, err := readRecordedReport(path); err == nil &&
			recorded.Report.EnforceCoverage &&
			recorded.Report.Origin == cli.ReportOriginCLI &&
			now.Sub(info.ModTime()) < reportValidationRetainDays*24*time.Hour {
			continue
		}
		_ = os.Remove(path)
	}
}

// RecordedReport is one report as it was recorded: the parsed document and the
// exact bytes on disk.
//
// Raw travels with it because a reader that PASSES THE DOCUMENT ON must not
// re-serialize it. The report is the contract a consumer binds to, and the
// binary reading a report is not always the binary that wrote it — a CI runner
// pins a CLI version while the recorded reports come from whichever version ran
// — so re-marshaling through this Go struct would silently drop a member a
// newer producer added.
type RecordedReport struct {
	// Report is the parsed document, for anything that inspects it.
	Report *cli.ReportFile
	// Raw is the recorded bytes, for anything that forwards it.
	Raw []byte
}

// List returns the session ids of every recorded report, newest first.
//
// The order is a reverse name sort: session ids are timestamp-prefixed, so the
// listing is chronological without stat-ing a single file. An unreadable or
// absent store lists nothing — this store is read to DISPLAY history, and a
// worktree that has never recorded a report is the ordinary case, not a failure.
func (rs *ReportStore) List() []string {
	entries, err := os.ReadDir(rs.root)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

// Read returns the report recorded for one session id.
//
// It is an exact lookup and applies no origin filter: an id is something the
// caller already chose, so an agent's own report is readable by asking for it
// by name. The id is validated as a plain file name for the reason Write
// validates it — a session id addresses a file inside this directory and is
// never a path.
func (rs *ReportStore) Read(sessionID string) (*RecordedReport, error) {
	if !plainFileName(sessionID) {
		return nil, fmt.Errorf("read report: invalid session id %q", sessionID)
	}
	return readRecordedReport(rs.Path(sessionID))
}

// Latest returns the newest recorded report a USER's run produced, or nil when
// the store holds none.
//
// Origin filters MCP runs out — an
// agent's run is not the user's build, and `putnami report` with no argument
// answers "what did my last run do" — a question an agent's background run must
// not answer. Asking for that report by --session still reads it.
//
// A report nothing can parse is skipped rather than fatal: it sorts by name
// like any other, and one unreadable document must not hide the run behind it.
func (rs *ReportStore) Latest() *RecordedReport {
	for _, id := range rs.List() {
		recorded, err := readRecordedReport(rs.Path(id))
		if err != nil || recorded.Report.Origin == cli.ReportOriginMCP {
			continue
		}
		return recorded
	}
	return nil
}

// readRecordedReport parses one recorded report, failing for anything it cannot
// read as one — a file removed between the listing and the read, a document a
// newer producer wrote, a write that never completed.
func readRecordedReport(path string) (*RecordedReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var report cli.ReportFile
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse report %s: %w", filepath.Base(path), err)
	}
	return &RecordedReport{Report: &report, Raw: data}, nil
}

// plainFileName reports whether name can address a file inside one directory:
// non-empty, no separator, and neither of the two relative entries.
func plainFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && name == filepath.Base(name)
}
