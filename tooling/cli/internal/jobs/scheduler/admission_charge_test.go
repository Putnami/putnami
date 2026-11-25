package scheduler

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestAdmissionChargesTheMeasuredOccupancyNotItsRoundedCeiling(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	t.Parallel()
	alloc := newCPUAllocator(4, "")
	job := statJob("/measured", "test")
	recommendation := alloc.recommend(1, 1_100, 1_000, 0, 0)
	if recommendation.budget != 2 {
		t.Fatalf("ceiling = %d, want 1.1 CPUs rounded up to 2", recommendation.budget)
	}
	if charge := planChargeMilli(job, recommendation, recommendation.budget, 4); charge != 1_100 {
		t.Fatalf("charge = %d, want the measured 1100 thousandths", charge)
	}

	// Three such tasks fit on four CPUs; rounded up, only two would.
	pool := newResourcePool(4, 0, nil)
	for i := range 3 {
		if _, ok := pool.tryAcquire(resourceRequest{cpuMilli: 1_100}); !ok {
			t.Fatalf("task %d refused with %d thousandths used, want three 1.1-CPU tasks on four CPUs", i, pool.cpuUsedMilli)
		}
	}
	if _, ok := pool.tryAcquire(resourceRequest{cpuMilli: 1_100}); ok {
		t.Fatal("a fourth 1.1-CPU task fitted on four CPUs, want it refused")
	}

	// A near-idle task is charged the floor, never zero.
	idle := alloc.recommend(1, 10, 1_000, 0, 0)
	if charge := planChargeMilli(job, idle, idle.budget, 4); charge != minChargeMilli {
		t.Fatalf("idle charge = %d, want the floor %d", charge, minChargeMilli)
	}
	// A weight scales the occupancy charged, within the ceiling.
	weighted := alloc.recommend(2, 1_100, 1_000, 0, 0)
	if charge := planChargeMilli(job, weighted, weighted.budget, 4); charge != 2_200 || weighted.budget != 4 {
		t.Fatalf("weighted charge = %d under ceiling %d, want 2200 under 4", charge, weighted.budget)
	}
}

func TestAdmissionChargesALiftedCeilingWhole(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	t.Parallel()
	alloc := newCPUAllocator(4, "")
	job := statJob("/long-pole", "test")
	recommendation := alloc.recommend(1, 900, 1_000, 0, 4)
	if !recommendation.lifted || recommendation.budget != 4 {
		t.Fatalf("recommendation = %+v, want the critical-path lift to 4", recommendation)
	}
	if charge := planChargeMilli(job, recommendation, recommendation.budget, 4); charge != 4*milliCPU {
		t.Fatalf("charge = %d, want the lifted ceiling 4000 whole", charge)
	}
	// Under the measured policy the same history is not lifted.
	measured := newCPUAllocator(4, cpuBudgetPolicyMeasured).recommend(1, 900, 1_000, 0, 4)
	if measured.lifted || planChargeMilli(job, measured, measured.budget, 4) != 900 {
		t.Fatalf("measured recommendation = %+v, want no lift and a 900 charge", measured)
	}
}

func TestAdmissionChargesTheOccupancyMeasuredAtTheGrantedCeiling(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	t.Parallel()
	job := statJob("/batched", "lint")
	job.ResourceProfile = taskResourceProfile{Observations: []taskResourceObservation{
		{CPUMs: 100, WallMs: 100, GrantedConcurrency: 1, BatchSize: 1, Samples: 4, Confidence: 1},
		{CPUMs: 250, WallMs: 100, GrantedConcurrency: 3, BatchSize: 1, Samples: 4, Confidence: 1},
	}}
	recommendation := newCPUAllocator(4, cpuBudgetPolicyMeasured).recommend(1, 100, 100, 0, 0)
	if charge := planChargeMilli(job, recommendation, 1, 4); charge != milliCPU {
		t.Fatalf("charge at its own ceiling = %d, want 1000", charge)
	}
	if charge := planChargeMilli(job, recommendation, 3, 4); charge != 2_500 {
		t.Fatalf("charge at a class ceiling of 3 = %d, want the 2500 measured there", charge)
	}
	// No observation at the ceiling: the aggregate occupancy.
	if charge := planChargeMilli(job, recommendation, 2, 4); charge != milliCPU {
		t.Fatalf("charge at an unobserved ceiling of 2 = %d, want the aggregate 1000", charge)
	}
}

func TestColdTaskKeepsTheWholeMachine(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "a-cold-task-is-granted-the-machine-and-charged-one-cpu")
	t.Parallel()
	pool := newResourcePool(4, 0, nil)
	if first := pool.grantCeiling(4, true); first != 4 {
		t.Fatalf("first cold grant = %d, want the whole machine", first)
	}
	if second := pool.grantCeiling(4, true); second != 4 {
		t.Fatalf("second cold grant = %d, want the whole machine beside others too", second)
	}
	// A task with history beside them keeps its planned ceiling.
	if planned := pool.grantCeiling(2, false); planned != 2 {
		t.Fatalf("history-backed grant = %d, want its planned 2", planned)
	}
	if pool.grantedCPU != 10 {
		t.Fatalf("granted = %d, want 4 + 4 + 2", pool.grantedCPU)
	}
}

