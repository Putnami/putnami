package jobs

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
)

// The equivalence contract.
//
// Nine tally loops walk the same result map today. This file pins that the one
// canonical reducer produces the SAME numbers as each of them on the same
// input, so a later migration can delete them and point their consumers here. The
// legacy loops are transcribed below, citing their source, because the test has
// to keep asserting the contract after the originals are gone.
//
// Event fixtures are built by running JSONL lines through ParseRawEvent and
// wire fixtures by unmarshaling JSON into batchWireResult — the same two
// entrypoints production uses. Hand-assembling renderer payloads here would
// test the fixtures rather than the parse.

func parseTestEvent(t *testing.T, line string) RawJobEvent {
	t.Helper()
	event, ok := ParseRawEvent(line)
	if !ok {
		t.Fatalf("fixture is not a valid protocol event: %s", line)
	}
	return event
}

func canonicalTestJob(project, jobName, taskKind string) *ScheduledJob {
	step := (*extension.PipelineStep)(nil)
	if taskKind != "" {
		step = &extension.PipelineStep{ID: jobName, Task: taskKind}
	}
	return &ScheduledJob{
		Project:   &workspace.Project{ID: "/" + project, Name: project, Path: project},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test"},
		JobDef:    &extension.JobDefinition{Name: jobName, CommandName: jobName},
		Step:      step,
	}
}

// canonicalFixture covers every axis the reducer has to get right: each status,
// each reuse kind, a reused FAILURE (the case where the legacy loops disagree
// with each other), and a status outside the canonical vocabulary.
func canonicalFixture(t *testing.T) ([]*ScheduledJob, map[string]*JobResult) {
	t.Helper()
	diagnosticEvent := parseTestEvent(t, `{"v":2,"type":"diagnostic","message":"unused var",`+
		`"severity":"error","code":"lint/unused",`+
		`"location":{"file":"src/a.ts","line":12,"column":3}}`)
	artifactEvent := parseTestEvent(t, `{"v":2,"type":"artifact","id":"bundle","name":"Bundle",`+
		`"kind":"archive","path":"out/a.zip"}`)

	planned := []*ScheduledJob{
		canonicalTestJob("ok", "build", "build-compile"),
		canonicalTestJob("bad", "lint", ""),
		canonicalTestJob("stopped", "test", ""),
		canonicalTestJob("dropped", "build", ""),
		canonicalTestJob("warm", "build", ""),
		canonicalTestJob("remote", "build", ""),
		canonicalTestJob("shared", "build", ""),
		canonicalTestJob("weird", "build", ""),
		canonicalTestJob("reusedfail", "lint", ""),
	}

	fresh := func(status string, duration time.Duration, events ...RawJobEvent) *JobResult {
		return &JobResult{Status: status, Duration: duration, Events: events}
	}
	reused := func(status string, kind ReuseKind, duration time.Duration) *JobResult {
		result := &JobResult{Status: status, Duration: duration}
		result.MarkReuse(kind)
		return result
	}

	failed := fresh("failed", 1500*time.Millisecond, diagnosticEvent)
	failed.Error = &JobError{Message: "lint failed", Code: "E_LINT"}

	results := map[string]*JobResult{
		planned[0].Key(): fresh("success", 2500*time.Millisecond, artifactEvent),
		planned[1].Key(): failed,
		planned[2].Key(): fresh("canceled", 700*time.Millisecond),
		planned[3].Key(): fresh("skipped", 0),
		planned[4].Key(): reused("success", ReuseLocalCache, 900*time.Millisecond),
		planned[5].Key(): reused("success", ReuseRemoteCache, 800*time.Millisecond),
		planned[6].Key(): reused("success", ReuseCoalesced, 600*time.Millisecond),
		planned[7].Key(): fresh("mystery", 250*time.Millisecond),
		planned[8].Key(): reused("failed", ReuseLocalCache, 100*time.Millisecond),
	}
	return planned, results
}

// legacyBuckets is the six-bucket tally every renderer repeats verbatim today:
// internal/output/json.go Finish, internal/output/jsonl.go Finish,
// internal/output/text_finish.go Finish, internal/output/cloud_logging.go
// Finish, internal/mcp/adapter.go summarizeRun, internal/cli/jobs_helpers.go
// buildSessionStats and internal/watch/session_iteration.go
// buildWatchSessionStats (deleted when the watch loop became a
// replan policy over Engine.Run). Transcribed unchanged.
type legacyBuckets struct {
	succeeded, failed, canceled, skipped, cached, coalesced int
	total                                                   int
	durationMs                                              int64
}

