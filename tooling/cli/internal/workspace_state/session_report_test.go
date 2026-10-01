package workspace_state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestSessionFinalizeV2WithCoverageUsesCompletedValidationRegardlessOfVerdict(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	project := &workspace.Project{ID: "/app", Name: "app"}
	job := &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/typescript"},
		JobDef:    &extension.JobDefinition{Name: "test"},
	}
	results := map[string]*jobs.JobResult{
		job.Key(): {
			Status: string(jobs.TaskStatusSuccess),
			Data: extension.ParamMap{"coverageSummary": extension.ParamMap{
				"percentage":  0.0,
				"granularity": runtimeproto.CoverageLines,
				"covered":     0,
				"total":       10,
			}},
		},
	}
	params := extension.ParamMap{"enforce-coverage": true, "unrelated": "kept"}
	wantParams := extension.ParamMap{"enforce-coverage": true, "unrelated": "kept"}

	validation := sessionForReport(t, store, "20260806-100000-000001")
	if err := validation.FinalizeV2WithCoverage(reportSessionFile(protocolcli.RunOutcomeSuccess), "", "", params,
		[]*jobs.ScheduledJob{job}, results, &jobs.SessionResult{},
		&jobs.JobContextVersion{Suffix: "abc1234", Branch: "main"}); err != nil {
		t.Fatalf("FinalizeV2WithCoverage: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(validation.Dir(), reportFileName))
	if err != nil {
		t.Fatalf("read validation report: %v", err)
	}
	var report SessionReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Coverage) != 1 || report.Coverage[0].Summary.Total != 10 || report.Coverage[0].Summary.Percentage != 0 {
		t.Fatalf("measured zero coverage was treated as absent: %+v", report.Coverage)
	}
	if report.Coverage[0].Provider != "@putnami/typescript" || report.Source.Revision != "abc1234" {
		t.Fatalf("coverage provenance = %+v", report)
	}
	if !reflect.DeepEqual(params, wantParams) {
		t.Fatalf("reporting mutated command params: got %#v want %#v", params, wantParams)
	}

	ordinary := sessionForReport(t, store, "20260806-100000-000002")
	if err := ordinary.FinalizeV2WithCoverage(reportSessionFile(protocolcli.RunOutcomeSuccess), "", "", nil,
		[]*jobs.ScheduledJob{job}, results, &jobs.SessionResult{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ordinary.Dir(), reportFileName)); !os.IsNotExist(err) {
		t.Fatalf("ordinary cadence report stat error = %v, want not-exist", err)
	}

	results[job.Key()] = &jobs.JobResult{
		Status: string(jobs.TaskStatusFailed),
		Data: extension.ParamMap{"coverageSummary": extension.ParamMap{
			"percentage":  25.0,
			"granularity": runtimeproto.CoverageLines,
			"covered":     10,
			"total":       40,
		}},
	}
	failed := sessionForReport(t, store, "20260806-100000-000003")
	failedRun := &jobs.SessionResult{Status: jobs.TaskCounts{Failed: 1}}
	if err := failed.FinalizeV2WithCoverage(reportSessionFile(protocolcli.RunOutcomeFailure), "", "", params,
		[]*jobs.ScheduledJob{job}, results, failedRun,
		&jobs.JobContextVersion{Suffix: "failedrev", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	got := store.LatestValidationCoverage([]string{"test"}, []string{"/app"})
	if got["/app"].Entry.Summary.Percentage != 25 || got["/app"].Source.Revision != "failedrev" {
		t.Fatalf("failed validation was not the newest replay: %+v", got["/app"])
	}

	aborted := sessionForReport(t, store, "20260806-100000-000004")
	abortedRun := &jobs.SessionResult{
		Status:    jobs.TaskCounts{Failed: 1},
		Aborted:   true,
		AbortedBy: jobs.AbortUser,
	}
	if err := aborted.FinalizeV2WithCoverage(reportSessionFile(protocolcli.RunOutcomeAborted), "", "", params,
		[]*jobs.ScheduledJob{job}, results, abortedRun,
		&jobs.JobContextVersion{Suffix: "abortedrev", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(aborted.Dir(), reportFileName)); !os.IsNotExist(err) {
		t.Fatalf("aborted validation report stat error = %v, want not-exist", err)
	}

	// Even if an external writer places a structurally valid report beside an
	// aborted session, the reader must not treat incomplete work as replayable.
	if err := aborted.WriteReport(reportWithCoverage("abortedrev",
		coverageEntry("/app", "app", 99, runtimeproto.CoverageLines, 99, 100))); err != nil {
		t.Fatal(err)
	}
	got = store.LatestValidationCoverage([]string{"test"}, []string{"/app"})
	if got["/app"].Entry.Summary.Percentage != 25 || got["/app"].Source.Revision != "failedrev" {
		t.Fatalf("aborted validation displaced completed failure: %+v", got["/app"])
	}
}

func TestSessionWriteReportPreservesMeasuredZeroAndPublishesAtomically(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	session := recordedSessionForReport(t, store, "20260806-100000-000001")
	report := reportWithCoverage("rev-zero", coverageEntry("/zero", "zero", 0, runtimeproto.CoverageLines, 0, 12))

	if err := session.WriteReport(report); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(session.Dir(), reportFileName))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	var got SessionReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse report.json: %v", err)
	}
	if got.ReportVersion != SessionReportVersion || got.SessionID != session.ID {
		t.Fatalf("report identity = version %d session %q", got.ReportVersion, got.SessionID)
	}
	if len(got.Coverage) != 1 || got.Coverage[0].Summary.Percentage != 0 || got.Coverage[0].Summary.Total != 12 {
		t.Fatalf("zero coverage was lost or rewritten: %+v", got.Coverage)
	}
	if leftovers, err := filepath.Glob(filepath.Join(session.Dir(), ".report-*.tmp")); err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary report files = %v, err=%v", leftovers, err)
	}
}

