package output

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// devVerbTraits are the traits the @putnami/dev vocabulary gives the verbs
// whose cells read a kind, a side effect or a workload.
var devVerbTraits = map[string]extensionproto.CommandTraits{
	"build":    {Kind: extensionproto.CommandKindFiles},
	"test":     {Kind: extensionproto.CommandKindVerification},
	"lint":     {Kind: extensionproto.CommandKindDiagnostics},
	"validate": {Kind: extensionproto.CommandKindCriteria},
	"serve":    {Workload: extensionproto.WorkloadServe},
	"run":      {Workload: extensionproto.WorkloadOnce},
	"publish":  {SideEffects: extensionproto.SideEffectsRegistry},
	"deploy":   {SideEffects: extensionproto.SideEffectsCloud},
}

// makeLiveTestJob plans jobName with the traits @putnami/dev gives its verb.
func makeLiveTestJob(projectName, jobName string) *jobs.ScheduledJob {
	return makeLiveTestJobWithTraits(projectName, jobName, devVerbTraits[stripStepSuffix(jobName)])
}

func makeLiveTestJobWithTraits(projectName, jobName string, traits extensionproto.CommandTraits) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: projectName, Name: projectName},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: jobName, Traits: traits},
	}
}

func makeCacheableLiveTestJob(projectName, jobName string) *jobs.ScheduledJob {
	job := makeLiveTestJob(projectName, jobName)
	job.JobDef.Cache = true
	task := "task-" + jobName
	job.Extension.Tasks = map[string]extension.TaskDefinition{
		task: {Declares: &extension.TaskDeclaration{}},
	}
	job.Step = &extension.PipelineStep{Task: task}
	return job
}

// newTestRenderer builds a renderer with colors globally disabled, a
// deterministic width, and a stopped redraw loop, ready for direct
// row/summary assertions.
func newTestRenderer(t *testing.T, width int, planned ...*jobs.ScheduledJob) (*LiveRenderer, *bytes.Buffer) {
	t.Helper()
	initial := ColorsEnabled()
	SetColorsEnabled(false)
	t.Cleanup(func() { SetColorsEnabled(initial) })

	var out, errOut bytes.Buffer
	r := NewLiveRenderer(&out, &errOut)
	r.width = width
	r.Start(planned)
	stopSpinner(r)
	return r, &errOut
}

func newTestRendererWithCoverage(
	t *testing.T,
	width int,
	replay map[string]CoverageReplay,
	planned ...*jobs.ScheduledJob,
) (*LiveRenderer, *bytes.Buffer) {
	t.Helper()
	initial := ColorsEnabled()
	SetColorsEnabled(false)
	t.Cleanup(func() { SetColorsEnabled(initial) })

	var out, errOut bytes.Buffer
	r := NewLiveRenderer(&out, &errOut)
	r.coverageReplay = replay
	r.width = width
	r.Start(planned)
	stopSpinner(r)
	return r, &errOut
}

// stopSpinner halts and joins the redraw goroutine so tests have stable output
// and can read renderer state without racing.
func stopSpinner(r *LiveRenderer) {
	r.stopRedraw()
}

func metricEvent(name string, value float64, unit string) jobs.RawJobEvent {
	return jobs.RawJobEvent{
		Type: jobs.EventTypeMetric,
		Data: map[string]any{"name": name, "value": value, "unit": unit},
	}
}

// --- formatting helpers ---

