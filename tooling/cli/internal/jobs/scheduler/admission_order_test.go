//go:build unix

package scheduler

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestScheduler_ATaskWaitingForCPUHoldsNoLeaseAndNoLock pins the order of a
// task's waits: it takes its CPU before its cache lease and its task-output
// locks. A session whose task waits for CPU a running task holds must not
// block another session that runs the same task, such as one the running task
// started: that session claims the lease and the locks at once.
func TestScheduler_ATaskWaitingForCPUHoldsNoLeaseAndNoLock(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	for _, tc := range []struct {
		name      string
		cacheable bool
	}{{"with a cache lease", true}, {"without a cache", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOutputRaceFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var waitingCache, nestedCache *store.CacheManager
			if tc.cacheable {
				storeRoot := filepath.Join(f.root, "store")
				waitingCache = store.NewCacheManager(store.NewLocalStore(storeRoot))
				nestedCache = store.NewCacheManager(store.NewLocalStore(storeRoot))
			}

			// Every CPU of the waiting session is held, as by a running task.
			waiting := f.scheduler(f.job(f.noopScript(t), tc.cacheable), outputRaceSessionA, waitingCache)
			pools := make(chan *resourcePool, 1)
			var held resourceReservation
			waiting.onResourcePool = func(pool *resourcePool) {
				held, _ = pool.tryAcquire(resourceRequest{cpuMilli: pool.cpuCapacityMilli})
				pools <- pool
			}
			first := runAsync(ctx, waiting)
			var pool *resourcePool
			select {
			case pool = <-pools:
			case <-time.After(answerWaitBudget):
				t.Fatal("the waiting session never created its resource pool")
			}
			if !held.acquired {
				t.Fatal("failed to hold the waiting session's CPU")
			}
			waitForWaiters(t, pool, 1)

			nestedJob := f.job(f.noopScript(t), tc.cacheable)
			nested := f.scheduler(nestedJob, "20260925-192013-000001", nestedCache)
			waits := make(chan string, 2)
			nested.onCacheLeaseWait = func(*ScheduledJob) { waits <- "the cache lease" }
			nested.onOutputLockWait = func(message string) { waits <- message }
			second := runAsync(ctx, nested)
			select {
			case result := <-second:
				requireSuccess(t, "nested", result, nestedJob.Key())
			case what := <-waits:
				t.Fatalf("the nested session waited for %s, held by a task waiting for CPU", what)
			case <-time.After(answerWaitBudget):
				t.Fatal("the nested session never finished")
			}

			pool.release(held)
			requireSuccess(t, "waiting", <-first, nestedJob.Key())
		})
	}
}

// TestAFollowerHoldsNoCPUWhileItsLeaderRuns: a follower of a plan-level shared
// node took CPU before its lease claim, and gives it back before it waits for
// its leader.
func TestAFollowerHoldsNoCPUWhileItsLeaderRuns(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "the-scheduler-admits-the-cpu-it-grants", "admission-charges-the-measured-occupancy")
	f := newOutputRaceFixture(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(f.root, "store")))
	job := f.job(f.noopScript(t), true)
	job.SharedExecutionID = "shared"
	s := f.scheduler(job, outputRaceSessionA, cache)
	s.resources = newResourcePool(2, 0, nil)
	s.resourcePlanByJob = map[string]scheduledResourcePlan{job.Key(): {ceiling: 1, chargeMilli: 1 * milliCPU}}
	leader := &sharedExecutionSlot{done: make(chan struct{})}
	s.sharedSlots = map[string]*sharedExecutionSlot{job.SharedExecutionID: leader}
	held := make(chan int, 1)
	s.onSharedFollowerWait = func(*ScheduledJob) {
		s.resources.mu.Lock()
		held <- s.resources.cpuUsedMilli
		s.resources.mu.Unlock()
	}
	admission := newGroupAdmission(s, []*ScheduledJob{job})
	type opened struct {
		item *taskWork
	}
	done := make(chan opened, 1)
	go func() {
		var mu sync.Mutex
		item, _ := s.openTask(t.Context(), job, &mu, map[string]string{}, admission)
		done <- opened{item}
	}()
	select {
	case cpu := <-held:
		if cpu != 0 {
			t.Fatalf("the follower held %d CPUs while it waited for its leader, want none", cpu)
		}
	case <-time.After(answerWaitBudget):
		t.Fatal("the follower never waited for its leader")
	}
	leader.result = &JobResult{Status: "success"}
	close(leader.done)
	got := <-done
	if got.item == nil || got.item.sharedResult == nil {
		t.Fatalf("follower = %+v, want it to adopt its leader's result", got.item)
	}
	got.item.release()
	assertPoolEmpty(t, s.resources)
}