func TestSessionStoreLatestValidationCoverageScansPastOrdinaryRunsPerProject(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	old := recordedSessionForReport(t, store, "20260806-100000-000001")
	if err := old.WriteReport(reportWithCoverage("oldrev",
		coverageEntry("/a", "a", 60, runtimeproto.CoverageStatements, 60, 100),
		coverageEntry("/b", "b", 70, runtimeproto.CoverageLines, 70, 100),
	)); err != nil {
		t.Fatalf("write old report: %v", err)
	}

	newer := recordedSessionForReport(t, store, "20260806-100000-000002")
	if err := newer.WriteReport(reportWithCoverage("newrev",
		coverageEntry("/a", "a", 85, runtimeproto.CoverageStatements, 85, 100),
	)); err != nil {
		t.Fatalf("write newer report: %v", err)
	}

	// The newest ordinary test run collected no validation coverage and thus has
	// no report. It must not erase either project's last measurement.
	_ = recordedSessionForReport(t, store, "20260806-100000-000003")

	got := store.LatestValidationCoverage([]string{"test"}, []string{"/a", "/b"})
	if len(got) != 2 {
		t.Fatalf("coverage projects = %v, want /a and /b", got)
	}
	if got["/a"].Entry.Summary.Percentage != 85 || got["/a"].Source.Revision != "newrev" {
		t.Errorf("/a replay = %+v, want newest report", got["/a"])
	}
	if got["/b"].Entry.Summary.Percentage != 70 || got["/b"].Source.Revision != "oldrev" {
		t.Errorf("/b replay = %+v, want older per-project report", got["/b"])
	}
	if got := store.LatestValidationCoverage([]string{"build"}, []string{"/a"}); len(got) != 0 {
		t.Fatalf("non-test command loaded coverage replay: %+v", got)
	}
}