func TestFormatClock(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "--:--"},
		{-time.Second, "--:--"},
		{41 * time.Second, "00:41"},
		{72 * time.Second, "01:12"},
		{123 * time.Second, "02:03"},
		{3723 * time.Second, "1:02:03"},
	}
	for _, tt := range tests {
		if got := formatClock(tt.d); got != tt.want {
			t.Errorf("formatClock(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFormatSessionDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{5 * time.Second, "5s"},
		{252 * time.Second, "4m12s"},
		{3661 * time.Second, "1h01m01s"},
	}
	for _, tt := range tests {
		if got := formatSessionDuration(tt.d); got != tt.want {
			t.Errorf("formatSessionDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestProjectElapsedTracksActiveWorkOnly(t *testing.T) {
	r, _ := newTestRenderer(t, 120,
		makeLiveTestJob("app", "lint"),
		makeLiveTestJob("app", "build"),
	)
	row := r.rowByID["app"]
	base := time.Unix(1000, 0)

	if got := r.elapsed(row); got != "--:--" {
		t.Fatalf("not-started elapsed = %q, want --:--", got)
	}

	r.startProjectWork(row, base)
	r.finishProjectWork(row, base.Add(2*time.Second))

	if got := r.projectElapsed(row, base.Add(2*time.Minute)); got != 2*time.Second {
		t.Fatalf("elapsed should freeze while waiting, got %s", got)
	}
	if got := r.elapsed(row); got != "00:02" {
		t.Fatalf("rendered elapsed = %q, want 00:02", got)
	}

	r.startProjectWork(row, base.Add(2*time.Minute))
	if got := r.projectElapsed(row, base.Add(2*time.Minute+3*time.Second)); got != 5*time.Second {
		t.Fatalf("elapsed should include active second command only, got %s", got)
	}
}

func TestProjectElapsedDoesNotDoubleCountConcurrentJobs(t *testing.T) {
	r, _ := newTestRenderer(t, 120,
		makeLiveTestJob("app", "test"),
		makeLiveTestJob("app", "build"),
	)
	row := r.rowByID["app"]
	base := time.Unix(1000, 0)

	r.startProjectWork(row, base)
	r.startProjectWork(row, base.Add(time.Second))

	if got := r.projectElapsed(row, base.Add(3*time.Second)); got != 3*time.Second {
		t.Fatalf("elapsed while two jobs run = %s, want 3s", got)
	}

	r.finishProjectWork(row, base.Add(4*time.Second))
	if got := r.projectElapsed(row, base.Add(7*time.Second)); got != 7*time.Second {
		t.Fatalf("elapsed after one concurrent job finishes = %s, want 7s", got)
	}

	r.finishProjectWork(row, base.Add(8*time.Second))
	if got := r.projectElapsed(row, base.Add(2*time.Minute)); got != 8*time.Second {
		t.Fatalf("final elapsed = %s, want 8s", got)
	}
}

func TestFormatThousands(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{2418, "2,418"},
		{1000000, "1,000,000"},
		{-2418, "-2,418"},
	}
	for _, tt := range tests {
		if got := formatThousands(tt.n); got != tt.want {
			t.Errorf("formatThousands(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate no-op = %q", got)
	}
	if got := truncate("hello world", 5); got != "hell…" {
		t.Errorf("truncate = %q, want %q", got, "hell…")
	}
}

// A command no loaded vocabulary names ranks after every verb it names, and
// the renderer orders those commands alphabetically: without a vocabulary,
// every column is in alphabetical order.
func TestColumnsNoVocabularyNamesRankAfterItAlphabetically(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "batch-parity-and-live-cells-follow-declared-kinds",
		"columns-no-vocabulary-names-rank-after-it-alphabetically")
	initial := ColorsEnabled()
	SetColorsEnabled(false)
	t.Cleanup(func() { SetColorsEnabled(initial) })

	names := []string{"serve", "unknown", "lint", "generate", "deploy", "audit", "test", "format", "build"}
	for _, test := range []struct {
		vocabulary []string
		want       []string
	}{
		{nil, []string{"audit", "build", "deploy", "format", "generate", "lint", "serve", "test", "unknown"}},
		{
			[]string{"build", "test", "audit", "lint"},
			[]string{"build", "test", "audit", "lint", "deploy", "format", "generate", "serve", "unknown"},
		},
	} {
		r := NewLiveRenderer(&bytes.Buffer{}, &bytes.Buffer{})
		r.verbOrder = test.vocabulary
		r.width = 400
		planned := make([]*jobs.ScheduledJob, 0, len(names))
		for _, name := range names {
			planned = append(planned, makeLiveTestJob("app", name))
		}
		r.Start(planned)
		stopSpinner(r)

		if !slices.Equal(r.colOrder, test.want) {
			t.Errorf("vocabulary %v: column order = %v, want %v", test.vocabulary, r.colOrder, test.want)
		}
		if !slices.Equal(r.rowByID["app"].commandOrder, test.want) {
			t.Errorf("vocabulary %v: row command order = %v, want %v", test.vocabulary, r.rowByID["app"].commandOrder, test.want)
		}
		if got := r.taskCommandOrder(); !slices.Equal(got, test.want) {
			t.Errorf("vocabulary %v: summary command order = %v, want %v", test.vocabulary, got, test.want)
		}
	}

	// A short vocabulary leaves an unknown command at the floor; a longer one
	// pushes it past the last verb.
	if got := commandRank(nil, "unknown"); got != unknownCommandRank {
		t.Errorf("unknown command rank = %d, want the floor %d", got, unknownCommandRank)
	}
	long := make([]string, unknownCommandRank+1)
	for i := range long {
		long[i] = fmt.Sprintf("verb-%03d", i)
	}
	if commandRank(long, "unknown") <= commandRank(long, long[len(long)-1]) {
		t.Error("a command no vocabulary names must rank after a vocabulary of more than 100 verbs")
	}
}

// TestLiveColumnsFollowTheVocabulary: the renderer orders each row's commands
// and the summary columns by the vocabulary it was given.
func TestLiveColumnsFollowTheVocabulary(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-verb-vocabulary", "the-live-view-orders-columns-by-the-loaded-vocabulary")
	initial := ColorsEnabled()
	SetColorsEnabled(false)
	t.Cleanup(func() { SetColorsEnabled(initial) })

	r := NewLiveRenderer(&bytes.Buffer{}, &bytes.Buffer{})
	r.verbOrder = []string{"build", "test"}
	r.width = 200
	r.Start([]*jobs.ScheduledJob{
		makeLiveTestJob("app", "lint"),
		makeLiveTestJob("app", "test"),
		makeLiveTestJob("app", "build"),
	})
	stopSpinner(r)

	want := []string{"build", "test", "lint"}
	if !slices.Equal(r.rowByID["app"].commandOrder, want) {
		t.Errorf("row command order = %v, want %v", r.rowByID["app"].commandOrder, want)
	}
	if !slices.Equal(r.colOrder, want) {
		t.Errorf("column order = %v, want %v", r.colOrder, want)
	}
	if got := r.taskCommandOrder(); !slices.Equal(got, want) {
		t.Errorf("summary command order = %v, want %v", got, want)
	}
}

func TestStripStepSuffix(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"build", "build"},
		{"build~generate", "build"},
		{"lint", "lint"},
	} {
		if got := stripStepSuffix(tt.in); got != tt.want {
			t.Errorf("stripStepSuffix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStepName(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"build", ""},
		{"build~generate", "generate"},
		{"build~transpile", "transpile"},
	} {
		if got := stepName(tt.in); got != tt.want {
			t.Errorf("stepName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- project model ---

func TestProjectRowsBuiltFromPlan(t *testing.T) {
	// Two projects, each with lint + test + build.
	var planned []*jobs.ScheduledJob
	for _, p := range []string{"app", "lib"} {
		for _, c := range []string{"build", "lint", "test"} {
			planned = append(planned, makeLiveTestJob(p, c))
		}
	}
	r, _ := newTestRenderer(t, 120, planned...)

	if len(r.rows) != 2 {
		t.Fatalf("expected 2 project rows, got %d", len(r.rows))
	}
	// No vocabulary is loaded, so the commands sort by name.
	app := r.rowByID["app"]
	want := []string{"build", "lint", "test"}
	if strings.Join(app.commandOrder, ",") != strings.Join(want, ",") {
		t.Errorf("command order = %v, want %v", app.commandOrder, want)
	}
}

func TestStableProjectColors(t *testing.T) {
	r, _ := newTestRenderer(t, 120,
		makeLiveTestJob("app", "build"),
		makeLiveTestJob("lib", "build"),
		makeLiveTestJob("web", "build"),
	)
	if r.rowByID["app"].color != projectColorPalette[0] {
		t.Errorf("app color = %q, want %q", r.rowByID["app"].color, projectColorPalette[0])
	}
	if r.rowByID["lib"].color != projectColorPalette[1] {
		t.Errorf("lib color = %q, want %q", r.rowByID["lib"].color, projectColorPalette[1])
	}
	if r.rowByID["web"].color != projectColorPalette[2] {
		t.Errorf("web color = %q, want %q", r.rowByID["web"].color, projectColorPalette[2])
	}
}

// TestUpstreamNotActiveWhileBlocked verifies the core fix: a project that only
// exists as a dependency is "running" while it works, and the downstream target
// shows "blocked", not active.
func TestUpstreamNotActiveWhileBlocked(t *testing.T) {
	app := makeLiveTestJob("app", "build")
	lib := makeLiveTestJob("lib", "build")
	app.DependsOn = []string{"lib:build"} // app waits on lib

	r, _ := newTestRenderer(t, 120, app, lib)

	// Before anything starts: app is blocked by lib, lib is queued.
	if got := r.projectStatus(r.rowByID["app"]); got != statusBlocked {
		t.Errorf("app status before start = %q, want blocked", got)
	}
	if got := r.blockedBy(r.rowByID["app"]); got != "lib" {
		t.Errorf("app blockedBy = %q, want lib", got)
	}
	if got := r.projectStatus(r.rowByID["lib"]); got != statusQueued {
		t.Errorf("lib status before start = %q, want queued", got)
	}

	// lib starts its own work → lib running, app still blocked (not active).
	r.JobStart(lib)
	if got := r.projectStatus(r.rowByID["lib"]); got != statusRunning {
		t.Errorf("lib status while working = %q, want running", got)
	}
	if got := r.projectStatus(r.rowByID["app"]); got != statusBlocked {
		t.Errorf("app status while lib works = %q, want blocked", got)
	}

	// lib finishes → app is unblocked (queued), then runs.
	r.JobComplete(lib, &jobs.JobResult{Status: "success"})
	if got := r.projectStatus(r.rowByID["app"]); got != statusQueued {
		t.Errorf("app status after lib done = %q, want queued", got)
	}
	r.JobStart(app)
	if got := r.projectStatus(r.rowByID["app"]); got != statusRunning {
		t.Errorf("app status while working = %q, want running", got)
	}
}

func TestProjectStatusLifecycle(t *testing.T) {
	job := makeLiveTestJob("app", "build")
	r, _ := newTestRenderer(t, 120, job)

	if got := r.projectStatus(r.rowByID["app"]); got != statusQueued {
		t.Errorf("initial = %q, want queued", got)
	}
	r.JobStart(job)
	if got := r.projectStatus(r.rowByID["app"]); got != statusRunning {
		t.Errorf("started = %q, want running", got)
	}
	r.JobComplete(job, &jobs.JobResult{Status: "success"})
	if got := r.projectStatus(r.rowByID["app"]); got != statusDone {
		t.Errorf("completed = %q, want done", got)
	}
}

func TestProjectStatusFailedAndSkipped(t *testing.T) {
	fail := makeLiveTestJob("app", "test")
	skip := makeLiveTestJob("lib", "test")
	r, _ := newTestRenderer(t, 120, fail, skip)

	r.JobStart(fail)
	r.JobComplete(fail, &jobs.JobResult{Status: "failed"})
	if got := r.projectStatus(r.rowByID["app"]); got != statusFailed {
		t.Errorf("app = %q, want failed", got)
	}

	r.JobStart(skip)
	r.JobComplete(skip, &jobs.JobResult{Status: "skipped"})
	if got := r.projectStatus(r.rowByID["lib"]); got != statusSkipped {
		t.Errorf("lib = %q, want skipped", got)
	}
}

func TestLiveFinishPreservesFailedPartialPublish(t *testing.T) {
	docker := makeLiveTestJob("auth/server", "publish~docker")
	config := makeLiveTestJob("auth/server", "publish~cloud-publish-config")
	later := makeLiveTestJob("auth/server", "publish~config")
	r, buf := newTestRenderer(t, 160, docker, config, later)

	r.JobStart(docker)
	r.JobComplete(docker, &jobs.JobResult{Status: "success"})
	r.JobStart(config)
	r.JobComplete(config, &jobs.JobResult{
		Status: "failed",
		Error:  &jobs.JobError{Message: "Invalid or expired refresh token. Run `putnami cloud login`."},
	})

	r.Finish(map[string]*jobs.JobResult{
		docker.Key(): {Status: "success"},
		config.Key(): {
			Status: "failed",
			Error:  &jobs.JobError{Message: "Invalid or expired refresh token. Run `putnami cloud login`."},
		},
		later.Key(): {Status: "canceled"},
	}, jobs.SessionOutcome{})

	out := buf.String()
	if !strings.Contains(out, "auth/server") || !strings.Contains(out, "failed") {
		t.Fatalf("final live output should show project failure, got:\n%s", out)
	}
	if !strings.Contains(out, "publish") || !strings.Contains(out, "1 failed") {
		t.Fatalf("tasks summary should report failed publish, got:\n%s", out)
	}
	if strings.Contains(out, "publish  skipped") {
		t.Fatalf("failed partial publish must not be summarized as skipped:\n%s", out)
	}
	if !strings.Contains(out, "Invalid or expired refresh token") || !strings.Contains(out, "putnami cloud login") {
		t.Fatalf("normal output should include actionable auth error, got:\n%s", out)
	}
}

func TestCacheHitCountsAsDone(t *testing.T) {
	job := makeLiveTestJob("app", "build")
	r, _ := newTestRenderer(t, 120, job)
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{CacheHit: true})
	if got := r.projectStatus(r.rowByID["app"]); got != statusDone {
		t.Errorf("cached project = %q, want done", got)
	}
}

func TestCoalescedCountsAsDoneAndStaysDistinct(t *testing.T) {
	job := makeLiveTestJob("app", "build")
	r, _ := newTestRenderer(t, 120, job)
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{Status: "success", Coalesced: true})
	row := r.rowByID["app"]
	if got := r.projectStatus(row); got != statusDone {
		t.Errorf("coalesced project = %q, want done", got)
	}
	if got := row.commands["build"].status; got != statusCoalesced {
		t.Errorf("coalesced command = %q, want %q", got, statusCoalesced)
	}
	if rendered := r.renderRow(row); !strings.Contains(rendered, statusCoalesced) {
		t.Errorf("coalesced row did not expose outcome: %s", rendered)
	}
}

func TestCoalescedCellsIgnoreReplayedMetricsAndSummaries(t *testing.T) {
	lint := makeLiveTestJob("app", "lint")
	test := makeLiveTestJob("app", "test")
	build := makeLiveTestJob("app", "build")
	r, _ := newTestRenderer(t, 120, lint, test, build)

	cases := []struct {
		job    *jobs.ScheduledJob
		events []jobs.RawJobEvent
		hidden string
	}{
		{lint, []jobs.RawJobEvent{
			metricEvent("lint-warnings", 2, "count"),
			{Type: jobs.EventTypeSummary, Data: map[string]any{"message": "lint replay"}},
		}, "2 warn"},
		{test, []jobs.RawJobEvent{
			metricEvent("tests-total", 45, "count"),
			metricEvent("tests-passed", 42, "count"),
			{Type: jobs.EventTypeSummary, Data: map[string]any{"message": "test replay"}},
		}, "42/45"},
		{build, []jobs.RawJobEvent{
			metricEvent("transpiled-files", 2, "count"),
			{Type: jobs.EventTypeSummary, Data: map[string]any{"message": "build replay"}},
		}, "2 files"},
	}
	for _, tc := range cases {
		r.JobStart(tc.job)
		r.JobComplete(tc.job, &jobs.JobResult{Status: "success", Coalesced: true, Events: tc.events})
	}

	rendered := r.renderRow(r.rowByID["app"])
	if got := strings.Count(rendered, statusCoalesced); got != len(cases) {
		t.Fatalf("coalesced markers = %d, want %d: %s", got, len(cases), rendered)
	}
	for _, tc := range cases {
		if strings.Contains(rendered, tc.hidden) {
			t.Errorf("replayed metric %q replaced coalesced marker: %s", tc.hidden, rendered)
		}
	}
}

func TestCachedCellsUseRestoredMetrics(t *testing.T) {
	lint := makeLiveTestJob("core-api", "lint")
	test := makeLiveTestJob("core-api", "test")
	build := makeLiveTestJob("core-api", "build")
	r, _ := newTestRenderer(t, 120, lint, test, build)

	r.JobStart(lint)
	r.JobComplete(lint, &jobs.JobResult{Status: "success", CacheHit: true, Events: []jobs.RawJobEvent{
		metricEvent("lint-warnings", 2, "count"),
	}})
	r.JobStart(test)
	r.JobComplete(test, &jobs.JobResult{Status: "success", CacheHit: true, Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 51, "count"),
		metricEvent("tests-passed", 51, "count"),
		metricEvent("coverage", 84.0, "percent"),
	}})
	r.JobStart(build)
	r.JobComplete(build, &jobs.JobResult{Status: "success", CacheHit: true, Events: []jobs.RawJobEvent{
		metricEvent("transpiled-files", 38, "count"),
	}})

	row := r.renderRow(r.rowByID["core-api"])
	if strings.Contains(row, "\x1b[") {
		t.Fatalf("cached row contains ANSI despite no-color rendering: %q", row)
	}
	if got := strings.Count(row, "↻"); got != 3 {
		t.Errorf("cached markers = %d, want one plain-text marker per cached command: %s", got, row)
	}
	for _, want := range []string{"2 warn", "51/51 84.0%", "38 files"} {
		if !strings.Contains(row, want) {
			t.Errorf("expected cached row to include %q: %s", want, row)
		}
	}
}

func TestFullyCachedCommandKeepsCachedGlyphWithAlwaysRunTail(t *testing.T) {
	compile := makeCacheableLiveTestJob("app", "build~compile")
	infra := makeLiveTestJob("app", "build~infra")
	// Cache: true alone is not enough: without a task-owned declaration, the
	// scheduler must execute this step and the renderer must show that tail.
	infra.JobDef.Cache = true
	r, _ := newTestRenderer(t, 120, compile, infra)

	r.JobStart(compile)
	r.JobComplete(compile, &jobs.JobResult{
		Status:   "success",
		CacheHit: true,
		Events:   []jobs.RawJobEvent{metricEvent("transpiled-files", 38, "count")},
	})
	r.JobStart(infra)
	r.JobComplete(infra, &jobs.JobResult{Status: "success", Duration: 10 * time.Millisecond})

	cmd := r.rowByID["app"].commands["build"]
	glyph, value := r.cellValue(cmd)
	if glyph.text != "↻" || glyph.color != Dim || value.color != Dim {
		t.Fatalf("fully cached rollup styling = glyph %+v, value %+v", glyph, value)
	}
	if value.text != "38 files (+infra 10ms)" {
		t.Fatalf("fully cached rollup = %q", value.text)
	}
	if row := r.renderRow(r.rowByID["app"]); !strings.Contains(row, "build ↻ 38 files (+infra 10ms)") {
		t.Fatalf("always-run provenance was truncated from cached command: %s", row)
	}
}

func TestFreshCacheableStepKeepsExecutedGlyph(t *testing.T) {
	compile := makeCacheableLiveTestJob("app", "build~compile")
	infra := makeLiveTestJob("app", "build~infra")
	r, _ := newTestRenderer(t, 120, compile, infra)

	for _, job := range []*jobs.ScheduledJob{compile, infra} {
		r.JobStart(job)
		r.JobComplete(job, &jobs.JobResult{Status: "success", Duration: 10 * time.Millisecond})
	}

	glyph, value := r.cellValue(r.rowByID["app"].commands["build"])
	if glyph.text != "✓" || value.text != "ok" {
		t.Fatalf("fresh command = glyph %+v, value %+v", glyph, value)
	}
}

func TestCachedCoverageInheritsDimmedReplayStyling(t *testing.T) {
	testJob := makeLiveTestJob("app", "test")
	testJob.JobDef.Cache = true
	r, _ := newTestRenderer(t, 120, testJob)
	r.JobStart(testJob)
	r.JobComplete(testJob, &jobs.JobResult{Status: "success", CacheHit: true, Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 51, "count"),
		metricEvent("tests-passed", 51, "count"),
		metricEvent("coverage", 84, "percent"),
	}})

	SetColorsEnabled(true)
	t.Cleanup(func() { SetColorsEnabled(false) })
	// Coverage lives in the test cell, so a cache hit dims it as part of one
	// replayed value instead of styling a column of its own.
	if cell := r.summaryCell(r.rowByID["app"], "test"); !strings.Contains(cell, colorize("51/51 84.0%", Dim)) {
		t.Fatalf("cached coverage was not dimmed as replay: %q", cell)
	}
}

// TestCoverageRidesInTestCell pins the rule that made the percentage visible
// again: it is part of the test cell's value, not a separate droppable column.
// A narrow terminal that keeps `test` therefore always keeps the coverage.
func TestCoverageRidesInTestCell(t *testing.T) {
	testJob := makeLiveTestJob("app", "test")
	r, _ := newTestRenderer(t, 120, testJob)
	r.JobStart(testJob)
	r.JobComplete(testJob, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 285, "count"),
		metricEvent("tests-passed", 285, "count"),
		metricEvent("coverage", 80.78, "percent"),
	}})

	if cell := r.summaryCell(r.rowByID["app"], "test"); !strings.Contains(cell, "285/285 80.8%") {
		t.Fatalf("coverage did not render beside the test counts: %q", cell)
	}
	if slices.Contains(r.colOrder, "cov") {
		t.Fatal("the standalone cov column must be gone; it was the first cell dropped on a narrow terminal")
	}
}

