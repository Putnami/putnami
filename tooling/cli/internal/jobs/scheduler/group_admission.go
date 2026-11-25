package scheduler

import (
	"context"
	"slices"
)

// groupAdmission is the CPU and memory one physical execution reserves. A
// member takes it once its cache lookup missed and before it claims the cache
// lease, so a lease owner already holds the CPU it runs on and never waits for
// CPU another task holds. The reservation covers the members already found to
// execute and the members not yet looked up; it is resized in place, without
// waiting, as members hit the cache, and ends as the request of the members
// that execute. A fewer-member batch may ask for more memory than a larger
// one learned; the difference is taken only when it fits at once, and
// otherwise the batch runs on what it holds. A member that loses the claim gives the reservation
// back before it waits for the lease owner, unless a member of its batch owns
// a lease; a follower of a plan-level shared node gives it back before it
// waits for its leader. A nil admission reserves nothing.
type groupAdmission struct {
	scheduler *Scheduler
	group     []*ScheduledJob
	// from is the first member not yet looked up.
	from int
	// executing are the members looked up that execute.
	executing []*ScheduledJob
	hold      resourceReservation
	held      bool
	pinned    bool
}

func newGroupAdmission(s *Scheduler, group []*ScheduledJob) *groupAdmission {
	return &groupAdmission{scheduler: s, group: group}
}

// candidates are the members the reservation covers: those that execute and
// those not yet looked up.
func (a *groupAdmission) candidates() []*ScheduledJob {
	return append(slices.Clone(a.executing), a.group[min(a.from, len(a.group)):]...)
}

// acquire waits for the reservation of the candidates, before a lease claim.
// It answers false, holding nothing, when ctx ends first.
func (a *groupAdmission) acquire(ctx context.Context) bool {
	if a == nil || a.held {
		return true
	}
	return a.take(ctx, a.candidates())
}

func (a *groupAdmission) take(ctx context.Context, jobs []*ScheduledJob) bool {
	s := a.scheduler
	hold, ok := s.resources.acquireExecution(ctx, s.groupResourceRequest(jobs), rankOf(jobs))
	if !ok {
		return false
	}
	a.hold, a.held = hold, true
	return true
}

// settle records whether the member just looked up executes, and resizes the
// reservation to the candidates left.
func (a *groupAdmission) settle(job *ScheduledJob, executes bool) {
	if a == nil {
		return
	}
	if executes {
		a.executing = append(a.executing, job)
	}
	a.from++
	a.fit()
}

func (a *groupAdmission) fit() {
	if !a.held {
		return
	}
	jobs := a.candidates()
	if len(jobs) == 0 {
		a.release()
		return
	}
	a.scheduler.resources.resize(&a.hold, a.scheduler.groupResourceRequest(jobs))
}

// execute settles the reservation of the members that execute, once every
// member was looked up: it was shrunk to them, or, when no member claimed a
// lease, it is taken now, still before their locks. It answers false, holding
// nothing, when ctx ends first.
func (a *groupAdmission) execute(ctx context.Context) bool {
	if len(a.executing) == 0 {
		a.release()
		return true
	}
	if !a.held && !a.take(ctx, a.executing) {
		return false
	}
	a.fit()
	a.scheduler.resources.recordGrant(resourceReservationKey(a.executing), a.hold.request)
	return true
}

// pin keeps the reservation: a member of the group owns a cache lease.
func (a *groupAdmission) pin() {
	if a != nil {
		a.pinned = true
	}
}

// yield gives the reservation back before a lease wait, unless a member owns
// a lease.
func (a *groupAdmission) yield() {
	if a == nil || !a.held || a.pinned {
		return
	}
	a.release()
}

func (a *groupAdmission) release() {
	if a == nil || !a.held {
		return
	}
	a.scheduler.resources.release(a.hold)
	a.hold, a.held, a.pinned = resourceReservation{}, false, false
}

// reservesCPU reports whether the held reservation charges CPU.
func (a *groupAdmission) reservesCPU() bool {
	return a != nil && a.held && (a.hold.request.cpu > 0 || a.scheduler.resources == nil)
}