func tallyLegacyBuckets(results map[string]*JobResult) legacyBuckets {
	var b legacyBuckets
	for _, result := range results {
		b.total++
		if result.Coalesced {
			b.coalesced++
			continue
		}
		if result.CacheHit {
			b.cached++
			continue
		}
		switch result.Status {
		case "success":
			b.succeeded++
		case "failed":
			b.failed++
		case "canceled":
			b.canceled++
		case "skipped":
			b.skipped++
		}
		// buildSessionStats/buildWatchSessionStats accumulate truncated
		// milliseconds per job, and only for jobs that executed.
		b.durationMs += result.Duration.Milliseconds()
	}
	return b
}

func TestReduceSession_MatchesLegacyTallyLoops(t *testing.T) {
	planned, results := canonicalFixture(t)
	legacy := tallyLegacyBuckets(results)

	session := ReduceSchedulerSession(planned, results, SessionOutcome{}, 5*time.Second, nil)

	for _, check := range []struct {
		name        string
		got, wanted int
	}{
		{"succeeded", session.Fresh.Succeeded, legacy.succeeded},
		{"failed", session.Fresh.Failed, legacy.failed},
		{"canceled", session.Fresh.Canceled, legacy.canceled},
		{"skipped", session.Fresh.Skipped, legacy.skipped},
		{"cached", session.Cached(), legacy.cached},
		{"coalesced", session.Reuse.Coalesced, legacy.coalesced},
		// mcp.summarizeRun and SessionStats count every result in Total.
		{"total", session.Tasks, legacy.total},
	} {
		if check.got != check.wanted {
			t.Errorf("%s: reducer %d, legacy tally %d", check.name, check.got, check.wanted)
		}
	}

	// output/json.go and output/jsonl.go compute Total as the sum of the six
	// buckets instead, which silently drops a status outside the vocabulary.
	// The reducer reproduces both figures, and they differ here by exactly the
	// one "mystery" job — proof that BucketTotal is not just Tasks renamed.
	wantBucketTotal := legacy.succeeded + legacy.failed + legacy.canceled +
		legacy.skipped + legacy.cached + legacy.coalesced
	if session.BucketTotal() != wantBucketTotal {
		t.Errorf("BucketTotal = %d, legacy bucket sum %d", session.BucketTotal(), wantBucketTotal)
	}
	if session.BucketTotal() == session.Tasks {
		t.Error("fixture no longer exercises the Tasks vs BucketTotal divergence")
	}

	if session.ExecutedDurationMs != legacy.durationMs {
		t.Errorf("ExecutedDurationMs = %d, legacy SessionStats %d", session.ExecutedDurationMs, legacy.durationMs)
	}
}

// legacyRunSucceeded is jobs.runSucceeded (internal/jobs/scheduler.go),
// transcribed unchanged. A later migration deleted the original and pointed
// SchedulerResult.Success at SessionResult.Success; the rule it encoded is still
// a shipped contract, so the assertion has to survive the deletion.
func legacyRunSucceeded(results map[string]*JobResult, outcome SessionOutcome) bool {
	if outcome.Aborted {
		return false
	}
	for _, r := range results {
		if r.Status == "failed" {
			return false
		}
	}
	return true
}

