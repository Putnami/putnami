package jobs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildTuningReport_CPUBudgetIncludesClaimExpectation(t *testing.T) {
	report := buildTuningReport(
		ParallelDecision{}, nil, nil, nil,
		[]cpuAllocation{{job: "/cli:test", weight: 2, expectedCPUMs: 12_500, budget: 4}},
	)
	if len(report.CPUBudgets) != 1 || report.CPUBudgets[0].ExpectedCPUMs != 12_500 {
		t.Fatalf("CPU budgets = %+v, want the allocator's resolved expectation", report.CPUBudgets)
	}
	wire, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"expectedCpuMs":12500`) {
		t.Fatalf("scheduler JSON does not include the history input: %s", wire)
	}
}

func TestComputeCriticalPath_LongestDurationChain(t *testing.T) {
	//   a
	//  / \
	// b   d
	// |
	// c
	jobA := makeScheduledJob("proj", "a", nil)
	jobB := makeScheduledJob("proj", "b", []string{"/proj:a"})
	jobC := makeScheduledJob("proj", "c", []string{"/proj:b"})
	jobD := makeScheduledJob("proj", "d", []string{"/proj:a"})
	planned := []*ScheduledJob{jobA, jobB, jobC, jobD}

	results := map[string]*JobResult{
		"/proj:a": {Duration: 100 * time.Millisecond},
		"/proj:b": {Duration: 200 * time.Millisecond},
		"/proj:c": {Duration: 50 * time.Millisecond},
		"/proj:d": {Duration: 500 * time.Millisecond},
	}

	cp := computeCriticalPath(planned, results)
	if cp == nil {
		t.Fatal("expected a critical path")
	}
	// a→d (100+500=600) beats a→b→c (100+200+50=350).
	if cp.DurationMs != 600 {
		t.Errorf("critical path duration = %d, want 600", cp.DurationMs)
	}
	if len(cp.Chain) != 2 {
		t.Fatalf("chain length = %d, want 2: %+v", len(cp.Chain), cp.Chain)
	}
	if cp.Chain[0].Job != "/proj:a" || cp.Chain[1].Job != "/proj:d" {
		t.Errorf("chain = %v, want [/proj:a /proj:d]", []string{cp.Chain[0].Job, cp.Chain[1].Job})
	}
	if cp.Chain[1].DurationMs != 500 {
		t.Errorf("leaf duration = %d, want 500", cp.Chain[1].DurationMs)
	}
}

func TestComputeCriticalPath_IgnoresOutOfPlanDeps(t *testing.T) {
	job := makeScheduledJob("proj", "a", []string{"/missing:x"})
	results := map[string]*JobResult{"/proj:a": {Duration: 100 * time.Millisecond}}

	cp := computeCriticalPath([]*ScheduledJob{job}, results)
	if cp == nil || cp.DurationMs != 100 {
		t.Fatalf("critical path = %+v, want duration 100", cp)
	}
	if len(cp.Chain) != 1 || cp.Chain[0].Job != "/proj:a" {
		t.Errorf("chain = %+v, want single /proj:a node", cp.Chain)
	}
}

func TestComputeCriticalPath_IncludesSerializeAfter(t *testing.T) {
	lintFix := makeScheduledJob("proj", "lint~format", nil)
	build := makeScheduledJob("proj", "build~compile", nil)
	build.SerializeAfter = []string{lintFix.Key()}

	results := map[string]*JobResult{
		lintFix.Key(): {Duration: 100 * time.Millisecond},
		build.Key():   {Duration: 75 * time.Millisecond},
	}

	cp := computeCriticalPath([]*ScheduledJob{lintFix, build}, results)
	if cp == nil {
		t.Fatal("expected a critical path")
	}
	if cp.DurationMs != 175 {
		t.Errorf("critical path duration = %d, want 175", cp.DurationMs)
	}
	if len(cp.Chain) != 2 || cp.Chain[0].Job != lintFix.Key() || cp.Chain[1].Job != build.Key() {
		t.Errorf("chain = %+v, want [%s %s]", cp.Chain, lintFix.Key(), build.Key())
	}
}

func TestComputeCriticalPath_NoDurationsReturnsNil(t *testing.T) {
	planned := []*ScheduledJob{makeScheduledJob("proj", "a", nil)}
	if cp := computeCriticalPath(planned, map[string]*JobResult{}); cp != nil {
		t.Fatalf("expected nil critical path without durations, got %+v", cp)
	}
	if cp := computeCriticalPath(nil, nil); cp != nil {
		t.Fatalf("expected nil critical path for empty plan, got %+v", cp)
	}
}

func TestAggregateReadyWait_GroupsByCommandSortedByTotal(t *testing.T) {
	planned := []*ScheduledJob{
		makeScheduledJob("proj", "build~x", nil),
		makeScheduledJob("proj", "build~y", nil),
		makeScheduledJob("proj", "test~z", nil),
	}
	waits := map[string]time.Duration{
		"/proj:build~x": 50 * time.Millisecond,
		"/proj:build~y": 30 * time.Millisecond,
		"/proj:test~z":  200 * time.Millisecond,
	}

	out := aggregateReadyWait(planned, waits)
	if len(out) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(out), out)
	}
	// Sorted by total wait descending: test (200) before build (80).
	if out[0].Command != "test" || out[0].TotalMs != 200 || out[0].Jobs != 1 || out[0].MaxMs != 200 {
		t.Errorf("group[0] = %+v, want test total=200 jobs=1 max=200", out[0])
	}
	if out[1].Command != "build" || out[1].TotalMs != 80 || out[1].Jobs != 2 || out[1].MaxMs != 50 {
		t.Errorf("group[1] = %+v, want build total=80 jobs=2 max=50", out[1])
	}
}

func TestAggregateReadyWait_DropsSubMillisecondAndEmpty(t *testing.T) {
	planned := []*ScheduledJob{makeScheduledJob("proj", "build~x", nil)}
	waits := map[string]time.Duration{"/proj:build~x": 200 * time.Microsecond}
	if out := aggregateReadyWait(planned, waits); out != nil {
		t.Errorf("expected nil for sub-millisecond waits, got %+v", out)
	}
	if out := aggregateReadyWait(planned, nil); out != nil {
		t.Errorf("expected nil for no waits, got %+v", out)
	}
}

func TestSchedulerMetrics_ReadyWaits(t *testing.T) {
	m := newSchedulerMetrics()
	now := time.Now()
	m.timing["started"] = &jobTiming{readyAt: now, startAt: now.Add(50 * time.Millisecond)}
	m.timing["never-started"] = &jobTiming{readyAt: now}
	m.timing["start-before-ready"] = &jobTiming{readyAt: now.Add(10 * time.Millisecond), startAt: now}

	waits := m.readyWaits()
	if got := waits["started"]; got != 50*time.Millisecond {
		t.Errorf("ready wait = %v, want 50ms", got)
	}
	if _, ok := waits["never-started"]; ok {
		t.Error("job that never started should have no ready wait")
	}
	if _, ok := waits["start-before-ready"]; ok {
		t.Error("negative ready wait should be skipped")
	}
}

func TestSchedulerMetrics_MarkReadyAndStartAreIdempotent(t *testing.T) {
	m := newSchedulerMetrics()
	m.markReady("a")
	firstReady := m.timing["a"].readyAt
	m.markReady("a")
	if m.timing["a"].readyAt != firstReady {
		t.Error("markReady should record only the first timestamp")
	}

	m.markStart("a")
	firstStart := m.timing["a"].startAt
	m.markStart("a")
	if m.timing["a"].startAt != firstStart {
		t.Error("markStart should record only the first timestamp")
	}
}
