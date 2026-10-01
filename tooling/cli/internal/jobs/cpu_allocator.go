package jobs

import (
	"math"
	"sort"
	"sync"
)

// cpuAllocation records one budget grant for the run's tuning report.
type cpuAllocation struct {
	job           string
	weight        float64
	expectedCPUMs float64
	budget        int
}

// cpuBudgetPolicy selects which deterministic quantity sizes a task's ceiling.
// Both answers are defensible and the right one depends on the repository's
// shape, so it is configurable rather than assumed.
type cpuBudgetPolicy string

const (
	// cpuBudgetPolicyCriticalPath sizes a ceiling by the share of the plan's
	// makespan the task occupies, floored by what its history proves it used.
	// Shortening the longest chain shortens the run one-for-one; shortening
	// anything running in its shadow shortens nothing. It suits a repository
	// with a pronounced long pole, where measured occupancy is the quantity a
	// previous throttling produced rather than the quantity the task could use.
	cpuBudgetPolicyCriticalPath cpuBudgetPolicy = "critical-path"
	// cpuBudgetPolicyMeasured sizes a ceiling by the parallelism the task's own
	// history demonstrably consumed, and nothing else. It suits a repository of
	// many comparable tasks, or a shared machine where honest per-task
	// accounting matters more than the makespan of one chain.
	cpuBudgetPolicyMeasured cpuBudgetPolicy = "measured"
)

// normalizeCPUBudgetPolicy resolves the configured policy. An empty value is
// the default; an unrecognized one is rejected before reaching here, so this
// never silently reinterprets a policy the operator asked for.
func normalizeCPUBudgetPolicy(value string) cpuBudgetPolicy {
	if cpuBudgetPolicy(value) == cpuBudgetPolicyMeasured {
		return cpuBudgetPolicyMeasured
	}
	return cpuBudgetPolicyCriticalPath
}

// cpuAllocator turns a job's persisted execution history into a CPU ceiling.
// A grant depends only on the machine size and that job's inputs; which peers
// happen to be live when it spawns is deliberately irrelevant. This makes the
// grant reproducible for an identical machine and history instead of letting
// subprocess timing choose it.
//
// Four signals form the ceiling:
//   - CPU ms / wall ms records parallelism the task demonstrably consumed;
//   - the largest prior successful history-backed singleton ceiling, recorded
//     before weight is applied, prevents a new policy from lowering a task
//     merely because a throttled observation made its occupancy look low;
//   - under the critical-path policy, the task's share of the plan's makespan
//     RAISES the two above, because occupancy measures what a task was allowed
//     to do and cannot on its own lift a long pole out of a starved fixed point;
//   - the configured/learned weight scales the result as a relative multiplier.
//
// A task with no history receives the whole machine. Cold history previously
// let a long pole draw one core from a busy live cohort and made the first run
// several times slower. That conservative fallback is not persisted as proven
// concurrency; the next run uses the first execution's measured evidence.
type cpuAllocator struct {
	mu       sync.Mutex
	totalCPU int
	policy   cpuBudgetPolicy
	grants   []cpuAllocation
}

// cpuRecommendation keeps provenance and the pre-weight ceiling beside the
// final budget so callers cannot independently reinterpret sanitized history.
// Only a history-backed singleton recommendation may teach the stats store a
// future floor; cold and batch ceilings are deliberately excluded.
type cpuRecommendation struct {
	budget            int
	unweightedCeiling int
	fromHistory       bool
}

func newCPUAllocator(totalCPU int, policy cpuBudgetPolicy) *cpuAllocator {
	if totalCPU < 1 {
		totalCPU = 1
	}
	return &cpuAllocator{totalCPU: totalCPU, policy: normalizeCPUBudgetPolicy(string(policy))}
}

// recommend returns a deterministic ceiling for one task-history record.
// Invalid or stale values degrade to neutral inputs, and the machine size is
// always the final cap.
func (a *cpuAllocator) recommend(
	weight, expectedCPUMs, expectedWallMs float64,
	historicalCeiling, criticalityCeiling int,
) cpuRecommendation {
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		weight = 1
	}
	if expectedCPUMs < 0 || math.IsNaN(expectedCPUMs) || math.IsInf(expectedCPUMs, 0) {
		expectedCPUMs = 0
	}
	if expectedWallMs < 0 || math.IsNaN(expectedWallMs) || math.IsInf(expectedWallMs, 0) {
		expectedWallMs = 0
	}
	if historicalCeiling < 0 {
		historicalCeiling = 0
	}
	if criticalityCeiling < 0 {
		criticalityCeiling = 0
	}

	if expectedCPUMs == 0 && expectedWallMs == 0 && historicalCeiling == 0 && criticalityCeiling == 0 {
		return cpuRecommendation{budget: a.totalCPU}
	}

	baseDemand := 1.0
	if expectedWallMs > 0 {
		baseDemand = max(baseDemand, expectedCPUMs/expectedWallMs)
	}
	// CPU budgets are integer cores. Round the history-derived default demand
	// first, then apply cpuWeight so weight 4 remains four times weight 1 for
	// otherwise-identical history (until the machine cap).
	//
	// unweightedCeiling is the OCCUPANCY answer, and it is the only one that
	// travels into the stats store: it depends on this task alone, so it means
	// the same thing in every plan.
	unweightedCeiling := max(int(math.Ceil(baseDemand)), historicalCeiling)
	unweightedCeiling = min(unweightedCeiling, a.totalCPU)

	// Under critical-path, the plan's makespan share RAISES that answer and
	// never lowers it. Occupancy is a measurement of what a task was allowed to
	// do, so using it alone as the next allowance is a feedback loop whose
	// stable point is a starved long pole: a task held at one core reports one
	// core of demand forever. Taking the maximum keeps every measured demand
	// honored while letting the chain that bounds the run reach for the
	// machine. Over-granting a CEILING is close to free — the task simply does
	// not use it, and admission still bounds real concurrent pressure — while
	// under-granting the long pole costs makespan one for one.
	effectiveCeiling := unweightedCeiling
	if a.policy == cpuBudgetPolicyCriticalPath {
		effectiveCeiling = max(effectiveCeiling, min(criticalityCeiling, a.totalCPU))
	}
	demand := float64(effectiveCeiling) * weight
	if demand >= float64(a.totalCPU) {
		return cpuRecommendation{
			budget:            a.totalCPU,
			unweightedCeiling: unweightedCeiling,
			fromHistory:       true,
		}
	}
	return cpuRecommendation{
		budget:            max(int(math.Ceil(demand)), 1),
		unweightedCeiling: unweightedCeiling,
		fromHistory:       true,
	}
}