// TestReduceSession_MatchesRunSucceeded pins BOTH success predicates against the
// legacy rules they replace, including the divergence those rules already had:
// runSucceeded counts a REUSED failure, output/jsonl.go's Success does not.
// SessionResult keeps both histograms so A2b could migrate each consumer's
// DERIVATION without changing its VERDICT (see the ruling in result_reduce.go).
func TestReduceSession_MatchesRunSucceeded(t *testing.T) {
	planned, results := canonicalFixture(t)

	for _, outcome := range []SessionOutcome{{}, {Aborted: true, AbortedBy: AbortUser}} {
		session := ReduceSchedulerSession(planned, results, outcome, time.Second, nil)
		if got, want := session.Success(), legacyRunSucceeded(results, outcome); got != want {
			t.Errorf("aborted=%t: Success() = %t, runSucceeded = %t", outcome.Aborted, got, want)
		}
		if session.Aborted != outcome.Aborted || session.AbortedBy != outcome.AbortedBy {
			t.Errorf("aborted state = (%t, %q), want (%t, %q)",
				session.Aborted, session.AbortedBy, outcome.Aborted, outcome.AbortedBy)
		}
	}

	// The reused failure is invisible to the fresh histogram but not to the
	// status one; that difference is the whole reason both exist.
	session := ReduceSchedulerSession(planned, results, SessionOutcome{}, time.Second, nil)
	if session.Status.Failed != session.Fresh.Failed+1 {
		t.Fatalf("Status.Failed = %d, Fresh.Failed = %d; fixture must contain exactly one reused failure",
			session.Status.Failed, session.Fresh.Failed)
	}

	// A run whose ONLY failure was reused was the single input the two v1
	// predicates disagreed on. An earlier change collapsed them onto the strict
	// rule: a reused failure sinks the run everywhere. This case remains so the
	// unified verdict cannot quietly drift back to leniency.
	reusedFailureOnly := ReduceSchedulerSession(
		planned[8:9],
		map[string]*JobResult{planned[8].Key(): results[planned[8].Key()]},
		SessionOutcome{}, time.Second, nil,
	)
	if reusedFailureOnly.Success() {
		t.Error("Success() green-lit a run whose reused result was a failure")
	}
}

// TestReduceRun_MatchesSchedulerReduction pins the records-free reduction the
// A2b consumers call against the full one the scheduler publishes: identical
// counts and identical failure identity, with the structured records
// deliberately absent (they cost an event walk no counting consumer needs).
func TestReduceRun_MatchesSchedulerReduction(t *testing.T) {
	planned, results := canonicalFixture(t)

	for _, outcome := range []SessionOutcome{{}, {Aborted: true, AbortedBy: AbortUser}} {
		full := ReduceSchedulerSession(planned, results, outcome, time.Second, nil)
		lean := ReduceRun(planned, results, outcome)

		if lean.Tasks != full.Tasks || lean.Status != full.Status || lean.Fresh != full.Fresh ||
			lean.Reuse != full.Reuse || lean.BucketTotal() != full.BucketTotal() ||
			lean.ExecutedDurationMs != full.ExecutedDurationMs ||
			lean.Success() != full.Success() ||
			lean.Aborted != full.Aborted || lean.AbortedBy != full.AbortedBy {
			t.Errorf("aborted=%t: ReduceRun = %+v, scheduler reduction = %+v", outcome.Aborted, lean, full)
		}

		if len(lean.Failures) != len(full.Failures) {
			t.Fatalf("aborted=%t: ReduceRun reported %d failures, scheduler reduction %d",
				outcome.Aborted, len(lean.Failures), len(full.Failures))
		}
		for i := range lean.Failures {
			if lean.Failures[i].Key != full.Failures[i].Key ||
				lean.Failures[i].Project != full.Failures[i].Project ||
				lean.Failures[i].Job != full.Failures[i].Job ||
				lean.Failures[i].Error != full.Failures[i].Error {
				t.Errorf("failure %d: ReduceRun %+v, scheduler reduction %+v",
					i, lean.Failures[i], full.Failures[i])
			}
			if lean.Failures[i].Diagnostics != nil {
				t.Errorf("failure %d carries diagnostics; ReduceRun must not walk the event stream", i)
			}
		}
		if lean.Diagnostics != nil || lean.Artifacts != nil {
			t.Error("ReduceRun extracted structured records; that walk is the cost it exists to avoid")
		}
		if full.Diagnostics == nil || full.Artifacts == nil {
			t.Error("fixture no longer produces records, so the divergence above proves nothing")
		}
	}

	// Order still has to be deterministic without a plan: renderers that keep no
	// plan pass nil, and their failure lists must not iterate the result map.
	first := ReduceRun(nil, results, SessionOutcome{})
	for i := 0; i < 8; i++ {
		if !reflect.DeepEqual(ReduceRun(nil, results, SessionOutcome{}).Failures, first.Failures) {
			t.Fatal("ReduceRun(nil, ...) failure order varies between calls")
		}
	}
}

