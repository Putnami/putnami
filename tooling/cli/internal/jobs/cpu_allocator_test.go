package jobs

import (
	"fmt"
	"math"
	"sync"
	"testing"
)

// grant mirrors what the scheduler does at dispatch: take the deterministic
// recommendation, then record it. There is no acquire step — a ceiling is not a
// live claim — so these tests drive the same two calls production drives.
func grant(
	a *cpuAllocator,
	key string,
	weight, expectedCPUMs, expectedWallMs float64,
	historicalCeiling, criticalityCeiling int,
) cpuRecommendation {
	recommendation := a.recommend(weight, expectedCPUMs, expectedWallMs, historicalCeiling, criticalityCeiling)
	recommendation.budget = a.recordGrant(key, weight, expectedCPUMs, recommendation.budget)
	return recommendation
}

func TestCPUAllocator_ColdTaskGetsWholeMachine(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(10, "")
	if got := grant(a, "p:lint", 1, 0, 0, 0, 0).budget; got != 10 {
		t.Fatalf("budget = %d, want 10 for a task without history", got)
	}
}

func TestCPUAllocator_GrantDoesNotDependOnAcquireOrderOrLiveCohort(t *testing.T) {
	t.Parallel()
	type input struct {
		key                   string
		weight, cpuMs, wallMs float64
		concurrency           int
	}
	inputs := []input{
		{key: "short", weight: 1, cpuMs: 1_000, wallMs: 1_000, concurrency: 1},
		{key: "long-pole", weight: 8, cpuMs: 65_000, wallMs: 88_000, concurrency: 1},
		{key: "parallel", weight: 1, cpuMs: 5_000, wallMs: 800, concurrency: 1},
	}

	grants := func(order []int) map[string]int {
		a := newCPUAllocator(10, "")
		out := make(map[string]int, len(order))
		for _, i := range order {
			in := inputs[i]
			out[in.key] = grant(a, in.key, in.weight, in.cpuMs, in.wallMs, in.concurrency, 0).budget
		}
		return out
	}
	forward := grants([]int{0, 1, 2})
	reverse := grants([]int{2, 1, 0})
	for _, in := range inputs {
		if forward[in.key] != reverse[in.key] {
			t.Fatalf("%s grant changed with acquire order: %d vs %d", in.key, forward[in.key], reverse[in.key])
		}
	}
	if forward["long-pole"] != 8 {
		t.Fatalf("long-pole budget = %d, want stable learned floor 8", forward["long-pole"])
	}
}

func TestCPUAllocator_MeasuredParallelismSetsDemand(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(10, "")
	if got := grant(a, "biome", 1, 5_020, 730, 1, 0).budget; got != 7 {
		t.Fatalf("budget = %d, want ceil(5020/730) = 7", got)
	}
}

func TestCPUAllocator_FractionalMeasuredParallelismUsesBindingCeiling(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(10, "")
	recommendation := a.recommend(1, 1_020, 1_000, 0, 0)
	if recommendation.budget != 2 || recommendation.unweightedCeiling != 2 {
		t.Fatalf("recommendation = %+v, want measured 1.02-core demand bounded by ceiling 2", recommendation)
	}
}

func TestCPUAllocator_RecommendationReportsSanitizedHistoryProvenance(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(6, "")
	cold := a.recommend(math.NaN(), math.Inf(1), math.Inf(1), -1, 0)
	if cold.budget != 6 || cold.fromHistory || cold.unweightedCeiling != 0 {
		t.Fatalf("sanitized cold recommendation = %+v, want whole-machine non-history ceiling", cold)
	}
	known := a.recommend(4, 1_000, 1_000, 0, 0)
	if known.budget != 4 || !known.fromHistory || known.unweightedCeiling != 1 {
		t.Fatalf("history recommendation = %+v, want weighted budget 4 from unweighted ceiling 1", known)
	}
}

func TestCPUAllocator_WeightScalesDemandForEqualHistory(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(10, "")
	defaultWeight := grant(a, "default", 1, 2_000, 1_000, 0, 0).budget
	weighted := grant(a, "weighted", 4, 2_000, 1_000, 0, 0).budget
	if defaultWeight != 2 || weighted != 8 {
		t.Fatalf("budgets = %d/%d, want relative weight 4 to scale equal demand 2 to 8", defaultWeight, weighted)
	}
}

func TestCPUAllocator_HistoricalUnweightedCeilingProtectsBudgetShapedLongPole(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(10, "")
	// Occupancy alone claims one core, but prior history-backed policy proved an
	// unweighted ceiling of eight. Do not recreate the known long-pole regression
	// by lowering it from a throttled CPU/wall sample.
	if got := grant(a, "cli:test", 1, 65_000, 88_000, 8, 0).budget; got != 8 {
		t.Fatalf("budget = %d, want historical floor 8", got)
	}
}

