package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const testMiB = uint64(1024 * 1024)

func assertResourcePoolIdle(t *testing.T, scheduler *Scheduler) {
	t.Helper()
	if scheduler.resources == nil {
		t.Fatal("scheduler resource pool was not initialized")
	}
	if scheduler.resources.cpuUsedMilli != 0*milliCPU || scheduler.resources.memoryUsed != 0 || len(scheduler.resources.live) != 0 {
		t.Fatalf("scheduler resources not fully released: %+v", scheduler.resources)
	}
}

func heavyResourceJob(id string) *ScheduledJob {
	job := statJob(id, "test")
	heavy := true
	job.JobDef.Heavy = &heavy
	return job
}

func TestFirstColdTaskIsGrantedTheMachineAndChargedOneCPU(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "a-cold-task-is-granted-the-machine-and-charged-one-cpu")
	tests := []struct {
		name           string
		cpu            int
		memoryMiB      uint64
		wantAdmissions int
	}{
		{name: "one-core CI", cpu: 1, memoryMiB: 2048, wantAdmissions: 1},
		{name: "two-core CI", cpu: 2, memoryMiB: 4096, wantAdmissions: 2},
		{name: "ten-core workstation", cpu: 10, memoryMiB: 16384, wantAdmissions: 9},
		{name: "RAM-constrained large CPU", cpu: 16, memoryMiB: 2048, wantAdmissions: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			usableMemory := usableMemoryBytes(tc.memoryMiB * testMiB)
			job := heavyResourceJob("/heavy")
			recommendation := newCPUAllocator(tc.cpu, "").recommend(1, 0, 0, 0, 0)
			if recommendation.budget != tc.cpu || recommendation.fromHistory {
				t.Fatalf("first-cold grant = %+v, want the machine %d", recommendation, tc.cpu)
			}
			charge := planChargeMilli(job, recommendation, recommendation.budget, tc.cpu)
			if charge != milliCPU {
				t.Fatalf("first-cold charge = %d, want one CPU", charge)
			}
			request := resourceRequest{cpuMilli: charge, memoryBytes: bootstrapMemoryBytes(job, usableMemory)}
			pool := newResourcePool(tc.cpu, usableMemory, nil)
			admitted := 0
			for range 32 {
				if _, ok := pool.tryAcquire(request); !ok {
					break
				}
				admitted++
			}
			if admitted != tc.wantAdmissions {
				t.Fatalf("admitted heavy first-cold jobs = %d, want %d", admitted, tc.wantAdmissions)
			}
		})
	}
}

func TestFirstColdChargeFollowsTheConfiguredWeight(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "a-cold-task-is-granted-the-machine-and-charged-one-cpu")
	job := heavyResourceJob("/weighted")
	job.CPUWeight = 2
	if charge := bootstrapCPUCharge(job, 4); charge != 2 {
		t.Fatalf("weighted first-cold charge = %d, want 2", charge)
	}
	job.CPUWeight = 9
	if charge := bootstrapCPUCharge(job, 4); charge != 4 {
		t.Fatalf("first-cold charge = %d, want the machine cap 4", charge)
	}
}

func TestAdmissionChargesTheHistoryBackedOccupancy(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	t.Parallel()
	job := statJob("/measured", "test")
	job.ExpectedCPUWorkMs = 1500
	job.ExpectedWallMs = 1000
	job.HistoricalCPUCeiling = 3
	scheduler := &Scheduler{
		cpuAlloc:  newCPUAllocator(10, ""),
		resources: newResourcePool(10, 0, nil),
	}
	recommendation := scheduler.cpuAlloc.recommend(1, 1500, 1000, 3, 0)
	scheduler.resourcePlanByJob = map[string]scheduledResourcePlan{
		job.Key(): {recommendation: recommendation, ceiling: recommendation.budget, chargeMilli: planChargeMilli(job, recommendation, recommendation.budget, 10)},
	}
	if request := scheduler.groupResourceRequest([]*ScheduledJob{job}); request.cpuMilli != 1500 {
		t.Fatalf("admission charge = %d, want the measured occupancy 1500 under the ceiling 3", request.cpuMilli)
	}
}

