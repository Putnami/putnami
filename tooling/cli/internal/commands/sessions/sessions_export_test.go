package sessions

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// writeRecordedSession writes one session directory with the given raw
// session.json bytes, exactly as a recorded run leaves them.
func writeRecordedSession(t *testing.T, wsRoot, id, document string) {
	t.Helper()
	dir := filepath.Join(wsRoot, ".putnami", "sessions", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create session dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(document), 0o644); err != nil {
		t.Fatalf("write session.json: %v", err)
	}
}

// exportLines runs the export and returns its emitted lines.
func exportLines(t *testing.T, wsRoot, since string) []string {
	t.Helper()
	out, err := sharedtest.CaptureStdout(t, func() error { return SessionsExport(wsRoot, since) })
	if err != nil {
		t.Fatalf("SessionsExport(%q) error: %v", since, err)
	}
	trimmed := strings.TrimSuffix(out, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// recordedV2 is a minimal but real version-2 session document, indented the way
// the writer records it (json.MarshalIndent).
func recordedV2(t *testing.T, id, startTime string, actualMs, allocatedMs int64) string {
	t.Helper()
	doc := protocolcli.SessionFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		SessionID:       id,
		StartTime:       startTime,
		EndTime:         startTime,
		Commands:        []string{"build"},
		Run: protocolcli.RunSummary{
			Outcome:    protocolcli.RunOutcomeSuccess,
			Counts:     protocolcli.RunCounts{Total: 3, Succeeded: 3},
			DurationMs: 1234,
			CPU:        &protocolcli.RunCPU{ActualMs: actualMs, AllocatedMs: allocatedMs},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal session document: %v", err)
	}
	return string(data)
}

// TestSessionsExport_OrdersOldestFirstAndDeduplicates pins the two properties an
// accounting consumer appends a ledger from: chronological order, and one line
// per session id however many directories claim it.
func TestSessionsExport_OrdersOldestFirstAndDeduplicates(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "recorded-sessions-export-verbatim-oldest-first-and-deduplicated")
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260301-090000-cccccc", recordedV2(t, "20260301-090000-cccccc", "2026-03-01T09:00:00Z", 300, 600))
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa", recordedV2(t, "20260101-120000-aaaaaa", "2026-01-01T12:00:00Z", 100, 200))
	writeRecordedSession(t, ws, "20260201-101500-bbbbbb", recordedV2(t, "20260201-101500-bbbbbb", "2026-02-01T10:15:00Z", 200, 400))
	// A directory whose recorded document claims an id already emitted. Only the
	// first occurrence in stream order survives.
	writeRecordedSession(t, ws, "20260401-080000-dddddd", recordedV2(t, "20260101-120000-aaaaaa", "2026-04-01T08:00:00Z", 999, 999))

	lines := exportLines(t, ws, "")
	if len(lines) != 3 {
		t.Fatalf("emitted %d lines, want 3 (one per distinct session id): %v", len(lines), lines)
	}

	var ids []string
	for _, line := range lines {
		var row protocolcli.SessionFile
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("emitted line is not a session document: %v (%s)", err, line)
		}
		ids = append(ids, row.SessionID)
	}
	want := []string{"20260101-120000-aaaaaa", "20260201-101500-bbbbbb", "20260301-090000-cccccc"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v (oldest first)", ids, want)
	}
}

// TestSessionsExport_OrdersSameSecondRecordsByRecordedStart pins the stream
// against the session id's resolution. An id is
// <date>-<HHMMSS>-<6 random hex>: it resolves to the second, and the suffix that
// breaks a collision is RANDOM. Ordering on the directory name alone therefore
// emits two same-second sessions in a coin-flip order, and two CLI invocations
// landing in one second is ordinary. The recorded startTime is sub-second, so it
// is what orders them. Here the LATER record carries the smaller suffix, so a
// name-ordered stream emits it first.
func TestSessionsExport_OrdersSameSecondRecordsByRecordedStart(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "the-export-order-comes-from-the-recorded-start-not-the-session-id")
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260301-090000-ffffff",
		recordedV2(t, "20260301-090000-ffffff", "2026-03-01T09:00:00.100000Z", 100, 200))
	writeRecordedSession(t, ws, "20260301-090000-000001",
		recordedV2(t, "20260301-090000-000001", "2026-03-01T09:00:00.900000Z", 300, 600))

	ids := exportedSessionIDs(t, exportLines(t, ws, ""))
	want := []string{"20260301-090000-ffffff", "20260301-090000-000001"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v (the .100 record precedes the .900 one despite the larger suffix)", ids, want)
	}
}