// TestTestCellWithoutCoverageIsUnchanged: a project that opted out with
// coverage:false still shows plain counts, with no stray percentage or spacing.
func TestTestCellWithoutCoverageIsUnchanged(t *testing.T) {
	testJob := makeLiveTestJob("app", "test")
	r, _ := newTestRenderer(t, 120, testJob)
	r.JobStart(testJob)
	r.JobComplete(testJob, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 12, "count"),
		metricEvent("tests-passed", 12, "count"),
	}})

	cell := r.summaryCell(r.rowByID["app"], "test")
	if !strings.Contains(cell, "12/12") || strings.Contains(cell, "%") {
		t.Fatalf("uninstrumented test cell should show bare counts: %q", cell)
	}
}

// --- row rendering ---

func TestRenderRowOneLinePerProject(t *testing.T) {
	a := makeLiveTestJob("app", "build")
	b := makeLiveTestJob("lib", "build")
	r, errOut := newTestRenderer(t, 120, a, b)

	r.JobStart(a)
	r.JobComplete(a, &jobs.JobResult{Status: "success"})
	r.JobStart(b)
	r.JobComplete(b, &jobs.JobResult{Status: "success"})

	r.Finish(map[string]*jobs.JobResult{
		a.Key(): {Status: "success"},
		b.Key(): {Status: "success"},
	}, jobs.SessionOutcome{})
	out := errOut.String()

	// No per-task / pipeline-step lines in normal mode.
	if strings.Contains(out, "~build") || strings.Contains(out, "build~") {
		t.Errorf("output should not contain per-task step lines: %s", out)
	}
	// One row per project + a global summary.
	for _, name := range []string{"app", "lib"} {
		if !strings.Contains(out, name) {
			t.Errorf("expected project %q in output: %s", name, out)
		}
	}
	if !strings.Contains(out, "2 succeeded") {
		t.Errorf("expected '2 succeeded' in summary: %s", out)
	}
}