func TestRegularColdProfileControlsMemoryAdmission(t *testing.T) {
	t.Parallel()
	profile := taskResourceProfile{Observations: []taskResourceObservation{{
		CPUMs:              3000,
		WallMs:             1000,
		MaxRSSBytes:        int64(600 * testMiB),
		GrantedConcurrency: 4,
		BatchSize:          1,
		Samples:            4,
		Confidence:         1,
		Saturation:         0.5,
	}}}
	recommendation := newCPUAllocator(8, "").recommend(1, 3000, 1000, 0, 0)
	if recommendation.budget != 3 {
		t.Fatalf("profile CPU ceiling = %d, want observed demand 3", recommendation.budget)
	}
	fallback := resourceRequest{cpuMilli: 2 * milliCPU, memoryBytes: 1280 * testMiB}
	memory, _ := profiledMemoryRequest(profile, recommendation.budget, 1, fallback, 4*1024*testMiB)
	if memory <= 600*testMiB || memory >= 700*testMiB {
		t.Fatalf("profile memory reservation = %d MiB, want confidence/saturation headroom over 600 MiB", memory/testMiB)
	}
	request := resourceRequest{cpuMilli: recommendation.budget * milliCPU, memoryBytes: memory}

	pool := newResourcePool(8, 2*1024*testMiB, nil)
	if _, ok := pool.tryAcquire(request); !ok {
		t.Fatal("first profiled task was not admitted")
	}
	if _, ok := pool.tryAcquire(request); !ok {
		t.Fatal("second profiled task should fit CPU and RAM")
	}
	if _, ok := pool.tryAcquire(request); ok {
		t.Fatal("third profiled task exceeded the granted CPU")
	}

	memoryBound := newResourcePool(8, 1000*testMiB, nil)
	if _, ok := memoryBound.tryAcquire(request); !ok {
		t.Fatal("first profiled task should fit the memory-bound pool")
	}
	if _, ok := memoryBound.tryAcquire(request); ok {
		t.Fatal("memory profile admitted an unsafe concurrent peer")
	}
}

func TestExecutionWaitsInLongestFirstOrder(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	pool := newResourcePool(4, 0, nil)
	occupied, ok := pool.acquireExecution(context.Background(), resourceRequest{cpuMilli: 4 * milliCPU}, executionRank{key: "/held"})
	if !ok {
		t.Fatal("failed to arrange occupied capacity")
	}
	long := executionRank{criticalPathMs: 10_000, key: "/long"}
	short := executionRank{criticalPathMs: 100, key: "/short"}
	order := make(chan string, 2)
	var wg sync.WaitGroup
	wait := func(rank executionRank, cpu int) {
		defer wg.Done()
		hold, ok := pool.acquireExecution(context.Background(), resourceRequest{cpuMilli: cpu}, rank)
		if !ok {
			t.Errorf("%s was not admitted", rank.key)
			return
		}
		order <- rank.key
		pool.release(hold)
	}
	wg.Add(1)
	go wait(long, 4*milliCPU)
	waitForWaiters(t, pool, 1)
	wg.Add(1)
	go wait(short, 500)
	waitForWaiters(t, pool, 2)
	// Nothing is free: neither starts, and once CPU frees the short execution
	// never overtakes the long pole.
	select {
	case key := <-order:
		t.Fatalf("%s started while the long pole waited", key)
	case <-time.After(50 * time.Millisecond):
	}
	pool.release(occupied)
	wg.Wait()
	if first := <-order; first != "/long" {
		t.Fatalf("first execution = %s, want the long pole", first)
	}
	assertPoolEmpty(t, pool)
}

