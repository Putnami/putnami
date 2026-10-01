package jobs

import (
	"strings"
	"testing"
)

// claimingJob is a planned task that declares it consumes `units` of `resource`
// per execution — the manifest's `resources: {name: units}` after resolution.
func claimingJob(id, jobName, resource string, units int) *ScheduledJob {
	job := statJob(id, jobName)
	job.JobDef.Resources = map[string]int{resource: units}
	return job
}

// TestResourceBudgetBlocksThirdClaimAndResumesOnRelease is the issue's own
// case: a 400-unit budget carries one 230-unit claimant, refuses the second,
// and admits it the moment the first releases. Blocking must be temporary —
// a budget that never frees is a deadlock, not a limit.
func TestResourceBudgetBlocksThirdClaimAndResumesOnRelease(t *testing.T) {
	t.Parallel()
	pool := newResourcePool(32, 0, map[string]int{"db-connections": 400})
	claim := resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 230}}

	first, ok := pool.tryAcquire(claim)
	if !ok {
		t.Fatal("first claimant was not admitted under a 400-unit budget")
	}
	if _, ok := pool.tryAcquire(claim); ok {
		t.Fatal("second 230-unit claimant was admitted over a 400-unit budget")
	}

	pool.release(first)
	if pool.claimed["db-connections"] != 0 {
		t.Fatalf("claimed units after release = %d, want 0", pool.claimed["db-connections"])
	}
	if _, ok := pool.tryAcquire(claim); !ok {
		t.Fatal("budget did not resume after the holder released")
	}
}

// TestResourceBudgetNeverGatesUnclaimingTasks is the whole point of the change:
// the lint and build tasks that never touch the resource keep running at the
// substrate's width while its budget is fully held.
func TestResourceBudgetNeverGatesUnclaimingTasks(t *testing.T) {
	t.Parallel()
	pool := newResourcePool(32, 0, map[string]int{"db-connections": 400})
	if _, ok := pool.tryAcquire(resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 400}}); !ok {
		t.Fatal("the sole claimant was not admitted")
	}
	for i := range 8 {
		if _, ok := pool.tryAcquire(resourceRequest{cpu: 1}); !ok {
			t.Fatalf("task %d, which claims nothing, was gated by an exhausted budget", i)
		}
	}
	// A claim on a DIFFERENT resource is equally ungated: budgets never pool.
	if _, ok := pool.tryAcquire(resourceRequest{cpu: 1, claims: map[string]int{"ports": 4}}); !ok {
		t.Fatal("a claim on an unrelated resource was gated by db-connections")
	}
}

// TestUndeclaredResourceBudgetIsUnlimited pins backward compatibility: a run
// that declares no budget — every run before this change — admits a claiming
// task exactly as it did before.
func TestUndeclaredResourceBudgetIsUnlimited(t *testing.T) {
	t.Parallel()
	pool := newResourcePool(32, 0, nil)
	claim := resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 230}}
	for i := range 8 {
		reservation, ok := pool.tryAcquire(claim)
		if !ok {
			t.Fatalf("claimant %d was gated by a resource the run never budgeted", i)
		}
		if len(reservation.request.claims) != 0 {
			t.Fatalf("unbudgeted claim was recorded on the reservation: %v", reservation.request.claims)
		}
	}
	if len(pool.claimed) != 0 {
		t.Fatalf("unbudgeted resource entered the ledger: %v", pool.claimed)
	}
}

// TestResourceBudgetChecksEveryClaimBeforeTakingAny pins admission atomicity:
// a dispatch refused on its second resource must not be holding units of its
// first, or a blocked task would leak budget on every retry.
func TestResourceBudgetChecksEveryClaimBeforeTakingAny(t *testing.T) {
	t.Parallel()
	pool := newResourcePool(32, 0, map[string]int{"db-connections": 400, "ports": 2})
	if _, ok := pool.tryAcquire(resourceRequest{cpu: 1, claims: map[string]int{"ports": 2}}); !ok {
		t.Fatal("the port holder was not admitted")
	}
	blocked := resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 100, "ports": 1}}
	for range 3 {
		if _, ok := pool.tryAcquire(blocked); ok {
			t.Fatal("a dispatch was admitted with no port budget left")
		}
	}
	if pool.claimed["db-connections"] != 0 {
		t.Fatalf("blocked dispatch leaked %d db-connections", pool.claimed["db-connections"])
	}
}