// TestReduceSession_MatchesAllHitWarmRun is the shape the performance budget
// cares about: every task a cache hit, nothing to allocate.
func TestReduceSession_MatchesAllHitWarmRun(t *testing.T) {
	planned := []*ScheduledJob{
		canonicalTestJob("a", "build", ""),
		canonicalTestJob("b", "build", ""),
	}
	results := map[string]*JobResult{}
	for _, job := range planned {
		result := &JobResult{Status: "success"}
		result.MarkReuse(ReuseLocalCache)
		results[job.Key()] = result
	}

	session := ReduceSchedulerSession(planned, results, SessionOutcome{}, time.Second, nil)
	if session.Cached() != 2 || session.Fresh.Total() != 0 || !session.Success() {
		t.Fatalf("warm run reduced to %+v", session)
	}
	if session.Diagnostics != nil || session.Artifacts != nil || session.Failures != nil {
		t.Error("warm all-hit run allocated record slices")
	}
}

// TestDiagnosticFromEvent_MatchesLegacyExtraction pins the canonical projection
// against the semantics internal/mcp/adapter.go diagnosticFromEvent and
// internal/output/diagnostic.go already implement: flat severity/message/code/
// file/line/column, with a nested "location" OBJECT overriding the flat fields
// field-by-field, and a zero override never winning. The expectations are
// spelled out rather than transcribed so a bug in the shared accessor cannot
// cancel itself out on both sides of the comparison.
func TestDiagnosticFromEvent_MatchesLegacyExtraction(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
		want TaskDiagnostic
	}{
		{
			name: "flat fields",
			line: `{"v":2,"type":"diagnostic","severity":"warning","message":"m","code":"c",` +
				`"file":"src/a.ts","line":4,"column":9}`,
			want: TaskDiagnostic{Severity: "warning", Message: "m", Code: "c", File: "src/a.ts", Line: 4, Column: 9},
		},
		{
			name: "nested location",
			line: `{"v":2,"type":"diagnostic","severity":"error",` +
				`"location":{"file":"src/b.ts","line":7,"column":2}}`,
			want: TaskDiagnostic{Severity: "error", File: "src/b.ts", Line: 7, Column: 2},
		},
		{
			name: "nested overrides flat, absent nested field keeps flat",
			line: `{"v":2,"type":"diagnostic","file":"flat.ts","line":1,"column":6,` +
				`"location":{"file":"nested.ts","line":5}}`,
			want: TaskDiagnostic{File: "nested.ts", Line: 5, Column: 6},
		},
		{
			name: "zero nested line does not clear the flat one",
			line: `{"v":2,"type":"diagnostic","line":3,"location":{"file":"n.ts","line":0}}`,
			want: TaskDiagnostic{File: "n.ts", Line: 3},
		},
		{
			name: "location that is not an object is ignored",
			line: `{"v":2,"type":"diagnostic","file":"flat.ts","location":"src/c.ts:1"}`,
			want: TaskDiagnostic{File: "flat.ts"},
		},
	} {
		if got := diagnosticFromEvent(parseTestEvent(t, test.line)); got != test.want {
			t.Errorf("%s: got %+v, want %+v", test.name, got, test.want)
		}
	}

	// An event with no data at all must not panic and must produce nothing.
	if got := diagnosticFromEvent(RawJobEvent{Type: EventTypeDiagnostic}); got != (TaskDiagnostic{}) {
		t.Errorf("data-less diagnostic event produced %+v", got)
	}
}

// TestReduceSession_DiagnosticsAndFailuresAreOrderedAndAttributed pins that the
// aggregate lists mcp.runResult builds by hand come out of the reducer with the
// same content, attribution and (plan) ordering.
func TestReduceSession_DiagnosticsAndFailuresAreOrderedAndAttributed(t *testing.T) {
	planned, results := canonicalFixture(t)
	session := ReduceSchedulerSession(planned, results, SessionOutcome{}, time.Second, nil)

	if len(session.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly the one emitted", session.Diagnostics)
	}
	want := TaskDiagnostic{
		Project: "bad", Job: "lint", Severity: "error", Code: "lint/unused",
		Message: "unused var", File: "src/a.ts", Line: 12, Column: 3,
	}
	if session.Diagnostics[0] != want {
		t.Errorf("diagnostic = %+v, want %+v", session.Diagnostics[0], want)
	}

	if len(session.Artifacts) != 1 || session.Artifacts[0].Project != "ok" ||
		session.Artifacts[0].Path != "out/a.zip" {
		t.Errorf("artifacts = %+v", session.Artifacts)
	}

	// Both the executed failure and the reused one are listed: a cache hit that
	// failed (a restored declared output that drifted from this checkout) is
	// reported by nothing else in the run. Only a COALESCED failure is left to
	// its leader's row.
	if len(session.Failures) != 2 {
		t.Fatalf("failures = %+v, want the executed failure and the reused one", session.Failures)
	}
	if session.Failures[1].Project != "reusedfail" || session.Failures[1].Job != "lint" {
		t.Errorf("reused failure = %+v", session.Failures[1])
	}
	failure := session.Failures[0]
	if failure.Project != "bad" || failure.Job != "lint" || failure.Error != "lint failed" ||
		len(failure.Diagnostics) != 1 {
		t.Errorf("failure = %+v", failure)
	}
}