func TestRenderRowBlockedHint(t *testing.T) {
	app := makeLiveTestJob("app", "build")
	lib := makeLiveTestJob("lib", "build")
	app.DependsOn = []string{"lib:build"}
	r, _ := newTestRenderer(t, 120, app, lib)

	row := r.renderRow(r.rowByID["app"])
	if !strings.Contains(row, "blocked") {
		t.Errorf("expected 'blocked' status: %s", row)
	}
	if !strings.Contains(row, "blocked by lib") {
		t.Errorf("expected 'blocked by lib' hint: %s", row)
	}
}

func TestRenderRowQueuedCells(t *testing.T) {
	r, _ := newTestRenderer(t, 120,
		makeLiveTestJob("app", "lint"),
		makeLiveTestJob("app", "test"),
		makeLiveTestJob("app", "build"),
	)
	row := r.renderRow(r.rowByID["app"])
	// Not-started commands show "queued" cells.
	if strings.Count(row, "queued") < 3 {
		t.Errorf("expected each command cell to show queued: %s", row)
	}
}

func TestRenderRowCompactSummaries(t *testing.T) {
	lint := makeLiveTestJob("core-api", "lint")
	test := makeLiveTestJob("core-api", "test")
	build := makeLiveTestJob("core-api", "build")
	r, _ := newTestRenderer(t, 120, lint, test, build)

	r.JobStart(lint)
	r.JobComplete(lint, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("lint-warnings", 2, "count"),
	}})
	r.JobStart(test)
	r.JobComplete(test, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 220, "count"),
		metricEvent("tests-passed", 220, "count"),
		metricEvent("coverage", 78.4, "percent"),
	}})
	r.JobStart(build)
	r.JobComplete(build, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("transpiled-files", 38, "count"),
	}})

	row := r.renderRow(r.rowByID["core-api"])
	for _, want := range []string{"lint", "2 warn", "test", "220/220 78.4%", "build", "38 files"} {
		if !strings.Contains(row, want) {
			t.Errorf("expected %q in row: %s", want, row)
		}
	}
}

func TestRenderRowProjectNameUsesAvailableWidth(t *testing.T) {
	name := "@example/04-configuration-and-options"
	test := makeLiveTestJob(name, "test")
	build := makeLiveTestJob(name, "build")
	r, _ := newTestRenderer(t, 140, test, build)

	r.JobStart(test)
	r.JobComplete(test, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 3, "count"),
		metricEvent("tests-passed", 3, "count"),
	}})
	r.JobStart(build)
	r.JobComplete(build, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("transpiled-files", 14, "count"),
	}})

	row := r.renderRow(r.rowByID[name])
	if !strings.Contains(row, name) {
		t.Fatalf("wide row should keep full project name, got: %s", row)
	}
	if strings.Contains(row, "…") {
		t.Fatalf("wide row should not truncate project name, got: %s", row)
	}
}

func TestRenderRowProjectNameTruncatesWhenWidthIsNeeded(t *testing.T) {
	name := "@example/04-configuration-and-options"
	test := makeLiveTestJob(name, "test")
	build := makeLiveTestJob(name, "build")
	r, _ := newTestRenderer(t, 72, test, build)

	row := r.renderRow(r.rowByID[name])
	if !strings.Contains(row, "…") {
		t.Fatalf("narrow row should truncate project name, got: %s", row)
	}
	if strings.Contains(row, name) {
		t.Fatalf("narrow row should not contain full project name, got: %s", row)
	}
}

func TestRenderRowRunningShowsPhaseAndProgress(t *testing.T) {
	test := makeLiveTestJob("app", "test")
	r, _ := newTestRenderer(t, 120, test)
	r.JobStart(test)
	r.JobEvent(test, jobs.RawJobEvent{
		Type: jobs.EventTypeProgress,
		Data: map[string]any{"current": float64(184), "total": float64(220)},
	})

	row := r.renderRow(r.rowByID["app"])
	// Status column shows the running command name.
	if !strings.Contains(row, "test") {
		t.Errorf("expected running command in status column: %s", row)
	}
	// Running cell shows live progress.
	if !strings.Contains(row, "184/220") {
		t.Errorf("expected progress 184/220 in row: %s", row)
	}
}

func TestRenderRowFailedSummary(t *testing.T) {
	test := makeLiveTestJob("billing", "test")
	r, _ := newTestRenderer(t, 120, test)
	r.JobStart(test)
	r.JobComplete(test, &jobs.JobResult{Status: "failed", Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 214, "count"),
		metricEvent("tests-passed", 211, "count"),
		metricEvent("tests-failed", 3, "count"),
	}})

	row := r.renderRow(r.rowByID["billing"])
	if !strings.Contains(row, "failed") {
		t.Errorf("expected failed status: %s", row)
	}
	if !strings.Contains(row, "3 failed") {
		t.Errorf("expected '3 failed' cell summary: %s", row)
	}
}

// --- header ---

func TestRenderHeaderCounts(t *testing.T) {
	running := makeLiveTestJob("a", "build")
	done := makeLiveTestJob("b", "build")
	blocked := makeLiveTestJob("c", "build")
	blocked.DependsOn = []string{"a:build"}
	r, _ := newTestRenderer(t, 120, running, done, blocked)

	r.JobStart(running)
	r.JobStart(done)
	r.JobComplete(done, &jobs.JobResult{Status: "success"})

	r.start = time.Unix(1000, 0)
	header := r.renderHeaderAt(r.start.Add(72 * time.Second))
	for _, want := range []string{"impacted: 3 projects", "elapsed: 01:12", "running: 1", "done: 1", "blocked: 1", "failed: 0"} {
		if !strings.Contains(header, want) {
			t.Errorf("expected %q in header: %s", want, header)
		}
	}
}

func TestSessionElapsedNotStarted(t *testing.T) {
	r := NewLiveRenderer(&bytes.Buffer{}, &bytes.Buffer{})
	if got := r.sessionElapsed(time.Unix(1000, 0)); got != "--:--" {
		t.Fatalf("sessionElapsed without start = %q, want --:--", got)
	}
}

func TestLiveRowsPrioritizeWorkInProgressWhenOverflow(t *testing.T) {
	var planned []*jobs.ScheduledJob
	for _, name := range []string{"done-0", "done-1", "done-2", "done-3", "active", "done-4"} {
		planned = append(planned, makeLiveTestJob(name, "build"))
	}
	r, _ := newTestRenderer(t, 120, planned...)

	for _, job := range planned {
		if job.Project.Name == "active" {
			continue
		}
		r.JobStart(job)
		r.JobComplete(job, &jobs.JobResult{Status: "success"})
	}
	active := r.rowByID["active"]
	r.JobStart(planned[4])

	visible, hidden := r.liveRowsAt(r.sortedRows(), 3, time.Now())
	if hidden != 0 {
		t.Fatalf("hidden = %d, want 0", hidden)
	}
	for _, row := range visible {
		if row == active {
			return
		}
	}
	t.Fatalf("active row should remain visible, got %v", rowNames(visible))
}

func TestLiveRowsHiddenCountIncludesOnlyNonDoneRowsOutsideView(t *testing.T) {
	done0 := makeLiveTestJob("done-0", "build")
	failed := makeLiveTestJob("failed", "build")
	running := makeLiveTestJob("running", "build")
	blocked := makeLiveTestJob("blocked", "build")
	blocked.DependsOn = []string{"running:build"}
	queued0 := makeLiveTestJob("queued-0", "build")
	done1 := makeLiveTestJob("done-1", "build")
	queued1 := makeLiveTestJob("queued-1", "build")
	planned := []*jobs.ScheduledJob{done0, failed, running, blocked, queued0, done1, queued1}

	r, _ := newTestRenderer(t, 120, planned...)
	for _, job := range []*jobs.ScheduledJob{done0, done1} {
		r.JobStart(job)
		r.JobComplete(job, &jobs.JobResult{Status: "success"})
	}
	r.JobStart(failed)
	r.JobComplete(failed, &jobs.JobResult{Status: "failed"})
	r.JobStart(running)

	visible, hidden := r.liveRowsAt(r.sortedRows(), 3, time.Now())
	if hidden != 2 {
		t.Fatalf("hidden = %d, want 2; visible=%v", hidden, rowNames(visible))
	}
	for _, want := range []string{"failed", "running", "blocked"} {
		if !hasRowNamed(visible, want) {
			t.Fatalf("expected %q to stay visible, got %v", want, rowNames(visible))
		}
	}
}

