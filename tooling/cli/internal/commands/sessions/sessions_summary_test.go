package sessions

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// summarySession builds a recorded version-2 document with the members a ledger
// row reads. parent is empty for a top-level run.
func summarySession(t *testing.T, id, parent, startTime string, commands []string, selection *protocolcli.SessionSelection, total, localHits int, actualMs, wallMs int64) string {
	t.Helper()
	doc := protocolcli.SessionFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		SessionID:       id,
		ParentSessionID: parent,
		StartTime:       startTime,
		EndTime:         startTime,
		Commands:        commands,
		Selection:       selection,
		Run: protocolcli.RunSummary{
			Outcome:    protocolcli.RunOutcomeSuccess,
			Counts:     protocolcli.RunCounts{Total: total, Succeeded: total},
			Reuse:      protocolcli.RunReuse{LocalCache: localHits},
			DurationMs: wallMs,
			CPU:        &protocolcli.RunCPU{ActualMs: actualMs, AllocatedMs: actualMs * 2},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal session document: %v", err)
	}
	return string(data)
}

// summaryRows runs the command in JSON mode and decodes the emitted rows, so
// every assertion reads the contract a consumer binds to rather than the table.
func summaryRows(t *testing.T, wsRoot, since, commands string) []sessionSummaryRow {
	t.Helper()
	out, err := sharedtest.CaptureStdout(t, func() error {
		return SessionsSummary(wsRoot, since, commands, false, "json")
	})
	if err != nil {
		t.Fatalf("SessionsSummary(since=%q, command=%q): %v", since, commands, err)
	}
	// Exactly ONE document, always, and always an array. The shared capture in
	// internal/cli unwraps a stream of one document into ResultV2.data, so this
	// is what keeps `data` an array at every row count instead of null at zero
	// and a bare object at one. Decoding strictly here is what would catch a
	// regression back to a JSONL stream.
	trimmed := strings.TrimSuffix(out, "\n")
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("summary emitted %d documents, want exactly one JSON array:\n%s",
			strings.Count(trimmed, "\n")+1, out)
	}
	var rows []sessionSummaryRow
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		t.Fatalf("emitted document is not a summary row array: %v (%s)", err, trimmed)
	}
	return rows
}

// gateAndNestedStore writes the shape this command exists for: one gate session
// spanning a nested `build` that its validation spawned, plus an unrelated
// earlier run, plus a duplicate directory claiming the gate's id — the symlink
// double-count the hand-written one-liners shipped.
func gateAndNestedStore(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260913-020000-000001", summarySession(t,
		"20260913-020000-000001", "", "2026-09-13T02:00:00Z", []string{"build"},
		&protocolcli.SessionSelection{Mode: protocolcli.SessionSelectionModeAll}, 12, 12, 500, 3000))
	writeRecordedSession(t, ws, "20260913-023611-4a5b87", summarySession(t,
		"20260913-023611-4a5b87", "", "2026-09-13T02:36:11Z", []string{"lint", "test", "build", "validate"},
		&protocolcli.SessionSelection{
			Mode: protocolcli.SessionSelectionModeProjects, Scoped: true,
			Projects: []string{"/protocols/cli", "/tooling/cli"},
		}, 40, 25, 120000, 33000))
	writeRecordedSession(t, ws, "20260913-023629-da7fb8", summarySession(t,
		"20260913-023629-da7fb8", "20260913-023611-4a5b87", "2026-09-13T02:36:29Z", []string{"build"},
		&protocolcli.SessionSelection{Mode: protocolcli.SessionSelectionModeAll}, 84, 84, 900, 4000))
	// The same gate reached through a second path: a duplicate directory whose
	// document claims the id already listed.
	writeRecordedSession(t, ws, "20260913-030000-dupdup", summarySession(t,
		"20260913-023611-4a5b87", "", "2026-09-13T03:00:00Z", []string{"lint", "test", "build", "validate"},
		nil, 40, 25, 120000, 33000))
	return ws
}

// TestSessionsSummary_ListsOneRowPerSessionOldestFirst pins the two properties
// the hand-written one-liner got wrong: a session reached through two paths is
// counted once, and the order is the recorded start.
func TestSessionsSummary_ListsOneRowPerSessionOldestFirst(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-summary-view", "one-row-per-session-oldest-first-and-deduplicated")
	rows := summaryRows(t, gateAndNestedStore(t), "", "")

	var ids []string
	for _, row := range rows {
		ids = append(ids, row.SessionID)
	}
	want := []string{"20260913-020000-000001", "20260913-023611-4a5b87", "20260913-023629-da7fb8"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v (oldest first, one row per session id)", ids, want)
	}
}

