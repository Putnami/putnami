package reportstream

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// parseFixture streams the golden capture (a trimmed, REAL `putnami test
// --output=jsonl` stream) through the parser in small chunks, so line
// reassembly across Write boundaries is exercised too.
func parseFixture(t *testing.T) *Batch {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "lint-test-build.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p := NewParser()
	const chunk = 700 // deliberately not line-aligned
	for i := 0; i < len(data); i += chunk {
		end := min(i+chunk, len(data))
		if _, err := p.Write(data[i:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return p.Batch()
}

// parseWarmFixture streams the real warm Putnami gate capture inside the
// runner's minimum framing shapes. Unlike the trimmed parser golden above, it
// owns both framing and every cached job; README documents the wrapper provenance.
func parseWarmFixture(t *testing.T) *Batch {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "warm-run.jsonl"))
	if err != nil {
		t.Fatalf("read warm runner fixture: %v", err)
	}
	p := NewParser()
	const chunk = 311 // deliberately crosses both runner and Putnami lines
	for i := 0; i < len(data); i += chunk {
		end := min(i+chunk, len(data))
		if _, err := p.Write(data[i:end]); err != nil {
			t.Fatalf("write warm fixture: %v", err)
		}
	}
	return p.Batch()
}

// parseNamedFixture streams any checked-in capture through the parser in small
// chunks, so line reassembly across Write boundaries is exercised there too.
func parseNamedFixture(t *testing.T, name string) *Batch {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	p := NewParser()
	const chunk = 700 // deliberately not line-aligned
	for i := 0; i < len(data); i += chunk {
		end := min(i+chunk, len(data))
		if _, err := p.Write(data[i:end]); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return p.Batch()
}

// TestGoldenFixtureStats pins the parse provenance of the real capture: every
// line is a recognized v1 event — nothing malformed, nothing unknown.
func TestGoldenFixtureStats(t *testing.T) {
	batch := parseFixture(t)
	if batch.Version != ContractVersion {
		t.Fatalf("version = %q, want %q", batch.Version, ContractVersion)
	}
	want := Stats{Lines: 96}
	if batch.Stats != want {
		t.Fatalf("stats = %+v, want %+v (a REAL capture must parse cleanly)", batch.Stats, want)
	}
}

// TestGoldenFixtureTests pins the tests report folded from the capture: the
// rich result testSummary wins, and packages without test data (the generate
// job) contribute no entry.
func TestGoldenFixtureTests(t *testing.T) {
	batch := parseFixture(t)
	if batch.Tests == nil {
		t.Fatal("no tests report folded from the capture")
	}
	if len(batch.Tests.Projects) != 2 {
		t.Fatalf("tests projects = %+v, want 2 (cli + apitest)", batch.Tests.Projects)
	}
	cli := batch.Tests.Projects[0]
	if cli.Project != "libs/cli" {
		t.Fatalf("first tests project = %q, want libs/cli", cli.Project)
	}
	if cli.Passed != 38 || cli.Failed != 0 || cli.Skipped != 0 || cli.Total != 38 {
		t.Fatalf("cli counts = %+v, want 38/0/0/38", cli)
	}
	if len(cli.FailingCases) != 0 {
		t.Fatalf("cli failing cases = %+v, want none (UNCOVERED_FILE diagnostics are warnings)", cli.FailingCases)
	}
	apitest := batch.Tests.Projects[1]
	if apitest.Project != "libs/internal/apitest" || apitest.Total != 0 {
		t.Fatalf("second tests project = %+v, want apitest with zero counts", apitest)
	}
}

// TestGoldenFixtureCoverage pins the coverage report: statements + percentage
// from the result coverageSummary, including the per-file breakdown.
func TestGoldenFixtureCoverage(t *testing.T) {
	batch := parseFixture(t)
	if batch.Coverage == nil {
		t.Fatal("no coverage report folded from the capture")
	}
	if len(batch.Coverage.Projects) != 2 {
		t.Fatalf("coverage projects = %+v, want 2", batch.Coverage.Projects)
	}
	cli := batch.Coverage.Projects[0]
	if cli.Project != "libs/cli" {
		t.Fatalf("first coverage project = %q", cli.Project)
	}
	if cli.CoveredStatements != 632 || cli.TotalStatements != 1586 || cli.Percentage != 39.85 {
		t.Fatalf("cli coverage = %+v, want 632/1586 @ 39.85", cli)
	}
	if len(cli.Files) != 10 {
		t.Fatalf("cli coverage files = %d, want 10", len(cli.Files))
	}
	// File paths are stored as the stream gives them (go import path form).
	if !strings.HasPrefix(cli.Files[0].File, "example.com/") {
		t.Fatalf("file path = %q, want the stream's import-path form untouched", cli.Files[0].File)
	}
}

// TestGoldenFixtureBuilds pins the builds report: one task per job:end with
// status, duration, and cache hit/miss.
func TestGoldenFixtureBuilds(t *testing.T) {
	batch := parseFixture(t)
	if batch.Builds == nil {
		t.Fatal("no builds report folded from the capture")
	}
	if len(batch.Builds.Tasks) != 8 {
		t.Fatalf("builds tasks = %d, want 8 job:end records", len(batch.Builds.Tasks))
	}
	byStatus := map[string]int{}
	for _, task := range batch.Builds.Tasks {
		byStatus[task.Status]++
	}
	if byStatus["success"] != 4 || byStatus["skipped"] != 2 || byStatus["failed"] != 1 || byStatus["canceled"] != 1 {
		t.Fatalf("statuses = %v, want success:4 skipped:2 failed:1 canceled:1", byStatus)
	}
	first := batch.Builds.Tasks[0]
	if first.Project != "libs/cli" || first.Task != "test~config-merge" ||
		first.Status != "skipped" || first.DurationMS != 13 || first.CacheHit {
		t.Fatalf("first task = %+v", first)
	}
	failed := batch.Builds.Tasks[6]
	if failed.Project != "libs/internal/apitest" || failed.Task != "test~test" ||
		failed.Status != "failed" || failed.DurationMS != 37994 {
		t.Fatalf("failed task = %+v", failed)
	}
}

// Runner-owned markers are first-class stream framing. They count as lines but
// never as malformed or unknown events and never manufacture report records.
func TestRunnerMarkersAreValidFraming(t *testing.T) {
	p := NewParser()
	for _, line := range []string{
		`{"kind":"ci.runner.start","runId":"run_1","workspaceId":"ws_1","sha":"abc123","cacheUrl":"https://cache.test"}`,
		`{"kind":"ci.runner.drift","runId":"run_1","exitCode":0}`,
		`{"kind":"ci.runner.end","runId":"run_1","exitCode":0}`,
		`{"kind":"ci.runner.ingest","runId":"run_1","conclusion":"success","posted":true}`,
		`{"kind":"ci.runner.publish","runId":"run_1","package":"apps/api","digest":"sha256:abc","channel":"pr-42","published":true}`,
		`{"kind":"ci.runner.publish","runId":"run_1","package":"apps/admin","channel":"pr-42","published":false}`,
		`{"kind":"ci.runner.phase","runId":"run_1","phase":"test","status":"in_progress","conclusion":"","posted":true}`,
		`{"kind":"ci.runner.phase","runId":"run_1","phase":"test","status":"completed","conclusion":"failure","posted":true}`,
		`{"kind":"ci.runner.gate-recovery","runId":"run_1","attempts":1,"recovery":"none","reason":"","firstSeconds":41,"secondSeconds":0,"evidence":null}`,
		`{"kind":"ci.runner.gate-recovery","runId":"run_1","attempts":2,"recovery":"recovered","reason":"passed after timeout recovery","firstSeconds":318,"secondSeconds":242,"evidence":{"task":"/apps/intelligence-server:lint~golangci-lint-check-only","durationMs":300002,"localCacheHits":646}}`,
		`{"kind":"ci.runner.heartbeat","runId":"run_1","session":"gs-1-2","attempt":1,"version":1,"intervalSeconds":20,"posted":12,"dropped":0}`,
		// A session whose beats ALL failed best-effort is still healthy framing: the
		// heartbeat is diagnostic, so a dropped beat must never make a run's report
		// stream partial.
		`{"kind":"ci.runner.heartbeat","runId":"run_1","session":"gs-1-3","attempt":2,"version":1,"intervalSeconds":20,"posted":0,"dropped":4}`,
		// The advisory change-plan verdict. A run that had no plan, and one
		// whose plan DISAGREED with what the gate selected, are both healthy framing:
		// the verdict is advisory and reaches the plane on the terminal ingest, so
		// neither may make a run's report stream partial.
		`{"kind":"ci.runner.changeplan","runId":"run_1","status":"match","version":1,"digest":"sha256:abc","reason":""}`,
		`{"kind":"ci.runner.changeplan","runId":"run_1","status":"absent","version":1,"digest":"","reason":"no change plan was provided to this run"}`,
		`{"kind":"ci.runner.changeplan","runId":"run_1","status":"mismatch","version":1,"digest":"sha256:abc","reason":"planned 3, gate selected 4; 1 unplanned (e.g. apps/echo)"}`,
	} {
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Stats != (Stats{Lines: 15}) {
		t.Fatalf("stats = %+v, want fifteen clean framing lines", batch.Stats)
	}
	if batch.HasReports() {
		t.Fatalf("runner framing fabricated reports: %+v", batch)
	}
}

// Marker recognition is fail-closed: a typo, missing required field, or wrong
// field type is malformed and therefore makes downstream ingestion partial.
func TestUnknownOrMalformedRunnerMarkersAreCounted(t *testing.T) {
	p := NewParser()
	for _, line := range []string{
		`{"kind":"ci.runner.typo","runId":"run_1"}`,
		`{"kind":"ci.runner.start","runId":"run_1"}`,
		`{"kind":"ci.runner.drift","runId":"run_1"}`,
		`{"kind":"ci.runner.end","runId":"run_1","exitCode":"zero"}`,
		`{"kind":"ci.runner.ingest","runId":"run_1","conclusion":"success"}`,
		`{"kind":"ci.runner.publish","runId":"run_1","package":"apps/api","channel":"pr-42"}`,
		`{"kind":"ci.runner.phase","runId":"run_1","phase":"test","status":"completed"}`,
		`{"kind":"ci.runner.gate-recovery","runId":"run_1","recovery":"recovered"}`,
		`{"kind":"ci.runner.heartbeat","runId":"run_1","session":"gs-1-2"}`,
		`{"kind":"ci.runner.heartbeat","runId":"run_1","posted":3}`,
		// A change-plan marker with no status states no verdict, and one with no
		// contract version states no field meanings — both stay malformed, so a
		// broken runner envelope can never suppress the honest partial signal.
		`{"kind":"ci.runner.changeplan","runId":"run_1","version":1}`,
		`{"kind":"ci.runner.changeplan","runId":"run_1","status":"match"}`,
	} {
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Stats != (Stats{Lines: 12, MalformedLines: 12}) {
		t.Fatalf("stats = %+v, want twelve malformed marker lines", batch.Stats)
	}
}

// The checked-in warm capture is healthy framing plus 14 real cache hits, so
// it folds builds without marking the stream partial. The Putnami CLI does not
// replay cached test/coverage events yet (a confirmed upstream gap);
// keeping nil reports here pins the gap rather than inventing data.
func TestWarmFixturePinsCachedResultContractGap(t *testing.T) {
	batch := parseWarmFixture(t)
	if batch.Stats != (Stats{Lines: 31}) {
		t.Fatalf("warm stats = %+v, want 31 clean lines", batch.Stats)
	}
	if batch.Builds == nil || len(batch.Builds.Tasks) != 14 {
		t.Fatalf("warm builds = %+v, want 14 cached tasks", batch.Builds)
	}
	for _, task := range batch.Builds.Tasks {
		if !task.CacheHit || task.Status != "success" || task.DurationMS != 0 {
			t.Fatalf("warm task = %+v, want a successful zero-duration cache hit", task)
		}
	}
	if batch.Tests != nil || batch.Coverage != nil {
		t.Fatalf("warm cached events unexpectedly fabricated reports: tests=%+v coverage=%+v", batch.Tests, batch.Coverage)
	}
}

// Malformed lines are skipped and counted, never fatal — the surviving lines
// still fold into the batch.
func TestMalformedLinesSkippedNotFatal(t *testing.T) {
	p := NewParser()
	_, _ = p.Write([]byte("this is not json\n"))
	_, _ = p.Write([]byte(`{"no_event_key":true}` + "\n"))
	_, _ = p.Write([]byte(`{"event":"job:end","package":"a","job":"test~test","status":"success","duration":5,"cache":true}` + "\n"))
	batch := p.Batch()
	if batch.Stats.MalformedLines != 2 {
		t.Fatalf("malformed = %d, want 2", batch.Stats.MalformedLines)
	}
	if batch.Builds == nil || len(batch.Builds.Tasks) != 1 {
		t.Fatalf("builds = %+v, want the surviving job:end folded", batch.Builds)
	}
	if !batch.Builds.Tasks[0].CacheHit {
		t.Fatal("cache=true must fold to CacheHit")
	}
}

// Unknown events and unknown job:event types are skipped and counted, never
// fatal (forward compatibility with future stream revisions).
func TestUnknownEventTypesSkippedAndCounted(t *testing.T) {
	p := NewParser()
	_, _ = p.Write([]byte(`{"event":"job:teleport","package":"a"}` + "\n"))
	_, _ = p.Write([]byte(`{"event":"job:event","package":"a","job":"test~test","type":"hologram","data":{"type":"hologram"}}` + "\n"))
	_, _ = p.Write([]byte(`{"event":"job:event","package":"a","job":"test~test","type":"metric","data":{"name":"generate-hash","unit":"count","value":1}}` + "\n"))
	batch := p.Batch()
	if batch.Stats.UnknownEvents != 2 {
		t.Fatalf("unknown = %d, want 2 (unknown metric NAMES are not unknown events)", batch.Stats.UnknownEvents)
	}
	if batch.HasReports() {
		t.Fatalf("batch = %+v, want empty", batch)
	}
}

// A line over the size cap is discarded whole and counted; the parser keeps
// bounded memory and keeps parsing subsequent lines.
func TestOversizedLineSkippedAndCounted(t *testing.T) {
	p := NewParser()
	huge := `{"event":"job:event","data":"` + strings.Repeat("x", MaxLineBytes) + `"}` + "\n"
	_, _ = p.Write([]byte(huge))
	_, _ = p.Write([]byte(`{"event":"job:end","package":"a","job":"test~test","status":"success","duration":1,"cache":false}` + "\n"))
	batch := p.Batch()
	if batch.Stats.OversizedLines != 1 {
		t.Fatalf("oversized = %d, want 1", batch.Stats.OversizedLines)
	}
	if batch.Builds == nil || len(batch.Builds.Tasks) != 1 {
		t.Fatalf("builds = %+v, want the following line still parsed", batch.Builds)
	}
}

// A trailing line without a newline is flushed by Batch().
func TestTrailingUnterminatedLineFlushed(t *testing.T) {
	p := NewParser()
	_, _ = p.Write([]byte(`{"event":"job:end","package":"a","job":"test~test","status":"success","duration":1,"cache":false}`))
	batch := p.Batch()
	if batch.Builds == nil || len(batch.Builds.Tasks) != 1 {
		t.Fatalf("builds = %+v, want the unterminated final line folded", batch.Builds)
	}
	if batch.Stats.Lines != 1 {
		t.Fatalf("lines = %d, want 1", batch.Stats.Lines)
	}
}

// Metric fallbacks fold tests/coverage when no rich result summary arrived,
// and the result summary wins when both are present.
func TestMetricFallbackAndResultPrecedence(t *testing.T) {
	p := NewParser()
	// Metrics only for package a.
	for _, line := range []string{
		`{"event":"job:event","package":"a","job":"test~test","type":"metric","data":{"name":"tests-total","unit":"count","value":7}}`,
		`{"event":"job:event","package":"a","job":"test~test","type":"metric","data":{"name":"tests-passed","unit":"count","value":6}}`,
		`{"event":"job:event","package":"a","job":"test~test","type":"metric","data":{"name":"tests-failed","unit":"count","value":1}}`,
		`{"event":"job:event","package":"a","job":"test~test","type":"metric","data":{"name":"coverage","unit":"percent","value":81.5}}`,
		// Package b: metric says 1 test, result summary corrects to 3.
		`{"event":"job:event","package":"b","job":"test~test","type":"metric","data":{"name":"tests-total","unit":"count","value":1}}`,
		`{"event":"job:event","package":"b","job":"test~test","type":"result","data":{"data":{"testSummary":{"passed":3,"failed":0,"skipped":0,"total":3}},"status":"OK","type":"result"}}`,
	} {
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Tests == nil || len(batch.Tests.Projects) != 2 {
		t.Fatalf("tests = %+v, want 2 projects", batch.Tests)
	}
	a := batch.Tests.Projects[0]
	if a.Project != "a" || a.Total != 7 || a.Passed != 6 || a.Failed != 1 {
		t.Fatalf("metric-only project = %+v", a)
	}
	b := batch.Tests.Projects[1]
	if b.Project != "b" || b.Total != 3 || b.Passed != 3 {
		t.Fatalf("result summary must win over metrics: %+v", b)
	}
	if batch.Coverage == nil || len(batch.Coverage.Projects) != 1 || batch.Coverage.Projects[0].Percentage != 81.5 {
		t.Fatalf("coverage = %+v", batch.Coverage)
	}
}

// Error-severity diagnostics become failing-case references with file:line;
// warnings do not.
func TestErrorDiagnosticsBecomeFailingCases(t *testing.T) {
	p := NewParser()
	for _, line := range []string{
		`{"event":"job:event","package":"a","job":"test~test","type":"diagnostic","data":{"code":"TEST_FAIL","severity":"error","message":"TestBoom failed","location":{"file":"a/boom_test.go","line":42},"type":"diagnostic"}}`,
		`{"event":"job:event","package":"a","job":"test~test","type":"diagnostic","data":{"code":"UNCOVERED_FILE","severity":"warning","message":"0% coverage","location":{"file":"a/cold.go"},"type":"diagnostic"}}`,
	} {
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Tests == nil || len(batch.Tests.Projects) != 1 {
		t.Fatalf("tests = %+v", batch.Tests)
	}
	cases := batch.Tests.Projects[0].FailingCases
	if len(cases) != 1 {
		t.Fatalf("failing cases = %+v, want exactly the error diagnostic", cases)
	}
	if cases[0].Name != "TEST_FAIL" || cases[0].File != "a/boom_test.go" || cases[0].Line != 42 {
		t.Fatalf("failing case = %+v", cases[0])
	}
}

// ---------------------------------------------------------------------------
// putnami v2 session records
//
// Both v2 fixtures are REAL captures of the SAME command run twice against the
// same tree, one per contract — see testdata/README.md for provenance.
// ---------------------------------------------------------------------------

const (
	v2Fixture     = "v2-lint-test-build.jsonl"
	v1TwinFixture = "v1-twin-lint-test-build.jsonl"
)

// TestV2GoldenFixtureStats pins the parse provenance of the real v2 capture:
// every one of its task:start / task:event / task:end / session:end records is
// recognized. Without v2 support this fixture parses to 91 unknown events and an
// empty batch, which would sink the aggregate CI check.
func TestV2GoldenFixtureStats(t *testing.T) {
	batch := parseNamedFixture(t, v2Fixture)
	want := Stats{Lines: 91}
	if batch.Stats != want {
		t.Fatalf("v2 stats = %+v, want %+v (a REAL v2 capture must parse cleanly)", batch.Stats, want)
	}
	if !batch.HasReports() {
		t.Fatal("v2 capture folded an EMPTY batch — v2 records are not being recognized")
	}
}

// TestV2GoldenFixtureBuilds pins the task:end fold: one build task per record,
// keyed by the identity's project NAME and canonical task name (never the
// derived identity key), with the reuse provenance collapsed onto v1's cache
// boolean.
func TestV2GoldenFixtureBuilds(t *testing.T) {
	batch := parseNamedFixture(t, v2Fixture)
	if batch.Builds == nil {
		t.Fatal("no builds report folded from the v2 capture")
	}
	if len(batch.Builds.Tasks) != 9 {
		t.Fatalf("v2 builds tasks = %d, want 9 task:end records", len(batch.Builds.Tasks))
	}
	byStatus := map[string]int{}
	for _, task := range batch.Builds.Tasks {
		byStatus[task.Status]++
		if task.Project != "go.putnami.dev/protocol/cli" {
			t.Fatalf("task project = %q, want the identity's project NAME "+
				"(not its id %q and not the derived key)", task.Project, "/protocols/cli")
		}
		if strings.Contains(task.Task, ":") {
			t.Fatalf("task name = %q, want the canonical plan name, not the identity key", task.Task)
		}
		if task.CacheHit {
			t.Fatalf("task %+v claims a cache hit; the capture ran --no-cache (reuse: none)", task)
		}
	}
	if byStatus["success"] != 7 || byStatus["skipped"] != 2 {
		t.Fatalf("v2 statuses = %v, want success:7 skipped:2", byStatus)
	}
	first := batch.Builds.Tasks[0]
	if first.StartedAt.IsZero() || first.FinishedAt.IsZero() || !first.FinishedAt.After(first.StartedAt) {
		t.Fatalf("first task clocks = %v..%v, want its matched task:start/task:end interval", first.StartedAt, first.FinishedAt)
	}
}

// TestV2GoldenFixtureTests pins the task:event fold: the rich result
// testSummary reaches the tests report through the v2 record exactly as it did
// through job:event.
func TestV2GoldenFixtureTests(t *testing.T) {
	batch := parseNamedFixture(t, v2Fixture)
	if batch.Tests == nil || len(batch.Tests.Projects) != 1 {
		t.Fatalf("v2 tests = %+v, want exactly the one selected project", batch.Tests)
	}
	project := batch.Tests.Projects[0]
	if project.Project != "go.putnami.dev/protocol/cli" {
		t.Fatalf("tests project = %q, want the identity's project name", project.Project)
	}
	if project.Passed != 183 || project.Failed != 0 || project.Skipped != 0 || project.Total != 183 {
		t.Fatalf("v2 test counts = %+v, want 183/0/0/183", project)
	}
	if len(project.FailingCases) != 0 {
		t.Fatalf("v2 failing cases = %+v, want none (the capture is a green run)", project.FailingCases)
	}
}

// TestV2BatchEquivalentToV1Twin is the contract test: the same run captured
// under both contracts must fold to the same report batch. It compares
// field-for-field on everything both wires can express — durations excluded,
// because the two captures are two real executions and their wall times legitimately
// differ. This is what fails if the task:end or task:event mapping is dropped
// or rekeyed.
func TestV2BatchEquivalentToV1Twin(t *testing.T) {
	v2 := parseNamedFixture(t, v2Fixture)
	v1 := parseNamedFixture(t, v1TwinFixture)

	if v1.Stats.Lines != v2.Stats.Lines {
		t.Fatalf("twin captures disagree on line count: v1=%d v2=%d", v1.Stats.Lines, v2.Stats.Lines)
	}
	if (v1.Stats != Stats{Lines: v1.Stats.Lines}) {
		t.Fatalf("v1 twin stats = %+v, want a clean parse", v1.Stats)
	}

	gotBuilds, wantBuilds := buildKeys(v2), buildKeys(v1)
	if !slices.Equal(gotBuilds, wantBuilds) {
		t.Fatalf("v2 builds differ from the v1 twin:\n v2 = %v\n v1 = %v", gotBuilds, wantBuilds)
	}
	if len(gotBuilds) == 0 {
		t.Fatal("both twins folded zero builds — the equivalence assertion would be vacuous")
	}

	if !reflect.DeepEqual(v2.Tests, v1.Tests) {
		t.Fatalf("v2 tests differ from the v1 twin:\n v2 = %+v\n v1 = %+v", v2.Tests, v1.Tests)
	}
	if v2.Tests == nil {
		t.Fatal("both twins folded zero tests — the tests equivalence assertion would be vacuous")
	}
	if !reflect.DeepEqual(v2.Coverage, v1.Coverage) {
		t.Fatalf("v2 coverage differs from the v1 twin:\n v2 = %+v\n v1 = %+v", v2.Coverage, v1.Coverage)
	}
}

// buildKeys renders a batch's build tasks as sorted, duration-free comparison
// keys. Sorting is required, not laziness: the scheduler is concurrent, so two
// independent runs legitimately terminate equal-cost tasks in different orders.
func buildKeys(batch *Batch) []string {
	if batch.Builds == nil {
		return nil
	}
	keys := make([]string, 0, len(batch.Builds.Tasks))
	for _, task := range batch.Builds.Tasks {
		keys = append(keys, fmt.Sprintf("%s|%s|%s|%t", task.Project, task.Task, task.Status, task.CacheHit))
	}
	sort.Strings(keys)
	return keys
}

// A v2 cache hit folds CacheHit exactly where v1's boolean did and reports its
// materialization wall time rather than its correctly-zero execution time.
// Coalescing is reuse but NOT a cache hit, and older v2 producers that omitted
// taskWallMs retain their durationMs replay measurement.
func TestV2ReuseFoldsOntoCacheHit(t *testing.T) {
	p := NewParser()
	for _, task := range []struct {
		reuse      string
		durationMS int
		taskWallMS int
	}{
		{reuse: "none", durationMS: 7, taskWallMS: 41},
		{reuse: "local-cache", taskWallMS: 42},
		{reuse: "remote-cache", taskWallMS: 43},
		{reuse: "coalesced", durationMS: 8, taskWallMS: 44},
		{reuse: "local-cache", durationMS: 9}, // pre-taskWallMs producer
	} {
		line := fmt.Sprintf(`{"protocolVersion":2,"record":"task:end","identity":{"key":"/a:test~test","scope":"project","project":{"id":"/a","name":"pkg/a"},"task":{"name":"test~test","command":"test","kind":"test-exec"},"provider":{"extension":"@putnami/go"}},"task":{"identity":{"key":"/a:test~test"},"status":"success","reuse":"%s","exitCode":0,"durationMs":%d,"taskWallMs":%d}}`, task.reuse, task.durationMS, task.taskWallMS)
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Builds == nil || len(batch.Builds.Tasks) != 5 {
		t.Fatalf("builds = %+v, want 5 tasks", batch.Builds)
	}
	wantHits := []bool{false, true, true, false, true}
	wantDurations := []int64{7, 42, 43, 8, 9}
	wantReuse := []string{"none", "local-cache", "remote-cache", "coalesced", "local-cache"}
	for i, task := range batch.Builds.Tasks {
		if task.CacheHit != wantHits[i] {
			t.Fatalf("task %d (%+v) cacheHit = %t, want %t", i, task, task.CacheHit, wantHits[i])
		}
		if task.Project != "pkg/a" || task.Task != "test~test" || task.DurationMS != wantDurations[i] {
			t.Fatalf("task %d = %+v", i, task)
		}
		if task.Reuse != wantReuse[i] {
			t.Fatalf("task %d (%+v) reuse = %q, want %q", i, task, task.Reuse, wantReuse[i])
		}
	}
}

// v2 metric, result and diagnostic events fold through the same aggregation the
// v1 job:event path uses — the inner runtime event object is identical, so the
// coverage and failing-case shapes the green capture never exercises are pinned
// here instead.
func TestV2TaskEventsFoldCoverageAndDiagnostics(t *testing.T) {
	p := NewParser()
	const identity = `"identity":{"key":"/a:test~test","scope":"project","project":{"id":"/a","name":"pkg/a"},"task":{"name":"test~test","command":"test","kind":"test-exec"},"provider":{"extension":"@putnami/go"}}`
	for _, line := range []string{
		`{"protocolVersion":2,"record":"task:start",` + identity + `}`,
		`{"protocolVersion":2,"record":"task:event",` + identity + `,"event":{"type":"metric","name":"tests-total","unit":"count","value":9}}`,
		`{"protocolVersion":2,"record":"task:event",` + identity + `,"event":{"type":"result","status":"FAIL","data":{"testSummary":{"passed":8,"failed":1,"skipped":0,"total":9},"coverageSummary":{"coveredStatements":40,"totalStatements":50,"percentage":80,"files":[{"file":"pkg/a/a.go","coveredStatements":40,"totalStatements":50,"percentage":80}]}}}}`,
		`{"protocolVersion":2,"record":"task:event",` + identity + `,"event":{"type":"diagnostic","code":"TEST_FAIL","severity":"error","message":"TestBoom failed","location":{"file":"pkg/a/boom_test.go","line":42}}}`,
		`{"protocolVersion":2,"record":"task:event",` + identity + `,"event":{"type":"diagnostic","code":"UNCOVERED_FILE","severity":"warning","message":"0% coverage","location":{"file":"pkg/a/cold.go"}}}`,
		`{"protocolVersion":2,"record":"session:end","run":{"outcome":"failure","exitCode":1,"counts":{"total":1,"succeeded":0,"failed":1,"canceled":0,"skipped":0},"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":11}}`,
	} {
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Stats != (Stats{Lines: 6}) {
		t.Fatalf("stats = %+v, want six clean v2 records", batch.Stats)
	}
	if batch.Tests == nil || len(batch.Tests.Projects) != 1 {
		t.Fatalf("tests = %+v", batch.Tests)
	}
	project := batch.Tests.Projects[0]
	if project.Project != "pkg/a" || project.Total != 9 || project.Passed != 8 || project.Failed != 1 {
		t.Fatalf("tests project = %+v, want the result summary to win over the metric", project)
	}
	if len(project.FailingCases) != 1 || project.FailingCases[0].Name != "TEST_FAIL" ||
		project.FailingCases[0].File != "pkg/a/boom_test.go" || project.FailingCases[0].Line != 42 {
		t.Fatalf("failing cases = %+v, want exactly the error diagnostic", project.FailingCases)
	}
	if batch.Coverage == nil || len(batch.Coverage.Projects) != 1 {
		t.Fatalf("coverage = %+v", batch.Coverage)
	}
	coverage := batch.Coverage.Projects[0]
	if coverage.Project != "pkg/a" || coverage.CoveredStatements != 40 || coverage.TotalStatements != 50 || coverage.Percentage != 80 {
		t.Fatalf("coverage = %+v", coverage)
	}
	if len(coverage.Files) != 1 || coverage.Files[0].File != "pkg/a/a.go" {
		t.Fatalf("coverage files = %+v", coverage.Files)
	}
	// session:end carries the run verdict in v2, but the batch is report data:
	// it must not fabricate a build task out of it, exactly as v1 did not.
	if batch.Builds != nil {
		t.Fatalf("builds = %+v, want none (no task:end in this stream)", batch.Builds)
	}
}

// Unknown v2 records and unknown inner event types are skipped and counted, and
// so is a record stamped with a protocol version this parser does not speak —
// forward compatibility without guessing at an unread contract.
func TestV2UnknownRecordsAndVersionsSkippedAndCounted(t *testing.T) {
	p := NewParser()
	const identity = `"identity":{"key":"/a:test~test","project":{"id":"/a","name":"pkg/a"},"task":{"name":"test~test"}}`
	for _, line := range []string{
		`{"protocolVersion":2,"record":"task:teleport",` + identity + `}`,
		`{"protocolVersion":2,"record":"task:event",` + identity + `,"event":{"type":"hologram"}}`,
		`{"protocolVersion":3,"record":"task:end",` + identity + `,"task":{"status":"success","reuse":"none","durationMs":1}}`,
		`{"protocolVersion":2,"record":"task:end",` + identity + `,"task":{"status":"success","reuse":"none","durationMs":1}}`,
	} {
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Stats != (Stats{Lines: 4, UnknownEvents: 3}) {
		t.Fatalf("stats = %+v, want 4 lines / 3 unknown", batch.Stats)
	}
	if batch.Builds == nil || len(batch.Builds.Tasks) != 1 {
		t.Fatalf("builds = %+v, want only the version-2 task:end folded", batch.Builds)
	}
}

// A v2 task record missing its required identity, report-key fields, or task
// member is malformed — fail-closed, like the runner framing above, rather than
// a build entry with empty or id-based names.
func TestV2IncompleteRecordsAreMalformed(t *testing.T) {
	p := NewParser()
	for _, line := range []string{
		`{"protocolVersion":2,"record":"task:end","task":{"status":"success","reuse":"none","durationMs":1}}`,
		`{"protocolVersion":2,"record":"task:end","identity":{"project":{"id":"/a","name":"pkg/a"},"task":{"name":"test~test"}}}`,
		`{"protocolVersion":2,"record":"task:event","event":{"type":"metric","name":"tests-total","value":3}}`,
		`{"protocolVersion":2,"record":"task:event","identity":{"project":{"id":"/a","name":"pkg/a"},"task":{"name":"test~test"}}}`,
		`{"protocolVersion":2,"record":"task:end","identity":{"project":{"id":"/a"},"task":{"name":"test~test"}},"task":{"status":"success","reuse":"none","durationMs":1}}`,
		`{"protocolVersion":2,"record":"task:end","identity":{"project":{"id":"/a","name":"pkg/a"},"task":{}},"task":{"status":"success","reuse":"none","durationMs":1}}`,
		`{"protocolVersion":2,"record":"task:event","identity":{"project":{"id":"/a"},"task":{"name":"test~test"}},"event":{"type":"metric","name":"tests-total","value":3}}`,
		`{"protocolVersion":2,"record":"task:event","identity":{"project":{"id":"/a","name":"pkg/a"},"task":{}},"event":{"type":"metric","name":"tests-total","value":3}}`,
	} {
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Stats != (Stats{Lines: 8, MalformedLines: 8}) {
		t.Fatalf("stats = %+v, want eight malformed v2 records", batch.Stats)
	}
	if batch.HasReports() {
		t.Fatalf("incomplete v2 records fabricated reports: %+v", batch)
	}
}

// Version detection is per line, so a file that interleaves both contracts (not
// a real case, but reachable through a concatenated capture) folds both halves
// and never panics.
func TestMixedVersionStreamFoldsBothAndNeverPanics(t *testing.T) {
	p := NewParser()
	for _, line := range []string{
		`{"kind":"ci.runner.start","runId":"run_1","workspaceId":"ws_1","sha":"abc123","cacheUrl":"https://cache.test"}`,
		`{"event":"job:end","package":"pkg/v1","job":"test~test","status":"success","duration":5,"cache":true}`,
		`{"protocolVersion":2,"record":"task:end","identity":{"key":"/b:test~test","project":{"id":"/b","name":"pkg/v2"},"task":{"name":"test~test"}},"task":{"status":"failed","reuse":"none","exitCode":1,"durationMs":9}}`,
		`{"event":"session:end","success":true,"succeeded":1,"failed":0,"canceled":0,"skipped":0,"cached":0,"total":1,"duration":5,"failures":[]}`,
		`{"protocolVersion":2,"record":"session:end","run":{"outcome":"failure","exitCode":1,"counts":{"total":1,"succeeded":0,"failed":1,"canceled":0,"skipped":0},"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":9}}`,
	} {
		_, _ = p.Write([]byte(line + "\n"))
	}
	batch := p.Batch()
	if batch.Stats != (Stats{Lines: 5}) {
		t.Fatalf("stats = %+v, want five recognized lines across both contracts", batch.Stats)
	}
	if batch.Builds == nil || len(batch.Builds.Tasks) != 2 {
		t.Fatalf("builds = %+v, want one task from each contract", batch.Builds)
	}
	if got := batch.Builds.Tasks[0]; got.Project != "pkg/v1" || !got.CacheHit || got.Status != "success" {
		t.Fatalf("v1 half = %+v", got)
	} else if got.Reuse != "" {
		t.Fatalf("v1 half reuse = %q, want empty because v1 cannot prove exact provenance", got.Reuse)
	}
	if got := batch.Builds.Tasks[1]; got.Project != "pkg/v2" || got.CacheHit || got.Status != "failed" || got.DurationMS != 9 {
		t.Fatalf("v2 half = %+v", got)
	}
}