func TestLiveRowsRetainsRecentlyDoneRowsBriefly(t *testing.T) {
	base := time.Unix(1000, 0)
	planned := []*jobs.ScheduledJob{
		makeLiveTestJob("done", "build"),
		makeLiveTestJob("running-0", "build"),
		makeLiveTestJob("running-1", "build"),
		makeLiveTestJob("running-2", "build"),
	}
	r, _ := newTestRenderer(t, 120, planned...)

	done := r.rowByID["done"]
	done.commands["build"].status = "success"
	done.doneVisibleUntil = base.Add(liveDoneRetention)
	for _, name := range []string{"running-0", "running-1", "running-2"} {
		r.rowByID[name].commands["build"].status = statusRunning
	}

	visible, hidden := r.liveRowsAt(r.sortedRows(), 2, base.Add(2*time.Second))
	if !hasRowNamed(visible, "done") {
		t.Fatalf("recently done row should remain visible, got %v", rowNames(visible))
	}
	if hidden != 2 {
		t.Fatalf("hidden while done retained = %d, want 2", hidden)
	}

	visible, hidden = r.liveRowsAt(r.sortedRows(), 2, base.Add(4*time.Second))
	if hasRowNamed(visible, "done") {
		t.Fatalf("expired done row should leave overflow view, got %v", rowNames(visible))
	}
	if hidden != 1 {
		t.Fatalf("hidden after done expires = %d, want 1", hidden)
	}
}

func TestJobCompleteRetainsVisibleDoneProject(t *testing.T) {
	job := makeLiveTestJob("app", "build")
	r, _ := newTestRenderer(t, 120, job)
	row := r.rowByID["app"]
	row.visible = true

	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{Status: "success"})

	if row.doneVisibleUntil.IsZero() {
		t.Fatal("visible done project should get a retention window")
	}
}

func rowNames(rows []*projectRow) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.name)
	}
	return names
}

func hasRowNamed(rows []*projectRow, name string) bool {
	for _, row := range rows {
		if row.name == name {
			return true
		}
	}
	return false
}

// --- final summary ---

func TestFinalSummaryReplaysMeasuredZeroWithProvenance(t *testing.T) {
	testJob := makeLiveTestJob("app", "test")
	r, errOut := newTestRendererWithCoverage(t, 120, map[string]CoverageReplay{
		"app": {
			Entry: workspace_state.SessionCoverageEntry{
				Command: "test",
				Summary: runtimeproto.CoverageSummary{
					Percentage:  0,
					Granularity: runtimeproto.CoverageLines,
					Covered:     0,
					Total:       12,
				},
			},
			Source:    workspace_state.SessionReportSource{Revision: "abc1234"},
			SessionID: "validation-session",
		},
	}, testJob)

	r.JobStart(testJob)
	r.JobComplete(testJob, &jobs.JobResult{Status: "success"})
	r.Finish(map[string]*jobs.JobResult{testJob.Key(): {Status: "success"}}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "coverage 0.0% (replayed: validation @ abc1234)") {
		t.Fatalf("replayed measured zero was absent or unprovenanced:\n%s", out)
	}
}

func TestFinalSummaryCurrentCoverageOverridesReplay(t *testing.T) {
	testJob := makeLiveTestJob("app", "test")
	r, errOut := newTestRendererWithCoverage(t, 120, map[string]CoverageReplay{
		"app": {
			Entry: workspace_state.SessionCoverageEntry{
				Command: "test",
				Summary: runtimeproto.CoverageSummary{Percentage: 12, Granularity: runtimeproto.CoverageStatements},
			},
			Source: workspace_state.SessionReportSource{Revision: "oldrev"},
		},
	}, testJob)

	live := extension.ParamMap{"coverageSummary": extension.ParamMap{
		"percentage":  91.0,
		"granularity": runtimeproto.CoverageStatements,
		"covered":     91,
		"total":       100,
	}}
	r.JobStart(testJob)
	r.JobComplete(testJob, &jobs.JobResult{Status: "success", Data: live})
	r.Finish(map[string]*jobs.JobResult{testJob.Key(): {Status: "success", Data: live}}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "coverage 91.0%") || strings.Contains(out, "stale") || strings.Contains(out, "12.0%") {
		t.Fatalf("current coverage did not override replay:\n%s", out)
	}
}

func TestFinalSummaryMixedGoLiveAndTypeScriptReplayUsesBothPercentages(t *testing.T) {
	goTest := makeLiveTestJob("go-app", "test")
	tsTest := makeLiveTestJob("ts-app", "test")
	r, errOut := newTestRendererWithCoverage(t, 140, map[string]CoverageReplay{
		"ts-app": {
			Entry: workspace_state.SessionCoverageEntry{
				Command: "test",
				Summary: runtimeproto.CoverageSummary{
					Percentage:  40,
					Granularity: runtimeproto.CoverageLines,
					Covered:     40,
					Total:       100,
				},
			},
			Source: workspace_state.SessionReportSource{Revision: "tsrev"},
		},
	}, goTest, tsTest)

	goLive := extension.ParamMap{"coverageSummary": extension.ParamMap{
		"percentage":  80.0,
		"granularity": runtimeproto.CoverageStatements,
		"covered":     80,
		"total":       100,
	}}
	r.JobStart(goTest)
	r.JobComplete(goTest, &jobs.JobResult{Status: "success", Data: goLive})
	r.JobStart(tsTest)
	r.JobComplete(tsTest, &jobs.JobResult{Status: "success"})
	r.Finish(map[string]*jobs.JobResult{
		goTest.Key(): {Status: "success", Data: goLive},
		tsTest.Key(): {Status: "success"},
	}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "coverage 60.0% (live + replayed: validation @ tsrev)") {
		t.Fatalf("mixed-granularity aggregate omitted a language or provenance:\n%s", out)
	}
}

// A replayed measurement goes to the command that recorded it, whatever that
// command is called, and never to another command of the same project.
func TestStartReplaysCoverageOnTheCommandThatRecordedIt(t *testing.T) {
	testJob := makeLiveTestJob("app", "test")
	checkJob := makeLiveTestJob("app", "check")
	r, _ := newTestRendererWithCoverage(t, 120, map[string]CoverageReplay{
		"app": {
			Entry: workspace_state.SessionCoverageEntry{
				Command: "check",
				Summary: runtimeproto.CoverageSummary{Percentage: 70, Granularity: runtimeproto.CoverageLines},
			},
			Source: workspace_state.SessionReportSource{Revision: "checkrev"},
		},
	}, testJob, checkJob)

	row := r.rowByID["app"]
	if row == nil {
		t.Fatal("no row for the project")
	}
	if replay := row.commands["check"].replayedCoverage; replay == nil || replay.Entry.Summary.Percentage != 70 {
		t.Fatalf("check replay = %+v, want the measurement recorded under check", replay)
	}
	if replay := row.commands["test"].replayedCoverage; replay != nil {
		t.Fatalf("test took a replay recorded under check: %+v", replay)
	}
}

// A command whose test step reports the counts keeps them when a later or
// earlier step of the same command reports none, as the Go race step does:
// the live cell and the Tasks line read the test step's counts in either
// completion order.
func TestAStepWithoutTestCountsKeepsTheTestStepCounts(t *testing.T) {
	testResult := func() *jobs.JobResult {
		return &jobs.JobResult{
			Status: "success",
			Events: []jobs.RawJobEvent{
				metricEvent("tests-total", 40, "count"),
				metricEvent("tests-passed", 40, "count"),
				{Type: jobs.EventTypeSummary, Data: map[string]any{"message": "40/40 passed"}},
			},
			Data: map[string]any{"testSummary": map[string]any{"total": 40, "passed": 40, "failed": 0, "skipped": 0}},
		}
	}
	raceResult := func() *jobs.JobResult {
		return &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
			logEvent("info", "Race detector on 2 package(s): 3/3 passed"),
		}}
	}
	for name, order := range map[string][]string{
		"race after test":  {"test~test", "test~test-race"},
		"race before test": {"test~test-race", "test~test"},
	} {
		test := makeLiveTestJob("app", "test~test")
		race := makeLiveTestJob("app", "test~test-race")
		jobsByName := map[string]*jobs.ScheduledJob{"test~test": test, "test~test-race": race}
		resultFor := map[string]func() *jobs.JobResult{"test~test": testResult, "test~test-race": raceResult}
		r, errOut := newTestRenderer(t, 120, test, race)
		for _, step := range order {
			r.JobStart(jobsByName[step])
			r.JobComplete(jobsByName[step], resultFor[step]())
		}
		cmd := r.rowByID["app"].commands["test"]
		if cmd.summary != "40/40 passed" || cmd.testSummary == nil || cmd.testSummary.Total != 40 {
			t.Errorf("%s: command summary %q, test summary %+v; want the test step's", name, cmd.summary, cmd.testSummary)
		}
		r.Finish(map[string]*jobs.JobResult{test.Key(): {Status: "success"}, race.Key(): {Status: "success"}}, jobs.SessionOutcome{})
		if out := errOut.String(); !strings.Contains(out, "40 tests") || strings.Contains(out, "43 tests") {
			t.Errorf("%s: the Tasks line does not report the test step's 40 tests alone:\n%s", name, out)
		}
	}
}