// TestResourceClaimReleasesExactlyOnce mirrors the CPU/memory guarantee: a
// duplicate completion must not hand back budget it never held.
func TestResourceClaimReleasesExactlyOnce(t *testing.T) {
	t.Parallel()
	scheduler := &Scheduler{resources: newResourcePool(4, 0, map[string]int{"db-connections": 400})}
	reservation, ok := scheduler.resources.tryAcquire(resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 230}})
	if !ok {
		t.Fatal("reservation was not acquired")
	}
	completed := jobGroupDone{jobs: []jobDone{{result: &JobResult{Status: "failed"}}}, reservation: reservation}
	scheduler.releaseGroupResources(completed)
	scheduler.releaseGroupResources(completed)

	if got := scheduler.resources.claimed["db-connections"]; got != 0 {
		t.Fatalf("claimed units after a duplicated release = %d, want 0", got)
	}
	if _, ok := scheduler.resources.tryAcquire(resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 400}}); !ok {
		t.Fatal("the whole budget was not reusable after release")
	}
}

// TestImpossibleClaimRunsAloneInsteadOfHanging covers the belt behind plan-time
// validation and budget-aware group formation: a claim that exceeds the whole
// budget anyway is clamped to it, so the dispatch runs while nothing else holds
// the resource rather than waiting for a release that can never be large
// enough. An idle pool that refused it would hang the coordinator.
func TestImpossibleClaimRunsAloneInsteadOfHanging(t *testing.T) {
	t.Parallel()
	pool := newResourcePool(32, 0, map[string]int{"db-connections": 400})
	impossible := resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 460}}
	reservation, ok := pool.tryAcquire(impossible)
	if !ok {
		t.Fatal("an over-budget claim was refused on an idle pool — it can never be admitted at all")
	}
	if got := reservation.request.claims["db-connections"]; got != 400 {
		t.Fatalf("clamped claim = %d, want the whole 400-unit budget", got)
	}
	if _, ok := pool.tryAcquire(resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 1}}); ok {
		t.Fatal("a clamped dispatch did not hold the whole budget while it ran")
	}
}

// TestGroupResourceClaimsSumsMembers pins the aggregation choice: named budgets
// stand for hard external ceilings, so a batch's members add up instead of
// sharing one envelope the way CPU and memory do.
func TestGroupResourceClaimsSumsMembers(t *testing.T) {
	t.Parallel()
	a := claimingJob("/a", "test", "db-connections", 230)
	b := claimingJob("/b", "test", "db-connections", 30)
	c := statJob("/c", "lint")

	claims := groupResourceClaims([]*ScheduledJob{a, b, c})
	if claims["db-connections"] != 260 {
		t.Fatalf("group claim = %d, want summed 260", claims["db-connections"])
	}
	if claims := groupResourceClaims([]*ScheduledJob{c}); len(claims) != 0 {
		t.Fatalf("a group of non-claimants produced claims: %v", claims)
	}
}

// TestGroupResourceRequestCarriesClaims pins the wiring: the coordinator's one
// admission request has to carry the group's claims, or the pool never sees
// them.
func TestGroupResourceRequestCarriesClaims(t *testing.T) {
	job := claimingJob("/app", "test", "db-connections", 230)
	scheduler := &Scheduler{
		resources:         newResourcePool(8, 0, map[string]int{"db-connections": 400}),
		resourcePlanByJob: map[string]scheduledResourcePlan{job.Key(): {ceiling: 2, cpu: 2}},
	}
	if got := scheduler.groupResourceRequest([]*ScheduledJob{job}).claims["db-connections"]; got != 230 {
		t.Fatalf("group request claim = %d, want 230", got)
	}
}

// TestCoordinatorAdmissionWaitsForResourceBudget exercises the coordinator seam
// itself: a claiming long pole stays at the head of the queue while the budget
// is held, and is dispatched once it frees. It is the count-plus-budget rule in
// one place — the worker count allows the task throughout.
func TestCoordinatorAdmissionWaitsForResourceBudget(t *testing.T) {
	job := claimingJob("/app", "test", "db-connections", 230)
	scheduler := &Scheduler{
		resources:         newResourcePool(8, 0, map[string]int{"db-connections": 400}),
		resourcePlanByJob: map[string]scheduledResourcePlan{job.Key(): {ceiling: 1, cpu: 1}},
	}
	held, ok := scheduler.resources.tryAcquire(resourceRequest{cpu: 1, claims: map[string]int{"db-connections": 230}})
	if !ok {
		t.Fatal("failed to arrange a held budget")
	}

	pending := []*ScheduledJob{job}
	group, rest, admitted := scheduler.takeAdmissiblePendingGroup(pending)
	if admitted {
		t.Fatalf("claiming task was dispatched over an exhausted budget: %+v", group.jobs)
	}
	if len(rest) != 1 || rest[0] != job {
		t.Fatalf("blocked queue = %+v, want the claiming task retained in place", rest)
	}

	scheduler.resources.release(held)
	group, rest, admitted = scheduler.takeAdmissiblePendingGroup(pending)
	if !admitted || len(group.jobs) != 1 || group.jobs[0] != job {
		t.Fatalf("released budget did not dispatch the waiting task: group=%+v admitted=%t", group.jobs, admitted)
	}
	if len(rest) != 0 {
		t.Fatalf("remaining queue = %+v, want empty", rest)
	}
}