func TestExecutionWaitEndsWithItsContext(t *testing.T) {
	t.Parallel()
	pool := newResourcePool(2, 0, nil)
	held, _ := pool.acquireExecution(context.Background(), resourceRequest{cpuMilli: 2 * milliCPU}, executionRank{key: "/a"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() {
		_, ok := pool.acquireExecution(ctx, resourceRequest{cpuMilli: 1 * milliCPU}, executionRank{key: "/b"})
		done <- ok
	}()
	waitForWaiters(t, pool, 1)
	cancel()
	if <-done {
		t.Fatal("a canceled wait was admitted")
	}
	pool.release(held)
	assertPoolEmpty(t, pool)
}

func TestACacheHitReservesNoCPU(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	job := statJob("/app", "test")
	scheduler := &Scheduler{
		resources:         newResourcePool(4, 0, nil),
		resourcePlanByJob: map[string]scheduledResourcePlan{job.Key(): {ceiling: 4, chargeMilli: 4 * milliCPU}},
	}
	for range 8 {
		group, _, ok := scheduler.takeAdmissiblePendingGroup([]*ScheduledJob{job}, false)
		if !ok || group.reservation.request.cpuMilli != 0*milliCPU || group.reservation.request.memoryBytes != 0 {
			t.Fatalf("dispatch reservation = %+v admitted=%v, want no CPU or memory before the cache lookup", group.reservation.request, ok)
		}
	}
}

func waitForWaiters(t *testing.T, pool *resourcePool, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pool.mu.Lock()
		n := len(pool.waiters)
		pool.mu.Unlock()
		if n == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", n, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertPoolEmpty(t *testing.T, pool *resourcePool) {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.cpuUsedMilli != 0 || pool.grantedCPU != 0 || len(pool.live) != 0 || len(pool.waiters) != 0 {
		t.Fatalf("pool not empty: used=%d granted=%d live=%d waiters=%d", pool.cpuUsedMilli, pool.grantedCPU, len(pool.live), len(pool.waiters))
	}
}

func TestFirstColdBatchSharesPhysicalClassEnvelopeAndCPU(t *testing.T) {
	a := statJob("/a", "test")
	b := statJob("/b", "test")
	scheduler := &Scheduler{resourcePlanByJob: map[string]scheduledResourcePlan{
		a.Key(): {ceiling: 3, chargeMilli: 3 * milliCPU, memoryFallback: 600 * testMiB},
		b.Key(): {ceiling: 2, chargeMilli: 2 * milliCPU, memoryFallback: 700 * testMiB},
	}}
	request := scheduler.groupResourceRequest([]*ScheduledJob{a, b})
	if request.cpuMilli != 3*milliCPU {
		t.Fatalf("batch CPU reservation = %d, want shared maximum 3", request.cpuMilli)
	}
	if request.memoryBytes != 700*testMiB {
		t.Fatalf("batch memory reservation = %d MiB, want one shared 700 MiB class envelope", request.memoryBytes/testMiB)
	}
}

func TestBatchAdmissionSumsLearnedSingletonMemory(t *testing.T) {
	a := statJob("/a", "test")
	b := statJob("/b", "test")
	a.ResourceProfile = taskResourceProfile{Observations: []taskResourceObservation{{
		MaxRSSBytes: 600 * int64(testMiB), GrantedConcurrency: 2, BatchSize: 1, Samples: 4, Confidence: 1,
	}}}
	b.ResourceProfile = taskResourceProfile{Observations: []taskResourceObservation{{
		MaxRSSBytes: 700 * int64(testMiB), GrantedConcurrency: 2, BatchSize: 1, Samples: 4, Confidence: 1,
	}}}
	scheduler := &Scheduler{
		resources: newResourcePool(8, 8*1024*testMiB, nil),
		resourcePlanByJob: map[string]scheduledResourcePlan{
			a.Key(): {ceiling: 2, chargeMilli: 2 * milliCPU, memoryFallback: 512 * testMiB},
			b.Key(): {ceiling: 2, chargeMilli: 2 * milliCPU, memoryFallback: 512 * testMiB},
		},
	}
	request := scheduler.groupResourceRequest([]*ScheduledJob{a, b})
	if request.memoryBytes != 1300*testMiB {
		t.Fatalf("learned singleton memory = %d MiB, want additive 1300 MiB", request.memoryBytes/testMiB)
	}
}

func TestBatchRSSContextAvoidsDoubleCountButKeepsSingletonEnvelope(t *testing.T) {
	profile := taskResourceProfile{Observations: []taskResourceObservation{{
		CPUMs:              2000,
		WallMs:             1000,
		MaxRSSBytes:        int64(1000 * testMiB),
		GrantedConcurrency: 4,
		BatchSize:          2,
		Samples:            4,
		Confidence:         1,
	}}}
	a := statJob("/a", "test")
	b := statJob("/b", "test")
	a.ResourceProfile = profile
	b.ResourceProfile = profile
	scheduler := &Scheduler{
		resources: newResourcePool(8, 8*1024*testMiB, nil),
		resourcePlanByJob: map[string]scheduledResourcePlan{
			a.Key(): {ceiling: 4, chargeMilli: 4 * milliCPU, memoryFallback: 1280 * testMiB},
			b.Key(): {ceiling: 4, chargeMilli: 4 * milliCPU, memoryFallback: 1280 * testMiB},
		},
	}

	repeatedBatch := scheduler.groupResourceRequest([]*ScheduledJob{a, b})
	if repeatedBatch.memoryBytes != 1000*testMiB {
		t.Fatalf("batch→batch reservation = %d MiB, want one 1000 MiB physical envelope", repeatedBatch.memoryBytes/testMiB)
	}
	singleton := scheduler.groupResourceRequest([]*ScheduledJob{a})
	if singleton.memoryBytes != 1000*testMiB {
		t.Fatalf("batch→singleton reservation = %d MiB, want the full 1000 MiB envelope", singleton.memoryBytes/testMiB)
	}
}

func TestResourceReservationReleasesOnEveryTerminalOutcome(t *testing.T) {
	for _, status := range []string{"success", "failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			scheduler := &Scheduler{resources: newResourcePool(2, 1024*testMiB, nil)}
			reservation, ok := scheduler.resources.tryAcquire(resourceRequest{cpuMilli: 2 * milliCPU, memoryBytes: 900 * testMiB})
			if !ok {
				t.Fatal("reservation was not acquired")
			}
			completed := jobGroupDone{
				jobs:        []jobDone{{result: &JobResult{Status: status}}},
				reservation: reservation,
			}
			scheduler.releaseGroupResources(completed)
			// A duplicate completion/release must not free someone else's claim.
			scheduler.releaseGroupResources(completed)
			assertResourcePoolIdle(t, scheduler)
			if _, ok := scheduler.resources.tryAcquire(resourceRequest{cpuMilli: 2 * milliCPU, memoryBytes: 1024 * testMiB}); !ok {
				t.Fatalf("capacity was not reusable after %s", status)
			}
		})
	}
}

