package workspace_state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/cli"
)

// The report store.
//
// A report is written ONCE, from a settled run, and named by the session it
// synthesizes — which is what lets a consumer bind the two without the
// set-difference over a directory listing cloud's CI runner had to guess with.
// These tests pin the three properties that makes it safe to write on every
// run: the name is the session id, the write is atomic, and a bad id is refused
// rather than resolved as a path.

func validReport(sessionID string) *cli.ReportFile {
	return &cli.ReportFile{
		ProtocolVersion: cli.ResultProtocolVersion,
		SessionID:       sessionID,
		StartTime:       "2026-08-07T09:30:00Z",
		EndTime:         "2026-08-07T09:30:02Z",
		Origin:          cli.ReportOriginCLI,
		EnforceCoverage: true,
		Run: cli.ReportRun{
			Outcome:    cli.RunOutcomeSuccess,
			ExitCode:   cli.ExitSuccess,
			Counts:     cli.RunCounts{Total: 1, Succeeded: 1},
			DurationMs: 2000,
		},
		Commands: []cli.ReportCommand{
			{Command: "build", Counts: cli.RunCounts{Total: 1, Succeeded: 1}, FreshWallMs: 700},
		},
		Jobs: []cli.ReportJob{
			{
				Key: "/proj:build~compile", Project: "/proj", Task: "build~compile",
				Command: "build", Outcome: cli.TaskStatusSuccess, Reuse: cli.TaskReuseNone,
				DurationMs: 700,
			},
		},
	}
}

// coverageReport is a recorded report carrying one test command's coverage, at
// the cadence and origin the replay lookup filters on.
func coverageReport(sessionID string, pct float64, enforced bool, origin string) *cli.ReportFile {
	report := validReport(sessionID)
	report.EnforceCoverage = enforced
	report.Origin = origin
	report.Git = &cli.ReportGit{Sha: "abcdef1234567890abcdef1234567890abcdef12"}
	report.Commands = []cli.ReportCommand{{
		Command:  "test",
		Counts:   cli.RunCounts{Total: 1, Succeeded: 1},
		Coverage: &cli.ReportCoverage{Percentage: pct, Granularity: cli.CoverageStatements, Enforced: enforced},
	}}
	return report
}