// TestBatchFormationStopsAtTheResourceBudget is the correctness case behind the
// SUM: a batched tool runs its members' work concurrently in one subprocess, so
// two 230-unit members inside a 400-unit budget would hold 460 of a hard
// external ceiling. The peer is left in the queue for its own turn instead.
func TestBatchFormationStopsAtTheResourceBudget(t *testing.T) {
	ws, planned := makeBatchSchedulerFixture(t, "")
	for _, job := range planned {
		job.JobDef.Resources = map[string]int{"db-connections": 230}
	}
	// One scheduler, three budget states: only the pool's budgets change between
	// them, and takePendingGroup reads nothing else about the run.
	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{}, &mockRenderer{}, nil)
	groupUnder := func(budget map[string]int) ([]*ScheduledJob, []*ScheduledJob) {
		scheduler.resources = newResourcePool(8, 0, budget)
		group, rest := scheduler.takePendingGroup(planned)
		return group.jobs, rest
	}

	if group, rest := groupUnder(map[string]int{"db-connections": 500}); len(group) != 2 || len(rest) != 0 {
		t.Fatalf("a 460-unit group inside a 500-unit budget formed as %d + %d, want 2 + 0", len(group), len(rest))
	}

	group, rest := groupUnder(map[string]int{"db-connections": 400})
	if len(group) != 1 || len(rest) != 1 {
		t.Fatalf("a 460-unit group inside a 400-unit budget formed as %d + %d, want 1 + 1", len(group), len(rest))
	}
	if got := groupResourceClaims(group)["db-connections"]; got != 230 {
		t.Fatalf("formed group claims %d units, want one member's 230", got)
	}

	// No budget at all leaves batching exactly as it was before named budgets.
	if group, rest := groupUnder(nil); len(group) != 2 || len(rest) != 0 {
		t.Fatalf("an unbudgeted run grouped as %d + %d, want the pre-existing 2 + 0", len(group), len(rest))
	}
}

// TestValidateResourceBudgetsRefusesImpossibleClaim is the plan-time half of
// the "never deadlock" rule: a single task claiming more than the whole budget
// is refused before any worker starts, with the resource and both amounts in
// the message.
func TestValidateResourceBudgetsRefusesImpossibleClaim(t *testing.T) {
	t.Parallel()
	job := claimingJob("/app", "test", "db-connections", 230)
	err := ValidateResourceBudgets([]*ScheduledJob{job}, map[string]int{"db-connections": 200})
	if err == nil {
		t.Fatal("a claim larger than the whole budget was accepted at plan time")
	}
	for _, want := range []string{"db-connections", "230", "200", job.Key()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q does not name %q", err.Error(), want)
		}
	}
}

// TestValidateResourceBudgetsAcceptsServableAndUndeclaredClaims pins the two
// ways a claim is legitimate: it fits the declared budget, or the run declared
// no budget for it at all (unlimited).
func TestValidateResourceBudgetsAcceptsServableAndUndeclaredClaims(t *testing.T) {
	t.Parallel()
	fits := claimingJob("/app", "test", "db-connections", 230)
	other := claimingJob("/tool", "test", "gpu", 8)
	plain := statJob("/lint", "lint")
	planned := []*ScheduledJob{fits, other, plain}

	if err := ValidateResourceBudgets(planned, map[string]int{"db-connections": 400}); err != nil {
		t.Fatalf("servable plan refused: %v", err)
	}
	if err := ValidateResourceBudgets(planned, nil); err != nil {
		t.Fatalf("plan refused under no declared budget at all: %v", err)
	}
}

// TestValidateResourceBudgetsReportsEveryOffender keeps the diagnostic useful
// on a plan with several impossible claims: fixing one at a time would need one
// run each.
func TestValidateResourceBudgetsReportsEveryOffender(t *testing.T) {
	t.Parallel()
	first := claimingJob("/app", "test", "db-connections", 230)
	second := claimingJob("/api", "test", "db-connections", 300)
	err := ValidateResourceBudgets([]*ScheduledJob{first, second}, map[string]int{"db-connections": 100})
	if err == nil {
		t.Fatal("impossible claims were accepted")
	}
	if lines := strings.Count(err.Error(), "claims "); lines != 2 {
		t.Fatalf("refusal named %d offenders, want 2:\n%s", lines, err.Error())
	}
}
