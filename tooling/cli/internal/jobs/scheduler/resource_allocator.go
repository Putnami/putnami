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
)

// resourceRequest is the scheduler reservation for one physical subprocess:
// the CPU ceiling it is granted, which is exported to the child, and the memory
// the coordinator admits concurrently.
type resourceRequest struct {
	cpu         int
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
	// charge is the CPU admission reserves for it: the ceiling for a task with
	// history, the first-cold charge otherwise (planCharge).
	charge int
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
	mu             sync.Mutex
	waiters        []*executionWaiter
	cpuCapacity    int
	memoryCapacity uint64
	cpuUsed        int
	memoryUsed     uint64
	nextID         uint64
	live           map[uint64]resourceRequest
	grants         map[string]resourceRequest
	// budgets is what the RUN says exists of each named external resource
	// (--resource <name>=<units>), and claimed is what live reservations hold of
	// it. A name absent from budgets is unlimited, which is why an unbudgeted
	// run behaves exactly as it did before named budgets existed.
	budgets map[string]int
	claimed map[string]int
}

func newResourcePool(cpu int, memoryBytes uint64, budgets map[string]int) *resourcePool {
	pool := &resourcePool{
		cpuCapacity:    max(cpu, 1),
		memoryCapacity: memoryBytes,
		live:           make(map[uint64]resourceRequest),
		grants:         make(map[string]resourceRequest),
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
			CPU:       request.cpu,
			MemoryMiB: int((request.memoryBytes + 1024*1024 - 1) / (1024 * 1024)),
		})
	}
	return out
}

// normalize clamps a request to what the pool can ever admit. A CPU of zero
// stays zero: a serve step reserves none (groupResourceRequest).
func (p *resourcePool) normalize(request resourceRequest) resourceRequest {
	request.cpu = min(max(request.cpu, 0), p.cpuCapacity)
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
	return p.tryAcquireLocked(p.normalize(request))
}

func (p *resourcePool) tryAcquireLocked(request resourceRequest) (resourceReservation, bool) {
	if request.cpu > p.cpuCapacity-p.cpuUsed {
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
	p.cpuUsed += request.cpu
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
// fits; it never lets a later waiter overtake one that does not fit yet.
func (p *resourcePool) grantWaitersLocked() {
	for len(p.waiters) > 0 {
		waiter := p.waiters[0]
		reservation, ok := p.tryAcquireLocked(waiter.request)
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
	next.cpu = min(request.cpu, held.cpu)
	next.memoryBytes = min(request.memoryBytes, held.memoryBytes)
	p.cpuUsed = max(p.cpuUsed-(held.cpu-next.cpu), 0)
	p.memoryUsed -= min(held.memoryBytes-next.memoryBytes, p.memoryUsed)
	growCPU := request.cpu - next.cpu
	growMemory := request.memoryBytes - next.memoryBytes
	fits := growCPU <= p.cpuCapacity-p.cpuUsed &&
		(p.memoryCapacity == 0 || growMemory <= p.memoryCapacity-p.memoryUsed)
	covered := growCPU == 0 && growMemory == 0
	if !covered && fits && len(p.waiters) == 0 {
		next.cpu += growCPU
		next.memoryBytes += growMemory
		p.cpuUsed += growCPU
		p.memoryUsed += growMemory
		covered = true
	}
	p.live[reservation.id] = next
	reservation.request = next
	p.grantWaitersLocked()
	return covered
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
	p.cpuUsed = max(p.cpuUsed-request.cpu, 0)
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

// planCharge is the CPU admission reserves for a task granted ceiling. A task
// with history is charged its ceiling. A task with none is granted the whole
// machine and charged one CPU, scaled by its configured weight: until every
// tool a job starts honors its grant, charging a cold task more than it fills
// leaves CPUs idle on a fresh checkout.
func planCharge(job *ScheduledJob, recommendation cpuRecommendation, ceiling, totalCPU int) int {
	if recommendation.fromHistory {
		return ceiling
	}
	return bootstrapCPUCharge(job, totalCPU)
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
	request := resourceRequest{cpu: 1, claims: groupResourceClaims(jobs)}
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
		request.cpu = max(request.cpu, plan.charge)
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
		request.cpu = 0
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
