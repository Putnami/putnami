package jobs

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const testMiB = uint64(1024 * 1024)

func assertResourcePoolIdle(t *testing.T, scheduler *Scheduler) {
	t.Helper()
	if scheduler.resources == nil {
		t.Fatal("scheduler resource pool was not initialized")
	}
	if scheduler.resources.cpuUsed != 0 || scheduler.resources.memoryUsed != 0 || len(scheduler.resources.live) != 0 {
		t.Fatalf("scheduler resources not fully released: %+v", scheduler.resources)
	}
}

func heavyResourceJob(id string) *ScheduledJob {
	job := statJob(id, "test")
	heavy := true
	job.JobDef.Heavy = &heavy
	return job
}

func TestFirstColdBootstrapFlexesAcrossMachineShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		cpu             int
		memoryMiB       uint64
		wantAdmissions  int
		wantReservation int
	}{
		{name: "one-core CI", cpu: 1, memoryMiB: 2048, wantAdmissions: 1, wantReservation: 1},
		{name: "two-core CI", cpu: 2, memoryMiB: 4096, wantAdmissions: 2, wantReservation: 1},
		{name: "ten-core workstation", cpu: 10, memoryMiB: 16384, wantAdmissions: 9, wantReservation: 1},
		{name: "RAM-constrained large CPU", cpu: 16, memoryMiB: 2048, wantAdmissions: 1, wantReservation: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			usableMemory := usableMemoryBytes(tc.memoryMiB * testMiB)
			job := heavyResourceJob("/heavy")
			request := bootstrapResourceRequest(job, tc.cpu, usableMemory)
			if request.cpu != tc.wantReservation {
				t.Fatalf("CPU reservation = %d, want %d", request.cpu, tc.wantReservation)
			}
			// First-cold keeps a portable full-machine subprocess ceiling. The
			// separate reservation is what prevents one full ceiling per worker.
			if ceiling := newCPUAllocator(tc.cpu, "").recommend(1, 0, 0, 0, 0).budget; ceiling != tc.cpu {
				t.Fatalf("first-cold ceiling = %d, want machine capacity %d", ceiling, tc.cpu)
			}

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

func TestFirstColdBootstrapHonorsExplicitCPUWeight(t *testing.T) {
	t.Parallel()
	job := heavyResourceJob("/weighted")
	job.CPUWeight = 2
	request := bootstrapResourceRequest(job, 4, usableMemoryBytes(8*1024*testMiB))
	if request.cpu != 2 {
		t.Fatalf("weighted first-cold CPU reservation = %d, want 2", request.cpu)
	}
}

func TestRegularColdProfileControlsCPUAndMemoryAdmission(t *testing.T) {
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
	fallback := resourceRequest{cpu: 2, memoryBytes: 1280 * testMiB}
	memory, _ := profiledMemoryRequest(profile, recommendation.budget, 1, fallback, 4*1024*testMiB)
	if memory <= 600*testMiB || memory >= 700*testMiB {
		t.Fatalf("profile memory reservation = %d MiB, want confidence/saturation headroom over 600 MiB", memory/testMiB)
	}
	request := resourceRequest{
		cpu:         profiledCPURequest(profile, recommendation.budget, 3000, 1000),
		memoryBytes: memory,
	}

	pool := newResourcePool(8, 2*1024*testMiB, nil)
	if _, ok := pool.tryAcquire(request); !ok {
		t.Fatal("first profiled task was not admitted")
	}
	if _, ok := pool.tryAcquire(request); !ok {
		t.Fatal("second profiled task should fit CPU and RAM")
	}
	if _, ok := pool.tryAcquire(request); ok {
		t.Fatal("third profiled task exceeded the learned CPU reservation")
	}

	memoryBound := newResourcePool(8, 1000*testMiB, nil)
	if _, ok := memoryBound.tryAcquire(request); !ok {
		t.Fatal("first profiled task should fit the memory-bound pool")
	}
	if _, ok := memoryBound.tryAcquire(request); ok {
		t.Fatal("memory profile admitted an unsafe concurrent peer")
	}
}

func TestRegularColdCeilingDoesNotBecomeCPUReservation(t *testing.T) {
	t.Parallel()
	profile := taskResourceProfile{Observations: []taskResourceObservation{{
		CPUMs:              1500,
		WallMs:             1000,
		GrantedConcurrency: 8,
		BatchSize:          1,
		Samples:            4,
		Confidence:         1,
	}}}
	recommendation := newCPUAllocator(10, "").recommend(1, 1500, 1000, 8, 0)
	if recommendation.budget != 8 {
		t.Fatalf("deterministic ceiling = %d, want historical ceiling 8", recommendation.budget)
	}
	if reservation := profiledCPURequest(profile, recommendation.budget, 1500, 1000); reservation != 2 {
		t.Fatalf("observed CPU reservation = %d, want ceil(1.5) = 2", reservation)
	}
	pool := newResourcePool(10, 0, nil)
	for i := range 5 {
		if _, ok := pool.tryAcquire(resourceRequest{cpu: 2}); !ok {
			t.Fatalf("profiled peer %d was blocked even though five 2-CPU reservations fit", i)
		}
	}
}

func TestCoordinatorAdmissionWaitsForLongestFirstCapacity(t *testing.T) {
	long := statJob("/long", "test")
	long.ExpectedWallMs = 10_000
	short := statJob("/short", "lint")
	short.ExpectedWallMs = 100
	scheduler := &Scheduler{
		resources: newResourcePool(4, 0, nil),
		resourcePlanByJob: map[string]scheduledResourcePlan{
			long.Key():  {ceiling: 4, cpu: 4},
			short.Key(): {ceiling: 1, cpu: 1},
		},
	}
	occupied, ok := scheduler.resources.tryAcquire(resourceRequest{cpu: 2})
	if !ok {
		t.Fatal("failed to arrange occupied capacity")
	}

	pending := []*ScheduledJob{long, short}
	group, rest, admitted := scheduler.takeAdmissiblePendingGroup(pending)
	if admitted || len(group.jobs) != 0 {
		t.Fatalf("admitted group = %+v admitted=%t, want the blocked long pole to retain priority", group.jobs, admitted)
	}
	if len(rest) != 2 || rest[0] != long || rest[1] != short {
		t.Fatalf("remaining queue = %+v, want the original longest-first queue", rest)
	}
	if scheduler.resourcePlanByJob[long.Key()].ceiling != 4 || scheduler.resourcePlanByJob[short.Key()].ceiling != 1 {
		t.Fatal("admission timing mutated deterministic subprocess ceilings")
	}
	scheduler.resources.release(occupied)
}

func TestFirstColdBatchSharesPhysicalClassEnvelopeAndCPU(t *testing.T) {
	a := statJob("/a", "test")
	b := statJob("/b", "test")
	scheduler := &Scheduler{resourcePlanByJob: map[string]scheduledResourcePlan{
		a.Key(): {cpu: 3, memoryFallback: 600 * testMiB},
		b.Key(): {cpu: 2, memoryFallback: 700 * testMiB},
	}}
	request := scheduler.groupResourceRequest([]*ScheduledJob{a, b})
	if request.cpu != 3 {
		t.Fatalf("batch CPU reservation = %d, want shared maximum 3", request.cpu)
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
			a.Key(): {ceiling: 2, cpu: 2, memoryFallback: 512 * testMiB},
			b.Key(): {ceiling: 2, cpu: 2, memoryFallback: 512 * testMiB},
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
			a.Key(): {ceiling: 4, cpu: 2, memoryFallback: 1280 * testMiB},
			b.Key(): {ceiling: 4, cpu: 2, memoryFallback: 1280 * testMiB},
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
			reservation, ok := scheduler.resources.tryAcquire(resourceRequest{cpu: 2, memoryBytes: 900 * testMiB})
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
			if _, ok := scheduler.resources.tryAcquire(resourceRequest{cpu: 2, memoryBytes: 1024 * testMiB}); !ok {
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
		pool.grants[key] = resourceRequest{cpu: 2, memoryBytes: 513 * testMiB}
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