func TestCPUAllocator_StaleHistoryIsSanitizedAndCapped(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(6, "")
	if got := grant(a, "stale", 1, 1_000, 1_000, 1_000, 0).budget; got != 6 {
		t.Fatalf("budget = %d, want machine cap 6", got)
	}
	if got := grant(a, "negative", 1, 1_000, 1_000, -4, 0).budget; got != 1 {
		t.Fatalf("budget = %d, want invalid historical concurrency ignored", got)
	}
	if got := grant(a, "invalid", math.NaN(), math.Inf(1), math.Inf(1), -1, 0).budget; got != 6 {
		t.Fatalf("budget = %d, want invalid empty history to use cold fallback 6", got)
	}
}

func TestAcquireGroupCPUBudget_UsesMaximumMemberCeilingWithoutPreparedClass(t *testing.T) {
	t.Parallel()
	light := statJob("/a", "test")
	light.ExpectedCPUWorkMs = 100
	light.ExpectedWallMs = 100
	light.HistoricalCPUCeiling = 1
	heavy := statJob("/b", "test")
	heavy.CPUWeight = 3
	heavy.ExpectedCPUWorkMs = 100
	heavy.ExpectedWallMs = 100
	heavy.HistoricalCPUCeiling = 1

	scheduler := &Scheduler{cpuAlloc: newCPUAllocator(8, "")}
	scheduler.acquireGroupCPUBudget([]taskWork{{job: light}, {job: heavy}})

	if light.CPUBudget != 3 || heavy.CPUBudget != 3 {
		t.Fatalf("batch budgets = %d/%d, want maximum member ceiling 3", light.CPUBudget, heavy.CPUBudget)
	}
	if light.CPUBudgetHistoryEligible || heavy.CPUBudgetHistoryEligible {
		t.Fatal("batch-inflated grant must not become a singleton history floor")
	}
	grants := scheduler.cpuAlloc.grantsSnapshot()
	if len(grants) != 1 || grants[0].weight != 4 || grants[0].expectedCPUMs != 100 || grants[0].budget != 3 {
		t.Fatalf("batch grant = %+v, want one weight=4 cpu=100 budget=3 record", grants)
	}
}

func TestAcquireGroupCPUBudget_ColdMemberPreservesWholeMachineFallback(t *testing.T) {
	t.Parallel()
	known := statJob("/a", "test")
	known.ExpectedCPUWorkMs = 100
	known.ExpectedWallMs = 100
	known.HistoricalCPUCeiling = 1
	cold := statJob("/b", "test")

	scheduler := &Scheduler{cpuAlloc: newCPUAllocator(8, "")}
	scheduler.acquireGroupCPUBudget([]taskWork{{job: known}, {job: cold}})

	if known.CPUBudget != 8 || cold.CPUBudget != 8 {
		t.Fatalf("batch budgets = %d/%d, want machine cap 8 with a cold member", known.CPUBudget, cold.CPUBudget)
	}
}

func TestCPUAllocator_GrantsSnapshotSortedAndComplete(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(8, "")
	grant(a, "z:test", 1, 1_000, 1_000, 1, 0)
	grant(a, "a:lint", 2, 2_000, 1_000, 2, 0)
	grants := a.grantsSnapshot()
	if len(grants) != 2 {
		t.Fatalf("grants = %d, want 2", len(grants))
	}
	if grants[0].job != "a:lint" || grants[1].job != "z:test" {
		t.Fatalf("grants not sorted by job: %+v", grants)
	}
	if grants[0].weight != 2 || grants[0].budget != 4 || grants[0].expectedCPUMs != 2_000 {
		t.Fatalf("unexpected grant record: %+v", grants[0])
	}
}

func TestCPUAllocator_ConcurrentUseStaysDeterministic(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(8, "")
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			budget := grant(a, "job"+string(rune('a'+n%26)), 3, 3_000, 3_000, 1, 0).budget
			if budget != 3 {
				t.Errorf("budget = %d, want deterministic value 3", budget)
			}
		}(i)
	}
	wg.Wait()
	if got := grant(a, "after", 3, 3_000, 3_000, 1, 0).budget; got != 3 {
		t.Fatalf("budget = %d after concurrent cohort, want 3", got)
	}
}

// The measured policy derives a ceiling from cpu/wall, which is a measurement
// of what the task was ALLOWED to do. The putnami CLI's own test suite reads
// 0.73 there — it is dominated by re-exec'd child processes — so measured alone
// hands the repository's longest task one core and makes the makespan worse.
// critical-path raises that to the machine because shortening the longest chain
// is the only work that shortens the run.
func TestCPUAllocator_CriticalPathLiftsTheLongPoleMeasuredWouldStarve(t *testing.T) {
	t.Parallel()
	const (
		longPoleWallMs = 147_450.0
		longPoleCPUMs  = 107_268.0
		machineCPU     = 10
	)

	measured := newCPUAllocator(machineCPU, cpuBudgetPolicyMeasured)
	if got := measured.recommend(1, longPoleCPUMs, longPoleWallMs, 0, machineCPU).budget; got != 1 {
		t.Fatalf("measured ceiling = %d, want the occupancy answer 1", got)
	}

	critical := newCPUAllocator(machineCPU, cpuBudgetPolicyCriticalPath)
	recommendation := critical.recommend(1, longPoleCPUMs, longPoleWallMs, 0, machineCPU)
	if recommendation.budget != machineCPU {
		t.Fatalf("critical-path ceiling = %d, want the whole machine %d", recommendation.budget, machineCPU)
	}
	// The plan-scoped lift must NOT be persisted: this task is a long pole in
	// `--all` and an ordinary job in a single-project run.
	if recommendation.unweightedCeiling != 1 {
		t.Fatalf("persisted unweighted ceiling = %d, want the plan-independent occupancy 1",
			recommendation.unweightedCeiling)
	}
}