// TestSessionsExport_OrdersUnreadableStartTimesByTheirRecordedSecond covers the
// ordering fallback. A record whose startTime is unreadable still belongs among
// its neighbors, so it orders on the second its directory name encodes rather
// than sinking to the epoch. The order stays total: the key is
// (instant, directory name), so no record can sort before one sibling and after
// another that sorts before it.
func TestSessionsExport_OrdersUnreadableStartTimesByTheirRecordedSecond(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "the-export-order-comes-from-the-recorded-start-not-the-session-id")
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa",
		recordedV2(t, "20260101-120000-aaaaaa", "2026-01-01T12:00:00Z", 100, 200))
	// Valid JSON, but the start time is not RFC 3339.
	writeRecordedSession(t, ws, "20260201-101500-bbbbbb",
		`{"protocolVersion":2,"sessionId":"20260201-101500-bbbbbb","startTime":"not-a-timestamp","commands":["build"]}`)
	writeRecordedSession(t, ws, "20260301-090000-cccccc",
		recordedV2(t, "20260301-090000-cccccc", "2026-03-01T09:00:00Z", 300, 600))

	ids := exportedSessionIDs(t, exportLines(t, ws, ""))
	want := []string{"20260101-120000-aaaaaa", "20260201-101500-bbbbbb", "20260301-090000-cccccc"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v (the unreadable record keeps its place, not the epoch)", ids, want)
	}
}

// exportedSessionIDs reads the session id out of every emitted line, proving the
// order from the stream itself rather than from the store.
func exportedSessionIDs(t *testing.T, lines []string) []string {
	t.Helper()
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		var row protocolcli.SessionFile
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("emitted line is not a session document: %v (%s)", err, line)
		}
		ids = append(ids, row.SessionID)
	}
	return ids
}

// TestSessionsExport_CarriesTheAccountingFields pins that the accounting fields
// survive the export, read back from the emitted line itself.
func TestSessionsExport_CarriesTheAccountingFields(t *testing.T) {
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa", recordedV2(t, "20260101-120000-aaaaaa", "2026-01-01T12:00:00Z", 4200, 9000))

	lines := exportLines(t, ws, "")
	if len(lines) != 1 {
		t.Fatalf("emitted %d lines, want 1", len(lines))
	}
	var row protocolcli.SessionFile
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("unmarshal emitted line: %v", err)
	}
	if row.Run.CPU == nil || row.Run.CPU.ActualMs != 4200 || row.Run.CPU.AllocatedMs != 9000 {
		t.Errorf("cpu = %+v, want actual 4200 / allocated 9000", row.Run.CPU)
	}
	if row.Run.ExitCode != 0 {
		t.Errorf("run.exitCode = %d, want 0", row.Run.ExitCode)
	}
	if row.Run.Counts.Total != 3 {
		t.Errorf("counts.total = %d, want 3", row.Run.Counts.Total)
	}
	if row.Run.DurationMs != 1234 {
		t.Errorf("run.durationMs = %d, want 1234", row.Run.DurationMs)
	}
	if len(row.Commands) != 1 || row.Commands[0] != "build" {
		t.Errorf("commands = %v, want [build]", row.Commands)
	}
}