func TestFinalSummaryBlocks(t *testing.T) {
	// app: lint+test+build all pass with metrics. lib: test fails.
	appLint := makeLiveTestJob("app", "lint")
	appTest := makeLiveTestJob("app", "test")
	appBuild := makeLiveTestJob("app", "build")
	libTest := makeLiveTestJob("lib", "test")

	r, errOut := newTestRenderer(t, 120, appLint, appTest, appBuild, libTest)

	r.JobStart(appLint)
	r.JobComplete(appLint, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("lint-warnings", 14, "count"),
	}})
	r.JobStart(appTest)
	r.JobComplete(appTest, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 2400, "count"),
		metricEvent("tests-passed", 2400, "count"),
		metricEvent("coverage", 84.0, "percent"),
	}})
	r.JobStart(appBuild)
	r.JobComplete(appBuild, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{
		metricEvent("transpiled-files", 300, "count"),
		{Type: jobs.EventTypeArtifact, Data: map[string]any{"name": "bundle", "kind": "archive"}},
	}})

	r.JobStart(libTest)
	r.JobComplete(libTest, &jobs.JobResult{Status: "failed", Events: []jobs.RawJobEvent{
		metricEvent("tests-total", 18, "count"),
		metricEvent("tests-failed", 3, "count"),
		metricEvent("coverage", 80.0, "percent"),
	}})

	results := map[string]*jobs.JobResult{
		appLint.Key():  {Status: "success"},
		appTest.Key():  {Status: "success"},
		appBuild.Key(): {Status: "success"},
		libTest.Key():  {Status: "failed", Error: &jobs.JobError{Message: "assertion failed"}},
	}
	r.Finish(results, jobs.SessionOutcome{})
	out := errOut.String()

	// Projects block.
	if !strings.Contains(out, "Projects:") {
		t.Errorf("expected Projects block: %s", out)
	}
	if !strings.Contains(out, "2 impacted") || !strings.Contains(out, "1 succeeded") || !strings.Contains(out, "1 failed") {
		t.Errorf("expected project counts: %s", out)
	}

	// Tasks block with aggregated metrics.
	if !strings.Contains(out, "Tasks:") {
		t.Errorf("expected Tasks block: %s", out)
	}
	if !strings.Contains(out, "14 warnings") {
		t.Errorf("expected aggregated lint warnings: %s", out)
	}
	if !strings.Contains(out, "2,418 tests") {
		t.Errorf("expected thousands-formatted test total (2400+18): %s", out)
	}
	if !strings.Contains(out, "coverage 82.0%") {
		t.Errorf("expected averaged coverage 82.0%%: %s", out)
	}
	if !strings.Contains(out, "300 files built") {
		t.Errorf("expected build files: %s", out)
	}
	if !strings.Contains(out, "1 artifacts") {
		t.Errorf("expected artifact count: %s", out)
	}

	// Failures block with cause + detail.
	if !strings.Contains(out, "Failures:") {
		t.Errorf("expected Failures block: %s", out)
	}
	if !strings.Contains(out, "lib") || !strings.Contains(out, "3 failing tests") {
		t.Errorf("expected failure cause: %s", out)
	}
	if !strings.Contains(out, "assertion failed") {
		t.Errorf("expected failure detail line: %s", out)
	}
}

// A cell reads the metrics its command's kind names, whatever the command is
// called, and a command without a kind shows its status only.
func TestLiveCellValuesFollowTheDeclaredKind(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "batch-parity-and-live-cells-follow-declared-kinds",
		"a-live-cell-values-by-the-declared-kind")
	none := extensionproto.CommandTraits{}
	warnings := []jobs.RawJobEvent{metricEvent("lint-warnings", 2, "count")}
	errorCounts := []jobs.RawJobEvent{metricEvent("lint-errors", 3, "count")}
	counts := []jobs.RawJobEvent{
		metricEvent("tests-total", 4, "count"),
		metricEvent("tests-passed", 3, "count"),
		metricEvent("coverage", 80, "percent"),
	}
	failing := []jobs.RawJobEvent{metricEvent("tests-failed", 2, "count")}
	files := []jobs.RawJobEvent{metricEvent("transpiled-files", 7, "count")}
	for _, test := range []struct {
		name   string
		traits extensionproto.CommandTraits
		status string
		events []jobs.RawJobEvent
		want   string
	}{
		{"check", devVerbTraits["lint"], "success", warnings, "2 warn"},
		{"lint", none, "success", warnings, "ok"},
		{"check", devVerbTraits["lint"], "failed", errorCounts, "3 err"},
		{"lint", none, "failed", errorCounts, "failed"},
		{"verify", devVerbTraits["test"], "success", counts, "3/4 80.0%"},
		{"test", none, "success", counts, "ok"},
		{"verify", devVerbTraits["test"], "failed", failing, "2 failed"},
		{"test", none, "failed", failing, "failed"},
		{"make", devVerbTraits["build"], "success", files, "7 files"},
		{"build", none, "success", files, "ok"},
		{"check", devVerbTraits["validate"], "success", warnings, "ok"},
	} {
		job := makeLiveTestJobWithTraits("app", test.name, test.traits)
		r, _ := newTestRenderer(t, 120, job)
		r.JobStart(job)
		r.JobComplete(job, &jobs.JobResult{Status: test.status, Events: test.events})
		if _, value := r.cellValue(r.rowByID["app"].commands[test.name]); value.text != test.want {
			t.Errorf("%s of kind %q, %s: cell value = %q, want %q", test.name, test.traits.Kind, test.status, value.text, test.want)
		}
	}
}

// The Tasks line and the failure cause read the command's kind: renamed
// commands with a kind aggregate and explain like the first-party verbs, and
// a lint or test without a kind reports its status only.
func TestTasksLineAndFailureCauseFollowTheKind(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "batch-parity-and-live-cells-follow-declared-kinds",
		"the-tasks-line-and-failure-cause-follow-the-kind")
	none := extensionproto.CommandTraits{}
	check := makeLiveTestJobWithTraits("app", "check", devVerbTraits["lint"])
	verify := makeLiveTestJobWithTraits("app", "verify", devVerbTraits["test"])
	build := makeLiveTestJobWithTraits("app", "make", devVerbTraits["build"])
	lint := makeLiveTestJobWithTraits("lib", "lint", none)
	test := makeLiveTestJobWithTraits("lib", "test", none)
	r, errOut := newTestRenderer(t, 160, check, verify, build, lint, test)

	for _, run := range []struct {
		job    *jobs.ScheduledJob
		status string
		events []jobs.RawJobEvent
	}{
		{check, "failed", []jobs.RawJobEvent{metricEvent("lint-errors", 3, "count"), metricEvent("lint-warnings", 1, "count")}},
		{verify, "failed", []jobs.RawJobEvent{metricEvent("tests-total", 18, "count"), metricEvent("tests-failed", 2, "count")}},
		{build, "success", []jobs.RawJobEvent{metricEvent("transpiled-files", 300, "count")}},
		{lint, "failed", []jobs.RawJobEvent{metricEvent("lint-errors", 5, "count")}},
		{test, "failed", []jobs.RawJobEvent{metricEvent("tests-total", 7, "count"), metricEvent("tests-failed", 4, "count")}},
	} {
		r.JobStart(run.job)
		r.JobComplete(run.job, &jobs.JobResult{Status: run.status, Events: run.events})
	}
	r.Finish(map[string]*jobs.JobResult{
		check.Key():  {Status: "failed"},
		verify.Key(): {Status: "failed"},
		build.Key():  {Status: "success"},
		lint.Key():   {Status: "failed"},
		test.Key():   {Status: "failed"},
	}, jobs.SessionOutcome{})
	out := errOut.String()

	tasksLine := func(command string) string {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, command+" ") {
				return line
			}
		}
		t.Fatalf("no Tasks line for %s: %s", command, out)
		return ""
	}
	for command, want := range map[string]string{
		"check":  "0 passed · 1 failed · 3 errors · 1 warnings",
		"verify": "0 passed · 1 failed · 18 tests · 2 failed",
		"make":   "1 passed · 0 failed · 300 files built",
		"lint":   "0 passed · 1 failed",
		"test":   "0 passed · 1 failed",
	} {
		if line := tasksLine(command); !strings.HasSuffix(line, "  "+want) {
			t.Errorf("Tasks line of %s = %q, want it to end with %q", command, line, want)
		}
	}
	for _, want := range []string{
		"app · check · 3 errors\n",
		"app · verify · 2 failing tests\n",
		"lib · lint · failed\n",
		"lib · test · failed\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected failure cause %q: %s", strings.TrimSpace(want), out)
		}
	}
}

// Hybrid coverage display. Only --enforce-coverage instruments, so an
// ordinary `putnami test` measures nothing and the summary
// lost its coverage part. These three pin the whole rule: the replay fills the
// gap, a live measurement always wins it back, and the replay is never
// mistakable for one.