// TestReduceSession_IsDeterministic pins the ordering invariant: a Go map
// iterates randomly, so the reduction has to impose the plan's order (then
// sorted keys for anything the plan does not name) or the lists it produces
// would differ between two reductions of the same run.
func TestReduceSession_IsDeterministic(t *testing.T) {
	planned, results := canonicalFixture(t)
	// Result keys the plan does not name, as drainInFlight can produce.
	results["/orphan:build"] = &JobResult{Status: "failed", Error: &JobError{Message: "orphan"}}
	results["/another:build"] = &JobResult{Status: "failed", Error: &JobError{Message: "another"}}

	first := ReduceSchedulerSession(planned, results, SessionOutcome{}, time.Second, nil)
	for i := 0; i < 20; i++ {
		next := ReduceSchedulerSession(planned, results, SessionOutcome{}, time.Second, nil)
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("reduction %d differs:\n first = %+v\n next  = %+v", i, first.Failures, next.Failures)
		}
	}
	if first.Tasks != len(results) {
		t.Errorf("Tasks = %d, want every result including the unplanned ones (%d)", first.Tasks, len(results))
	}
	if got := first.Failures[len(first.Failures)-1].Key; got != "/orphan:build" {
		t.Errorf("unplanned failures are not key-sorted last: %q", got)
	}
}

func TestJobResult_ReuseKindAndBooleansAgree(t *testing.T) {
	for _, test := range []struct {
		kind      ReuseKind
		cacheHit  bool
		coalesced bool
		outcome   string
	}{
		{ReuseNone, false, false, "success"},
		{ReuseLocalCache, true, false, JobOutcomeCached},
		{ReuseRemoteCache, true, false, JobOutcomeCached},
		{ReuseCoalesced, false, true, JobOutcomeCoalesced},
	} {
		result := &JobResult{Status: "success"}
		result.MarkReuse(test.kind)
		if result.CacheHit != test.cacheHit || result.Coalesced != test.coalesced {
			t.Errorf("%s: CacheHit=%t Coalesced=%t, want %t/%t",
				test.kind, result.CacheHit, result.Coalesced, test.cacheHit, test.coalesced)
		}
		if got := result.Outcome(); got != test.outcome {
			t.Errorf("%s: Outcome() = %q, want %q (the recorded session vocabulary)", test.kind, got, test.outcome)
		}
		if got := result.ReuseKind(); got != test.kind {
			t.Errorf("ReuseKind() = %q, want %q", got, test.kind)
		}
	}

	// A JobResult assembled without MarkReuse (tests, pre-A2a call sites) still
	// classifies from the booleans, so nothing silently reads as "not reused".
	for _, test := range []struct {
		result *JobResult
		want   ReuseKind
	}{
		{&JobResult{Status: "success"}, ReuseNone},
		{&JobResult{Status: "success", CacheHit: true}, ReuseLocalCache},
		{&JobResult{Status: "success", Coalesced: true}, ReuseCoalesced},
	} {
		if got := test.result.ReuseKind(); got != test.want {
			t.Errorf("legacy booleans %+v classified as %q, want %q", test.result, got, test.want)
		}
	}
}

// TestTaskResultOf_MatchesLegacyOutcomeVocabulary pins that the canonical
// outcome string is the one already written to sessions on disk, for every
// combination of status and reuse. workspace_state reads sessions written by
// older CLIs, so this vocabulary is a compatibility surface.
func TestTaskResultOf_MatchesLegacyOutcomeVocabulary(t *testing.T) {
	job := canonicalTestJob("a", "build", "build-compile")
	for _, status := range []string{"success", "failed", "canceled", "skipped"} {
		for _, kind := range []ReuseKind{ReuseNone, ReuseLocalCache, ReuseRemoteCache, ReuseCoalesced} {
			result := &JobResult{Status: status}
			result.MarkReuse(kind)
			task := TaskResultOf(job, result)
			if got, want := task.Outcome(), result.Outcome(); got != want {
				t.Errorf("status=%s reuse=%s: TaskResult.Outcome = %q, JobResult.Outcome = %q",
					status, kind, got, want)
			}
			if got, want := string(task.Status), result.Status; got != want {
				t.Errorf("status=%s reuse=%s: TaskStatus = %q, JobResult.Status = %q", status, kind, got, want)
			}
			if task.TaskKind != "build-compile" || task.Project != "a" || task.Extension != "@putnami/test" {
				t.Errorf("identity not stamped: %+v", task)
			}
		}
	}
}