// TestSessionsExport_CarriesTheGatedTreeFingerprint is the collection half:
// the session record names the tree it gated, and export is where that
// record leaves a worktree before the worktree is deleted. It rides along by
// construction — export forwards the recorded bytes — so this pins the
// construction rather than a field mapping nobody wrote.
func TestSessionsExport_CarriesTheGatedTreeFingerprint(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "gated-tree-identity", "a-recorded-session-names-the-tree-it-opened-on")
	ws := t.TempDir()
	fingerprint := "4f3d0d6f0c1ba6c8c4b9a2e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6b5a49"
	headSHA := "ad048a6b3c0d1e2f30415263748596a7b8c9d0e1"
	// Stated as the writer records it (json.MarshalIndent), so what the export
	// forwards is the shape a real session.json has on disk.
	recorded := `{
  "protocolVersion": 2,
  "sessionId": "20260913-101500-aaaaaa",
  "startTime": "2026-09-13T10:15:00Z",
  "commands": ["lint", "test", "build", "validate"],
  "tree": {
    "fingerprint": "` + fingerprint + `",
    "dirty": true,
    "headSHA": "` + headSHA + `"
  },
  "run": {"outcome": "success", "exitCode": 0, "counts": {"total": 1, "succeeded": 1}}
}`
	writeRecordedSession(t, ws, "20260913-101500-aaaaaa", recorded)

	lines := exportLines(t, ws, "")
	if len(lines) != 1 {
		t.Fatalf("emitted %d lines, want 1", len(lines))
	}
	var row protocolcli.SessionFile
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("emitted line is not a session document: %v (%s)", err, lines[0])
	}
	if row.Tree == nil {
		t.Fatalf("the exported line dropped the tree block: %s", lines[0])
	}
	if row.Tree.Fingerprint != fingerprint || row.Tree.HeadSHA != headSHA || !row.Tree.Dirty {
		t.Errorf("tree = %+v, want the recorded fingerprint, head and dirty verdict", row.Tree)
	}
}

// TestSessionsExport_ForwardsRecordedBytesVerbatim is the reason export does not
// round-trip through protocolcli.SessionFile: a member a NEWER producer added is
// unknown to this binary's struct, and re-serializing would drop it silently.
// Only whitespace may change.
func TestSessionsExport_ForwardsRecordedBytesVerbatim(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "recorded-sessions-export-verbatim-oldest-first-and-deduplicated")
	ws := t.TempDir()
	recorded := `{
  "protocolVersion": 2,
  "sessionId": "20260101-120000-aaaaaa",
  "startTime": "2026-01-01T12:00:00Z",
  "commands": ["build"],
  "run": {"counts": {"total": 1}, "unknownRunMember": {"nested": [1, 2]}},
  "unknownTopLevelMember": "from-a-newer-cli"
}`
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa", recorded)

	lines := exportLines(t, ws, "")
	if len(lines) != 1 {
		t.Fatalf("emitted %d lines, want 1", len(lines))
	}

	var got, want any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal emitted line: %v", err)
	}
	if err := json.Unmarshal([]byte(recorded), &want); err != nil {
		t.Fatal(err)
	}
	gotBytes, _ := json.Marshal(got)
	wantBytes, _ := json.Marshal(want)
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("export altered the recorded document.\n got: %s\nwant: %s", gotBytes, wantBytes)
	}
	if !strings.Contains(lines[0], `"unknownTopLevelMember":"from-a-newer-cli"`) {
		t.Errorf("a member this binary's struct does not know was dropped: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"unknownRunMember"`) {
		t.Errorf("a nested member this binary's struct does not know was dropped: %s", lines[0])
	}
	if strings.Contains(lines[0], "\n") {
		t.Errorf("the emitted document is not one line: %q", lines[0])
	}
}

