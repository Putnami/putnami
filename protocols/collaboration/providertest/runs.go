package providertest

import (
	"slices"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

// RunMissionRuns runs the missions v2 run scenarios, each against a fresh
// target and in a mission of its own. Every call names missions version 2,
// whatever the target's MissionsVersion. The target answers sync, ask and
// reply under versions 1 and 2, since a version 2 provider also serves the
// workspaces bound to version 1, and start, report and runs under version 2. Every scenario calls as one caller,
// so none covers a report refused as run.denied: a provider with more than
// one caller tests that itself.
func RunMissionRuns(t *testing.T, newTarget func(t *testing.T) Target) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(t *testing.T, m *missionRun)
	}{
		{"a repeated start replays the run", runsStartReplay},
		{"a key started again with other content is a conflict", runsStartMismatch},
		{"a succeeded report ends the run with its result", runsReportSucceeded},
		{"a failed or canceled report ends the run without a result", runsReportFailedOrCanceled},
		{"another report on an ended run is a conflict", runsReportClosed},
		{"a report on an unknown run is not found", runsReportUnknown},
		{"a result of another kind is refused and the run stays running", runsReportKindMismatch},
		{"runs lists a mission's runs, the latest start first", runsList},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			target := newTarget(t)
			target.MissionsVersion = 2
			required := []collab.OperationKey{}
			for _, operation := range []string{collab.OperationSync, collab.OperationAsk, collab.OperationReply} {
				required = append(required, collab.OperationKey{Contract: collab.ContractMissions, Version: 1, Operation: operation},
					collab.OperationKey{Contract: collab.ContractMissions, Version: 2, Operation: operation})
			}
			for _, operation := range []string{collab.OperationStart, collab.OperationReport, collab.OperationRuns} {
				required = append(required, collab.OperationKey{Contract: collab.ContractMissions, Version: 2, Operation: operation})
			}
			for _, key := range required {
				if _, ok := target.Handlers[key]; !ok {
					t.Fatalf("a missions v2 target answers %s", key)
				}
			}
			run := Unique(t)
			scenario.run(t, &missionRun{
				Session: Open(t, target),
				run:     run,
				mission: collab.MissionName{Repository: "providertest/runs", Key: "pt-" + run, Title: "Scenario mission " + run},
				session: collab.SessionRef{Engine: "providertest", ID: "pt-" + run},
				start:   time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
			})
		})
	}
}

// startKey is an idempotency key no other run uses.
func (m *missionRun) startKey(suffix string) string { return "pt-start-" + m.run + "-" + suffix }

// reviewInput is the input of a review run of the scenario.
func (m *missionRun) reviewInput() collab.RunInput {
	return collab.RunInput{Review: &collab.ReviewRunInput{
		Change: collab.Change{Base: "main", Head: "pt-" + m.run}, Commit: "0123abc",
	}}
}

// reviewResult is a result of a review run, with one finding.
func reviewResult() *collab.RunResult {
	return &collab.RunResult{Review: &collab.ReviewRunResult{
		Commit: "0123abc", Verdict: collab.ReviewVerdictRequestChanges,
		Findings: []collab.Finding{{Severity: collab.FindingSeverityHigh, Title: "Unchecked error", File: "main.go", Line: 12, Body: "Check it."}},
	}}
}

// startDocument is a start request of the scenario's mission.
func (m *missionRun) startDocument(key string, kind collab.RunKind, input collab.RunInput) collab.MissionsStartInput {
	return collab.MissionsStartInput{Mission: m.mission, Kind: kind, Input: input, Session: &m.session, IdempotencyKey: key}
}

// startRun sends a start and checks that its run holds what the start named.
func (m *missionRun) startRun(in collab.MissionsStartInput) collab.MissionsRunResult {
	m.t.Helper()
	var result collab.MissionsRunResult
	m.OK(collab.ContractMissions, collab.OperationStart, encode(in), &result)
	run := result.Run
	if run.Mission.Repository != in.Mission.Repository || run.Mission.Key != in.Mission.Key || run.Kind != in.Kind ||
		encode(run.Input) != encode(in.Input) || (in.Session != nil && (run.Session == nil || *run.Session != *in.Session)) {
		m.t.Fatalf("a start of %s answered the run %s", encode(in), encode(run))
	}
	return result
}