// TestTaskResultOf_PrefersCanonicalOverSyntheticEvents pins the direction of
// the dependency: when a producer supplied typed records, the event stream is
// not consulted at all. Without this, a future edit could quietly reintroduce
// the second parse the batch path used to depend on.
func TestTaskResultOf_PrefersCanonicalOverSyntheticEvents(t *testing.T) {
	job := canonicalTestJob("a", "lint", "")
	canonical := TaskResult{Metrics: []TaskMetric{{Name: "lint-errors", Value: 1, Unit: "count"}}}
	result := &JobResult{
		Status:    "success",
		Canonical: &canonical,
		Events: []RawJobEvent{
			parseTestEvent(t, `{"v":2,"type":"metric","name":"from-events","value":99,"unit":"count"}`),
			parseTestEvent(t, `{"v":2,"type":"diagnostic","severity":"error","message":"from-events"}`),
		},
	}

	task := TaskResultOf(job, result)
	if len(task.Metrics) != 1 || task.Metrics[0].Name != "lint-errors" {
		t.Errorf("metrics were re-derived from the event stream: %+v", task.Metrics)
	}
	if len(task.Diagnostics) != 0 {
		t.Errorf("diagnostics were re-derived from the event stream: %+v", task.Diagnostics)
	}
}

// TestTaskResultOf_InputDigestNamesTheKeyOfEveryVerdict pins the one derivation
// of a task's input digest: the key in the `sha256:` spelling for every task
// that reached a verdict, whatever its reuse, and nothing for a skipped task, a
// status outside the vocabulary, a task with no key, or a value the store could
// not have computed.
func TestTaskResultOf_InputDigestNamesTheKeyOfEveryVerdict(t *testing.T) {
	job := canonicalTestJob("a", "build", "build-compile")
	key := strings.Repeat("0f", 32)
	for _, status := range []string{"success", "failed", "canceled"} {
		for _, kind := range []ReuseKind{ReuseNone, ReuseLocalCache, ReuseRemoteCache, ReuseCoalesced} {
			result := &JobResult{Status: status, CacheKey: key}
			result.MarkReuse(kind)
			if got := TaskResultOf(job, result).InputDigest; got != "sha256:"+key {
				t.Errorf("status=%s reuse=%s: inputDigest = %q, want the sha256: spelling of the key", status, kind, got)
			}
			if got := TaskSummaryOf(job, result).InputDigest; got != "sha256:"+key {
				t.Errorf("status=%s reuse=%s: the summary projection disagrees: %q", status, kind, got)
			}
		}
	}
	for name, result := range map[string]*JobResult{
		"skipped":             {Status: "skipped", CacheKey: key},
		"outside vocabulary":  {Status: "mystery", CacheKey: key},
		"no cache identity":   {Status: "success"},
		"uppercase hex":       {Status: "success", CacheKey: strings.ToUpper(key)},
		"not a sha256 length": {Status: "success", CacheKey: key[:40]},
	} {
		if got := TaskResultOf(job, result).InputDigest; got != "" {
			t.Errorf("%s: inputDigest = %q, want empty", name, got)
		}
	}
}

func TestNormalizeStatus_SharesOneVocabularyWithTaskStatus(t *testing.T) {
	for input, want := range map[string]string{
		"success": "success", "succeeded": "success", "OK": "success", "ok": "success",
		"failed": "failed", "failure": "failed", "error": "failed", "FAILED": "failed",
		"skipped": "skipped", "skip": "skipped", "SKIP": "skipped",
		"canceled": "canceled",
		"mystery":  "mystery",
		"":         "",
	} {
		if got := NormalizeStatus(input); got != want {
			t.Errorf("NormalizeStatus(%q) = %q, want %q", input, got, want)
		}
	}
}