// TestSessionsSummary_MarksNestedRunsWithoutDroppingThem is the issue's second
// acceptance criterion. A nested run is real work with real CPU, so it is
// listed; what it must never be is indistinguishable from a run of its own.
func TestSessionsSummary_MarksNestedRunsWithoutDroppingThem(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-summary-view", "nested-runs-are-marked-and-never-counted-as-gates")
	rows := summaryRows(t, gateAndNestedStore(t), "", "")

	byID := make(map[string]sessionSummaryRow, len(rows))
	for _, row := range rows {
		byID[row.SessionID] = row
	}
	nested, ok := byID["20260913-023629-da7fb8"]
	if !ok {
		t.Fatal("the nested run was dropped; it is marked, not hidden")
	}
	if !nested.Nested || nested.ParentSessionID != "20260913-023611-4a5b87" {
		t.Errorf("nested row = %+v, want nested=true and the spawning session named", nested)
	}
	if gate := byID["20260913-023611-4a5b87"]; gate.Nested || gate.ParentSessionID != "" {
		t.Errorf("gate row = %+v, want a top-level run", gate)
	}

	// The table says so too, and says it on the nested row only.
	out, err := sharedtest.CaptureStdout(t, func() error {
		return SessionsSummary(gateAndNestedStore(t), "", "", false, "text")
	})
	if err != nil {
		t.Fatalf("SessionsSummary(text): %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "20260913-023629-da7fb8") && !strings.Contains(line, "nested") {
			t.Errorf("the nested row is not marked in the table: %q", line)
		}
		if strings.Contains(line, "20260913-023611-4a5b87") && strings.Contains(line, "nested") {
			t.Errorf("the gate row is marked nested: %q", line)
		}
	}
}

// TestSessionsSummary_CommandFilterIsExactSetEquality is the behavior that makes
// "list the gate sessions of the last /fix" answerable. A gate is exactly
// lint,test,build,validate; subset matching would also return the bare `build`
// the gate's own validation spawned, which is the miscount being removed.
func TestSessionsSummary_CommandFilterIsExactSetEquality(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-summary-view", "nested-runs-are-marked-and-never-counted-as-gates")
	ws := gateAndNestedStore(t)

	gates := summaryRows(t, ws, "", "lint,test,build,validate")
	if len(gates) != 1 || gates[0].SessionID != "20260913-023611-4a5b87" {
		t.Fatalf("gate rows = %+v, want exactly the lint,test,build,validate session", gates)
	}

	// Order is not identity: the same set typed in another order selects the
	// same run.
	if reordered := summaryRows(t, ws, "", "validate,build,test,lint"); len(reordered) != 1 ||
		reordered[0].SessionID != gates[0].SessionID {
		t.Errorf("reordered filter = %+v, want the same single gate", reordered)
	}

	// A subset of the gate's commands selects the runs that ran EXACTLY it, and
	// never the gate. The store holds two sessions whose command set is [build],
	// but one of them is the nested run the gate's validation spawned: --command
	// asks which runs of this shape HAPPENED, and a run a task spawned is not a
	// run of its own.
	builds := summaryRows(t, ws, "", "build")
	if len(builds) != 1 || builds[0].SessionID != "20260913-020000-000001" {
		t.Fatalf("build rows = %+v, want only the top-level [build] session", builds)
	}
	if builds[0].Nested {
		t.Error("--command returned a nested run: it would be counted as a run of its own")
	}
}