func TestSessionStoreLatestValidationCoverageRejectsUntrustedNewerReport(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	trusted := recordedSessionForReport(t, store, "20260806-100000-000001")
	if err := trusted.WriteReport(reportWithCoverage("trusted",
		coverageEntry("/a", "a", 75, runtimeproto.CoverageStatements, 75, 100),
	)); err != nil {
		t.Fatal(err)
	}

	untrusted := recordedSessionForReport(t, store, "20260806-100000-000002")
	report := reportWithCoverage("untrusted",
		coverageEntry("/a", "a", 99, runtimeproto.CoverageStatements, 99, 100),
	)
	report.ReportVersion = SessionReportVersion
	report.SessionID = "another-session"
	report.GeneratedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(untrusted.Dir(), reportFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	incomplete := sessionForReport(t, store, "20260806-100000-000003")
	incompleteMeta := reportSessionFile(protocolcli.RunOutcomeSuccess)
	incompleteMeta.ProtocolVersion = protocolcli.ResultProtocolVersion
	incompleteMeta.SessionID = incomplete.ID
	incompleteMeta.StartTime = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	data, err = json.Marshal(incompleteMeta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomplete.Dir(), "session.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := incomplete.WriteReport(reportWithCoverage("incomplete",
		coverageEntry("/a", "a", 100, runtimeproto.CoverageStatements, 100, 100))); err != nil {
		t.Fatal(err)
	}

	got := store.LatestValidationCoverage([]string{"test"}, []string{"/a"})
	if got["/a"].Source.Revision != "trusted" || got["/a"].Entry.Summary.Percentage != 75 {
		t.Fatalf("untrusted newer report won replay selection: %+v", got["/a"])
	}
}

func recordedSessionForReport(t *testing.T, store *SessionStore, id string) *Session {
	t.Helper()
	session := sessionForReport(t, store, id)
	if err := session.FinalizeV2(reportSessionFile(protocolcli.RunOutcomeSuccess), "", ""); err != nil {
		t.Fatalf("FinalizeV2: %v", err)
	}
	return session
}

func sessionForReport(t *testing.T, store *SessionStore, id string) *Session {
	t.Helper()
	dir := filepath.Join(store.Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Session{ID: id, StartTime: time.Now().Add(-time.Second), dir: dir}
}

func reportSessionFile(outcome string) *protocolcli.SessionFile {
	run := protocolcli.RunSummary{Outcome: outcome}
	switch outcome {
	case protocolcli.RunOutcomeFailure:
		run.ExitCode = protocolcli.ExitFailure
		run.Counts = protocolcli.RunCounts{Total: 1, Failed: 1}
	case protocolcli.RunOutcomeAborted:
		run.ExitCode = protocolcli.ExitSignal
		run.AbortedBy = protocolcli.AbortedByUser
		run.Counts = protocolcli.RunCounts{Total: 1, Failed: 1}
	default:
		run.ExitCode = protocolcli.ExitSuccess
		run.Counts = protocolcli.RunCounts{Total: 1, Succeeded: 1}
	}
	return &protocolcli.SessionFile{
		Commands: []string{"test"},
		Run:      run,
	}
}

func reportWithCoverage(revision string, entries ...SessionCoverageEntry) *SessionReport {
	return &SessionReport{
		Source:   SessionReportSource{Cadence: ReportCadenceValidation, Revision: revision},
		Coverage: entries,
	}
}

func coverageEntry(projectID, projectName string, percentage float64, granularity string, covered, total int) SessionCoverageEntry {
	return SessionCoverageEntry{
		Project:  SessionReportProject{ID: projectID, Name: projectName},
		TaskKey:  projectID + ":test",
		Command:  "test",
		Provider: "@putnami/test",
		Summary: runtimeproto.CoverageSummary{
			Percentage:  percentage,
			Granularity: granularity,
			Covered:     covered,
			Total:       total,
		},
	}
}