// TestSessionsExport_EmitsVersionOneRecordsAsRecorded pins the pre-v2 caveat: a
// record written before the version-2 flip carries no protocolVersion and the
// version-1 shape, and export states it rather than upgrading it. Every line
// declares its own version.
func TestSessionsExport_EmitsVersionOneRecordsAsRecorded(t *testing.T) {
	ws := t.TempDir()
	v1 := `{
  "id": "20251201-100000-000001",
  "startTime": "2025-12-01T10:00:00Z",
  "endTime": "2025-12-01T10:00:05Z",
  "durationMs": 5000,
  "stats": {"total": 2, "succeeded": 2},
  "commands": ["test"]
}`
	writeRecordedSession(t, ws, "20251201-100000-000001", v1)
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa", recordedV2(t, "20260101-120000-aaaaaa", "2026-01-01T12:00:00Z", 1, 2))

	lines := exportLines(t, ws, "")
	if len(lines) != 2 {
		t.Fatalf("emitted %d lines, want 2", len(lines))
	}

	// The version claim is read with a pointer so "absent" stays distinguishable
	// from "present and zero" — that difference IS the version-1 shape.
	type versionProbe struct {
		ProtocolVersion *int   `json:"protocolVersion"`
		ID              string `json:"id"`
		DurationMs      int64  `json:"durationMs"`
	}
	var older versionProbe
	if err := json.Unmarshal([]byte(lines[0]), &older); err != nil {
		t.Fatalf("unmarshal v1 line: %v", err)
	}
	if older.ProtocolVersion != nil {
		t.Errorf("a version-1 record must not gain a protocolVersion on export: %s", lines[0])
	}
	if older.ID != "20251201-100000-000001" {
		t.Errorf("v1 line lost its id member: %s", lines[0])
	}
	if older.DurationMs != 5000 {
		t.Errorf("v1 line lost its durationMs member: %s", lines[0])
	}

	var newer versionProbe
	if err := json.Unmarshal([]byte(lines[1]), &newer); err != nil {
		t.Fatalf("unmarshal v2 line: %v", err)
	}
	if newer.ProtocolVersion == nil || *newer.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("v2 line lost its own version claim: %s", lines[1])
	}
}

