package scheduler

import (
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// batchAdmission is a scheduler with a 4-CPU pool and a two-member batch
// whose members are charged cpu0 and cpu1.
func batchAdmission(cpu0, cpu1 int) (*Scheduler, *groupAdmission, *ScheduledJob, *ScheduledJob) {
	m0 := statJob("/a", "test")
	m1 := statJob("/b", "test")
	s := &Scheduler{
		resources: newResourcePool(4, 0, nil),
		resourcePlanByJob: map[string]scheduledResourcePlan{
			m0.Key(): {ceiling: cpu0, chargeMilli: cpu0 * milliCPU},
			m1.Key(): {ceiling: cpu1, chargeMilli: cpu1 * milliCPU},
		},
	}
	return s, newGroupAdmission(s, []*ScheduledJob{m0, m1}), m0, m1
}

// TestABatchReservesAMemberThatRunsWithoutALease: a first member that
// executes without a lease (no cache) is in the reservation a later member
// takes before its lease claim.
func TestABatchReservesAMemberThatRunsWithoutALease(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	s, admission, m0, m1 := batchAdmission(4, 1)
	admission.settle(m0, true)
	if !admission.acquire(t.Context()) {
		t.Fatal("the second member was not admitted")
	}
	admission.pin()
	admission.settle(m1, true)
	if !admission.execute(t.Context()) || admission.hold.request.cpuMilli != 4*milliCPU {
		t.Fatalf("batch reservation = %d CPUs, want the 4 its first member is granted", admission.hold.request.cpuMilli)
	}
	if grants := s.resources.grantsSnapshot(); len(grants) != 1 || grants[0].CPU != 4 {
		t.Fatalf("reservation telemetry = %+v, want one 4-CPU batch", grants)
	}
	admission.release()
	assertPoolEmpty(t, s.resources)
}

// TestABatchGivesBackWhatAMemberThatHitReserved: a first member that claims
// its lease reserves for the members not yet looked up; one of them that hits
// the cache leaves the reservation at once, without a wait.
func TestABatchGivesBackWhatAMemberThatHitReserved(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	s, admission, m0, m1 := batchAdmission(1, 4)
	if !admission.acquire(t.Context()) || admission.hold.request.cpuMilli != 4*milliCPU {
		t.Fatalf("first reservation = %d CPUs, want 4 for the members not yet looked up", admission.hold.request.cpuMilli)
	}
	admission.pin()
	admission.settle(m0, true)
	admission.settle(m1, false)
	if s.resources.cpuUsedMilli != 1*milliCPU {
		t.Fatalf("pool holds %d CPUs once the second member hit, want 1", s.resources.cpuUsedMilli)
	}
	if !admission.execute(t.Context()) || admission.hold.request.cpuMilli != 1*milliCPU {
		t.Fatalf("batch reservation = %d CPUs, want the 1 its executing member is granted", admission.hold.request.cpuMilli)
	}
	if grants := s.resources.grantsSnapshot(); len(grants) != 1 || grants[0].Job != m0.Key() || grants[0].CPU != 1 {
		t.Fatalf("reservation telemetry = %+v, want the executing member alone", grants)
	}
	admission.release()
	assertPoolEmpty(t, s.resources)
}

// TestAGroupThatHitsReservesNothing: a member served by a coalesced wait took
// CPU before its claim; it ends holding none and is not listed.
func TestAGroupThatHitsReservesNothing(t *testing.T) {
	t.Parallel()
	s, admission, m0, m1 := batchAdmission(2, 2)
	if !admission.acquire(t.Context()) {
		t.Fatal("the first member was not admitted")
	}
	admission.yield()
	admission.settle(m0, false)
	admission.settle(m1, false)
	if !admission.execute(t.Context()) || admission.held {
		t.Fatal("a group whose members all hit holds a reservation")
	}
	if grants := s.resources.grantsSnapshot(); len(grants) != 0 {
		t.Fatalf("reservation telemetry = %+v, want none", grants)
	}
	assertPoolEmpty(t, s.resources)
}

// TestALeaseLoserGivesItsCPUBackWhileItWaits: a task that lost the cache lease
// to another session waits for its owner holding no CPU, and takes the CPU
// again before it claims the lease the owner abandoned.
func TestALeaseLoserGivesItsCPUBackWhileItWaits(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	root := t.TempDir()
	owner := store.NewCacheManager(store.NewLocalStore(root))
	waiter := store.NewCacheManager(store.NewLocalStore(root))
	const hash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	won, abandon := owner.TryClaimTaskEntry(hash, time.Second)
	if !won {
		t.Fatal("the owner did not claim the cold key")
	}
	t.Cleanup(abandon)

	job := statJob("/app", "build")
	job.ExpectedWallMs = 1000
	job.JobDef.TimeoutMs = 10_000
	scheduler := &Scheduler{
		cache:             waiter,
		resources:         newResourcePool(1, 0, nil),
		resourcePlanByJob: map[string]scheduledResourcePlan{job.Key(): {ceiling: 1, chargeMilli: 1 * milliCPU}},
	}
	waiting := make(chan struct{}, 1)
	scheduler.onCacheLeaseWait = func(*ScheduledJob) { waiting <- struct{}{} }
	admission := newGroupAdmission(scheduler, []*ScheduledJob{job})
	type outcome struct {
		result  *JobResult
		release func()
	}
	done := make(chan outcome, 1)
	go func() {
		result, release := scheduler.coalesceMiss(t.Context(), job, hash, schedulerLeaseTestCoalescer{cache: waiter}, admission)
		done <- outcome{result, release}
	}()
	select {
	case <-waiting:
	case <-time.After(answerWaitBudget):
		t.Fatal("the loser never waited for the lease owner")
	}
	free, ok := scheduler.resources.tryAcquire(resourceRequest{cpuMilli: 1 * milliCPU})
	if !ok {
		t.Fatal("the lease loser held its CPU while it waited")
	}
	scheduler.resources.release(free)

	abandon()
	got := <-done
	defer got.release()
	if got.result != nil {
		t.Fatalf("abandoned lease restored %+v, want the loser to own the lease", got.result)
	}
	if !admission.held || !admission.pinned || admission.hold.request.cpuMilli != 1*milliCPU {
		t.Fatalf("admission = held %v pinned %v cpu %d, want the lease owner holding one CPU", admission.held, admission.pinned, admission.hold.request.cpuMilli)
	}
	admission.release()
	assertPoolEmpty(t, scheduler.resources)
}

// TestResizeFreesWithoutWaitingAndGrantsWhatFits: lowering a reservation
// never waits, even behind a waiter the freed CPU does not fit, and grants
// exactly the waiters the freed CPU fits, in order.
func TestResizeFreesWithoutWaitingAndGrantsWhatFits(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	pool := newResourcePool(4, 0, nil)
	held, _ := pool.tryAcquire(resourceRequest{cpuMilli: 4 * milliCPU})
	granted := make(chan resourceReservation, 2)
	for _, rank := range []executionRank{{criticalPathMs: 2, key: "/first"}, {criticalPathMs: 1, key: "/second"}} {
		go func() {
			if hold, ok := pool.acquireExecution(t.Context(), resourceRequest{cpuMilli: 2 * milliCPU}, rank); ok {
				granted <- hold
			}
		}()
	}
	waitForWaiters(t, pool, 2)
	if !pool.resize(&held, resourceRequest{cpuMilli: 4 * milliCPU}) || held.request.cpuMilli != 4*milliCPU {
		t.Fatalf("resize to 4 = %+v, want 4 CPUs held", held.request)
	}
	waitForWaiters(t, pool, 2)
	if !pool.resize(&held, resourceRequest{cpuMilli: 2 * milliCPU}) {
		t.Fatal("resize to 2 did not cover the request")
	}
	first := <-granted
	waitForWaiters(t, pool, 1)
	select {
	case extra := <-granted:
		t.Fatalf("two waiters granted from 2 freed CPUs: %+v", extra.request)
	case <-time.After(20 * time.Millisecond):
	}
	pool.release(held)
	pool.release(first)
	pool.release(<-granted)
	assertPoolEmpty(t, pool)
}

// TestResizeRaisesOnlyWhatFitsAtOnce: a raise is taken when it fits and no
// execution waits; otherwise the reservation keeps what it holds. A released
// reservation is left alone.
func TestResizeRaisesOnlyWhatFitsAtOnce(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	pool := newResourcePool(4, 0, nil)
	held, _ := pool.tryAcquire(resourceRequest{cpuMilli: 3_200})
	other, _ := pool.tryAcquire(resourceRequest{cpuMilli: 800})
	if pool.resize(&held, resourceRequest{cpuMilli: 3_500}) || held.request.cpuMilli != 3_200 {
		t.Fatalf("a raise that does not fit = %+v, want the 3.2 CPUs held", held.request)
	}
	waiting := make(chan bool, 1)
	go func() {
		hold, ok := pool.acquireExecution(t.Context(), resourceRequest{cpuMilli: 3 * milliCPU}, executionRank{key: "/waiting"})
		pool.release(hold)
		waiting <- ok
	}()
	waitForWaiters(t, pool, 1)
	if pool.resize(&held, resourceRequest{cpuMilli: 3_500}) || held.request.cpuMilli != 3_200 {
		t.Fatalf("a raise past a waiter = %+v, want the 3.2 CPUs held", held.request)
	}
	pool.release(other)
	pool.release(held)
	<-waiting
	held, _ = pool.tryAcquire(resourceRequest{cpuMilli: 2 * milliCPU})
	if !pool.resize(&held, resourceRequest{cpuMilli: 3 * milliCPU}) || held.request.cpuMilli != 3*milliCPU || pool.cpuUsedMilli != 3*milliCPU {
		t.Fatalf("a raise that fits = %+v used %d, want 3", held.request, pool.cpuUsedMilli)
	}
	pool.release(held)
	if pool.resize(&held, resourceRequest{cpuMilli: 1 * milliCPU}) {
		t.Fatal("a released reservation was resized")
	}
	assertPoolEmpty(t, pool)
}

// TestABatchTakesTheMemoryItsExecutingMemberNeeds: one member of a learned
// two-member batch executes and needs more memory alone than the pair
// learned; the batch takes the difference, which fits at once.
func TestABatchTakesTheMemoryItsExecutingMemberNeeds(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	profile := taskResourceProfile{Observations: []taskResourceObservation{
		{CPUMs: 1000, WallMs: 1000, MaxRSSBytes: int64(2000 * testMiB), GrantedConcurrency: 1, BatchSize: 1, Samples: 4, Confidence: 1},
		{CPUMs: 1000, WallMs: 1000, MaxRSSBytes: int64(1000 * testMiB), GrantedConcurrency: 1, BatchSize: 2, Samples: 4, Confidence: 1},
	}}
	m0 := statJob("/a", "test")
	m1 := statJob("/b", "test")
	m0.ResourceProfile, m1.ResourceProfile = profile, profile
	s := &Scheduler{
		resources: newResourcePool(4, 8*1024*testMiB, nil),
		resourcePlanByJob: map[string]scheduledResourcePlan{
			m0.Key(): {ceiling: 1, chargeMilli: 1 * milliCPU, memoryFallback: 512 * testMiB},
			m1.Key(): {ceiling: 1, chargeMilli: 1 * milliCPU, memoryFallback: 512 * testMiB},
		},
	}
	alone := s.groupResourceRequest([]*ScheduledJob{m0}).memoryBytes
	if pair := s.groupResourceRequest([]*ScheduledJob{m0, m1}).memoryBytes; alone <= pair {
		t.Fatalf("fixture: alone %d MiB, pair %d MiB, want alone above the pair", alone/testMiB, pair/testMiB)
	}
	admission := newGroupAdmission(s, []*ScheduledJob{m0, m1})
	if !admission.acquire(t.Context()) {
		t.Fatal("the batch was not admitted")
	}
	admission.pin()
	admission.settle(m0, true)
	admission.settle(m1, false)
	if !admission.execute(t.Context()) || admission.hold.request.memoryBytes != alone {
		t.Fatalf("batch memory = %d MiB, want the %d MiB its executing member needs", admission.hold.request.memoryBytes/testMiB, alone/testMiB)
	}
	admission.release()
	assertPoolEmpty(t, s.resources)
}