func TestResourceReservationTelemetryIsBoundedSortedAndPublic(t *testing.T) {
	t.Parallel()
	pool := newResourcePool(8, 8*1024*testMiB, nil)
	for i := maxResourceReservationRecords + 5; i >= 0; i-- {
		key := fmt.Sprintf("/job-%04d:test", i)
		pool.grants[key] = resourceRequest{cpuMilli: 2 * milliCPU, memoryBytes: 513 * testMiB}
	}

	snapshot := pool.grantsSnapshot()
	if len(snapshot) != maxResourceReservationRecords {
		t.Fatalf("reservation records = %d, want bounded %d", len(snapshot), maxResourceReservationRecords)
	}
	if snapshot[0].Job != "/job-0000:test" || snapshot[len(snapshot)-1].Job != "/job-1023:test" {
		t.Fatalf("reservation ordering/bound = first %q last %q", snapshot[0].Job, snapshot[len(snapshot)-1].Job)
	}
	if snapshot[0].CPU != 2 || snapshot[0].MemoryMiB != 513 {
		t.Fatalf("public reservation = %+v, want CPU/RAM claim", snapshot[0])
	}
	wire, err := json.Marshal(snapshot[0])
	if err != nil {
		t.Fatalf("marshal reservation: %v", err)
	}
	if strings.Contains(string(wire), `"id"`) {
		t.Fatalf("reservation leaked internal identifier: %s", wire)
	}
}

func TestAServeStepReservesNoCPU(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	serve := statJob("/app", "serve~serve")
	finite := statJob("/app", "test")
	scheduler := &Scheduler{
		cfg:       SchedulerConfig{ServeSteps: map[string]bool{serve.Key(): true}},
		resources: newResourcePool(4, 0, nil),
		resourcePlanByJob: map[string]scheduledResourcePlan{
			serve.Key():  {ceiling: 2, chargeMilli: 2 * milliCPU},
			finite.Key(): {ceiling: 4, chargeMilli: 4 * milliCPU},
		},
	}
	held, ok := scheduler.resources.tryAcquire(scheduler.groupResourceRequest([]*ScheduledJob{serve}))
	if !ok || held.request.cpuMilli != 0*milliCPU {
		t.Fatalf("serve reservation = %+v admitted=%v, want no CPU", held.request, ok)
	}
	if request := scheduler.groupResourceRequest([]*ScheduledJob{finite}); request.cpuMilli != 4*milliCPU {
		t.Fatalf("finite reservation = %d, want its ceiling 4", request.cpuMilli)
	}
	if _, ok := scheduler.resources.tryAcquire(scheduler.groupResourceRequest([]*ScheduledJob{finite})); !ok {
		t.Fatal("a running serve held CPU a finite task waits for")
	}
}