// TestSessionsExport_SkipsUnreadableRecords pins that one bad record never costs
// a consumer the rest of the export: an interrupted run recorded no session.json
// at all, and a truncated document does not parse.
func TestSessionsExport_SkipsUnreadableRecords(t *testing.T) {
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa", recordedV2(t, "20260101-120000-aaaaaa", "2026-01-01T12:00:00Z", 1, 2))
	// An interrupted run: the directory exists, the document does not.
	if err := os.MkdirAll(filepath.Join(ws, ".putnami", "sessions", "20260102-120000-bbbbbb"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRecordedSession(t, ws, "20260103-120000-cccccc", `{"protocolVersion": 2, "sessionId": "trunc`)
	writeRecordedSession(t, ws, "20260104-120000-dddddd", recordedV2(t, "20260104-120000-dddddd", "2026-01-04T12:00:00Z", 3, 4))

	lines := exportLines(t, ws, "")
	if len(lines) != 2 {
		t.Fatalf("emitted %d lines, want 2 (the two readable records): %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "20260101-120000-aaaaaa") || !strings.Contains(lines[1], "20260104-120000-dddddd") {
		t.Errorf("emitted the wrong records: %v", lines)
	}
}

// TestSessionsExport_SinceFiltersOnTheRecordedStartTime pins the boundary and
// the authority: the cutoff is inclusive, and it is read from the recorded start
// time rather than from the directory name.
func TestSessionsExport_SinceFiltersOnTheRecordedStartTime(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "since-filters-on-the-recorded-start-inclusively")
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa", recordedV2(t, "20260101-120000-aaaaaa", "2026-01-01T12:00:00Z", 1, 2))
	writeRecordedSession(t, ws, "20260201-120000-bbbbbb", recordedV2(t, "20260201-120000-bbbbbb", "2026-02-01T12:00:00Z", 3, 4))
	writeRecordedSession(t, ws, "20260301-120000-cccccc", recordedV2(t, "20260301-120000-cccccc", "2026-03-01T12:00:00Z", 5, 6))

	between := exportLines(t, ws, "2026-01-15T00:00:00Z")
	if len(between) != 2 {
		t.Fatalf("--since between two sessions emitted %d lines, want 2: %v", len(between), between)
	}

	// A record recorded EXACTLY at the cutoff is kept.
	boundary := exportLines(t, ws, "2026-02-01T12:00:00Z")
	if len(boundary) != 2 {
		t.Fatalf("--since at a record's exact start time emitted %d lines, want 2: %v", len(boundary), boundary)
	}
	if !strings.Contains(boundary[0], "20260201-120000-bbbbbb") {
		t.Errorf("the record at the cutoff was dropped: %v", boundary)
	}

	// The recorded start time wins over the directory name. This directory is
	// named as if it ran in 2026, and its document says 2024.
	writeRecordedSession(t, ws, "20260401-120000-dddddd", recordedV2(t, "20260401-120000-dddddd", "2024-04-01T12:00:00Z", 7, 8))
	misnamed := exportLines(t, ws, "2026-01-15T00:00:00Z")
	for _, line := range misnamed {
		if strings.Contains(line, "20260401-120000-dddddd") {
			t.Errorf("--since kept a record whose RECORDED start precedes the cutoff: %v", misnamed)
		}
	}
}

// TestSessionsExport_SinceDropsUnreadableStartTimes pins the fail-closed half of
// the filter: a record that cannot be shown to satisfy --since is not emitted
// under it, and is still emitted without it.
func TestSessionsExport_SinceDropsUnreadableStartTimes(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-record-durability", "since-filters-on-the-recorded-start-inclusively")
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa",
		`{"protocolVersion": 2, "sessionId": "20260101-120000-aaaaaa", "commands": ["build"]}`)
	writeRecordedSession(t, ws, "20260201-120000-bbbbbb",
		`{"protocolVersion": 2, "sessionId": "20260201-120000-bbbbbb", "startTime": "not-a-timestamp", "commands": ["build"]}`)
	writeRecordedSession(t, ws, "20260301-120000-cccccc", recordedV2(t, "20260301-120000-cccccc", "2026-03-01T12:00:00Z", 1, 2))

	all := exportLines(t, ws, "")
	if len(all) != 3 {
		t.Fatalf("without --since, emitted %d lines, want all 3: %v", len(all), all)
	}

	filtered := exportLines(t, ws, "2020-01-01T00:00:00Z")
	if len(filtered) != 1 {
		t.Fatalf("under --since, emitted %d lines, want only the record with a readable start: %v", len(filtered), filtered)
	}
	if !strings.Contains(filtered[0], "20260301-120000-cccccc") {
		t.Errorf("kept the wrong record: %v", filtered)
	}
}

// TestSessionsExport_RejectsAnInvalidSince pins that a malformed cutoff is a
// usage error. Silently dropping the filter would hand an accounting consumer
// more records than it asked for without saying so.
func TestSessionsExport_RejectsAnInvalidSince(t *testing.T) {
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa", recordedV2(t, "20260101-120000-aaaaaa", "2026-01-01T12:00:00Z", 1, 2))

	out, err := sharedtest.CaptureStdout(t, func() error { return SessionsExport(ws, "yesterday") })
	if err == nil {
		t.Fatal("expected an error for a non-RFC-3339 --since value")
	}
	if !errors.Is(err, cmderr.ErrUsage) {
		t.Errorf("error = %v, want a usage error", err)
	}
	if out != "" {
		t.Errorf("a rejected --since must emit nothing, got %q", out)
	}
}

// TestSessionsExport_EmptyStoreEmitsNothing pins that a worktree with no
// recorded sessions exports an empty stream rather than failing.
func TestSessionsExport_EmptyStoreEmitsNothing(t *testing.T) {
	out, err := sharedtest.CaptureStdout(t, func() error { return SessionsExport(t.TempDir(), "") })
	if err != nil {
		t.Fatalf("SessionsExport() on an empty store: %v", err)
	}
	if out != "" {
		t.Errorf("expected no output, got %q", out)
	}
}