func TestATaskWithHistoryIsGrantedTheCPUsLeftFreeAboveItsCeiling(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "a-task-with-history-is-granted-at-least-the-cpus-left-free")
	t.Parallel()
	pool := newResourcePool(4, 0, nil)
	// The last task of a run, measured at one CPU while it shared the machine,
	// has the whole machine when it runs alone.
	alone := pool.grantCeiling(1, false)
	if alone != 4 {
		t.Fatalf("grant of a one-CPU task running alone = %d, want the 4 left free", alone)
	}
	pool.releaseCeiling(alone)
	held := pool.grantCeiling(3, false)
	if beside := pool.grantCeiling(1, false); beside != 1 {
		t.Fatalf("grant of a one-CPU task beside a 3-CPU ceiling = %d, want 1", beside)
	}
	pool.releaseCeiling(held)
	if pool.grantedCPU != 1 {
		t.Fatalf("granted = %d, want the 1 still running", pool.grantedCPU)
	}
}

func TestAcquireGroupCPUBudgetGrantsTheCPUsLeftFree(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "a-task-with-history-is-granted-at-least-the-cpus-left-free")
	t.Parallel()
	scheduler := &Scheduler{cpuAlloc: newCPUAllocator(4, ""), resources: newResourcePool(4, 0, nil)}
	known := statJob("/known", "test")
	known.ExpectedCPUWorkMs = 1_000
	known.ExpectedWallMs = 1_000
	if granted := scheduler.acquireGroupCPUBudget([]taskWork{{job: known}}, true); granted != 4 || known.CPUBudget != 4 {
		t.Fatalf("grant of a one-CPU task alone = %d (budget %d), want the 4 left free", granted, known.CPUBudget)
	}
	if known.CPUBudgetUnweightedCeiling != 1 {
		t.Fatalf("recorded ceiling = %d, want its own planned 1, not the free CPUs", known.CPUBudgetUnweightedCeiling)
	}
	cold := statJob("/cold", "test")
	if granted := scheduler.acquireGroupCPUBudget([]taskWork{{job: cold}}, true); granted != 4 || cold.CPUBudget != 4 {
		t.Fatalf("cold grant on a granted machine = %d (budget %d), want the whole machine", granted, cold.CPUBudget)
	}
	// A task with history on a granted machine keeps its planned ceiling.
	second := statJob("/second", "test")
	second.ExpectedCPUWorkMs = 2_000
	second.ExpectedWallMs = 1_000
	if granted := scheduler.acquireGroupCPUBudget([]taskWork{{job: second}}, true); granted != 2 || second.CPUBudget != 2 {
		t.Fatalf("history-backed grant on a granted machine = %d (budget %d), want its planned 2", granted, second.CPUBudget)
	}
	// An execution that reserves no CPU, such as a serve step, keeps its
	// planned ceiling and counts nothing.
	serve := statJob("/serve", "serve")
	if granted := scheduler.acquireGroupCPUBudget([]taskWork{{job: serve}}, false); granted != 0 || serve.CPUBudget != 4 {
		t.Fatalf("serve grant = %d (budget %d), want 0 counted and the planned 4", granted, serve.CPUBudget)
	}
	if scheduler.resources.grantedCPU != 10 {
		t.Fatalf("granted = %d, want 4 + 4 + 2", scheduler.resources.grantedCPU)
	}
}

func TestAWaitingExecutionStartsWhileAnyCPUIsFree(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	pool := newResourcePool(4, 0, nil)
	occupied, _ := pool.tryAcquire(resourceRequest{cpuMilli: 4 * milliCPU})
	admitted := make(chan resourceReservation, 2)
	go func() {
		if hold, ok := pool.acquireExecution(t.Context(), resourceRequest{cpuMilli: 3_200}, executionRank{criticalPathMs: 2, key: "/wide"}); ok {
			admitted <- hold
		}
	}()
	waitForWaiters(t, pool, 1)
	go func() {
		if hold, ok := pool.acquireExecution(t.Context(), resourceRequest{cpuMilli: 500}, executionRank{criticalPathMs: 1, key: "/narrow"}); ok {
			admitted <- hold
		}
	}()
	waitForWaiters(t, pool, 2)
	// A fifth of a CPU free: the wide execution starts though its 3.2 do not
	// fit, rather than hold every waiter while that CPU idles.
	if !pool.resize(&occupied, resourceRequest{cpuMilli: 3_800}) {
		t.Fatal("shrinking the occupied reservation did not cover it")
	}
	wide := <-admitted
	if wide.request.cpuMilli != 3_200 || pool.cpuUsedMilli != 7_000 {
		t.Fatalf("admitted %+v with %d used, want the whole 3.2 CPUs charged, 7 used", wide.request, pool.cpuUsedMilli)
	}
	// Over the capacity, the narrow one waits until CPU is free again.
	waitForWaiters(t, pool, 1)
	pool.release(occupied)
	narrow := <-admitted
	pool.release(wide)
	pool.release(narrow)
	assertPoolEmpty(t, pool)
}