func TestFinalSummaryDeployPartial(t *testing.T) {
	ok := makeLiveTestJob("svc-a", "deploy")
	skip := makeLiveTestJob("svc-b", "deploy")
	r, errOut := newTestRenderer(t, 120, ok, skip)
	r.JobStart(ok)
	r.JobComplete(ok, &jobs.JobResult{Status: "success"})
	r.JobStart(skip)
	r.JobComplete(skip, &jobs.JobResult{Status: "skipped"})
	r.Finish(map[string]*jobs.JobResult{ok.Key(): {Status: "success"}, skip.Key(): {Status: "skipped"}}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "1 passed") || !strings.Contains(out, "1 skipped") {
		t.Errorf("expected deploy passed+skipped counts: %s", out)
	}
}

func TestFinalSummaryDeploySkipped(t *testing.T) {
	deploy := makeLiveTestJob("app", "deploy")
	r, errOut := newTestRenderer(t, 120, deploy)
	r.JobStart(deploy)
	r.JobComplete(deploy, &jobs.JobResult{Status: "skipped"})
	r.Finish(map[string]*jobs.JobResult{deploy.Key(): {Status: "skipped"}}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "deploy") || !strings.Contains(out, "skipped") {
		t.Errorf("expected deploy skipped in tasks: %s", out)
	}
}

func TestFinalSummarySkippedCommandShowsSummary(t *testing.T) {
	publish := makeLiveTestJob("app", "publish")
	r, errOut := newTestRenderer(t, 140, publish)
	summary := jobs.RawJobEvent{
		Type: jobs.EventTypeSummary,
		Data: map[string]any{"message": "Skipped: docker channel not in package metadata"},
	}

	r.JobStart(publish)
	r.JobComplete(publish, &jobs.JobResult{Status: "skipped", Events: []jobs.RawJobEvent{summary}})
	r.Finish(map[string]*jobs.JobResult{publish.Key(): {Status: "skipped", Events: []jobs.RawJobEvent{summary}}}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "docker channel") {
		t.Errorf("expected skipped summary in final table: %s", out)
	}
}

func TestFinalSummaryVisibleSummary(t *testing.T) {
	publish := makeLiveTestJob("app", "publish-config")
	r, errOut := newTestRenderer(t, 140, publish)
	payload := "{\n  \"appName\": \"api\"\n}"
	summary := jobs.RawJobEvent{
		Type: jobs.EventTypeSummary,
		Data: map[string]any{
			"message":    "Dry-run config schema payload:\n" + payload,
			"visibility": "always",
		},
	}

	r.JobStart(publish)
	r.JobComplete(publish, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{summary}})
	r.Finish(map[string]*jobs.JobResult{publish.Key(): {Status: "success", Events: []jobs.RawJobEvent{summary}}}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, payload) {
		t.Errorf("expected visible summary payload in final output: %s", out)
	}
}

// A lifecycle run prints a summary the job marks always visible, such as the
// plan of deps add --dry-run, and no session summary.
func TestLifecycleFinishPrintsAVisibleSummary(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "deps-edit-runs-in-the-project-extension",
		"a-lifecycle-run-prints-an-always-visible-summary")
	edit := makeLiveTestJob("api", "deps-edit")
	r, errOut := newTestRenderer(t, 140, edit)
	r.lifecycleMode = true
	plan := visibleDryRunPlan
	summary := visibleDryRunPlanEvent()

	r.JobStart(edit)
	r.JobComplete(edit, &jobs.JobResult{Status: "success", Events: []jobs.RawJobEvent{summary}})
	r.Finish(map[string]*jobs.JobResult{edit.Key(): {Status: "success", Events: []jobs.RawJobEvent{summary}}}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, plan) {
		t.Errorf("expected the plan in the lifecycle output: %s", out)
	}
	if strings.Contains(out, "Session completed") {
		t.Errorf("a lifecycle run printed a session summary: %s", out)
	}
}

func TestFinalSummaryFailureDetailsIncludeErrorLogs(t *testing.T) {
	build := makeLiveTestJob("app", "build")
	r, errOut := newTestRenderer(t, 120, build)
	errEvent := jobs.RawJobEvent{
		Type: jobs.EventTypeLog,
		Data: map[string]any{
			"level":   "error",
			"message": "pre-build hooks: exit status 1",
		},
	}

	r.JobStart(build)
	r.JobComplete(build, &jobs.JobResult{Status: "failed", Events: []jobs.RawJobEvent{errEvent}})
	r.Finish(map[string]*jobs.JobResult{
		build.Key(): {Status: "failed", Events: []jobs.RawJobEvent{errEvent}},
	}, jobs.SessionOutcome{})

	out := errOut.String()
	for _, want := range []string{"Failures:", "app", "build", "error: pre-build hooks: exit status 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in failure summary, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(no details") {
		t.Errorf("did not expect missing-details hint when error logs are available:\n%s", out)
	}
}

func TestFinalSummaryFailureDetailsPreferDiagnosticsOverGenericExitStatus(t *testing.T) {
	lint := makeLiveTestJob("app", "lint~check")
	r, errOut := newTestRenderer(t, 120, lint)
	diag := jobs.RawJobEvent{
		Type: jobs.EventTypeDiagnostic,
		Data: map[string]any{
			"severity": "error",
			"code":     "lint/style/useConst",
			"message":  "Use const instead of let.",
			"location": map[string]any{
				"file":   "src/index.ts",
				"line":   float64(12),
				"column": float64(5),
			},
		},
	}

	r.JobStart(lint)
	r.JobComplete(lint, &jobs.JobResult{
		Status: "failed",
		Error:  &jobs.JobError{Message: "exit status 1\n\nHint: run with --output=jsonl for full diagnostic details"},
		Events: []jobs.RawJobEvent{diag},
	})
	r.Finish(map[string]*jobs.JobResult{
		lint.Key(): {
			Status: "failed",
			Error:  &jobs.JobError{Message: "exit status 1\n\nHint: run with --output=jsonl for full diagnostic details"},
			Events: []jobs.RawJobEvent{diag},
		},
	}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "src/index.ts:12:5 error[lint/style/useConst] Use const instead of let.") {
		t.Fatalf("expected actionable diagnostic in failure summary, got:\n%s", out)
	}
	if strings.Contains(out, "exit status 1") {
		t.Fatalf("generic exit status should not displace diagnostics, got:\n%s", out)
	}
	if count := strings.Count(out, "--output=jsonl"); count != 1 {
		t.Fatalf("expected one JSONL hint, got %d:\n%s", count, out)
	}
}

func TestFinalSummaryFailureDetailsGenericExitStatusFallback(t *testing.T) {
	lint := makeLiveTestJob("app", "lint~check")
	r, errOut := newTestRenderer(t, 120, lint)

	r.JobStart(lint)
	r.JobComplete(lint, &jobs.JobResult{
		Status: "failed",
		Error:  &jobs.JobError{Message: "exit status 1"},
	})
	r.Finish(map[string]*jobs.JobResult{
		lint.Key(): {Status: "failed", Error: &jobs.JobError{Message: "exit status 1"}},
	}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "no diagnostics emitted") {
		t.Fatalf("expected missing-diagnostics fallback, got:\n%s", out)
	}
	if count := strings.Count(out, "--output=jsonl"); count != 1 {
		t.Fatalf("expected one JSONL hint, got %d:\n%s", count, out)
	}
}

// --- events ---

func TestJobEventProgressAndPhase(t *testing.T) {
	job := makeLiveTestJob("app", "build")
	r, _ := newTestRenderer(t, 120, job)
	r.JobStart(job)

	r.JobEvent(job, jobs.RawJobEvent{
		Type: jobs.EventTypeProgress,
		Data: map[string]any{"current": float64(5), "total": float64(10), "message": "compiling"},
	})
	r.JobEvent(job, jobs.RawJobEvent{
		Type: jobs.EventTypePhase,
		Data: map[string]any{"name": "compile", "action": "start"},
	})

	cmd := r.rowByID["app"].commands["build"]
	if cmd.progress == nil || cmd.progress.current != 5 || cmd.progress.total != 10 {
		t.Errorf("unexpected progress: %+v", cmd.progress)
	}
	if cmd.phase != "compile" {
		t.Errorf("phase = %q, want compile", cmd.phase)
	}
}

func TestPipelineStepsFoldIntoOneCommand(t *testing.T) {
	gen := makeLiveTestJob("app", "build~generate")
	tr := makeLiveTestJob("app", "build~transpile")
	r, _ := newTestRenderer(t, 120, gen, tr)

	cmd := r.rowByID["app"].commands["build"]
	if cmd == nil || cmd.totalSteps != 2 {
		t.Fatalf("expected build command with 2 steps, got %+v", cmd)
	}

	r.JobStart(gen)
	r.JobComplete(gen, &jobs.JobResult{Status: "success"})
	if r.projectStatus(r.rowByID["app"]) != statusRunning {
		t.Error("project should still be running after 1/2 steps")
	}
	r.JobStart(tr)
	r.JobComplete(tr, &jobs.JobResult{Status: "success"})
	if r.projectStatus(r.rowByID["app"]) != statusDone {
		t.Error("project should be done after 2/2 steps")
	}
}

// --- watch-mode reset ---

