package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// SessionsExport streams every recorded session.json still present under
// .putnami/sessions, one compacted document per line, oldest first.
//
// It exists because the session store is the only per-run source of
// cpu.actualMs, cpu.allocatedMs, task counts and cache reuse, it is pruned to a
// small number of records, and an agent worktree is deleted with its records.
// Accounting has to be collected while the worktree exists, so this is
// the collection point.
//
// The emitted line is the RECORDED DOCUMENT, forwarded verbatim rather than
// re-serialized through protocolcli.SessionFile. That is the same discipline
// workspace_state.RecordedReport.Raw documents: the binary reading a record is
// not always the binary that wrote it, so a round trip through a typed struct
// would silently drop a member a newer producer added. Only whitespace changes
// here — json.Compact folds the writer's json.MarshalIndent layout onto one
// line and touches nothing else, so member order and unknown members survive.
//
// Every line therefore declares its own version. A record written before the v2
// flip carries no protocolVersion and the version-1 shape; it is emitted as it
// is, exactly as readSessionMeta already treats it at the read side.
func SessionsExport(wsRoot string, since string) error {
	documents, err := recordedSessionStream(wsRoot, since)
	if err != nil {
		return err
	}
	for _, document := range documents {
		iox.Fprintln(os.Stdout, document.line)
	}
	return nil
}

// recordedSessionStream returns this worktree's recorded session documents in
// stream order: oldest first by recorded start time, deduplicated by session id,
// and narrowed by the --since cutoff.
//
// It is the ONE implementation `sessions export` and `sessions summary` share,
// and sharing it is the point rather than a convenience. Deduplication by
// recorded session id is what stops a store reached through two paths — a
// worktree whose .putnami/sessions is a symlink into a shared root is the
// ordinary case — from counting the same run twice, which is a bug the
// hand-written one-liners this pair replaces actually shipped. A second,
// separately-written ordering would drift from this one silently.
func recordedSessionStream(wsRoot string, since string) ([]recordedSessionDocument, error) {
	cutoff, hasCutoff, err := parseExportSince(since)
	if err != nil {
		return nil, err
	}

	store := workspace_state.NewSessionStore(wsRoot)
	ids, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	// An accounting consumer appends to a ledger, so the stream is oldest first.
	//
	// The directory name alone cannot order it. A session id is
	// <date>-<HHMMSS>-<6 random hex>: it resolves to the SECOND, and the suffix
	// that disambiguates a collision is random, not monotonic. Two sessions
	// started in the same second would therefore sort by a coin flip — and two
	// CLI invocations landing in one second is ordinary, not exotic. The
	// recorded startTime carries sub-second precision, so it is what actually
	// orders the stream.
	documents := make([]recordedSessionDocument, 0, len(ids))
	for _, id := range ids {
		if document, ok := readRecordedSessionDocument(store, id); ok {
			documents = append(documents, document)
		}
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].before(documents[j]) })

	emitted := make(map[string]bool, len(documents))
	kept := documents[:0]
	for _, document := range documents {
		if hasCutoff && !document.startsAtOrAfter(cutoff) {
			continue
		}
		if emitted[document.key] {
			continue
		}
		emitted[document.key] = true
		kept = append(kept, document)
	}
	return kept, nil
}

// recordedSessionDocument is one session.json ready to emit: the bytes the
// writer recorded, compacted to a single line, plus the two facts export needs
// to order and filter it without parsing the rest.
type recordedSessionDocument struct {
	// key is the identity deduplication is keyed on: the recorded session id,
	// falling back to the directory name for a document that carries none, so
	// two unidentified records stay distinct instead of collapsing into one.
	key string
	// dir is the session directory name, which is also the ordering tiebreak.
	dir string
	// startTime is the recorded start, absent when the document carries none or
	// it does not parse as RFC 3339.
	startTime time.Time
	hasStart  bool
	// orderAt is the instant the stream is ordered on: the recorded startTime
	// when it is readable, otherwise the second encoded in the directory name.
	// It is kept apart from startTime because --since may only trust what the
	// document itself recorded, while ordering must still place a record whose
	// start time is unreadable somewhere defensible rather than at the epoch.
	orderAt time.Time
	// line is the recorded document, compacted. Never re-serialized.
	line string
}