// criticalityDecay shapes how fast the makespan lift falls off behind the long
// pole. It is deliberately SUPERLINEAR: a proportional lift would contradict the
// reason the lift exists. Work running in the long pole's shadow finishes before
// the run ends whatever it is granted, so cores handed to it buy no makespan and
// are taken from the one chain where they would. Squaring keeps the long pole at
// the machine, gives a task at half its length roughly a quarter, and leaves the
// tail where measured occupancy put it.
//
// The exponent is the one free parameter here and it is not yet settled by
// measurement — a clean cache-miss campaign on an idle machine should confirm
// it against 1 (proportional) and against a hard longest-only rule.
const criticalityDecay = 2

// planCriticalityCeilings maps each planned job to the share of the machine its
// own expected wall time claims within THIS plan, decayed by criticalityDecay.
//
// It is deliberately plan-scoped and never persisted. The same task is the long
// pole of `--all` and an ordinary job of a single-project run, so freezing one
// plan's shape into task history would make a grant depend on which projects
// the previous run happened to select — the very coupling this whole change
// removes. A plan with no wall history at all yields nothing, leaving the
// existing first-cold rule (the whole machine) untouched.
func planCriticalityCeilings(planned []*ScheduledJob, totalCPU, workers int) map[string]int {
	longest, total := int64(0), int64(0)
	for _, job := range planned {
		if job == nil || job.ExpectedWallMs <= 0 {
			continue
		}
		longest = max(longest, job.ExpectedWallMs)
		total += job.ExpectedWallMs
	}
	if longest <= 0 || totalCPU < 1 {
		return nil
	}

	// Is this plan bounded by ONE task, or by throughput? The lift only pays
	// when the longest task is still running after everything else is done —
	// then and only then does widening it shorten the run. Compare it against
	// what the rest of the plan can absorb in parallel; below that line no
	// task's ceiling can move the makespan, and the lift would just hand cores
	// to work that finishes anyway.
	//
	// This is what makes one default safe for repositories of different shape.
	// Measured on the two real stores at hand: this repo's long pole is 76.8s
	// against 17.4s of absorbable remainder (bounded by one task — lift), while
	// a large consumer workspace's is 30.4s against 59.4s (throughput-bound
	// over 408 tasks, its longest being 2.5% of total wall — no lift, occupancy governs).
	//
	// The comparison ignores dependencies, so it is optimistic about the
	// remainder draining: it errs toward lifting. That is the intended bias —
	// over-granting a ceiling is nearly free, under-granting the long pole
	// costs makespan one for one.
	if float64(longest) <= float64(total-longest)/float64(max(workers, 1)) {
		return nil
	}
	ceilings := make(map[string]int, len(planned))
	for _, job := range planned {
		if job == nil || job.ExpectedWallMs <= 0 {
			continue
		}
		share := math.Pow(float64(job.ExpectedWallMs)/float64(longest), criticalityDecay)
		ceilings[job.Key()] = min(max(int(math.Ceil(share*float64(totalCPU))), 1), totalCPU)
	}
	return ceilings
}

// recordGrant records a pre-combined batch ceiling and applies the same machine
// cap as singleton recommendations.
func (a *cpuAllocator) recordGrant(key string, weight, expectedCPUMs float64, budget int) int {
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		weight = 1
	}
	if expectedCPUMs < 0 || math.IsNaN(expectedCPUMs) || math.IsInf(expectedCPUMs, 0) {
		expectedCPUMs = 0
	}
	budget = min(max(budget, 1), a.totalCPU)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.grants = append(a.grants, cpuAllocation{
		job:           key,
		weight:        weight,
		expectedCPUMs: expectedCPUMs,
		budget:        budget,
	})
	return budget
}

// grantsSnapshot returns the budgets granted so far, sorted by job key for
// deterministic reporting.
func (a *cpuAllocator) grantsSnapshot() []cpuAllocation {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]cpuAllocation, len(a.grants))
	copy(out, a.grants)
	sort.Slice(out, func(i, j int) bool { return out[i].job < out[j].job })
	return out
}