// Criticality only ever RAISES a ceiling. A task whose measured occupancy
// exceeds its makespan share keeps the larger, measured answer.
func TestCPUAllocator_CriticalityNeverLowersMeasuredDemand(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(10, cpuBudgetPolicyCriticalPath)
	got := a.recommend(1, 8_000, 1_000, 0, 2)
	if got.budget != 8 {
		t.Fatalf("ceiling = %d, want the measured demand 8 kept over the criticality share 2", got.budget)
	}
}

func TestCPUAllocator_CriticalPathStillHonoursWeightAndMachineCap(t *testing.T) {
	t.Parallel()
	a := newCPUAllocator(10, cpuBudgetPolicyCriticalPath)
	if got := a.recommend(4, 1_000, 1_000, 0, 2).budget; got != 8 {
		t.Fatalf("weighted ceiling = %d, want criticality 2 x weight 4", got)
	}
	if got := a.recommend(4, 1_000, 1_000, 0, 6).budget; got != 10 {
		t.Fatalf("weighted ceiling = %d, want the machine cap 10", got)
	}
}

func TestPlanCriticalityCeilings_ScalesByWallShareWithinThePlan(t *testing.T) {
	t.Parallel()
	longPole := statJob("/cli", "test")
	longPole.ExpectedWallMs = 100_000
	half := statJob("/mid", "test")
	half.ExpectedWallMs = 50_000
	tiny := statJob("/tiny", "lint")
	tiny.ExpectedWallMs = 100
	unknown := statJob("/unknown", "build")

	ceilings := planCriticalityCeilings([]*ScheduledJob{longPole, half, tiny, unknown}, 10, 4)

	if ceilings[longPole.Key()] != 10 {
		t.Fatalf("long pole ceiling = %d, want the whole machine", ceilings[longPole.Key()])
	}
	// Superlinear decay: half the long pole's length is a QUARTER of the machine,
	// not half. Cores given to work that finishes inside the long pole's shadow
	// buy no makespan and come out of the one chain where they would.
	if ceilings[half.Key()] != 3 {
		t.Fatalf("half-length ceiling = %d, want ceil(0.5^2 * 10) = 3", ceilings[half.Key()])
	}
	if ceilings[tiny.Key()] != 1 {
		t.Fatalf("short job ceiling = %d, want the floor 1", ceilings[tiny.Key()])
	}
	if _, ok := ceilings[unknown.Key()]; ok {
		t.Fatal("a job with no wall history must get no criticality ceiling, keeping the first-cold rule")
	}
}

// A plan where nothing has run yet must not synthesize a ranking out of zeros:
// the existing first-cold rule (the whole machine) has to survive untouched.
func TestPlanCriticalityCeilings_EmptyWithoutWallHistory(t *testing.T) {
	t.Parallel()
	if got := planCriticalityCeilings([]*ScheduledJob{statJob("/a", "test"), nil}, 8, 4); got != nil {
		t.Fatalf("ceilings = %+v, want none without wall history", got)
	}
}

// One default has to serve repositories of different shape. A plan bounded by
// throughput — many comparable tasks, none of them still running at the end —
// gets no lift at all, so critical-path degrades to measured instead of handing
// big ceilings to work that cannot move the makespan.
func TestPlanCriticalityCeilings_SkipsThroughputBoundPlans(t *testing.T) {
	t.Parallel()
	// Shaped from a large consumer workspace's real store: the longest task is
	// a small fraction of total wall and several peers sit within 2x of it.
	var flat []*ScheduledJob
	for i := range 40 {
		job := statJob(fmt.Sprintf("/svc%d", i), "test")
		job.ExpectedWallMs = int64(30_000 - 300*i)
		flat = append(flat, job)
	}
	if got := planCriticalityCeilings(flat, 10, 20); got != nil {
		t.Fatalf("throughput-bound plan received %d lifts, want none", len(got))
	}

	// Shaped from this repo: one task dwarfs a tail that drains quickly.
	pole := statJob("/cli", "test")
	pole.ExpectedWallMs = 76_800
	bounded := []*ScheduledJob{pole}
	for i := range 20 {
		job := statJob(fmt.Sprintf("/small%d", i), "test")
		job.ExpectedWallMs = 17_000
		bounded = append(bounded, job)
	}
	got := planCriticalityCeilings(bounded, 10, 20)
	if got[pole.Key()] != 10 {
		t.Fatalf("makespan-bound long pole ceiling = %d, want the machine", got[pole.Key()])
	}
}