// begin starts a run of kind with its input under a key of its own.
func (m *missionRun) begin(suffix string, kind collab.RunKind, input collab.RunInput) collab.Run {
	m.t.Helper()
	result := m.startRun(m.startDocument(m.startKey(suffix), kind, input))
	if result.Replayed || result.Run.State != collab.RunStateRunning || result.Run.EndedAt != "" || result.Run.Result != nil {
		m.t.Fatalf("a first start answered %s; want a running run, not replayed", encode(result))
	}
	return result.Run
}

// report sends a report and checks that its run is the run reported, in the
// reported state.
func (m *missionRun) report(in collab.MissionsReportInput) collab.MissionsRunResult {
	m.t.Helper()
	var result collab.MissionsRunResult
	m.OK(collab.ContractMissions, collab.OperationReport, encode(in), &result)
	if result.Run.ID != in.Run || result.Run.State != in.State {
		m.t.Fatalf("a report of %s answered the run %s", encode(in), encode(result.Run))
	}
	return result
}

// runs reads one page of a mission's runs.
func (m *missionRun) runs(in collab.MissionsRunsInput) collab.MissionsRunsResult {
	m.t.Helper()
	var result collab.MissionsRunsResult
	m.OK(collab.ContractMissions, collab.OperationRuns, encode(in), &result)
	if in.Page != nil && in.Page.Size > 0 && len(result.Items) > in.Page.Size {
		m.t.Fatalf("a runs page of %d answered %d runs", in.Page.Size, len(result.Items))
	}
	for _, run := range result.Items {
		if run.Mission.Repository != in.Mission.Repository || run.Mission.Key != in.Mission.Key {
			m.t.Fatalf("the runs of mission %q listed %s, a run of mission %q", in.Mission.Key, run.ID, run.Mission.Key)
		}
		if in.Kind != "" && run.Kind != in.Kind {
			m.t.Fatalf("the %s runs listed %s, a %s run", in.Kind, run.ID, run.Kind)
		}
	}
	return result
}

// listed reads every page of a mission's runs, one run per page.
func (m *missionRun) listed(mission collab.MissionName, kind collab.RunKind) []collab.Run {
	m.t.Helper()
	var all []collab.Run
	page := &collab.PageRequest{Size: 1}
	for pages := 0; ; pages++ {
		if pages > 50 {
			m.t.Fatalf("the runs traversal did not end after %d pages", pages)
		}
		result := m.runs(collab.MissionsRunsInput{Mission: mission, Kind: kind, Page: page})
		all = append(all, result.Items...)
		if result.Page.Next == "" {
			return all
		}
		page = &collab.PageRequest{Size: 1, Cursor: result.Page.Next}
	}
}

// find returns the run of a mission with id, as runs lists it.
func (m *missionRun) find(id string) collab.Run {
	m.t.Helper()
	for _, run := range m.listed(m.mission, "") {
		if run.ID == id {
			return run
		}
	}
	m.t.Fatalf("the runs of the mission lack run %s", id)
	return collab.Run{}
}

func runsStartReplay(t *testing.T, m *missionRun) {
	first := m.begin("a", collab.RunKindReview, m.reviewInput())
	again := m.startRun(m.startDocument(m.startKey("a"), collab.RunKindReview, m.reviewInput()))
	if !again.Replayed || again.Run.ID != first.ID || again.Run.StartedAt != first.StartedAt {
		t.Fatalf("a repeated start answered %s; want run %s replayed", encode(again), first.ID)
	}
	retitled := m.startDocument(m.startKey("a"), collab.RunKindReview, m.reviewInput())
	retitled.Mission.Title = "Another title " + m.run
	if replay := m.startRun(retitled); !replay.Replayed || replay.Run.ID != first.ID {
		t.Fatalf("a start that changed only the mission title answered %s; want run %s replayed", encode(replay), first.ID)
	}
	if listed := m.listed(m.mission, ""); len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatalf("three starts under one key listed %s; want run %s once", encode(listed), first.ID)
	}
}

