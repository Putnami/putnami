package scheduler

import (
	"context"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
)

const (
	lightBootstrapMemoryMiB       = 512
	heavyBootstrapMemoryMiB       = 1280
	memoryHeadroomNumerator       = 3
	memoryHeadroomDenominator     = 4
	maxResourceReservationRecords = 1024
	// milliCPU is one CPU in the unit admission counts: a task with history is
	// charged the occupancy it measured, a fraction of a CPU (planChargeMilli).
	milliCPU = 1000
)

// resourceRequest is the scheduler reservation for one physical subprocess:
// the CPU admission charges for it (planChargeMilli), not the ceiling exported
// to the child, and the memory the coordinator admits concurrently.
type resourceRequest struct {
	// cpuMilli is the CPU admission charges, in thousandths of a CPU.
	cpuMilli    int
	memoryBytes uint64
	// claims are the dispatch's named external budget claims, summed
	// over the group. They ride on the SAME request as cpu/memory so admission
	// stays one atomic decision: a partially applied reservation — machine
	// capacity taken while a named budget refused — would leak capacity on every
	// blocked dispatch. Nil for the overwhelming majority of tasks, which claim
	// nothing. See resource_budgets.go.
	claims map[string]int
}

type resourceReservation struct {
	id       uint64
	request  resourceRequest
	acquired bool
}

type scheduledResourcePlan struct {
	recommendation cpuRecommendation
	// ceiling is the CPU the task's subprocess is granted.
	ceiling int
	// chargeMilli is the CPU admission reserves for it, in thousandths of a
	// CPU: the occupancy of a task with history, the first-cold charge
	// otherwise (planChargeMilli).
	chargeMilli int
	// memoryFallback is the first-cold physical-process envelope. There is no
	// per-task memory estimate beside it on purpose: learned memory depends on
	// the batch size a group actually has, which only groupResourceRequest
	// knows, so it is derived there rather than precomputed at a batch size of
	// one and then discarded.
	memoryFallback uint64
}

// resourcePool holds two kinds of reservation. The coordinator takes a
// dispatch's named claims (tryAcquire): they travel on jobGroup and come back
// on jobGroupDone for exactly-once release. A worker takes the CPU and memory
// of a subprocess when it starts and releases them when it ends
// (acquireExecution): a cache hit, which starts none, holds neither. Waiting
// executions are served in priority order, the first never overtaken, so
// goroutine timing cannot choose who runs first; a holder waits for nothing,
// so no wait can deadlock.
type resourcePool struct {
	mu      sync.Mutex
	waiters []*executionWaiter
	// cpuCapacityMilli and cpuUsedMilli count thousandths of a CPU.
	cpuCapacityMilli int
	memoryCapacity   uint64
	cpuUsedMilli     int
	// cpuCapacity is the whole CPUs of the machine, and grantedCPU the sum of
	// the ceilings of the running subprocesses that reserve CPU: a task with
	// history is granted at least what that sum leaves free (grantCeiling).
	cpuCapacity int
	grantedCPU  int
	memoryUsed  uint64
	nextID      uint64
	live        map[uint64]resourceRequest
	grants      map[string]resourceRequest
	// budgets is what the RUN says exists of each named external resource
	// (--resource <name>=<units>), and claimed is what live reservations hold of
	// it. A name absent from budgets is unlimited, which is why an unbudgeted
	// run behaves exactly as it did before named budgets existed.
	budgets map[string]int
	claimed map[string]int
}

func newResourcePool(cpu int, memoryBytes uint64, budgets map[string]int) *resourcePool {
	pool := &resourcePool{
		cpuCapacityMilli: max(cpu, 1) * milliCPU,
		cpuCapacity:      max(cpu, 1),
		memoryCapacity:   memoryBytes,
		live:             make(map[uint64]resourceRequest),
		grants:           make(map[string]resourceRequest),
	}
	for name, units := range budgets {
		if units < 0 {
			continue
		}
		if pool.budgets == nil {
			pool.budgets = make(map[string]int, len(budgets))
			pool.claimed = make(map[string]int, len(budgets))
		}
		pool.budgets[name] = units
	}
	return pool
}

