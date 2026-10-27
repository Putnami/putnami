package reportstream

import (
	"fmt"
	"reflect"
	"testing"
)

// The summaries a Go test-exec member and a TypeScript test-run member put in
// their result data. Go measures statements; TypeScript measures lines and has
// no statement counts, so its row keeps the percentage alone.
const (
	goMemberData = `{"testSummary":{"failed":0,"passed":12,"skipped":1,"total":13},` +
		`"coverageSummary":{"percentage":87.5,"granularity":"statements","covered":7,"total":8,"threshold":70,` +
		`"totalStatements":8,"coveredStatements":7,"fileCount":1,"uncoveredFiles":0,` +
		`"files":[{"file":"libs/a/lib.go","totalStatements":8,"coveredStatements":7,"percentage":87.5}]}}`
	tsMemberData = `{"testSummary":{"failed":1,"passed":29,"skipped":0,"total":30},` +
		`"coverageSummary":{"percentage":75,"granularity":"lines","covered":30,"total":40,"threshold":70}}`
)

// memberIdentity is a project-scoped v2 identity, as machine.Identity writes it.
func memberIdentity(id, name string) string {
	return fmt.Sprintf(`"identity":{"key":"%s:test~test","scope":"project","project":{"id":"%s","name":"%s"},`+
		`"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@putnami/go"}}`, id, id, name)
}

// batchIdentity is the workspace-scoped identity machine.BatchTaskEvent records
// a shared batch subprocess under.
const batchIdentity = `"identity":{"key":"/workspace:test~test-batch","scope":"workspace","project":{"id":"/workspace","name":"workspace"},` +
	`"task":{"name":"test~test-batch","command":"test","step":"test","kind":"test-exec-batch"},"provider":{"extension":"@putnami/go"}}`

func taskStart(identity string) string {
	return `{"protocolVersion":2,"record":"task:start","time":"2026-10-27T10:00:00Z",` + identity + `}`
}

// resultEvent is a task:event result in the normalized shape the CLI writes.
func resultEvent(identity, data string) string {
	return `{"protocolVersion":2,"record":"task:event","time":"2026-10-27T10:00:01Z",` + identity +
		`,"event":{"data":` + data + `,"level":"info","message":"Job OK","status":"OK","time":"2026-10-27T10:00:01Z","type":"result"}}`
}

// batchResult is the batch subprocess's own result: one entry per member.
func batchResult(members ...string) string {
	data := `{"batchResults":[`
	for i, member := range members {
		if i > 0 {
			data += ","
		}
		data += member
	}
	return resultEvent(batchIdentity, data+`]}`)
}

func batchMember(id, data string) string {
	return `{"projectId":"` + id + `","status":"OK","data":` + data + `,"summary":{"errors":0,"warnings":0,"infos":0}}`
}

// failedBatchMember is a FAILED batchResults entry with diagnostics, in the
// shape the batch producers write. An empty data omits the member.
func failedBatchMember(id, data, diagnostics string) string {
	entry := `{"projectId":"` + id + `","status":"FAILED"`
	if data != "" {
		entry += `,"data":` + data
	}
	return entry + `,"diagnostics":` + diagnostics + `,"summary":{"errors":1,"warnings":0,"infos":0}}`
}

// diagnosticEvent is a task:event diagnostic in the normalized shape the CLI
// writes for a task that runs on its own.
func diagnosticEvent(identity, diagnostic string) string {
	return `{"protocolVersion":2,"record":"task:event","time":"2026-10-27T10:00:01Z",` + identity +
		`,"event":{"type":"diagnostic","time":"2026-10-27T10:00:01Z",` + diagnostic + `}}`
}

// The same three findings, as a batch member's diagnostics and as the
// diagnostic events the same task writes when it runs on its own: a test
// failure with a location, an error with a line but no file, which names no
// location, and a warning, which is not a failing case.
const (
	memberDiagnostics = `[` +
		`{"category":"TEST_FAILED","severity":"error","description":"Test failed: adds\nexpected 2","file":"libs/b/src/add.test.ts","line":12,"column":3},` +
		`{"category":"COVERAGE_THRESHOLD_NOT_MET","severity":"error","description":"Coverage 75% is below 80%","line":4},` +
		`{"category":"UNCOVERED_FILE","severity":"warning","description":"0% coverage","file":"libs/b/src/x.ts"}]`
	soloFailure   = `"severity":"error","code":"TEST_FAILED","message":"Test failed: adds\nexpected 2","location":{"file":"libs/b/src/add.test.ts","line":12,"column":3}`
	soloThreshold = `"severity":"error","code":"COVERAGE_THRESHOLD_NOT_MET","message":"Coverage 75% is below 80%"`
	soloUncovered = `"severity":"warning","code":"UNCOVERED_FILE","message":"0% coverage","location":{"file":"libs/b/src/x.ts"}`
)