func runsStartMismatch(t *testing.T, m *missionRun) {
	m.begin("a", collab.RunKindReview, m.reviewInput())
	other := m.reviewInput()
	other.Review.Commit = "4567def"
	failure := m.Fails(collab.ContractMissions, collab.OperationStart, encode(m.startDocument(m.startKey("a"), collab.RunKindReview, other)), collab.OutcomeConflict)
	refused(t, failure, collab.ReasonIdempotencyMismatch)
	triage := collab.RunInput{Triage: &collab.TriageRunInput{Task: collab.Ref{Source: "github:providertest/runs", ID: "7"}}}
	failure = m.Fails(collab.ContractMissions, collab.OperationStart, encode(m.startDocument(m.startKey("a"), collab.RunKindTriage, triage)), collab.OutcomeConflict)
	refused(t, failure, collab.ReasonIdempotencyMismatch)
}

// ended checks that a run ended no earlier than it started.
func ended(t *testing.T, run collab.Run) {
	t.Helper()
	started, err := time.Parse(time.RFC3339Nano, run.StartedAt)
	if err != nil {
		t.Fatalf("run %s started at %q: %v", run.ID, run.StartedAt, err)
	}
	end, err := time.Parse(time.RFC3339Nano, run.EndedAt)
	if err != nil || end.Before(started) {
		t.Fatalf("run %s started at %s and ended at %q", run.ID, run.StartedAt, run.EndedAt)
	}
}

func runsReportSucceeded(t *testing.T, m *missionRun) {
	started := m.begin("a", collab.RunKindReview, m.reviewInput())
	report := collab.MissionsReportInput{Run: started.ID, State: collab.RunStateSucceeded, Result: reviewResult()}
	first := m.report(report)
	if first.Replayed || encode(first.Run.Result) != encode(report.Result) || first.Run.StartedAt != started.StartedAt {
		t.Fatalf("a succeeded report answered %s; want the result %s, not replayed", encode(first), encode(report.Result))
	}
	ended(t, first.Run)
	again := m.report(report)
	if !again.Replayed || encode(again.Run) != encode(first.Run) {
		t.Fatalf("a repeated report answered %s; want %s replayed", encode(again), encode(first.Run))
	}
	if listed := m.find(started.ID); encode(listed) != encode(first.Run) {
		t.Fatalf("runs lists %s; the report answered %s", encode(listed), encode(first.Run))
	}
	// A start replayed after the end answers the ended run.
	if replay := m.startRun(m.startDocument(m.startKey("a"), collab.RunKindReview, m.reviewInput())); !replay.Replayed || encode(replay.Run) != encode(first.Run) {
		t.Fatalf("a start repeated after the report answered %s; want %s replayed", encode(replay), encode(first.Run))
	}
}

func runsReportFailedOrCanceled(t *testing.T, m *missionRun) {
	failed := m.begin("failed", collab.RunKindReview, m.reviewInput())
	result := m.report(collab.MissionsReportInput{Run: failed.ID, State: collab.RunStateFailed, Failure: "The gate failed."})
	if result.Run.Failure != "The gate failed." || result.Run.Result != nil {
		t.Fatalf("a failed report answered %s; want its failure and no result", encode(result))
	}
	ended(t, result.Run)
	canceled := m.begin("canceled", collab.RunKindReview, m.reviewInput())
	result = m.report(collab.MissionsReportInput{Run: canceled.ID, State: collab.RunStateCanceled})
	if result.Run.Failure != "" || result.Run.Result != nil {
		t.Fatalf("a canceled report answered %s; want no failure and no result", encode(result))
	}
	ended(t, result.Run)
}

// runsReportClosed: a run ends once. Every other report on it is refused, and
// the run keeps its first end.
func runsReportClosed(t *testing.T, m *missionRun) {
	started := m.begin("a", collab.RunKindReview, m.reviewInput())
	first := m.report(collab.MissionsReportInput{Run: started.ID, State: collab.RunStateSucceeded, Result: reviewResult()})
	approved := reviewResult()
	approved.Review.Verdict, approved.Review.Findings = collab.ReviewVerdictApprove, []collab.Finding{}
	for _, other := range []collab.MissionsReportInput{
		{Run: started.ID, State: collab.RunStateSucceeded, Result: approved},
		{Run: started.ID, State: collab.RunStateFailed, Failure: "The gate failed."},
		{Run: started.ID, State: collab.RunStateCanceled},
	} {
		failure := m.Fails(collab.ContractMissions, collab.OperationReport, encode(other), collab.OutcomeConflict)
		refused(t, failure, collab.ReasonRunClosed)
	}
	if listed := m.find(started.ID); encode(listed) != encode(first.Run) {
		t.Fatalf("after refused reports, runs lists %s; want the first end %s", encode(listed), encode(first.Run))
	}
}

