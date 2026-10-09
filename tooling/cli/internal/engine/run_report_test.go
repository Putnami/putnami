package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// The report seam: the engine writes the run's contracted bilan
// beside the session it synthesizes, from the SETTLED session, best-effort.

// settledSession creates and finalizes a session under root, so the report is
// written from the same interval the recorded session states.
func settledSession(t *testing.T, root string) *workspace_state.Session {
	t.Helper()
	session, err := workspace_state.NewSessionStore(root).Create()
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(session.Close)
	if err := session.FinalizeV2(&protocolcli.SessionFile{}, "", ""); err != nil {
		t.Fatalf("finalize session: %v", err)
	}
	return session
}

// reportedRun is a one-task run: enough to produce a document, and the job list
// names the task so a reader can tell the report describes this run.
func reportedRun() machine.Run {
	return machine.Run{
		Session: &jobs.SessionResult{Tasks: 1, Status: jobs.TaskCounts{Succeeded: 1}},
		Tasks: []machine.Task{
			{
				Identity: protocolcli.TaskIdentity{
					Key:      "/proj:build~compile",
					Scope:    protocolcli.TaskScopeProject,
					Project:  protocolcli.ProjectIdentity{ID: "/proj", Name: "proj"},
					Task:     protocolcli.TaskRef{Name: "build~compile", Command: "build", Kind: "compile"},
					Provider: protocolcli.ProviderIdentity{Extension: "@putnami/go"},
				},
				Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess},
			},
		},
	}
}

// TestWriteRunReport_RecordsAContractValidReport is the seam end to end: a
// settled session produces a report named after it, valid against the contract,
// stating the commit it was produced at.
func TestWriteRunReport_RecordsAContractValidReport(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initCLISelectionGitRepo(t, root)
	// The snapshot is captured BEFORE the run, which is where the engine takes it
	// (Request.VersionSnapshot): the report states the tree the run observed, not
	// the one its own recording left behind.
	snapshot, snapshotErr := git.TreeState(root)
	if snapshotErr != nil {
		t.Fatalf("capture the tree state: %v", snapshotErr)
	}
	session := settledSession(t, root)

	req := &Request{WorkspaceRoot: root, VersionSnapshot: snapshot}
	req.Global.Baseline = "main"
	if err := writeRunReport(root, session, reportedRun(), req, nil, nil); err != nil {
		t.Fatalf("writeRunReport failed: %v", err)
	}

	path := workspace_state.NewReportStore(root).Path(session.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentReportFile, data); len(violations) != 0 {
		t.Fatalf("recorded report violates the contract: %v\n%s", violations, data)
	}

	var report protocolcli.ReportFile
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if report.SessionID != session.ID {
		t.Errorf("sessionId = %q, want the settled session %q", report.SessionID, session.ID)
	}
	if report.Origin != protocolcli.ReportOriginCLI {
		t.Errorf("origin = %q, want a command-line run", report.Origin)
	}
	if report.StartTime == "" || report.EndTime == "" {
		t.Errorf("report states no settled interval: %q → %q", report.StartTime, report.EndTime)
	}
	if report.Git == nil || len(report.Git.Sha) != 40 || report.Git.Branch != "main" || report.Git.Baseline != "main" {
		t.Fatalf("git block = %+v, want the full object id of the checked-out commit", report.Git)
	}
	// Dirtiness is a determination, and it was made: absent would mean the
	// producer could not tell whether the tree matched the sha.
	if report.Git.Dirty == nil {
		t.Error("dirty is absent although the run captured the worktree state")
	} else if *report.Git.Dirty {
		t.Error("dirty = true on a clean checkout")
	}
}

// TestWriteRunReport_AdapterRunsAreRecordedWithTheirOrigin: an ephemeral run
// stays out of the `latest` rotation but is still reported — the report carries
// the origin field session metadata never had, so a reader filters instead of
// guessing.
func TestWriteRunReport_AdapterRunsAreRecordedWithTheirOrigin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	session := settledSession(t, root)

	req := &Request{WorkspaceRoot: root, EphemeralSession: true}
	if err := writeRunReport(root, session, reportedRun(), req, nil, nil); err != nil {
		t.Fatalf("writeRunReport failed: %v", err)
	}

	data, err := os.ReadFile(workspace_state.NewReportStore(root).Path(session.ID))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report protocolcli.ReportFile
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if report.Origin != protocolcli.ReportOriginMCP {
		t.Errorf("origin = %q, want %q for an adapter run", report.Origin, protocolcli.ReportOriginMCP)
	}
	// Outside a worktree the block is absent rather than partially guessed.
	if report.Git != nil {
		t.Errorf("git block = %+v outside a worktree", report.Git)
	}
}

// TestWriteRunReport_WithoutASessionIsANonEvent: a run that recorded no session
// has nothing to synthesize, and the report is a projection — never an input —
// so it fails nothing.
func TestWriteRunReport_WithoutASessionIsANonEvent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := writeRunReport(root, nil, reportedRun(), &Request{WorkspaceRoot: root}, nil, nil); err != nil {
		t.Errorf("a run with no session failed the report: %v", err)
	}
	if _, err := os.Stat(workspace_state.NewReportStore(root).Root()); !os.IsNotExist(err) {
		t.Errorf("a run with no session created the report store: %v", err)
	}
}

// TestReportEnforceCoverage_ReadsTheCadence pins the marker that lets a
// longitudinal consumer compare like with like instead of reading a fast run's
// absent coverage as a regression. buildCommandParams produces the bool for
// `--enforce-coverage` and the string for `--enforce-coverage=true`.
func TestReportEnforceCoverage_ReadsTheCadence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		param any
		want  bool
	}{
		{nil, false},
		{true, true},
		{false, false},
		{"true", true},
		{"1", true},
		{"no", false},
		{1, false},
	} {
		if got := reportEnforceCoverage(tc.param); got != tc.want {
			t.Errorf("reportEnforceCoverage(%v) = %v, want %v", tc.param, got, tc.want)
		}
	}
}