// TestSessionsSummary_CommandFilterNeverReturnsANestedRun is the finding the
// exact-set-equality rule alone did NOT cover.
//
// Matching on the command set made the filter's correctness depend on an
// assumption nothing enforces: that no task ever invokes the CLI with the set
// being asked for. A validation guard that ran the full gate would be returned
// AS a gate, and a ledger summing `--command lint,test,build,validate` would
// count the same work twice — the parent already charges the spawning task's
// subprocess tree. Nesting, not the command set, is what decides this.
func TestSessionsSummary_CommandFilterNeverReturnsANestedRun(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-summary-view", "nested-runs-are-marked-and-never-counted-as-gates")
	gate := []string{"lint", "test", "build", "validate"}
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260913-023611-4a5b87", summarySession(t,
		"20260913-023611-4a5b87", "", "2026-09-13T02:36:11Z", gate, nil, 40, 25, 120000, 33000))
	// A nested run that ran the gate's own command set.
	writeRecordedSession(t, ws, "20260913-023629-da7fb8", summarySession(t,
		"20260913-023629-da7fb8", "20260913-023611-4a5b87", "2026-09-13T02:36:29Z", gate, nil, 40, 25, 90000, 21000))

	rows := summaryRows(t, ws, "", "lint,test,build,validate")
	if len(rows) != 1 || rows[0].SessionID != "20260913-023611-4a5b87" {
		t.Fatalf("gate rows = %+v, want only the top-level gate: a nested run that ran the gate double-counts", rows)
	}

	// It is still LISTED and MARKED when nothing is filtered — the fix removes it
	// from the count, never from the ledger.
	all := summaryRows(t, ws, "", "")
	if len(all) != 2 {
		t.Fatalf("unfiltered rows = %+v, want both sessions listed", all)
	}
	if !all[1].Nested || all[1].ParentSessionID != "20260913-023611-4a5b87" {
		t.Errorf("nested row = %+v, want it marked and naming its parent", all[1])
	}
}

// TestSessionsSummary_JSONIsAlwaysAnArray pins the shape a ledger binds to.
//
// The rows reach a consumer as ResultV2.data, and the shared capture in
// internal/cli unwraps a stream of exactly one document. Emitting them as JSONL
// would therefore make `data` null at zero rows and a BARE OBJECT at one — and
// one row is the ordinary shape of `--command lint,test,build,validate` on a
// fresh worktree, so `.data[]` would break on the most common case rather than
// on an exotic one. One array document is unwrapped to itself.
func TestSessionsSummary_JSONIsAlwaysAnArray(t *testing.T) {
	ws := gateAndNestedStore(t)
	for _, tc := range []struct {
		name           string
		since, command string
		want           int
	}{
		{name: "no rows", since: "2030-01-01T00:00:00Z", want: 0},
		{name: "exactly one row", command: "lint,test,build,validate", want: 1},
		{name: "several rows", want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := sharedtest.CaptureStdout(t, func() error {
				return SessionsSummary(ws, tc.since, tc.command, false, "json")
			})
			if err != nil {
				t.Fatalf("SessionsSummary: %v", err)
			}
			trimmed := strings.TrimSuffix(out, "\n")
			// A JSON array, decoded as one — never null, never an object.
			var rows []sessionSummaryRow
			if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
				t.Fatalf("output is not a JSON array: %v (%s)", err, trimmed)
			}
			if rows == nil {
				t.Fatalf("output decoded to a nil slice (%s): `.data[]` breaks on null", trimmed)
			}
			if len(rows) != tc.want {
				t.Errorf("rows = %d, want %d", len(rows), tc.want)
			}
		})
	}
}

// TestSessionsSummary_ReducesTheAccountingFields pins the numbers a ledger row
// carries, including the derivation executed = total - reused.
func TestSessionsSummary_ReducesTheAccountingFields(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-summary-view", "rows-carry-tasks-cpu-wall-selection-and-outcome")
	rows := summaryRows(t, gateAndNestedStore(t), "", "lint,test,build,validate")
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	row := rows[0]
	if row.Tasks.Total != 40 || row.Tasks.Reused != 25 || row.Tasks.Executed != 15 {
		t.Errorf("tasks = %+v, want total 40 / executed 15 / reused 25", row.Tasks)
	}
	if row.CPUActualMs == nil || *row.CPUActualMs != 120000 {
		t.Errorf("cpuActualMs = %v, want 120000", row.CPUActualMs)
	}
	if row.WallMs != 33000 {
		t.Errorf("wallMs = %d, want 33000", row.WallMs)
	}
	if row.Outcome != protocolcli.RunOutcomeSuccess {
		t.Errorf("outcome = %q, want %q", row.Outcome, protocolcli.RunOutcomeSuccess)
	}
	if row.Selection == nil || row.Selection.Mode != protocolcli.SessionSelectionModeProjects ||
		!row.Selection.Scoped || len(row.Selection.Projects) != 2 {
		t.Errorf("selection = %+v, want a scoped explicit projects selection over 2 projects", row.Selection)
	}
}