func resourceReservationKey(jobs []*ScheduledJob) string {
	keys := make([]string, 0, len(jobs))
	for _, job := range jobs {
		keys = append(keys, job.Key())
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// executionRank orders waiting executions as the coordinator orders pending
// jobs (sortPendingLongestFirst): the longest critical path first, then the
// longest expected wall, then the key.
type executionRank struct {
	criticalPathMs, expectedWallMs int64
	key                            string
}

func (r executionRank) before(other executionRank) bool {
	if r.criticalPathMs != other.criticalPathMs {
		return r.criticalPathMs > other.criticalPathMs
	}
	if r.expectedWallMs != other.expectedWallMs {
		return r.expectedWallMs > other.expectedWallMs
	}
	return r.key < other.key
}

// rankOf is the rank of a physical execution: its highest-ranked member.
func rankOf(jobs []*ScheduledJob) executionRank {
	var rank executionRank
	for i, job := range jobs {
		member := executionRank{criticalPathMs: job.CriticalPathMs, expectedWallMs: job.ExpectedWallMs, key: job.Key()}
		if i == 0 || member.before(rank) {
			rank = member
		}
	}
	return rank
}

type executionWaiter struct {
	request resourceRequest
	rank    executionRank
	granted chan resourceReservation
}

func (p *resourcePool) grantsSnapshot() []JobResourceReservation {
	if p == nil || len(p.grants) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.grants))
	for key := range p.grants {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > maxResourceReservationRecords {
		keys = keys[:maxResourceReservationRecords]
	}
	out := make([]JobResourceReservation, 0, len(keys))
	for _, key := range keys {
		request := p.grants[key]
		out = append(out, JobResourceReservation{
			Job:       key,
			CPU:       (request.cpuMilli + milliCPU - 1) / milliCPU,
			MemoryMiB: int((request.memoryBytes + 1024*1024 - 1) / (1024 * 1024)),
		})
	}
	return out
}

// normalize clamps a request to what the pool can ever admit. A CPU of zero
// stays zero: a serve step reserves none (groupResourceRequest).
func (p *resourcePool) normalize(request resourceRequest) resourceRequest {
	request.cpuMilli = min(max(request.cpuMilli, 0), p.cpuCapacityMilli)
	if p.memoryCapacity > 0 {
		request.memoryBytes = min(request.memoryBytes, p.memoryCapacity)
	} else {
		request.memoryBytes = 0
	}
	request.claims = p.normalizeClaims(request.claims)
	return request
}

// normalizeClaims reduces a dispatch's claims to the ones this pool can
// actually gate on: a resource with no declared budget is unlimited and drops
// out entirely, and a surviving claim is clamped to the whole budget.
//
// The clamp is a belt behind the two places that already guarantee a dispatch
// fits — ValidateResourceBudgets refuses an impossible single claim at plan
// time, and batchClaimAdmitter never forms a group that exceeds a budget. It
// exists because it turns the one remaining way to hang the coordinator, a
// claim no release can ever satisfy, into "this dispatch runs while nothing
// else holds the resource".
func (p *resourcePool) normalizeClaims(claims map[string]int) map[string]int {
	if len(claims) == 0 || len(p.budgets) == 0 {
		return nil
	}
	var normalized map[string]int
	for name, units := range claims {
		budget, declared := p.budgets[name]
		if !declared || units <= 0 {
			continue
		}
		if normalized == nil {
			normalized = make(map[string]int, len(claims))
		}
		normalized[name] = min(units, budget)
	}
	return normalized
}

// claimsFitBudgets reports whether a set of claims could EVER be admitted,
// ignoring what is currently held. It answers the group-formation question —
// "may these members share one dispatch?" — which is about the budget's size,
// not about who holds it right now.
func (p *resourcePool) claimsFitBudgets(claims map[string]int) bool {
	if p == nil || len(claims) == 0 || len(p.budgets) == 0 {
		return true
	}
	for name, units := range claims {
		if budget, declared := p.budgets[name]; declared && units > budget {
			return false
		}
	}
	return true
}

func (p *resourcePool) tryAcquire(request resourceRequest) (resourceReservation, bool) {
	if p == nil {
		return resourceReservation{}, true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tryAcquireLocked(p.normalize(request), false)
}

// tryAcquireLocked takes request when it fits. With workConserving, a CPU
// charge also fits while any CPU is free, even when it is larger than what is
// free: a waiting execution then never leaves CPU idle behind it. A request
// that charges no CPU, such as a dispatch's claims or a serve step, fits
// whatever the CPU charges, even above the capacity; a waiting one still
// starts in rank order, behind a waiter that cannot start yet.
func (p *resourcePool) tryAcquireLocked(request resourceRequest, workConserving bool) (resourceReservation, bool) {
	freeCPU := p.cpuCapacityMilli - p.cpuUsedMilli
	if request.cpuMilli > 0 && request.cpuMilli > freeCPU && (!workConserving || freeCPU <= 0) {
		return resourceReservation{}, false
	}
	if p.memoryCapacity > 0 && request.memoryBytes > p.memoryCapacity-p.memoryUsed {
		return resourceReservation{}, false
	}
	// Every claim is checked before any is taken, so a dispatch blocked on its
	// second resource never holds units of its first.
	for name, units := range request.claims {
		if units > p.budgets[name]-p.claimed[name] {
			return resourceReservation{}, false
		}
	}
	p.cpuUsedMilli += request.cpuMilli
	p.memoryUsed += request.memoryBytes
	for name, units := range request.claims {
		p.claimed[name] += units
	}
	p.nextID++
	p.live[p.nextID] = request
	return resourceReservation{id: p.nextID, request: request, acquired: true}, true
}

// acquireExecution waits until the CPU and memory of one subprocess fit, in
// rank order, and takes them. It answers false, holding nothing, when ctx ends
// first.
func (p *resourcePool) acquireExecution(ctx context.Context, request resourceRequest, rank executionRank) (resourceReservation, bool) {
	if p == nil {
		return resourceReservation{}, true
	}
	request.claims = nil
	p.mu.Lock()
	waiter := &executionWaiter{request: p.normalize(request), rank: rank, granted: make(chan resourceReservation, 1)}
	at := sort.Search(len(p.waiters), func(i int) bool { return waiter.rank.before(p.waiters[i].rank) })
	p.waiters = slices.Insert(p.waiters, at, waiter)
	p.grantWaitersLocked()
	p.mu.Unlock()

	select {
	case reservation := <-waiter.granted:
		return reservation, true
	case <-ctx.Done():
	}
	p.mu.Lock()
	if index := slices.Index(p.waiters, waiter); index >= 0 {
		p.waiters = slices.Delete(p.waiters, index, index+1)
		p.grantWaitersLocked()
		p.mu.Unlock()
		return resourceReservation{}, false
	}
	p.mu.Unlock()
	// Granted while ctx ended: the caller holds it and releases it.
	return <-waiter.granted, true
}

// grantWaitersLocked grants waiting executions in rank order while the first
// fits; it never lets a later waiter overtake one that does not fit yet. The
// first fits while any CPU is free, even when its charge is larger than what
// is free: measured charges are fractions of a CPU, and a charge larger than
// what is free would otherwise hold every waiter behind it while that CPU
// idles, until enough of it frees at once. The charges then exceed the
// capacity by less than the first's, and no other waiter starts until they are
// below it again.
func (p *resourcePool) grantWaitersLocked() {
	for len(p.waiters) > 0 {
		waiter := p.waiters[0]
		reservation, ok := p.tryAcquireLocked(waiter.request, true)
		if !ok {
			return
		}
		p.waiters = p.waiters[1:]
		waiter.granted <- reservation
	}
}

// resize sets a held execution reservation to request without waiting. What
// the request lowers is freed, and granted to the waiters it fits. What it
// raises is taken only when it fits at once and no execution waits, since the
// holder may own a cache lease and must neither wait nor overtake a waiter;
// otherwise the reservation keeps what it holds of it. It answers whether the
// reservation now covers request. A released reservation is left alone.
func (p *resourcePool) resize(reservation *resourceReservation, request resourceRequest) bool {
	if p == nil || reservation == nil || !reservation.acquired {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	held, ok := p.live[reservation.id]
	if !ok {
		return false
	}
	request = p.normalize(request)
	next := held
	next.cpuMilli = min(request.cpuMilli, held.cpuMilli)
	next.memoryBytes = min(request.memoryBytes, held.memoryBytes)
	p.cpuUsedMilli = max(p.cpuUsedMilli-(held.cpuMilli-next.cpuMilli), 0)
	p.memoryUsed -= min(held.memoryBytes-next.memoryBytes, p.memoryUsed)
	growCPU := request.cpuMilli - next.cpuMilli
	growMemory := request.memoryBytes - next.memoryBytes
	fits := (growCPU <= 0 || growCPU <= p.cpuCapacityMilli-p.cpuUsedMilli) &&
		(p.memoryCapacity == 0 || growMemory <= p.memoryCapacity-p.memoryUsed)
	covered := growCPU == 0 && growMemory == 0
	if !covered && fits && len(p.waiters) == 0 {
		next.cpuMilli += growCPU
		next.memoryBytes += growMemory
		p.cpuUsedMilli += growCPU
		p.memoryUsed += growMemory
		covered = true
	}
	p.live[reservation.id] = next
	reservation.request = next
	p.grantWaitersLocked()
	return covered
}

// grantCeiling records that a subprocess which reserves CPU starts with
// ceiling, and answers the ceiling it is granted. A task with history (cold
// false) is granted its planned ceiling raised to the CPUs the running
// ceilings leave free, so a task that runs alone, such as the last one of a
// run, has the whole machine whatever its history measured while it shared
// it. A task with no history (cold) keeps its planned ceiling, the whole
// machine: it may be the run's long pole, and a ceiling never grows once its
// subprocess started. Admission still charges what planChargeMilli says.
// releaseCeiling returns what grantCeiling answered.
func (p *resourcePool) grantCeiling(ceiling int, cold bool) int {
	if p == nil {
		return ceiling
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !cold {
		ceiling = max(ceiling, p.cpuCapacity-p.grantedCPU)
	}
	p.grantedCPU += ceiling
	return ceiling
}

func (p *resourcePool) releaseCeiling(ceiling int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grantedCPU = max(p.grantedCPU-ceiling, 0)
}

// recordGrant lists what an execution reserved in the run's reservation
// telemetry, under key.
func (p *resourcePool) recordGrant(key string, request resourceRequest) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.grants[key] = request
}

func (p *resourcePool) release(reservation resourceReservation) {
	if p == nil || !reservation.acquired {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.grantWaitersLocked()
	request, ok := p.live[reservation.id]
	if !ok {
		return
	}
	delete(p.live, reservation.id)
	p.cpuUsedMilli = max(p.cpuUsedMilli-request.cpuMilli, 0)
	if request.memoryBytes >= p.memoryUsed {
		p.memoryUsed = 0
	} else {
		p.memoryUsed -= request.memoryBytes
	}
	// live is the exactly-once ledger for named claims too: a duplicate release
	// finds no entry above and returns before reaching this loop, so a repeated
	// completion can never hand back budget it did not hold.
	for name, units := range request.claims {
		p.claimed[name] = max(p.claimed[name]-units, 0)
	}
}

func usableMemoryBytes(capacity uint64) uint64 {
	if capacity == 0 {
		return 0
	}
	return capacity / memoryHeadroomDenominator * memoryHeadroomNumerator
}

// planChargeMilli is the CPU admission reserves for a task granted ceiling,
// in thousandths of a CPU. A task with history is charged the occupancy it
// measured, scaled by its weight, at least one CPU and at most its ceiling:
// rounding a task that fills 1.1 CPUs up to the 2 it is granted would hold
// back work for CPU it leaves idle, while a task measured below one CPU still
// holds a worker, and the one-CPU floor is what the measurements behind ADR
// 0100's amendment ran with. A ceiling the critical-path lift raised, or one
// with no measured wall time, is charged whole: the lift's cores come from the
// work admission holds back. A task with none is charged one CPU, scaled by
// its configured weight: charging a cold task more than it fills left CPUs
// idle and lengthened a no-cache gate, as ADR 0100's amendment of 2026-11-27
// measured.
func planChargeMilli(job *ScheduledJob, recommendation cpuRecommendation, ceiling, totalCPU int) int {
	if !recommendation.fromHistory {
		return bootstrapCPUCharge(job, totalCPU) * milliCPU
	}
	whole := max(ceiling, 1) * milliCPU
	if recommendation.lifted {
		return whole
	}
	demand := recommendation.demandMilli
	// Occupancy depends on the ceiling it was measured under: an observation
	// taken at this very ceiling, such as a batch class ceiling above the
	// task's own, is the closer answer.
	if observation, ok := job.ResourceProfile.NearestObservation(ceiling, 1); ok &&
		observation.GrantedConcurrency == ceiling && observation.WallMs > 0 && observation.CPUMs >= 0 {
		weight := job.EffectiveCPUWeight()
		if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			weight = 1
		}
		demand = int(math.Ceil(math.Round(observation.CPUMs/observation.WallMs*weight*1e6) / 1e3))
	}
	if demand <= 0 {
		return whole
	}
	return min(max(demand, milliCPU), whole)
}

// bootstrapCPUCharge is the first-cold CPU charge: one CPU, raised by a
// configured weight, capped by the machine.
func bootstrapCPUCharge(job *ScheduledJob, totalCPU int) int {
	totalCPU = max(totalCPU, 1)
	charge := 1.0
	if weight := job.EffectiveCPUWeight(); weight > 1 && !math.IsNaN(weight) && !math.IsInf(weight, 0) {
		charge = math.Ceil(weight)
	}
	return int(min(charge, float64(totalCPU)))
}

// bootstrapMemoryBytes is the first-cold physical-process memory envelope:
// 512 MiB for a light step, 1280 MiB for a declared-heavy one, clamped to
// usable memory.
func bootstrapMemoryBytes(job *ScheduledJob, usableMemory uint64) uint64 {
	memoryMiB := lightBootstrapMemoryMiB
	if isHeavyJob(job) {
		memoryMiB = heavyBootstrapMemoryMiB
	}
	memory := uint64(memoryMiB) * 1024 * 1024
	if usableMemory > 0 {
		memory = min(memory, usableMemory)
	}
	return memory
}

// profiledMemoryRequest estimates peak RSS at ceiling from the nearest
// concurrency-conditioned observation. Lower ceilings never discount the
// measured high-water mark; higher ceilings scale it proportionally. Sparse
// evidence keeps confidence headroom, and CPU saturation adds a small guard
// because a process filling its grant is the one most likely to fan out more at
// a larger ceiling.
func profiledMemoryRequest(
	profile taskResourceProfile,
	ceiling int,
	targetBatchSize int,
	fallback resourceRequest,
	usableMemory uint64,
) (memoryBytes uint64, observedBatchSize int) {
	observation, ok := profile.NearestObservation(ceiling, targetBatchSize)
	if !ok || observation.MaxRSSBytes <= 0 || observation.GrantedConcurrency <= 0 {
		// The bootstrap is a physical-process class, not a per-project charge.
		// Attribute one shared envelope across a cold batch; group aggregation
		// below keeps at least the full envelope. A singleton still gets all of it.
		return fallback.memoryBytes, max(targetBatchSize, 1)
	}
	scale := 1.0
	if ceiling > observation.GrantedConcurrency {
		scale = float64(ceiling) / float64(observation.GrantedConcurrency)
	}
	confidence := min(max(observation.Confidence, 0), 1)
	margin := 1 + (1-confidence)*0.5 + min(max(observation.Saturation, 0), 1)*0.1
	estimated := math.Ceil(float64(observation.MaxRSSBytes) * scale * margin)
	if usableMemory > 0 && estimated >= float64(usableMemory) {
		return usableMemory, max(observation.BatchSize, 1)
	}
	if estimated >= float64(^uint64(0)) {
		return ^uint64(0), max(observation.BatchSize, 1)
	}
	return uint64(estimated), max(observation.BatchSize, 1)
}

func (s *Scheduler) groupResourceRequest(jobs []*ScheduledJob) resourceRequest {
	// Named external claims are summed, not shared: see groupResourceClaims.
	request := resourceRequest{cpuMilli: milliCPU, claims: groupResourceClaims(jobs)}
	maxPhysicalMemory := uint64(0)
	batchSize := max(len(jobs), 1)
	usableMemory := uint64(0)
	if s.resources != nil {
		usableMemory = s.resources.memoryCapacity
	}
	for _, job := range jobs {
		plan, ok := s.resourcePlanByJob[job.Key()]
		if !ok {
			continue
		}
		// The members share one tool-native pool and one ceiling, so the CPU is
		// its maximum. Bootstrap
		// memory is likewise one physical-process class envelope. Learned singleton
		// observations remain additive, while a learned batch RSS is already the
		// WHOLE process: divide that envelope by its observed member count before
		// summing logical members, then retain at least one full envelope for fixed
		// process memory. A singleton consequently keeps the full envelope, while a
		// repeated N-way batch does not multiply the same physical RSS by N.
		request.cpuMilli = max(request.cpuMilli, plan.chargeMilli)
		fallback := resourceRequest{memoryBytes: plan.memoryFallback}
		memberMemory, observedBatchSize := profiledMemoryRequest(
			job.ResourceProfile,
			plan.ceiling,
			batchSize,
			fallback,
			usableMemory,
		)
		maxPhysicalMemory = max(maxPhysicalMemory, memberMemory)
		memberShare := memberMemory / uint64(max(observedBatchSize, 1))
		if memberMemory%uint64(max(observedBatchSize, 1)) != 0 {
			memberShare++
		}
		if ^uint64(0)-request.memoryBytes < memberShare {
			request.memoryBytes = ^uint64(0)
		} else {
			request.memoryBytes += memberShare
		}
	}
	request.memoryBytes = max(request.memoryBytes, maxPhysicalMemory)
	// A serve step runs until the run is interrupted, so a CPU it held would
	// never come back to the finite tasks that wait for it: it reserves none,
	// and keeps its ceiling.
	if s.allServeSteps(jobs) {
		request.cpuMilli = 0
	}
	return request
}

// allServeSteps reports whether every job is a serve step
// (SchedulerConfig.ServeSteps).
func (s *Scheduler) allServeSteps(jobs []*ScheduledJob) bool {
	if len(jobs) == 0 || len(s.cfg.ServeSteps) == 0 {
		return false
	}
	for _, job := range jobs {
		if !s.cfg.ServeSteps[job.Key()] {
			return false
		}
	}
	return true
}

// dispatchRequest is what the coordinator reserves for a dispatch: its named
// claims. Its CPU and memory are taken when a subprocess starts
// (acquireExecution), so a cache hit holds none.
func dispatchRequest(jobs []*ScheduledJob) resourceRequest {
	return resourceRequest{claims: groupResourceClaims(jobs)}
}

func (s *Scheduler) releaseGroupResources(completed jobGroupDone) {
	s.resources.release(completed.reservation)
	jobs := make([]*ScheduledJob, len(completed.jobs))
	for i, done := range completed.jobs {
		jobs[i] = done.job
	}
	s.verbWaves.track(jobs, -1)
}

// takeAdmissiblePendingGroup preserves the existing longest-first candidate and
// waits when current reservations temporarily prevent that group. Backfilling
// with shorter work looked attractive as local packing, but it repeatedly
// consumed each newly released slot and starved the critical-path chain. The
// coordinator therefore keeps DAG priority ahead of instantaneous utilization;
// worker acquisition order still never chooses a winner.
//
// A delegated task that waits for its verb's peers (scheduler_batch_verb.go)
// gives its turn to the first pending job that does not wait. When every
// pending job waits, nothing is dispatched while work that ends is in flight
// (inFlight: a serve, which runs until interrupted, does not count), since a
// completion is what makes a peer ready; with nothing such in flight, the
// queue's first job runs with the peers that are ready.
func (s *Scheduler) takeAdmissiblePendingGroup(pending []*ScheduledJob, inFlight bool) (jobGroup, []*ScheduledJob, bool) {
	if len(pending) == 0 {
		return jobGroup{}, pending, false
	}
	if ordered, ok := s.withoutWaiting(pending); ok {
		pending = ordered
	} else if inFlight {
		return jobGroup{}, pending, false
	}
	primary, rest := s.takePendingGroup(pending)
	if reservation, ok := s.resources.tryAcquire(dispatchRequest(primary.jobs)); ok {
		primary.reservation = reservation
		return primary, rest, true
	}
	return jobGroup{}, pending, false
}
