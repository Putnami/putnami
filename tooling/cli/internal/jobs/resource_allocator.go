package jobs

import (
	"math"
	"sort"
	"strings"
)

const (
	firstColdBootstrapCPU         = 1
	lightBootstrapMemoryMiB       = 512
	heavyBootstrapMemoryMiB       = 1280
	memoryHeadroomNumerator       = 3
	memoryHeadroomDenominator     = 4
	maxResourceReservationRecords = 1024
)

// resourceRequest is the scheduler reservation for one physical subprocess.
// It is deliberately distinct from cpuRecommendation.budget: the latter is a
// tool-native ceiling exported to the child, while this request is the amount
// of stable machine capacity the coordinator admits concurrently.
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
	ceiling        int
	// cpu is the task's admission demand — measured occupancy where a profile
	// exists, the first-cold bootstrap charge otherwise. It is deliberately not
	// the ceiling; see profiledCPURequest.
	cpu int
	// memoryFallback is the first-cold physical-process envelope. There is no
	// per-task memory estimate beside it on purpose: learned memory depends on
	// the batch size a group actually has, which only groupResourceRequest
	// knows, so it is derived there rather than precomputed at a batch size of
	// one and then discarded.
	memoryFallback uint64
}

// resourcePool is coordinator-owned. No worker acquires from it, so readiness
// priority and packing cannot race on goroutine timing. Every successful claim
// travels on jobGroup and comes back on jobGroupDone for exactly-once release.
type resourcePool struct {
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

func (p *resourcePool) record(group jobGroup) {
	if p == nil || !group.reservation.acquired {
		return
	}
	p.grants[resourceReservationKey(group.jobs)] = group.reservation.request
}

func (p *resourcePool) grantsSnapshot() []JobResourceReservation {
	if p == nil || len(p.grants) == 0 {
		return nil
	}
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

func (p *resourcePool) normalize(request resourceRequest) resourceRequest {
	request.cpu = min(max(request.cpu, 1), p.cpuCapacity)
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
	request = p.normalize(request)
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

func (p *resourcePool) release(reservation resourceReservation) {
	if p == nil || !reservation.acquired {
		return
	}
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

func bootstrapResourceRequest(job *ScheduledJob, totalCPU int, usableMemory uint64) resourceRequest {
	// Without a task profile, `heavy` is useful memory-risk evidence but not a
	// measurement of CPU occupancy. Charging every first-cold process one CPU
	// keeps the fallback neutral and lets the pool scale with a 2-core CI runner
	// as well as a larger workstation. An explicit cpuWeight below may raise the
	// charge; regular-cold runs replace it with measured CPU/wall occupancy.
	cpu := firstColdBootstrapCPU
	memoryMiB := lightBootstrapMemoryMiB
	if isHeavyJob(job) {
		memoryMiB = heavyBootstrapMemoryMiB
	}
	weight := job.EffectiveCPUWeight()
	if weight > 1 && !math.IsNaN(weight) && !math.IsInf(weight, 0) {
		if float64(cpu)*weight >= float64(max(totalCPU, 1)) {
			cpu = max(totalCPU, 1)
		} else {
			cpu = int(math.Ceil(float64(cpu) * weight))
		}
	}
	request := resourceRequest{
		cpu:         min(max(cpu, 1), max(totalCPU, 1)),
		memoryBytes: uint64(memoryMiB) * 1024 * 1024,
	}
	if usableMemory > 0 {
		request.memoryBytes = min(request.memoryBytes, usableMemory)
	}
	return request
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

// profiledCPURequest is admission demand, not the subprocess ceiling. It uses
// measured occupancy directly so a task may keep a deterministic ceiling of 8
// for burst headroom while reserving only 2 CPUs after consuming 1.5. Applying
// cpuWeight again here would double-count the weight that shaped the observed
// execution; weight still controls the ceiling, while admission follows the
// work the process actually demonstrated. Sparse evidence gets a small safety
// margin, and v1/v2 aggregate CPU/wall is the conservative fallback when no
// concurrency-conditioned observation exists.
func profiledCPURequest(profile taskResourceProfile, ceiling int, aggregateCPUMs, aggregateWallMs float64) int {
	cpuMs, wallMs, confidence := aggregateCPUMs, aggregateWallMs, 0.0
	if observation, ok := profile.NearestObservation(ceiling, 1); ok && observation.WallMs > 0 {
		cpuMs, wallMs = observation.CPUMs, observation.WallMs
		confidence = min(max(observation.Confidence, 0), 1)
	}
	if !validResourceFloat(cpuMs) || !validResourceFloat(wallMs) || wallMs <= 0 {
		return 1
	}
	margin := 1 + (1-confidence)*0.25
	demand := math.Ceil(cpuMs / wallMs * margin)
	if demand >= float64(max(ceiling, 1)) {
		return max(ceiling, 1)
	}
	request := int(demand)
	return min(max(request, 1), max(ceiling, 1))
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
		// CPU demand shares one tool-native pool, so take its maximum. Bootstrap
		// memory is likewise one physical-process class envelope. Learned singleton
		// observations remain additive, while a learned batch RSS is already the
		// WHOLE process: divide that envelope by its observed member count before
		// summing logical members, then retain at least one full envelope for fixed
		// process memory. A singleton consequently keeps the full envelope, while a
		// repeated N-way batch does not multiply the same physical RSS by N.
		request.cpu = max(request.cpu, plan.cpu)
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
	return request
}

func (s *Scheduler) releaseGroupResources(completed jobGroupDone) {
	s.resources.release(completed.reservation)
}

// takeAdmissiblePendingGroup preserves the existing longest-first candidate and
// waits when current reservations temporarily prevent that group. Backfilling
// with shorter work looked attractive as local packing, but it repeatedly
// consumed each newly released slot and starved the critical-path chain. The
// coordinator therefore keeps DAG priority ahead of instantaneous utilization;
// worker acquisition order still never chooses a winner.
func (s *Scheduler) takeAdmissiblePendingGroup(pending []*ScheduledJob) (jobGroup, []*ScheduledJob, bool) {
	if len(pending) == 0 {
		return jobGroup{}, pending, false
	}
	primary, rest := s.takePendingGroup(pending)
	if reservation, ok := s.resources.tryAcquire(s.groupResourceRequest(primary.jobs)); ok {
		primary.reservation = reservation
		return primary, rest, true
	}
	return jobGroup{}, pending, false
}