func runsReportUnknown(t *testing.T, m *missionRun) {
	report := collab.MissionsReportInput{Run: m.unknownID(), State: collab.RunStateCanceled}
	failure := m.Fails(collab.ContractMissions, collab.OperationReport, encode(report), collab.OutcomeNotFound)
	refused(t, failure, collab.ReasonRunNotFound)
}

func runsReportKindMismatch(t *testing.T, m *missionRun) {
	started := m.begin("a", collab.RunKindAudit, collab.RunInput{Audit: &collab.AuditRunInput{Scope: "providertest", Revision: "0123abc"}})
	report := collab.MissionsReportInput{Run: started.ID, State: collab.RunStateSucceeded, Result: reviewResult()}
	failure := m.Fails(collab.ContractMissions, collab.OperationReport, encode(report), collab.OutcomeInvalid)
	refused(t, failure, collab.ReasonRunKindMismatch)
	if listed := m.find(started.ID); listed.State != collab.RunStateRunning || listed.EndedAt != "" {
		t.Fatalf("a refused report changed the run: %s", encode(listed))
	}
}

// runsList: runs lists every run of the mission once, the latest start
// first and, of runs started at one instant, the last started first, through
// a traversal one run per page; the kind filter
// keeps the runs of one kind; another mission's runs are not listed; and a
// mission the provider does not hold lists none.
func runsList(t *testing.T, m *missionRun) {
	triage := collab.RunInput{Triage: &collab.TriageRunInput{Task: collab.Ref{Source: "github:providertest/runs", ID: "7"}}}
	build := collab.RunInput{Build: &collab.BuildRunInput{Intention: "Scenario " + m.run}}
	want := []string{
		m.begin("review", collab.RunKindReview, m.reviewInput()).ID,
		m.begin("triage", collab.RunKindTriage, triage).ID,
		m.begin("build", collab.RunKindBuild, build).ID,
	}
	other := m.other("other")
	otherStart := m.startDocument(m.startKey("other"), collab.RunKindTriage, triage)
	otherStart.Mission = other
	otherID := m.startRun(otherStart).Run.ID

	// The runs started one after the other, so the latest start first is the
	// reverse of the start order, whether the provider's clock told the
	// starts apart or not.
	slices.Reverse(want)
	listed := m.listed(m.mission, "")
	ids := make([]string, 0, len(listed))
	for _, run := range listed {
		ids = append(ids, run.ID)
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("a traversal one run per page listed %v; want %v, the last started first, each once", ids, want)
	}
	for i := 1; i < len(listed); i++ {
		if timeKey(t, listed[i].StartedAt) > timeKey(t, listed[i-1].StartedAt) {
			t.Fatalf("run %s started after run %s, which runs lists before it", listed[i].ID, listed[i-1].ID)
		}
	}
	if triages := m.listed(m.mission, collab.RunKindTriage); len(triages) != 1 || triages[0].ID != want[1] {
		t.Fatalf("the triage runs listed %s; want run %s", encode(triages), want[1])
	}
	if others := m.listed(other, ""); len(others) != 1 || others[0].ID != otherID {
		t.Fatalf("the other mission lists %s; want run %s", encode(others), otherID)
	}
	if none := m.runs(collab.MissionsRunsInput{Mission: m.other("unknown")}); len(none.Items) != 0 || none.Page.Next != "" {
		t.Fatalf("a mission the provider does not hold lists %s", encode(none))
	}
}

// timeKey makes an RFC 3339 timestamp comparable as a string.
func timeKey(t *testing.T, value string) string {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("timestamp %q: %v", value, err)
	}
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