// taskEnd is a member's task:end: it carries no test or coverage summary.
func taskEnd(identity, reuse string) string {
	return `{"protocolVersion":2,"record":"task:end","time":"2026-10-27T10:00:02Z",` + identity +
		`,"task":{"identity":{"key":"k"},"status":"success","reuse":"` + reuse + `","exitCode":0,"durationMs":5,"taskWallMs":6}}`
}

func fold(t *testing.T, lines ...string) *Batch {
	t.Helper()
	p := NewParser()
	for _, line := range lines {
		if _, err := p.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	batch := p.Batch()
	if batch.Stats.MalformedLines != 0 || batch.Stats.UnknownEvents != 0 {
		t.Fatalf("stats = %+v, want every line recognized", batch.Stats)
	}
	return batch
}

// A v2 stream records no task:event for a batch member, and its task:end
// carries no summary. Its tests and coverage rows and its failing cases fold
// from the batch result's batchResults entry, and they equal the rows the same
// task yields when it runs on its own: its result event and its diagnostic
// events. A member with no data still reports its failing cases.
func TestBatchMembersFoldFromTheBatchResult(t *testing.T) {
	a, b := memberIdentity("/libs/a", "libs/a"), memberIdentity("/libs/b", "libs/b")
	c := memberIdentity("/libs/c", "libs/c")
	const preparation = `[{"category":"PREPARE_FAILED","severity":"error","description":"no go.mod"}]`
	batched := fold(t,
		taskStart(a),
		taskStart(b),
		taskStart(c),
		batchResult(
			batchMember("/libs/a", goMemberData),
			failedBatchMember("/libs/b", tsMemberData, memberDiagnostics),
			failedBatchMember("/libs/c", "", preparation),
		),
		taskEnd(a, "none"),
		taskEnd(b, "none"),
		taskEnd(c, "none"),
	)
	solo := fold(t,
		resultEvent(a, goMemberData),
		diagnosticEvent(b, soloFailure),
		diagnosticEvent(b, soloThreshold),
		diagnosticEvent(b, soloUncovered),
		resultEvent(b, tsMemberData),
		diagnosticEvent(c, `"severity":"error","code":"PREPARE_FAILED","message":"no go.mod"`),
	)

	wantTests := &TestsReport{Projects: []ProjectTests{
		{Project: "libs/a", Passed: 12, Failed: 0, Skipped: 1, Total: 13},
		{Project: "libs/b", Passed: 29, Failed: 1, Skipped: 0, Total: 30, FailingCases: []FailingCase{
			{Name: "TEST_FAILED", Message: "Test failed: adds\nexpected 2", File: "libs/b/src/add.test.ts", Line: 12},
			{Name: "COVERAGE_THRESHOLD_NOT_MET", Message: "Coverage 75% is below 80%"},
		}},
		{Project: "libs/c", FailingCases: []FailingCase{{Name: "PREPARE_FAILED", Message: "no go.mod"}}},
	}}
	wantCoverage := &CoverageReport{Projects: []ProjectCoverage{
		{Project: "libs/a", CoveredStatements: 7, TotalStatements: 8, Percentage: 87.5,
			Files: []FileCoverage{{File: "libs/a/lib.go", CoveredStatements: 7, TotalStatements: 8, Percentage: 87.5}}},
		{Project: "libs/b", Percentage: 75},
	}}
	for name, batch := range map[string]*Batch{"batched": batched, "solo": solo} {
		if !reflect.DeepEqual(batch.Tests, wantTests) {
			t.Fatalf("%s tests = %+v, want %+v", name, batch.Tests, wantTests)
		}
		if !reflect.DeepEqual(batch.Coverage, wantCoverage) {
			t.Fatalf("%s coverage = %+v, want %+v", name, batch.Coverage, wantCoverage)
		}
	}
	if batched.Builds == nil || len(batched.Builds.Tasks) != 3 {
		t.Fatalf("builds = %+v, want the three member terminals", batched.Builds)
	}
}

// A member's failing cases wait for its project name like its summaries do.
func TestBatchMemberFailingCasesWaitForTheProjectName(t *testing.T) {
	b := memberIdentity("/libs/b", "libs/b")
	batch := fold(t,
		batchResult(failedBatchMember("/libs/b", "", memberDiagnostics)),
		taskEnd(b, "none"),
	)
	want := &TestsReport{Projects: []ProjectTests{{Project: "libs/b", FailingCases: []FailingCase{
		{Name: "TEST_FAILED", Message: "Test failed: adds\nexpected 2", File: "libs/b/src/add.test.ts", Line: 12},
		{Name: "COVERAGE_THRESHOLD_NOT_MET", Message: "Coverage 75% is below 80%"},
	}}}}
	if !reflect.DeepEqual(batch.Tests, want) {
		t.Fatalf("tests = %+v, want %+v", batch.Tests, want)
	}
}

// When a task wrote its own error diagnostics and a batch member entry carries
// diagnostics for the same project and task, the task's own win in either
// stream order: its failures are listed once. A member failure of another task
// of the same project is kept.
func TestOwnDiagnosticsWinOverBatchMemberDiagnostics(t *testing.T) {
	b := memberIdentity("/libs/b", "libs/b")
	lint := `"identity":{"key":"/libs/b:lint","scope":"project","project":{"id":"/libs/b","name":"libs/b"},` +
		`"task":{"name":"lint","command":"lint","kind":"lint"},"provider":{"extension":"@putnami/go"}}`
	const own = `"severity":"error","code":"TEST_FAILED","message":"Test failed: subtracts","location":{"file":"libs/b/src/sub.test.ts","line":7}`
	ownCase := FailingCase{Name: "TEST_FAILED", Message: "Test failed: subtracts", File: "libs/b/src/sub.test.ts", Line: 7}
	lintCase := FailingCase{Name: "LINT", Message: "unused import", File: "libs/b/src/a.ts", Line: 1}
	const lintBatch = `{"protocolVersion":2,"record":"task:event","time":"2026-10-27T10:00:01Z",` +
		`"identity":{"key":"/workspace:lint-batch","scope":"workspace","project":{"id":"/workspace","name":"workspace"},` +
		`"task":{"name":"lint-batch","command":"lint","kind":"lint-batch"},"provider":{"extension":"@putnami/go"}}` +
		`,"event":{"type":"result","status":"OK","data":{"batchResults":[{"projectId":"/libs/b","status":"FAILED",` +
		`"diagnostics":[{"category":"LINT","severity":"error","description":"unused import","file":"libs/b/src/a.ts","line":1}]}]}}}`
	testBatch := batchResult(failedBatchMember("/libs/b", "", memberDiagnostics))
	for _, tc := range []struct {
		name  string
		lines []string
		want  []FailingCase
	}{
		{"own first", []string{taskStart(b), taskStart(lint), diagnosticEvent(b, own), testBatch, lintBatch}, []FailingCase{ownCase, lintCase}},
		{"batch first", []string{taskStart(b), taskStart(lint), lintBatch, testBatch, diagnosticEvent(b, own)}, []FailingCase{lintCase, ownCase}},
		{"batch pending", []string{lintBatch, testBatch, diagnosticEvent(b, own)}, []FailingCase{lintCase, ownCase}},
	} {
		batch := fold(t, tc.lines...)
		want := &TestsReport{Projects: []ProjectTests{{Project: "libs/b", FailingCases: tc.want}}}
		if !reflect.DeepEqual(batch.Tests, want) {
			t.Fatalf("%s: tests = %+v, want %+v", tc.name, batch.Tests, want)
		}
	}
}

// A member named by no record before the batch result folds once a later
// record names it. A member that no record ever names folds nowhere: a row
// keyed by project id would fragment the report history.
func TestBatchMemberWaitsForItsProjectName(t *testing.T) {
	a := memberIdentity("/libs/a", "libs/a")
	batch := fold(t,
		batchResult(batchMember("/libs/a", goMemberData), batchMember("/libs/unnamed", tsMemberData)),
		taskEnd(a, "none"),
	)
	if batch.Coverage == nil || len(batch.Coverage.Projects) != 1 {
		t.Fatalf("coverage = %+v, want the one named member", batch.Coverage)
	}
	if got := batch.Coverage.Projects[0]; got.Project != "libs/a" || got.CoveredStatements != 7 || got.TotalStatements != 8 || got.Percentage != 87.5 {
		t.Fatalf("coverage = %+v, want the member's Go summary", got)
	}
	if batch.Tests == nil || len(batch.Tests.Projects) != 1 || batch.Tests.Projects[0].Project != "libs/a" {
		t.Fatalf("tests = %+v, want the one named member", batch.Tests)
	}
}

// When a task has its own result event and batch member data too, such as a
// member that ran again solo, the task's own result wins in either stream
// order: one row, equal to the event's.
func TestOwnResultWinsOverBatchMemberData(t *testing.T) {
	a := memberIdentity("/libs/a", "libs/a")
	const own = `{"testSummary":{"failed":0,"passed":40,"skipped":0,"total":40},` +
		`"coverageSummary":{"percentage":90,"granularity":"statements","totalStatements":10,"coveredStatements":9,` +
		`"files":[{"file":"libs/a/lib.go","totalStatements":10,"coveredStatements":9,"percentage":90}]}}`
	wantTests := &TestsReport{Projects: []ProjectTests{{Project: "libs/a", Passed: 40, Total: 40}}}
	wantCoverage := &CoverageReport{Projects: []ProjectCoverage{{Project: "libs/a", CoveredStatements: 9, TotalStatements: 10, Percentage: 90,
		Files: []FileCoverage{{File: "libs/a/lib.go", CoveredStatements: 9, TotalStatements: 10, Percentage: 90}}}}}
	for name, lines := range map[string][]string{
		"own result first": {taskStart(a), resultEvent(a, own), batchResult(batchMember("/libs/a", goMemberData)), taskEnd(a, "none")},
		"batch first":      {taskStart(a), batchResult(batchMember("/libs/a", goMemberData)), resultEvent(a, own), taskEnd(a, "none")},
		"batch pending":    {batchResult(batchMember("/libs/a", goMemberData)), resultEvent(a, own), taskEnd(a, "none")},
	} {
		batch := fold(t, lines...)
		if !reflect.DeepEqual(batch.Tests, wantTests) {
			t.Fatalf("%s: tests = %+v, want %+v", name, batch.Tests, wantTests)
		}
		if !reflect.DeepEqual(batch.Coverage, wantCoverage) {
			t.Fatalf("%s: coverage = %+v, want %+v", name, batch.Coverage, wantCoverage)
		}
	}
}

// Batch member data is a result summary, so it keeps the precedence result
// summaries have over flat metrics, in either stream order.
func TestBatchMemberDataWinsOverMetrics(t *testing.T) {
	a := memberIdentity("/libs/a", "libs/a")
	metric := `{"protocolVersion":2,"record":"task:event",` + a + `,"event":{"type":"metric","name":"coverage","unit":"percent","value":12}}`
	for name, lines := range map[string][]string{
		"metric first": {taskStart(a), metric, batchResult(batchMember("/libs/a", goMemberData))},
		"metric last":  {taskStart(a), batchResult(batchMember("/libs/a", goMemberData)), metric},
	} {
		batch := fold(t, lines...)
		if batch.Coverage == nil || len(batch.Coverage.Projects) != 1 || batch.Coverage.Projects[0].Percentage != 87.5 {
			t.Fatalf("%s: coverage = %+v, want the member summary's 87.5", name, batch.Coverage)
		}
	}
}

// A cache hit replays its stored result as a task:event, local or remote, so
// its rows fold from that event like an executed task's.
func TestCacheHitFoldsItsReplayedResult(t *testing.T) {
	for _, reuse := range []string{"local-cache", "remote-cache"} {
		a := memberIdentity("/libs/a", "libs/a")
		batch := fold(t, taskStart(a), resultEvent(a, goMemberData), taskEnd(a, reuse))
		if batch.Builds == nil || len(batch.Builds.Tasks) != 1 || !batch.Builds.Tasks[0].CacheHit {
			t.Fatalf("%s: builds = %+v, want one cache hit", reuse, batch.Builds)
		}
		if batch.Coverage == nil || len(batch.Coverage.Projects) != 1 {
			t.Fatalf("%s: coverage = %+v, want the hit's row", reuse, batch.Coverage)
		}
		if got := batch.Coverage.Projects[0]; got.Project != "libs/a" || got.CoveredStatements != 7 || got.Percentage != 87.5 {
			t.Fatalf("%s: coverage = %+v", reuse, got)
		}
	}
}