// TestSessionsSummary_RendersTheSelectionTheWayItWasSpelled covers the column
// the issue names. A record that states no selection renders as unknown, never
// as `--all`: inventing the broadest selection for a record that stated none
// would misreport what a run covered.
func TestSessionsSummary_RendersTheSelectionTheWayItWasSpelled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		selection *protocolcli.SessionSelection
		want      string
	}{
		{"unstated", nil, "-"},
		{"whole workspace", &protocolcli.SessionSelection{Mode: protocolcli.SessionSelectionModeAll}, "--all"},
		{
			"filtered workspace",
			&protocolcli.SessionSelection{Mode: protocolcli.SessionSelectionModeAll, Scoped: true, Projects: []string{"/a"}},
			"--all filtered (1)",
		},
		{
			"impact projection",
			&protocolcli.SessionSelection{Mode: protocolcli.SessionSelectionModeImpacted, Scoped: true, Projects: []string{"/a", "/b"}},
			"--impacted (2)",
		},
		{
			"explicit projects",
			&protocolcli.SessionSelection{Mode: protocolcli.SessionSelectionModeProjects, Scoped: true, Projects: []string{"/a", "/b"}},
			"--projects /a,/b",
		},
	}
	for _, tc := range cases {
		if got := formatSummarySelection(tc.selection); got != tc.want {
			t.Errorf("%s: selection = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSessionsSummary_ListsVersion1RecordsWithTheNewColumnsAbsent keeps the
// pre-version-2 fallback the session readers already carry. A workspace that
// upgraded across the flip still holds those records, and a ledger that skipped
// them would understate the period; what they cannot carry is the members
// version 2 added, which read as absent rather than as zero.
func TestSessionsSummary_ListsVersion1RecordsWithTheNewColumnsAbsent(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-summary-view", "rows-carry-tasks-cpu-wall-selection-and-outcome")
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260101-120000-aaaaaa", `{
  "id": "20260101-120000-aaaaaa",
  "startTime": "2026-01-01T12:00:00Z",
  "durationMs": 2500,
  "commands": ["build"],
  "stats": { "total": 9, "succeeded": 9, "failed": 0, "cached": 4, "coalesced": 1 }
}`)

	rows := summaryRows(t, ws, "", "")
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the version-1 record listed", rows)
	}
	row := rows[0]
	if row.SessionID != "20260101-120000-aaaaaa" || row.WallMs != 2500 {
		t.Errorf("row = %+v, want the recorded id and wall", row)
	}
	if row.Tasks.Total != 9 || row.Tasks.Reused != 5 || row.Tasks.Executed != 4 {
		t.Errorf("tasks = %+v, want total 9 / executed 4 / reused 5", row.Tasks)
	}
	if row.CPUActualMs != nil {
		t.Errorf("cpuActualMs = %v, want absent — version 1 recorded no CPU", *row.CPUActualMs)
	}
	if row.Selection != nil || row.ParentSessionID != "" || row.Nested {
		t.Errorf("row = %+v, want no selection and no parent", row)
	}
	if row.Outcome != protocolcli.RunOutcomeSuccess {
		t.Errorf("outcome = %q, want the verdict recovered from the version-1 counters", row.Outcome)
	}
}

// TestSessionsSummary_SinceFiltersOnTheRecordedStart reuses export's cutoff, so
// the two surfaces answer the same question about the same period.
func TestSessionsSummary_SinceFiltersOnTheRecordedStart(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "session-summary-view", "one-row-per-session-oldest-first-and-deduplicated")
	ws := gateAndNestedStore(t)
	rows := summaryRows(t, ws, "2026-09-13T02:36:11Z", "")
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.SessionID)
	}
	want := []string{"20260913-023611-4a5b87", "20260913-023629-da7fb8"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v (the cutoff is inclusive at the boundary)", ids, want)
	}
}

// TestSessionsSummary_RejectsUnusableFilters keeps a mistyped filter a usage
// error rather than a silent no-op. Dropping the filter would hand a ledger
// consumer more rows than it asked for without saying so.
func TestSessionsSummary_RejectsUnusableFilters(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	if err := SessionsSummary(ws, "yesterday", "", false, "json"); !errors.Is(err, cmderr.ErrUsage) {
		t.Errorf("--since yesterday: err = %v, want ErrUsage", err)
	}
	if err := SessionsSummary(ws, "", "lint,,test", false, "json"); !errors.Is(err, cmderr.ErrUsage) {
		t.Errorf("--command lint,,test: err = %v, want ErrUsage", err)
	}
}

// TestSessionsSummary_EmptyStoreSaysSo covers the table's empty answer: a store
// with nothing to show says so instead of printing a bare header.
func TestSessionsSummary_EmptyStoreSaysSo(t *testing.T) {
	t.Parallel()
	out, err := sharedtest.CaptureStdout(t, func() error {
		return SessionsSummary(t.TempDir(), "", "", false, "text")
	})
	if err != nil {
		t.Fatalf("SessionsSummary on an empty store: %v", err)
	}
	if !strings.Contains(out, "No sessions match.") {
		t.Errorf("output = %q, want it to say no sessions match", out)
	}
}

// digestSession builds a recorded version-2 document whose task records carry
// the given digests. An empty digest leaves the member absent, which is what a
// skipped record and every older producer write.
func digestSession(t *testing.T, id, startTime string, commands []string, tasks []protocolcli.TaskRecord) string {
	t.Helper()
	counts := protocolcli.RunCounts{Total: len(tasks)}
	var reuse protocolcli.RunReuse
	for _, task := range tasks {
		switch task.Status {
		case protocolcli.TaskStatusFailed:
			counts.Failed++
		case protocolcli.TaskStatusSkipped:
			counts.Skipped++
		default:
			counts.Succeeded++
		}
		if task.Reuse == protocolcli.TaskReuseLocalCache {
			reuse.LocalCache++
		}
	}
	doc := protocolcli.SessionFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		SessionID:       id,
		StartTime:       startTime,
		EndTime:         startTime,
		Commands:        commands,
		Run: protocolcli.RunSummary{
			Outcome:    protocolcli.RunOutcomeSuccess,
			Counts:     counts,
			Reuse:      reuse,
			DurationMs: 1000,
		},
		Tasks: tasks,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal session document: %v", err)
	}
	return string(data)
}

func digestTask(key, digest, status, reuse string) protocolcli.TaskRecord {
	project, name, _ := strings.Cut(key, ":")
	return protocolcli.TaskRecord{
		Identity: protocolcli.TaskIdentity{
			Key:     key,
			Scope:   protocolcli.TaskScopeProject,
			Project: protocolcli.ProjectIdentity{ID: project, Name: project},
			Task:    protocolcli.TaskRef{Name: name, Command: "test", Kind: name},
		},
		InputDigest: digest,
		Status:      status,
		Reuse:       reuse,
	}
}

// digestStore is three runs of one test task: red at one digest on a baseline
// run, the same digest replayed red on a later run, then green at a new digest
// and reused by a gate that also skipped a task.
func digestStore(t *testing.T) (string, string, string) {
	t.Helper()
	red := "sha256:" + strings.Repeat("aa", 32)
	green := "sha256:" + strings.Repeat("bb", 32)
	ws := t.TempDir()
	writeRecordedSession(t, ws, "20260917-080000-000001", digestSession(t, "20260917-080000-000001",
		"2026-09-17T08:00:00Z", []string{"test"}, []protocolcli.TaskRecord{
			digestTask("/tooling/cli:test~test", red, protocolcli.TaskStatusFailed, protocolcli.TaskReuseNone),
		}))
	writeRecordedSession(t, ws, "20260917-090000-000002", digestSession(t, "20260917-090000-000002",
		"2026-09-17T09:00:00Z", []string{"test"}, []protocolcli.TaskRecord{
			digestTask("/tooling/cli:test~test", red, protocolcli.TaskStatusFailed, protocolcli.TaskReuseNone),
			digestTask("/tooling/cli:test~test", green, protocolcli.TaskStatusSuccess, protocolcli.TaskReuseNone),
		}))
	writeRecordedSession(t, ws, "20260917-100000-000003", digestSession(t, "20260917-100000-000003",
		"2026-09-17T10:00:00Z", []string{"lint", "test", "build", "validate"}, []protocolcli.TaskRecord{
			digestTask("/tooling/cli:test~test", green, protocolcli.TaskStatusSuccess, protocolcli.TaskReuseLocalCache),
			digestTask("/tooling/cli:build~compile", "", protocolcli.TaskStatusSkipped, protocolcli.TaskReuseNone),
		}))
	return ws, red, green
}

func digestRows(t *testing.T, ws, since, commands string) []digestSummaryRow {
	t.Helper()
	out, err := sharedtest.CaptureStdout(t, func() error {
		return SessionsSummary(ws, since, commands, true, "json")
	})
	if err != nil {
		t.Fatalf("SessionsSummary(--by-digest): %v", err)
	}
	var rows []digestSummaryRow
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("--by-digest output is not a digest row array: %v (%s)", err, out)
	}
	if rows == nil {
		t.Fatalf("--by-digest decoded to a nil slice (%s): `.data[]` breaks on null", out)
	}
	return rows
}

// TestSessionsSummary_ByDigestGroupsRecordsAndSaysWhichReused is the grouping
// the issue names: how many records carry each digest, and which of them reused.
// Digests list in first-recorded order, and a record with no digest is never
// grouped under an empty one.
func TestSessionsSummary_ByDigestGroupsRecordsAndSaysWhichReused(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "task-input-digest", "summary-groups-records-by-digest")
	ws, red, green := digestStore(t)

	rows := digestRows(t, ws, "", "")
	if len(rows) != 2 || rows[0].InputDigest != red || rows[1].InputDigest != green {
		t.Fatalf("rows = %+v, want the red digest then the green one, and nothing for the skipped record", rows)
	}
	if got := rows[0]; got.Records != 2 || got.Executed != 2 || got.Reused != 0 || got.Failed != 2 {
		t.Errorf("red digest = %+v, want 2 records, both executed and failed", got)
	}
	if got := rows[1]; got.Records != 2 || got.Executed != 1 || got.Reused != 1 || got.Failed != 0 {
		t.Errorf("green digest = %+v, want 2 records, 1 executed, 1 reused", got)
	}
	if got := rows[1].Tasks; len(got) != 1 || got[0] != "/tooling/cli:test~test" {
		t.Errorf("green digest tasks = %v, want the one task keyed on it", got)
	}
	want := []digestOccurrence{
		{SessionID: "20260917-090000-000002", Task: "/tooling/cli:test~test", Status: "success", Reuse: "none"},
		{SessionID: "20260917-100000-000003", Task: "/tooling/cli:test~test", Status: "success", Reuse: "local-cache"},
	}
	if got := rows[1].Occurrences; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("green occurrences = %+v, want %+v", got, want)
	}

	// --command selects the sessions first, exactly as it does for session rows.
	gate := digestRows(t, ws, "", "lint,test,build,validate")
	if len(gate) != 1 || gate[0].InputDigest != green || gate[0].Records != 1 || gate[0].Reused != 1 {
		t.Errorf("gate rows = %+v, want only the green digest, reused once", gate)
	}
	if none := digestRows(t, ws, "2030-01-01T00:00:00Z", ""); len(none) != 0 {
		t.Errorf("rows past every session = %+v, want an empty array", none)
	}
}

// TestSessionsSummary_ByDigestTableCountsRecordsWithoutADigest keeps the records
// the grouping leaves out visible in the table, so their absence never reads as
// zero.
func TestSessionsSummary_ByDigestTableCountsRecordsWithoutADigest(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "task-input-digest", "summary-groups-records-by-digest")
	ws, _, _ := digestStore(t)
	out, err := sharedtest.CaptureStdout(t, func() error {
		return SessionsSummary(ws, "", "", true, "text")
	})
	if err != nil {
		t.Fatalf("SessionsSummary(--by-digest, text): %v", err)
	}
	for _, want := range []string{"DIGEST", "/tooling/cli:test~test", "2 digests over 4 records", "1 record without a digest"} {
		if !strings.Contains(out, want) {
			t.Errorf("table does not contain %q:\n%s", want, out)
		}
	}

	empty, err := sharedtest.CaptureStdout(t, func() error {
		return SessionsSummary(t.TempDir(), "", "", true, "text")
	})
	if err != nil {
		t.Fatalf("SessionsSummary(--by-digest) on an empty store: %v", err)
	}
	if !strings.Contains(empty, "No sessions match.") {
		t.Errorf("empty store output = %q, want it to say no sessions match", empty)
	}
}