// TestReportFix_ReadsTheExplicitFlag pins the values the report states:
// buildCommandParams produces the bool for `--fix` and `--no-fix` and the string
// for `--fix=<value>`. Only "true" and "false" are read; any other value, and a
// run without the flag, state nothing.
func TestReportFix_ReadsTheExplicitFlag(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	for _, tc := range []struct {
		param any
		want  *bool
	}{
		{nil, nil},
		{true, &yes},
		{false, &no},
		{"true", &yes},
		{"false", &no},
		{"0", nil},
		{"1", nil},
		{"no", nil},
		{"FALSE", nil},
		{"", nil},
		{0, nil},
	} {
		got := reportFix(tc.param)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("reportFix(%#v) = %v, want absent", tc.param, *got)
		case tc.want != nil && got == nil:
			t.Errorf("reportFix(%#v) is absent, want %v", tc.param, *tc.want)
		case tc.want != nil && *got != *tc.want:
			t.Errorf("reportFix(%#v) = %v, want %v", tc.param, *got, *tc.want)
		}
	}
}

// TestWriteRunReport_RecordsTheExplicitFixFlag: the run's `fix` command param
// reaches the recorded report as a boolean, and a run without the flag records
// no member at all.
func TestWriteRunReport_RecordsTheExplicitFixFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		params extension.ParamMap
		// want is the report's fix member as JSON, empty when the member is absent.
		want string
	}{
		{name: "--fix=false", params: extension.ParamMap{"fix": "false"}, want: "false"},
		{name: "--no-fix", params: extension.ParamMap{"fix": false}, want: "false"},
		{name: "--fix", params: extension.ParamMap{"fix": true}, want: "true"},
		{name: "--fix=0", params: extension.ParamMap{"fix": "0"}},
		{name: "without the flag", params: extension.ParamMap{"enforce-coverage": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			session := settledSession(t, root)
			req := &Request{WorkspaceRoot: root, CommandParams: tc.params}
			if err := writeRunReport(root, session, reportedRun(), req, nil, nil); err != nil {
				t.Fatalf("writeRunReport failed: %v", err)
			}
			data, err := os.ReadFile(workspace_state.NewReportStore(root).Path(session.ID))
			if err != nil {
				t.Fatalf("read report: %v", err)
			}
			if violations := protocolcli.ValidateDocument(protocolcli.DocumentReportFile, data); len(violations) != 0 {
				t.Fatalf("recorded report violates the contract: %v\n%s", violations, data)
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(data, &members); err != nil {
				t.Fatalf("parse report: %v", err)
			}
			got, present := members["fix"]
			if tc.want == "" {
				if present {
					t.Errorf("fix = %s, want the member absent: %s", got, data)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("fix = %s, want %s: %s", got, tc.want, data)
			}
		})
	}
}

// TestReportGit_DetachedHeadNamesNoBranch: "HEAD" is what a detached checkout
// reports and is not a branch, so the member is absent — while the sha, the
// thing a consumer files the report under, is still stated.
func TestReportGit_DetachedHeadNamesNoBranch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initCLISelectionGitRepo(t, root)
	sha, err := git.HeadSHA(root)
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}
	runCLISelectionGit(t, root, "checkout", "--detach", sha)

	detached, detachedErr := git.TreeState(root)
	if detachedErr != nil {
		t.Fatalf("capture the tree state: %v", detachedErr)
	}
	state := reportGit(root, &Request{WorkspaceRoot: root, VersionSnapshot: detached})
	if state == nil {
		t.Fatal("no git block inside a worktree")
	}
	if state.Sha != sha {
		t.Errorf("sha = %q, want %q", state.Sha, sha)
	}
	if state.Branch != "" {
		t.Errorf("branch = %q, want none on a detached HEAD", state.Branch)
	}
}

// TestReportGit_MovedHeadDropsTheBlock: the full object id is read after the
// run, so it is checked against the pre-run snapshot — a job or hook that
// commits during the run moves HEAD, and a report filed under the RESULTING
// commit would bind measurements to code they never ran against. The block is
// then absent, never wrong.
func TestReportGit_MovedHeadDropsTheBlock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	initCLISelectionGitRepo(t, root)
	snapshot, snapshotErr := git.TreeState(root)
	if snapshotErr != nil {
		t.Fatalf("capture the tree state: %v", snapshotErr)
	}
	if snapshot == nil || snapshot.SHA == "" {
		t.Fatal("no pre-run snapshot")
	}

	// The "run" commits: HEAD after the run is not the commit the snapshot saw.
	if err := os.WriteFile(filepath.Join(root, "made-by-a-job.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLISelectionGit(t, root, "add", ".")
	runCLISelectionGit(t, root, "commit", "-m", "made by a job")

	if state := reportGit(root, &Request{WorkspaceRoot: root, VersionSnapshot: snapshot}); state != nil {
		t.Errorf("a moved HEAD still produced a git block: %+v", state)
	}

	// An unmoved HEAD keeps the block, sha agreeing with the snapshot.
	fresh, freshErr := git.TreeState(root)
	if freshErr != nil {
		t.Fatalf("capture the tree state: %v", freshErr)
	}
	state := reportGit(root, &Request{WorkspaceRoot: root, VersionSnapshot: fresh})
	if state == nil {
		t.Fatal("an unmoved HEAD lost its git block")
	}
	if !strings.HasPrefix(state.Sha, fresh.SHA) {
		t.Errorf("sha = %q does not extend the snapshot's %q", state.Sha, fresh.SHA)
	}
}