func TestStartResetsState(t *testing.T) {
	job := makeLiveTestJob("app", "build")
	r, _ := newTestRenderer(t, 120, job)
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{Status: "success"})

	// Re-Start (watch iteration) should clear prior rows.
	r.Start([]*jobs.ScheduledJob{makeLiveTestJob("lib", "test")})
	stopSpinner(r)
	if len(r.rows) != 1 || r.rowByID["app"] != nil {
		t.Errorf("expected state reset on re-Start, rows=%d", len(r.rows))
	}
}

func TestLongProjectNameTruncatedInNarrowTerminal(t *testing.T) {
	long := "go.putnami.dev/some/really/long/module/name"
	r, _ := newTestRenderer(t, 48, makeLiveTestJob(long, "build"))
	row := r.renderRow(r.rowByID[long])
	// Name is clamped only when the row needs the width for status summaries.
	if strings.Contains(row, long) {
		t.Errorf("expected long name to be truncated: %s", row)
	}
	if !strings.Contains(row, "…") {
		t.Errorf("expected ellipsis on truncated name: %s", row)
	}
}

// --- narrow terminal degradation ---

func TestNarrowTerminalDropsColumns(t *testing.T) {
	lint := makeLiveTestJob("app", "lint")
	test := makeLiveTestJob("app", "test")
	build := makeLiveTestJob("app", "build")
	r, _ := newTestRenderer(t, 60, lint, test, build) // narrow

	r.JobStart(test)
	r.JobEvent(test, jobs.RawJobEvent{
		Type: jobs.EventTypeProgress,
		Data: map[string]any{"current": float64(1), "total": float64(2)},
	})
	row := r.renderRow(r.rowByID["app"])

	// Must not blow past the terminal width (allowing the trailing trim).
	if w := len([]rune(row)); w > 60 {
		t.Errorf("row width %d exceeds terminal width 60: %q", w, row)
	}
}

// A column's width and hide priority derive from its command's traits, never
// its name: renamed commands with the first-party traits reserve the same
// widths and hide in the same order. A status-only cell hides first, then the
// diagnostics tally, the verification counts, the produced files, a registry
// publication, and last a cloud effect.
func TestNarrowTerminalHidesColumnsByAchievementPriority(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "batch-parity-and-live-cells-follow-declared-kinds",
		"cell-width-and-hide-priority-derive-from-traits")
	traits := []extensionproto.CommandTraits{
		devVerbTraits["lint"],
		devVerbTraits["test"],
		devVerbTraits["build"],
		{InjectPublishChannels: true},
		devVerbTraits["publish"],
		devVerbTraits["deploy"],
	}
	for _, names := range [][]string{
		{"lint", "test", "build", "package", "publish", "deploy"},
		{"check", "verify", "make", "archive", "release", "launch"},
	} {
		initial := ColorsEnabled()
		SetColorsEnabled(false)
		r := NewLiveRenderer(&bytes.Buffer{}, &bytes.Buffer{})
		r.verbOrder = names
		r.width = 140
		planned := make([]*jobs.ScheduledJob, 0, len(names))
		for i, name := range names {
			planned = append(planned, makeLiveTestJobWithTraits("app", name, traits[i]))
		}
		r.Start(planned)
		stopSpinner(r)
		SetColorsEnabled(initial)

		widths := make([]int, 0, len(names))
		for _, name := range names {
			widths = append(widths, r.colWidth[name])
		}
		if want := []int{15, 22, 16, len(names[3]) + 10, len(names[4]) + 10, len(names[5]) + 10}; !slices.Equal(widths, want) {
			t.Errorf("%v: cell widths = %v, want %v", names, widths, want)
		}

		tests := []struct {
			budget int
			want   []int
		}{
			{budget: 120, want: []int{0, 1, 2, 3, 4, 5}},
			{budget: 110, want: []int{0, 1, 2, 4, 5}},
			{budget: 90, want: []int{1, 2, 4, 5}},
			{budget: 70, want: []int{2, 4, 5}},
			{budget: 45, want: []int{4, 5}},
			{budget: 20, want: []int{5}},
		}
		for _, tt := range tests {
			want := make([]string, 0, len(tt.want))
			for _, i := range tt.want {
				want = append(want, names[i])
			}
			fit, _ := r.visibleColumnsForBudget(tt.budget)
			if !slices.Equal(fit.names, want) {
				t.Errorf("budget %d names = %v, want %v", tt.budget, fit.names, want)
			}
			if fit.hidden != len(names)-len(want) {
				t.Errorf("budget %d hidden = %d, want %d", tt.budget, fit.hidden, len(names)-len(want))
			}
		}
	}

	// The remaining traits: a workload hides last, a criteria tally with a
	// diagnostics one, and a command without traits first.
	for _, test := range []struct {
		traits extensionproto.CommandTraits
		want   int
	}{
		{devVerbTraits["serve"], 6},
		{devVerbTraits["run"], 6},
		{devVerbTraits["validate"], 2},
		{extensionproto.CommandTraits{RewritesSources: true}, 0},
		{extensionproto.CommandTraits{}, 0},
	} {
		if got := summaryColumnHideRank(test.traits); got != test.want {
			t.Errorf("hide rank of %+v = %d, want %d", test.traits, got, test.want)
		}
	}
}

// The end-of-run summary must not read as a clean finish when the user killed
// the run. Reporting "Session completed" plus a pile of "skipped" projects made
// an interrupted build indistinguishable from one that had nothing left to do.
func TestFinalSummaryInterruptedByUser(t *testing.T) {
	done := makeLiveTestJob("lib", "build")
	killed := makeLiveTestJob("app", "build")
	r, errOut := newTestRenderer(t, 120, done, killed)

	r.JobStart(done)
	r.JobComplete(done, &jobs.JobResult{Status: "success"})
	// Still running when the signal landed: no JobComplete ever arrives.
	r.JobStart(killed)

	r.Finish(map[string]*jobs.JobResult{
		done.Key():   {Status: "success"},
		killed.Key(): {Status: "canceled"},
	}, jobs.SessionOutcome{Aborted: true, AbortedBy: jobs.AbortUser})

	out := errOut.String()
	if !strings.Contains(out, "Session interrupted by user") {
		t.Errorf("summary must name the user as the cause:\n%s", out)
	}
	if strings.Contains(out, "Session completed") {
		t.Errorf("interrupted run must not claim completion:\n%s", out)
	}
	if !strings.Contains(out, "1 aborted") {
		t.Errorf("cut-off project must be counted as aborted:\n%s", out)
	}
	// The killed project must not inflate the skipped count, where it would
	// read as work the run legitimately decided against.
	if !strings.Contains(out, "0 skipped") {
		t.Errorf("aborted work must stay out of the skipped count:\n%s", out)
	}
}

// A task whose every job was killed reported "skipped", which reads as "this
// task was never going to run" rather than "you stopped it".
func TestFinalSummaryInterruptedTaskLine(t *testing.T) {
	lint := makeLiveTestJob("app", "lint")
	r, errOut := newTestRenderer(t, 120, lint)
	r.JobStart(lint)

	r.Finish(map[string]*jobs.JobResult{
		lint.Key(): {Status: "canceled"},
	}, jobs.SessionOutcome{Aborted: true, AbortedBy: jobs.AbortUser})

	out := errOut.String()
	if !strings.Contains(out, "aborted") {
		t.Errorf("wholly cut-off task must read as aborted:\n%s", out)
	}
}

// An abort with no attributable source still says the run stopped short; it
// just does not blame the user for a signal it cannot trace.
func TestFinalSummaryInterruptedWithoutSource(t *testing.T) {
	build := makeLiveTestJob("app", "build")
	r, errOut := newTestRenderer(t, 120, build)
	r.JobStart(build)

	r.Finish(map[string]*jobs.JobResult{
		build.Key(): {Status: "canceled"},
	}, jobs.SessionOutcome{Aborted: true})

	out := errOut.String()
	if !strings.Contains(out, "Session interrupted after") {
		t.Errorf("unattributed abort must still be reported:\n%s", out)
	}
	if strings.Contains(out, "by user") {
		t.Errorf("must not blame the user for an unattributed abort:\n%s", out)
	}
}

// Genuine skips (a dependency failed, a task did not apply) keep reading as
// skipped: the abort vocabulary must not swallow them.
func TestFinalSummaryCompletedRunKeepsSkipped(t *testing.T) {
	ok := makeLiveTestJob("app", "build")
	skipped := makeLiveTestJob("lib", "build")
	r, errOut := newTestRenderer(t, 120, ok, skipped)
	r.JobStart(ok)
	r.JobComplete(ok, &jobs.JobResult{Status: "success"})
	r.JobStart(skipped)
	r.JobComplete(skipped, &jobs.JobResult{Status: "skipped"})

	r.Finish(map[string]*jobs.JobResult{
		ok.Key():      {Status: "success"},
		skipped.Key(): {Status: "skipped"},
	}, jobs.SessionOutcome{})

	out := errOut.String()
	if !strings.Contains(out, "Session completed in") {
		t.Errorf("an uninterrupted run still completes:\n%s", out)
	}
	if !strings.Contains(out, "1 skipped") {
		t.Errorf("real skips must stay skipped:\n%s", out)
	}
	if strings.Contains(out, "aborted") {
		t.Errorf("no abort vocabulary on a clean run:\n%s", out)
	}
}