// TestReportStore_ReadsTheNewestUserReport: `putnami report` with no argument
// answers "what did my last run do", so the newest lookup passes over an
// agent's MCP-driven run — which an exact --session read still returns.
func TestReportStore_ReadsTheNewestUserReport(t *testing.T) {
	root := t.TempDir()
	store := NewReportStore(root)

	agentRun := validReport("20260807-094000-cccccc")
	agentRun.Origin = cli.ReportOriginMCP
	for _, report := range []*cli.ReportFile{
		validReport("20260807-090000-aaaaaa"),
		validReport("20260807-093000-bbbbbb"),
		agentRun,
	} {
		if err := store.Write(report); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	if ids := store.List(); len(ids) != 3 || ids[0] != "20260807-094000-cccccc" {
		t.Fatalf("List = %v, want three ids newest first", ids)
	}

	latest := store.Latest()
	if latest == nil || latest.Report.SessionID != "20260807-093000-bbbbbb" {
		t.Fatalf("Latest = %v, want the newest CLI-driven report", latest)
	}
	// The raw bytes travel with the parsed document: a consumer forwards the
	// recorded contract rather than this Go struct's re-serialization.
	if !strings.Contains(string(latest.Raw), `"sessionId": "20260807-093000-bbbbbb"`) {
		t.Errorf("Latest raw bytes are not the recorded document:\n%s", latest.Raw)
	}

	byID, err := store.Read("20260807-094000-cccccc")
	if err != nil || byID.Report.Origin != cli.ReportOriginMCP {
		t.Fatalf("Read of the agent's report = %v, %v, want it read back by name", byID, err)
	}
}

// TestReportStore_ReadFailsForWhatIsNotAReport: an absent, unparseable, or
// path-shaped id fails rather than resolving — the reader turns each into the
// message a caller sees, and none of them may reach outside the store.
func TestReportStore_ReadFailsForWhatIsNotAReport(t *testing.T) {
	root := t.TempDir()
	store := NewReportStore(root)
	if err := store.Write(validReport("20260807-090000-aaaaaa")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if _, err := store.Read("20260807-999999-zzzzzz"); err == nil {
		t.Error("reading an unrecorded session reported success")
	}
	for _, id := range []string{"", "..", "../escape", "nested/id"} {
		if _, err := store.Read(id); err == nil {
			t.Errorf("session id %q was accepted as a file name", id)
		}
	}

	if err := os.WriteFile(store.Path("20260807-093000-bbbbbb"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read("20260807-093000-bbbbbb"); err == nil {
		t.Error("reading an unparseable document reported success")
	}
	// One unreadable document must not hide the run behind it: it sorts first.
	latest := store.Latest()
	if latest == nil || latest.Report.SessionID != "20260807-090000-aaaaaa" {
		t.Fatalf("Latest = %v, want the parseable report behind the corrupted one", latest)
	}
}

// TestReportStore_LatestIsAbsentWithoutAStore: a worktree that never recorded a
// report is the ordinary case, not a failure.
func TestReportStore_LatestIsAbsentWithoutAStore(t *testing.T) {
	store := NewReportStore(t.TempDir())
	if got := store.List(); got != nil {
		t.Errorf("List of an absent store = %v, want nothing", got)
	}
	if got := store.Latest(); got != nil {
		t.Errorf("Latest of an absent store = %v, want nothing", got)
	}
}

// TestReportStore_WritesTheSessionsReport: the file is named by the session id
// and holds a contract-valid document a reader can bind to directly.
func TestReportStore_WritesTheSessionsReport(t *testing.T) {
	root := t.TempDir()
	store := NewReportStore(root)

	if err := store.Write(validReport("20260807-093000-ab12cd")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	want := filepath.Join(root, ".putnami", "reports", "20260807-093000-ab12cd.json")
	if got := store.Path("20260807-093000-ab12cd"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if violations := cli.ValidateDocument(cli.DocumentReportFile, data); len(violations) != 0 {
		t.Fatalf("recorded report violates the contract: %v\n%s", violations, data)
	}
	var parsed cli.ReportFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if parsed.SessionID != "20260807-093000-ab12cd" || parsed.Origin != cli.ReportOriginCLI {
		t.Errorf("recorded identity = %q / %q", parsed.SessionID, parsed.Origin)
	}

	// Nothing but reports lands in the directory: the write stages through a temp
	// file, and a leftover would be read by anything listing the store.
	entries, err := os.ReadDir(filepath.Dir(want))
	if err != nil {
		t.Fatalf("read reports dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "20260807-093000-ab12cd.json" {
		t.Errorf("reports dir = %v, want only the recorded report", entries)
	}
	// A report exists to be collected, so it is readable like the session
	// documents beside it rather than at the 0600 the staging file was opened at.
	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat report: %v", err)
	}
	// On Windows the permission bits reduce to a read-only attribute, and a
	// readable, writable file reports 0666.
	wantPerm := os.FileMode(0o644)
	if runtime.GOOS == "windows" {
		wantPerm = 0o666
	}
	if info.Mode().Perm() != wantPerm {
		t.Errorf("report mode = %v, want %v", info.Mode().Perm(), wantPerm)
	}
}

// TestReportStore_ReplacesInPlace: a rewritten report replaces the previous
// bytes wholesale rather than merging with them, so a re-run of the same session
// id cannot leave a document half from each.
func TestReportStore_ReplacesInPlace(t *testing.T) {
	root := t.TempDir()
	store := NewReportStore(root)

	first := validReport("20260807-093000-ab12cd")
	if err := store.Write(first); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	second := validReport("20260807-093000-ab12cd")
	second.Origin = cli.ReportOriginMCP
	second.Commands = nil
	second.Jobs = nil
	second.Run.Counts = cli.RunCounts{}
	if err := store.Write(second); err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}

	data, err := os.ReadFile(store.Path("20260807-093000-ab12cd"))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(data), `"origin": "mcp"`) || strings.Contains(string(data), "build~compile") {
		t.Errorf("report was not replaced wholesale:\n%s", data)
	}
}

// TestReportStore_RefusesAnIdThatIsNotAName: the file name comes out of the
// document, and a document is not a place to accept a path from.
func TestReportStore_RefusesAnIdThatIsNotAName(t *testing.T) {
	root := t.TempDir()
	store := NewReportStore(root)

	for _, id := range []string{"", ".", "..", "../escape", "nested/id", `back\slash`} {
		if err := store.Write(validReport(id)); err == nil {
			t.Errorf("session id %q was accepted as a file name", id)
		}
	}
	if _, err := os.Stat(store.Root()); !os.IsNotExist(err) {
		t.Errorf("a refused write created the store directory: %v", err)
	}
}

// TestReportStore_ReportsAStoreItCannotCreate: the store never swallows a
// failure of its own. Best-effort is the CALLER's policy — the engine drops the
// error because a missing report is a non-event — and a store that returned nil
// here would take that decision away from it.
func TestReportStore_ReportsAStoreItCannotCreate(t *testing.T) {
	root := t.TempDir()
	store := NewReportStore(root)
	if err := os.MkdirAll(filepath.Dir(store.Root()), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where the reports directory belongs.
	if err := os.WriteFile(store.Root(), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(validReport("20260807-093000-ab12cd")); err == nil {
		t.Error("a store that cannot be created reported success")
	}
}

// TestReportStore_NoReportIsANonEvent: the report is a projection, never an
// input, so a producer with nothing to write writes nothing and reports no
// error — the caller's best-effort call has nothing to swallow.
func TestReportStore_NoReportIsANonEvent(t *testing.T) {
	root := t.TempDir()
	store := NewReportStore(root)
	if err := store.Write(nil); err != nil {
		t.Errorf("writing no report failed: %v", err)
	}
	if _, err := os.Stat(store.Root()); !os.IsNotExist(err) {
		t.Errorf("writing no report created the store directory: %v", err)
	}
}

// --- retention ---------------------------------------------------------

// pruneFixture records count reports named so their reverse name sort is the
// write order, marks the cadence per report via enforced, and backdates every
// file past the grace window so the prune may consider all of them.
func pruneFixture(t *testing.T, rs *ReportStore, count int, enforced func(i int) bool) {
	t.Helper()
	for i := range count {
		report := validReport(fmt.Sprintf("20260807-%06d-abcdef", i))
		report.EnforceCoverage = enforced(i)
		if err := rs.Write(report); err != nil {
			t.Fatalf("write report %d: %v", i, err)
		}
	}
	aged := time.Now().Add(-2 * reportPruneGraceWindow)
	for _, id := range rs.List() {
		if err := os.Chtimes(rs.Path(id), aged, aged); err != nil {
			t.Fatalf("age report %s: %v", id, err)
		}
	}
}

// TestReportStore_PruneKeepsTheNewestReports: past the count cap the oldest
// fast-cadence reports go, the newest stay, and the survivors are exactly the
// budget.
func TestReportStore_PruneKeepsTheNewestReports(t *testing.T) {
	rs := NewReportStore(t.TempDir())
	pruneFixture(t, rs, reportRetainCount+25, func(int) bool { return false })

	rs.Prune()

	ids := rs.List()
	if len(ids) != reportRetainCount {
		t.Fatalf("prune kept %d reports, want the cap %d", len(ids), reportRetainCount)
	}
	if ids[len(ids)-1] != fmt.Sprintf("20260807-%06d-abcdef", 25) {
		t.Errorf("oldest survivor = %s, want the 25 oldest gone", ids[len(ids)-1])
	}
}

// TestReportStore_PruneSparesYoungValidationReports: an enforceCoverage report
// past the count cap survives while inside its retention days — it is the
// figure the replay display and a longitudinal consumer file under a commit.
func TestReportStore_PruneSparesYoungValidationReports(t *testing.T) {
	rs := NewReportStore(t.TempDir())
	// The OLDEST five are validation reports; every one is past the count cap.
	pruneFixture(t, rs, reportRetainCount+25, func(i int) bool { return i < 5 })

	rs.Prune()

	ids := rs.List()
	if len(ids) != reportRetainCount+5 {
		t.Fatalf("prune kept %d reports, want the cap plus the 5 validation reports", len(ids))
	}
	for i := range 5 {
		id := fmt.Sprintf("20260807-%06d-abcdef", i)
		if _, err := os.Stat(rs.Path(id)); err != nil {
			t.Errorf("validation report %s was pruned: %v", id, err)
		}
	}
}

// TestReportStore_PruneDropsExpiredValidationReports: past the retention days a
// validation report is count-pruned like the rest — "keep longest" is not
// "keep forever".
func TestReportStore_PruneDropsExpiredValidationReports(t *testing.T) {
	rs := NewReportStore(t.TempDir())
	pruneFixture(t, rs, reportRetainCount+1, func(i int) bool { return i == 0 })
	expired := time.Now().Add(-(reportValidationRetainDays + 1) * 24 * time.Hour)
	old := fmt.Sprintf("20260807-%06d-abcdef", 0)
	if err := os.Chtimes(rs.Path(old), expired, expired); err != nil {
		t.Fatalf("expire report: %v", err)
	}

	rs.Prune()

	if _, err := os.Stat(rs.Path(old)); !os.IsNotExist(err) {
		t.Errorf("expired validation report survived the cap: %v", err)
	}
}

// TestReportStore_PruneSparesTheGraceWindow: a file modified inside the grace
// window is untouchable whatever its name sorts as — the prune rides a write
// and must never race a concurrent writer.
func TestReportStore_PruneSparesTheGraceWindow(t *testing.T) {
	rs := NewReportStore(t.TempDir())
	pruneFixture(t, rs, reportRetainCount+1, func(int) bool { return false })
	fresh := fmt.Sprintf("20260807-%06d-abcdef", 0)
	now := time.Now()
	if err := os.Chtimes(rs.Path(fresh), now, now); err != nil {
		t.Fatalf("freshen report: %v", err)
	}

	rs.Prune()

	if _, err := os.Stat(rs.Path(fresh)); err != nil {
		t.Errorf("a file inside the grace window was pruned: %v", err)
	}
}

// TestReportStore_PruneWithinBudgetIsFree: a store inside its budget deletes
// nothing and reads no file bodies (proven by planting an unreadable body the
// prune would otherwise choke on — best-effort or not, it must not even look).
func TestReportStore_PruneWithinBudgetIsFree(t *testing.T) {
	rs := NewReportStore(t.TempDir())
	pruneFixture(t, rs, 3, func(int) bool { return false })

	rs.Prune()

	if got := len(rs.List()); got != 3 {
		t.Errorf("prune inside the budget removed reports: %d left of 3", got)
	}
}

// TestReportStore_PruneDoesNotExemptAgentValidationRuns: the 365-day exemption
// is for the USER's validation history; an MCP-origin validation report is
// count-pruned like any other report.
func TestReportStore_PruneDoesNotExemptAgentValidationRuns(t *testing.T) {
	rs := NewReportStore(t.TempDir())
	pruneFixture(t, rs, reportRetainCount+1, func(int) bool { return false })
	agent := fmt.Sprintf("20260807-%06d-abcdef", 0)
	report := validReport(agent)
	report.EnforceCoverage = true
	report.Origin = cli.ReportOriginMCP
	if err := rs.Write(report); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-2 * reportPruneGraceWindow)
	if err := os.Chtimes(rs.Path(agent), aged, aged); err != nil {
		t.Fatal(err)
	}

	rs.Prune()

	if _, err := os.Stat(rs.Path(agent)); !os.IsNotExist(err) {
		t.Errorf("an agent's validation report survived the count cap: %v", err)
	}
}

// TestReportStore_LatestScansPastAnAgentBurst: the newest-first lookups carry
// no scan cap, because any fixed window can be filled by a burst of MCP-origin
// reports — a busy agent afternoon — and would turn "no run report recorded"
// into a wrong answer while the user's report sits just past it.
func TestReportStore_LatestScansPastAnAgentBurst(t *testing.T) {
	rs := NewReportStore(t.TempDir())
	user := coverageReport("20260807-000000-user00", 77.5, true, cli.ReportOriginCLI)
	if err := rs.Write(user); err != nil {
		t.Fatal(err)
	}
	for i := range 300 {
		agent := validReport(fmt.Sprintf("20260807-%06d-agent0", i+1))
		agent.Origin = cli.ReportOriginMCP
		if err := rs.Write(agent); err != nil {
			t.Fatal(err)
		}
	}

	recorded := rs.Latest()
	if recorded == nil || recorded.Report.SessionID != "20260807-000000-user00" {
		t.Fatalf("Latest = %v, want the user's report behind the 300-report agent burst", recorded)
	}
}