// before orders two records oldest first. The key is (orderAt, dir): comparing
// a tuple keeps the order TOTAL and transitive, which "compare by time when both
// sides have one, else by name" would not be — a record with no readable time
// could then sort before one record and after another that sorts before it.
func (d recordedSessionDocument) before(other recordedSessionDocument) bool {
	if !d.orderAt.Equal(other.orderAt) {
		return d.orderAt.Before(other.orderAt)
	}
	return d.dir < other.dir
}

// startsAtOrAfter reports whether the record satisfies a --since cutoff. A
// record whose start time is unreadable is NOT kept: the filter can only emit
// what it can show to satisfy it, and the same record is still emitted when no
// cutoff was given.
func (d recordedSessionDocument) startsAtOrAfter(cutoff time.Time) bool {
	return d.hasStart && !d.startTime.Before(cutoff)
}

// recordedSessionProbe reads only what export needs from a recorded document.
// `sessionId` is the version-2 spelling and `id` the version-1 one; both
// versions spell the start time `startTime`.
type recordedSessionProbe struct {
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	StartTime string `json:"startTime"`
}

// readRecordedSessionDocument loads one session's recorded document. It reports
// false — never an error — for a session directory with no session.json (an
// interrupted run recorded none) and for a document that does not parse, so one
// unreadable record cannot cost a consumer the whole export.
func readRecordedSessionDocument(store *workspace_state.SessionStore, id string) (recordedSessionDocument, bool) {
	data, err := os.ReadFile(filepath.Join(store.Root(), id, "session.json"))
	if err != nil {
		return recordedSessionDocument{}, false
	}

	var probe recordedSessionProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		return recordedSessionDocument{}, false
	}

	var compacted bytes.Buffer
	if err := json.Compact(&compacted, data); err != nil {
		return recordedSessionDocument{}, false
	}

	document := recordedSessionDocument{key: probe.SessionID, dir: id, line: compacted.String()}
	if document.key == "" {
		document.key = probe.ID
	}
	if document.key == "" {
		document.key = id
	}
	if probe.StartTime != "" {
		if start, err := time.Parse(time.RFC3339Nano, probe.StartTime); err == nil {
			document.startTime, document.hasStart = start, true
		}
	}
	document.orderAt = document.startTime
	if !document.hasStart {
		document.orderAt = sessionIDSecond(id)
	}
	return document, true
}

// sessionIDSecond recovers the second encoded in a session directory name
// (YYYYMMDD-HHMMSS-<suffix>) as a local instant. It is the ordering fallback for
// a record whose own startTime is unreadable: the name is generated from the
// local clock, so reading it back in time.Local puts the record among its
// neighbors instead of at the epoch. A name that does not parse yields the zero
// time, and the directory-name tiebreak then keeps the order defined.
func sessionIDSecond(id string) time.Time {
	if len(id) < len("20060102-150405") {
		return time.Time{}
	}
	second, err := time.ParseInLocation("20060102-150405", id[:len("20060102-150405")], time.Local)
	if err != nil {
		return time.Time{}
	}
	return second
}

// parseExportSince parses the --since value. An empty value means no filter; a
// value that is not an RFC 3339 timestamp is a usage error rather than a silent
// no-op, because silently dropping the filter would hand an accounting consumer
// more records than it asked for without saying so.
func parseExportSince(since string) (time.Time, bool, error) {
	if since == "" {
		return time.Time{}, false, nil
	}
	cutoff, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return time.Time{}, false, cmderr.Usagef(
			"invalid --since value %q: expected an RFC 3339 timestamp such as 2026-09-13T00:00:00Z", since)
	}
	return cutoff, true, nil
}
