package machine

import (
	"sort"
	"strings"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
)

// Task is one task as v2 reports it: the typed identity plus the canonical
// result the one reducer already folded.
type Task struct {
	Identity protocolcli.TaskIdentity
	Result   jobs.TaskResult
	// silent marks a row that does NOT vote on the run's verdict — today only a
	// `runOn: finally` finalizer, which the contract keeps from changing the
	// invocation's outcome. The row is still listed, rendered and recorded; it
	// simply reaches no bucket, exactly as in the one reduction
	// (jobs.VotesOnTheVerdict, whose rule this is stamped from rather than
	// restated). The zero value votes, so a hand-built view counts normally.
	//
	// Only the report reads it, and it has to: that document's accounting
	// clauses tie its job list and its per-command partition to the run's own
	// counts.total, so a projection that counted a finalizer would emit a
	// document a consumer must reject.
	silent bool
}

// Run is the whole-run view every v2 surface renders. It is deliberately a thin
// pair: the canonical reduction decides every count and the verdict, and Tasks
// is the same list that reduction was folded from (jobs.RunTasks), so the
// documents below never re-derive a tally.
type Run struct {
	Session            *jobs.SessionResult
	Tasks              []Task
	publishConcurrency publishConcurrency
}

// publishConcurrency is supplied by the scheduler-facing renderer after it has
// observed the Docker registry phases. It deliberately stays internal: the
// public spelling belongs to protocols/cli, while a zero value means a caller
// did not execute through a renderer that measured this run.
type publishConcurrency struct {
	configuredCap int
	effective     int
}

// RunOf folds a completed run for a consumer that holds the plan and the raw
// results but no reduction yet — the renderers, whose Finish is handed both.
func RunOf(
	planned []*jobs.ScheduledJob,
	results map[string]*jobs.JobResult,
	outcome jobs.SessionOutcome,
) Run {
	return Run{
		Session: jobs.ReduceRun(planned, results, outcome),
		Tasks:   tasksOf(planned, results),
	}
}

// RunFrom pairs an already-reduced session with its tasks, for the consumers
// the engine hands both (the MCP adapter and the session writer). Reducing a
// second time would be the duplicate tally ADR 0001 §2 forbids.
func RunFrom(
	session *jobs.SessionResult,
	planned []*jobs.ScheduledJob,
	results map[string]*jobs.JobResult,
) Run {
	return Run{Session: session, Tasks: tasksOf(planned, results)}
}

// WithPublishConcurrency attaches scheduler-measured Docker registry
// concurrency to this run view. It is value-returning so a renderer can attach
// its observation without mutating a view shared with another surface.
func (r Run) WithPublishConcurrency(configuredCap, effective int) Run {
	r.publishConcurrency = publishConcurrency{
		configuredCap: configuredCap,
		effective:     effective,
	}
	return r
}

func tasksOf(planned []*jobs.ScheduledJob, results map[string]*jobs.JobResult) []Task {
	scheduled := jobs.RunTasks(planned, results)
	tasks := make([]Task, 0, len(scheduled))
	for i := range scheduled {
		identity := Identity(scheduled[i].Job)
		if scheduled[i].Job == nil {
			identity = identityOfKey(scheduled[i].Task.Key)
		}
		tasks = append(tasks, Task{
			Identity: identity,
			Result:   scheduled[i].Task,
			silent:   !jobs.VotesOnTheVerdict(scheduled[i].Job),
		})
	}
	return tasks
}

// Summary is the canonical v2 run verdict, identical on all four surfaces.
//
// It encodes the two decisions B0a settled and B1a implements for v1:
//
//   - UNIFIED STRICT SUCCESS. counts is the verdict histogram over every
//     selected task, reuse included (SessionResult.Status, not Fresh), and
//     reuse is counted apart. A reused failure therefore fails the run, and the
//     lenient tally the v1 JSONL summary reads has no spelling here.
//   - ABORT WINS. aborted > failure > success, with the failures still counted
//     and listed, so the precedence hides nothing.
func (r Run) Summary(durationMs int64) protocolcli.RunSummary {
	session := r.Session
	if session == nil {
		session = &jobs.SessionResult{}
	}
	counts := protocolcli.RunCounts{
		Succeeded: session.Status.Succeeded,
		Failed:    session.Status.Failed,
		Canceled:  session.Status.Canceled,
		Skipped:   session.Status.Skipped,
	}
	// Total is the sum of the four buckets rather than SessionResult.Tasks: the
	// contract requires them to agree, and a task carrying a status outside the
	// canonical vocabulary is in Tasks but in no bucket.
	counts.Total = counts.Sum()

	summary := protocolcli.RunSummary{
		Counts: counts,
		Reuse: protocolcli.RunReuse{
			LocalCache:  session.Reuse.LocalCache,
			RemoteCache: session.Reuse.RemoteCache,
			Coalesced:   session.Reuse.Coalesced,
		},
		DurationMs: nonNegative64(durationMs),
	}
	switch {
	case session.Aborted:
		summary.Outcome = protocolcli.RunOutcomeAborted
		summary.AbortedBy = abortSource(session.AbortedBy)
		summary.ExitCode = protocolcli.ExitSignal
	case counts.Failed > 0:
		summary.Outcome = protocolcli.RunOutcomeFailure
		summary.ExitCode = protocolcli.ExitFailure
	default:
		summary.Outcome = protocolcli.RunOutcomeSuccess
		summary.ExitCode = protocolcli.ExitSuccess
	}
	summary.Failures = r.failures(counts.Failed)
	summary.Publications = r.publications()
	summary.CPU = r.cpuBalance(summary.DurationMs)
	summary.Cache = r.cacheSummary()
	return summary
}

// StreamSummary is the bounded session:end verdict. It is deliberately a
// projection of Summary rather than a second reduction: normal and verbose
// machine output may retain different amounts of detail, but they cannot
// change counts, reuse, the run outcome, or the process exit code.
func (r Run) StreamSummary(durationMs int64) protocolcli.StreamRunSummary {
	summary := r.Summary(durationMs)
	return protocolcli.StreamRunSummary{
		Outcome:    summary.Outcome,
		AbortedBy:  summary.AbortedBy,
		ExitCode:   summary.ExitCode,
		Counts:     summary.Counts,
		Reuse:      summary.Reuse,
		DurationMs: summary.DurationMs,
		CPU:        summary.CPU,
	}
}

func (r Run) cacheSummary() *protocolcli.RunCache {
	if r.Session == nil || r.Session.Cache == nil {
		return nil
	}
	cache := r.Session.Cache
	if cache.LocalHits == 0 && cache.LocalMisses == 0 && cache.LocalServedMs == 0 {
		return nil
	}
	return &protocolcli.RunCache{Local: &protocolcli.RunLocalCache{
		Hits:             nonNegative64(cache.LocalHits),
		Misses:           nonNegative64(cache.LocalMisses),
		ServedMs:         nonNegative64(cache.LocalServedMs),
		KeysMs:           nonNegative64(cache.LocalKeysMs),
		BindingsMs:       nonNegative64(cache.LocalBindingsMs),
		RestoreVerifyMs:  nonNegative64(cache.LocalRestoreVerifyMs),
		SpawnedProcesses: nonNegative64(cache.LocalSpawnedProcesses),
	}}
}

// cpuBalance is the run's actual-vs-allocated CPU.
//
// ACTUAL is summed from the PHYSICAL ledger, so each subprocess contributes
// exactly once however many logical task records it produced. That is the whole
// point of stating it here rather than leaving it to a consumer: summing the
// task list instead multiplies a batch's cost by its fan-out, and the epic's
// own baseline (796 logical records against 57.1 de-duplicated physical
// task-minutes) shows how far that diverges.
//
// ALLOCATED is what the runner granted over this run's wall — the cgroup quota
// where one is in force, the visible core count otherwise. It needs the
// captured environment, so the block is omitted when there is none: an
// allocation figure with a guessed denominator is worse than no figure, because
// every utilization ratio downstream would inherit the guess silently.
//
// The two are stated against the SAME durationMs this summary reports, which
// the protocol validator enforces, so a reader can divide them without checking
// which wall each was measured over.
func (r Run) cpuBalance(durationMs int64) *protocolcli.RunCPU {
	if r.Session == nil || len(r.Session.Executions) == 0 || r.Environment() == nil {
		return nil
	}
	millicores, fromQuota := r.Session.Environment.AllocatedMillicores()
	if millicores < 1 {
		return nil
	}
	source := protocolcli.CPUAllocationLogicalCPUs
	if fromQuota {
		source = protocolcli.CPUAllocationCgroupQuota
	}
	var actual time.Duration
	for i := range r.Session.Executions {
		actual += r.Session.Executions[i].CPUTime()
	}
	return &protocolcli.RunCPU{
		AllocatedMillicores: millicores,
		AllocatedSource:     source,
		AllocatedMs:         nonNegative64(durationMs) * int64(millicores) / 1000,
		ActualMs:            millis(actual),
		Executions:          len(r.Session.Executions),
	}
}

// Environment is the runner this session ran on, projected onto the contract.
//
// Every member is carried across only when it was MEASURED. The three nested
// blocks exist precisely so that a zero inside them can mean "measured zero" —
// "the quota never throttled us" is the finding that separates contention from
// quota starvation — while their absence means "this platform does not expose
// it". Collapsing either onto an omitempty scalar would erase the distinction
// the whole capture exists for.
func (r Run) Environment() *protocolcli.SessionEnvironment {
	if r.Session == nil || r.Session.Environment == nil {
		return nil
	}
	source := r.Session.Environment
	if source.OS == "" || source.Arch == "" || source.LogicalCPUs < 1 {
		return nil
	}
	environment := &protocolcli.SessionEnvironment{
		OS:          source.OS,
		Arch:        source.Arch,
		LogicalCPUs: source.LogicalCPUs,
		CPUModel:    source.CPUModel,
		WindowMs:    millis(source.Window),
	}
	if cgroup := source.Cgroup; cgroup != nil && cgroup.PeriodUs > 0 {
		environment.CgroupCPU = &protocolcli.CgroupCPU{
			PeriodUs: cgroup.PeriodUs,
			// A quota of 0 is an unlimited v2 "max" or v1 -1, and
			// omitempty is the spelling of that. It is not a ceiling of
			// nothing.
			QuotaUs: nonNegative64(cgroup.QuotaUs),
		}
		if cgroup.HasUsage {
			environment.CgroupCPU.UsageUs = nonNegative64(cgroup.UsageUs)
		}
		if throttle := cgroup.Throttle; throttle != nil {
			environment.CgroupCPU.Throttle = &protocolcli.CgroupThrottle{
				Periods:          nonNegative64(throttle.Periods),
				ThrottledPeriods: nonNegative64(throttle.ThrottledPeriods),
				ThrottledUs:      nonNegative64(throttle.ThrottledUs),
			}
		}
	}
	if pressure := source.CPUPressure; pressure != nil {
		environment.CPUPressure = &protocolcli.CPUPressure{SomeStalledUs: nonNegative64(pressure.SomeStalledUs)}
	}
	if host := source.HostCPU; host != nil {
		environment.HostCPU = &protocolcli.HostCPUTime{
			StealTicks:  nonNegative64(host.StealTicks),
			IOWaitTicks: nonNegative64(host.IOWaitTicks),
			TotalTicks:  nonNegative64(host.TotalTicks),
		}
	}
	if capacity := source.MemoryCapacity; validMemoryCapacity(capacity) {
		environment.MemoryCapacity = &protocolcli.MemoryCapacity{
			EffectiveBytes:  capacity.EffectiveBytes,
			EffectiveSource: capacity.EffectiveSource,
		}
		if validPositiveSafeInteger(capacity.PhysicalBytes) {
			environment.MemoryCapacity.PhysicalBytes = capacity.PhysicalBytes
		}
		if limit := capacity.CgroupLimit; limit != nil && validPositiveSafeInteger(limit.Bytes) &&
			(limit.Source == protocolcli.CgroupMemoryV1 || limit.Source == protocolcli.CgroupMemoryV2) {
			environment.MemoryCapacity.CgroupLimit = &protocolcli.CgroupMemoryLimit{
				Bytes: limit.Bytes, Source: limit.Source,
			}
		}
	}
	if memory := source.CgroupMemory; memory != nil &&
		(memory.Source == protocolcli.CgroupMemoryV1 || memory.Source == protocolcli.CgroupMemoryV2) &&
		validNonNegativeSafeInteger(memory.Closing.CurrentBytes) {
		projected := &protocolcli.CgroupMemory{
			Source:  memory.Source,
			Closing: protocolcli.CgroupMemoryClosing{CurrentBytes: memory.Closing.CurrentBytes},
		}
		if composition := memory.Closing.Composition; memory.Source == protocolcli.CgroupMemoryV2 &&
			validMemoryComposition(composition) {
			projected.Closing.Composition = &protocolcli.CgroupMemoryComposition{
				AnonBytes: composition.AnonBytes, FileBytes: composition.FileBytes, ShmemBytes: composition.ShmemBytes,
			}
		}
		if peak := memory.Closing.LifetimePeak; peak != nil && validNonNegativeSafeInteger(peak.Bytes) {
			projected.Closing.LifetimePeak = &protocolcli.CgroupMemoryLifetimePeak{Bytes: peak.Bytes}
		}
		if events := memory.Events; memory.Source == protocolcli.CgroupMemoryV2 && validMemoryEvents(events) {
			projected.Events = &protocolcli.CgroupMemoryEvents{
				Low: events.Low, High: events.High, Max: events.Max, OOM: events.OOM, OOMKill: events.OOMKill,
			}
		}
		environment.CgroupMemory = projected
	}
	if pressure := source.MemoryPressure; validMemoryPressure(pressure, environment.CgroupMemory) {
		environment.MemoryPressure = &protocolcli.MemoryPressure{
			Scope: pressure.Scope, SomeStalledUs: pressure.SomeStalledUs, FullStalledUs: pressure.FullStalledUs,
		}
	}
	return environment
}

const maxSafeJSONInt64 = int64((1 << 53) - 1)

func validNonNegativeSafeInteger(value int64) bool {
	return value >= 0 && value <= maxSafeJSONInt64
}

func validPositiveSafeInteger(value int64) bool {
	return value > 0 && value <= maxSafeJSONInt64
}

func validMemoryCapacity(capacity *jobs.MemoryCapacity) bool {
	if capacity == nil || !validPositiveSafeInteger(capacity.EffectiveBytes) {
		return false
	}
	physicalValid := validPositiveSafeInteger(capacity.PhysicalBytes)
	limitValid := capacity.CgroupLimit != nil && validPositiveSafeInteger(capacity.CgroupLimit.Bytes) &&
		(capacity.CgroupLimit.Source == protocolcli.CgroupMemoryV1 ||
			capacity.CgroupLimit.Source == protocolcli.CgroupMemoryV2)
	if !physicalValid && !limitValid {
		return false
	}
	want := capacity.PhysicalBytes
	if !physicalValid || (limitValid && capacity.CgroupLimit.Bytes < want) {
		want = capacity.CgroupLimit.Bytes
	}
	if capacity.EffectiveBytes != want {
		return false
	}
	switch capacity.EffectiveSource {
	case protocolcli.MemoryCapacityPhysical:
		return physicalValid && capacity.EffectiveBytes == capacity.PhysicalBytes
	case protocolcli.MemoryCapacityCgroupLimit:
		return limitValid && capacity.EffectiveBytes == capacity.CgroupLimit.Bytes
	default:
		return false
	}
}

func validMemoryComposition(composition *jobs.CgroupMemoryComposition) bool {
	return composition != nil && validNonNegativeSafeInteger(composition.AnonBytes) &&
		validNonNegativeSafeInteger(composition.FileBytes) && validNonNegativeSafeInteger(composition.ShmemBytes) &&
		composition.ShmemBytes <= composition.FileBytes
}

func validMemoryPressure(pressure *jobs.MemoryPressure, memory *protocolcli.CgroupMemory) bool {
	if pressure == nil || !validNonNegativeSafeInteger(pressure.SomeStalledUs) ||
		!validNonNegativeSafeInteger(pressure.FullStalledUs) || pressure.FullStalledUs > pressure.SomeStalledUs {
		return false
	}
	if pressure.Scope == protocolcli.MemoryPressureCgroup {
		return memory != nil && memory.Source == protocolcli.CgroupMemoryV2
	}
	return pressure.Scope == protocolcli.MemoryPressureHost
}

func validMemoryEvents(events *jobs.CgroupMemoryEvents) bool {
	return events != nil && validNonNegativeSafeInteger(events.Low) && validNonNegativeSafeInteger(events.High) &&
		validNonNegativeSafeInteger(events.Max) && validNonNegativeSafeInteger(events.OOM) &&
		validNonNegativeSafeInteger(events.OOMKill)
}

// publications joins the Docker publisher's typed artifact facts with the
// corresponding package task's measured wall time and the renderer's observed
// scheduler bound. The terminal summary is intentionally conservative: a
// record is omitted unless it has a concrete target registry and a verified
// immutable digest, so a tag can never slip into the machine contract as
// provenance.
func (r Run) publications() []protocolcli.DockerPublication {
	if len(r.Tasks) == 0 || r.publishConcurrency.configuredCap < 1 {
		return nil
	}

	buildMsByProject := make(map[string]int64)
	for i := range r.Tasks {
		task := r.Tasks[i]
		if task.Identity.Task.Command != "package" || !dockerTask(task.Identity) {
			continue
		}
		buildMs := millis(task.Result.Timing.TaskWall)
		if buildMs == 0 {
			buildMs = millis(task.Result.Timing.Duration)
		}
		projectID := task.Identity.Project.ID
		if buildMs > buildMsByProject[projectID] {
			buildMsByProject[projectID] = buildMs
		}
	}

	concurrency := r.dockerPublishConcurrency()
	publications := make([]protocolcli.DockerPublication, 0)
	for i := range r.Tasks {
		task := r.Tasks[i]
		for _, publication := range task.Result.Publications {
			if !terminalDockerPublication(publication) {
				continue
			}
			timings := protocolcli.DockerPublishTimings{
				BuildMs: buildMsByProject[task.Identity.Project.ID],
			}
			if publication.PublishTimings != nil {
				timings.CacheLookupMs = nonNegative64(publication.PublishTimings.CacheLookupMs)
				timings.CacheTransferMs = nonNegative64(publication.PublishTimings.CacheTransferMs)
				timings.RegistryPushMs = nonNegative64(publication.PublishTimings.RegistryPushMs)
				timings.ReferencePublishMs = nonNegative64(publication.PublishTimings.ReferencePublishMs)
				timings.DigestResolveMs = nonNegative64(publication.PublishTimings.DigestResolveMs)
			}
			publications = append(publications, protocolcli.DockerPublication{
				Identity:      task.Identity,
				Session:       orUnknown(publication.Version),
				Registry:      publication.TargetRegistry,
				Image:         publication.Name,
				ImmutableRef:  publication.ImmutableRef,
				Tags:          append([]string(nil), publication.Tags...),
				ImageDigest:   publication.ImageDigest,
				ContentStatus: publication.ContentStatus,
				CacheOutcome:  publication.CacheOutcome,
				DigestReused:  publication.DigestReused,
				Timings:       timings,
				Concurrency:   concurrency,
			})
		}
	}
	if len(publications) == 0 {
		return nil
	}
	sort.Slice(publications, func(i, j int) bool {
		left, right := publications[i], publications[j]
		if left.Identity.Key != right.Identity.Key {
			return left.Identity.Key < right.Identity.Key
		}
		if left.Image != right.Image {
			return left.Image < right.Image
		}
		return left.ImageDigest < right.ImageDigest
	})
	return publications
}

func (r Run) dockerPublishConcurrency() protocolcli.DockerPublishConcurrency {
	cap := r.publishConcurrency.configuredCap
	if cap < 1 {
		cap = 1
	}
	effective := r.publishConcurrency.effective
	if effective < 0 {
		effective = 0
	}
	if effective > cap {
		effective = cap
	}
	return protocolcli.DockerPublishConcurrency{ConfiguredCap: cap, Effective: effective}
}

func terminalDockerPublication(publication jobs.Publication) bool {
	if publication.Registry != "docker" || publication.DryRun || publication.TargetRegistry == "" ||
		publication.Name == "" || !publication.DigestVerified || !immutableDigest(publication.ImageDigest) ||
		publication.ImmutableRef != publication.Name+"@"+publication.ImageDigest {
		return false
	}
	switch publication.CacheOutcome {
	case "hit":
		return (publication.ContentStatus == "retagged" || publication.ContentStatus == "reused") && publication.DigestReused
	case "miss":
		return publication.ContentStatus == "pushed" && !publication.DigestReused
	default:
		return false
	}
}

func dockerTask(identity protocolcli.TaskIdentity) bool {
	return identity.Task.Step == "docker" ||
		strings.Contains(identity.Task.Name, "docker") ||
		strings.Contains(identity.Task.Kind, "docker")
}

func immutableDigest(digest string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+64 {
		return false
	}
	for _, b := range digest[len(prefix):] {
		if !(b >= '0' && b <= '9') && !(b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

// failures lists every failed task, reuse included — the list v2 requires to be
// complete: a counted failure cannot be omitted from what an agent reads.
//
// It is built from the task list rather than SessionResult.Failures, which
// carries only the tasks that FRESHLY failed (a reused failure was reported by
// whoever built it, which is precisely the v1 accounting v2 drops). When the
// two cannot be reconciled — a run reduced from tasks this view was not given —
// the member is omitted rather than emitted at the wrong length, because a
// short list is a contract violation while an absent one is not.
func (r Run) failures(failed int) []protocolcli.TaskFailure {
	if failed == 0 {
		return nil
	}
	out := make([]protocolcli.TaskFailure, 0, failed)
	for i := range r.Tasks {
		task := &r.Tasks[i]
		if task.Result.Status != jobs.TaskStatusFailed {
			continue
		}
		out = append(out, protocolcli.TaskFailure{
			Identity:    task.Identity,
			Error:       taskError(task.Result.Error),
			Diagnostics: diagnostics(task.Result.Diagnostics),
		})
	}
	if len(out) != failed {
		return nil
	}
	return out
}

// Records projects every task onto its terminal v2 record, in reduction order.
func (r Run) Records() []protocolcli.TaskRecord {
	if len(r.Tasks) == 0 {
		return nil
	}
	records := make([]protocolcli.TaskRecord, 0, len(r.Tasks))
	for i := range r.Tasks {
		records = append(records, TaskRecord(r.Tasks[i]))
	}
	return records
}

// Executions is the run's PHYSICAL ledger: every subprocess this run spawned,
// listed ONCE however many task records it produced.
//
// The executions come from the reduction's ACCUMULATED ledger, which the
// scheduler appends to as each attempt finishes — never from the task list.
// That is deliberate: a task result keeps only its FINAL attempt, so deriving
// the ledger from it would silently drop a superseded retry's wall, CPU and
// peak RSS, and would drop an entire batch leader whose members all
// re-executed solo after a split failure. Both are cost the machine really
// paid, and a baseline that under-counts is worse than useless.
//
// Only `tasks` is counted from the task list, and counting it (rather than
// carrying it) is what keeps it honest: it reports how many logical records in
// THIS document reference the execution, so it is 0 for a superseded attempt
// and n for a live batch, with no second number able to drift from the first.
//
// Order is completion order, and every execution a task references is declared
// here — the referential rule protocols/cli enforces holds by construction,
// because a referenced execution is by definition one the scheduler recorded.
func (r Run) Executions() []protocolcli.ExecutionRecord {
	if r.Session == nil || len(r.Session.Executions) == 0 {
		return nil
	}
	fanOut := make(map[string]int, len(r.Tasks))
	for i := range r.Tasks {
		if execution := r.Tasks[i].Result.Execution; execution != nil {
			fanOut[execution.ID]++
		}
	}
	records := make([]protocolcli.ExecutionRecord, 0, len(r.Session.Executions))
	for i := range r.Session.Executions {
		execution := &r.Session.Executions[i]
		if execution.ID == "" {
			continue
		}
		records = append(records, protocolcli.ExecutionRecord{
			ID:          execution.ID,
			WallMs:      millis(execution.Wall),
			UserCPUMs:   millis(execution.UserCPU),
			SystemCPUMs: millis(execution.SystemCPU),
			MaxRSSBytes: nonNegative64(execution.MaxRSSBytes),
			IOInBlocks:  nonNegative64(execution.IOInBlocks),
			IOOutBlocks: nonNegative64(execution.IOOutBlocks),
			Concurrency: nonNegative(execution.Concurrency),
			Tasks:       fanOut[execution.ID],
		})
	}
	if len(records) == 0 {
		return nil
	}
	return records
}

// Preparation projects the dependency-preparation stage's ownership
// attribution.
//
// It is carried, not derived, for a stronger reason than the ledger above: the
// stage runs in the composer's phase 1c/1d, BEFORE planning, so no task and no
// execution in this document could account for it even in principle. The
// projection is a straight unit conversion and keeps the producer's canonical
// phase order; a phase the stage never entered was never recorded and is
// therefore absent here too, never a zero row.
func (r Run) Preparation() *protocolcli.SessionPreparation {
	if r.Session == nil || r.Session.Preparation == nil {
		return nil
	}
	source := r.Session.Preparation
	if len(source.Phases) == 0 {
		return nil
	}
	phases := make([]protocolcli.PreparationPhaseRecord, 0, len(source.Phases))
	for i := range source.Phases {
		phase := &source.Phases[i]
		if phase.Steps <= 0 {
			continue
		}
		phases = append(phases, protocolcli.PreparationPhaseRecord{
			Phase:  string(phase.Phase),
			WallMs: millis(phase.Wall),
			Steps:  nonNegative(phase.Steps),
			CPUMs:  millis(phase.CPU),
		})
	}
	if len(phases) == 0 {
		return nil
	}
	return &protocolcli.SessionPreparation{
		WallMs:      millis(source.Wall),
		Parallelism: max(source.Parallelism, 1),
		Phases:      phases,
	}
}

// TaskRecord projects one task onto its terminal v2 record. Status and reuse
// stay orthogonal: a reused result carries the verdict it stored.
func TaskRecord(task Task) protocolcli.TaskRecord {
	record := protocolcli.TaskRecord{
		Identity: task.Identity,
		// The physical execution this record came out of, absent when the task
		// spawned nothing — which is exactly how a consumer tells work from reuse
		// without inferring it from a duration.
		ExecutionID: executionID(task.Result.Execution),
		Status:      taskStatus(task.Result.Status),
		Reuse:       taskReuse(task.Result.Reuse),
		ExitCode:    nonNegative(task.Result.ExitCode),
		DurationMs:  millis(task.Result.Timing.Duration),
		TaskWallMs:  millis(task.Result.Timing.TaskWall),
		Diagnostics: diagnostics(task.Result.Diagnostics),
	}
	// The cases the task ran that have no test:case record: the producer's
	// count plus what the CLI's own bound dropped (jobs.TaskResult).
	record.TestCasesDropped = nonNegative(task.Result.TestCasesDropped)
	// The key the task was keyed on, derived once in the canonical model. The
	// guard reads the WIRE status, because this projection is what turns a
	// verdict outside the vocabulary into "skipped", and a skipped record that
	// named a digest is one the contract rejects.
	if record.Status != protocolcli.TaskStatusSkipped {
		record.InputDigest = task.Result.InputDigest
	}
	// Absent when no subprocess event was observed, which is every cache hit:
	// zero would claim an instant spawn instead of no spawn at all.
	if task.Result.Timing.FirstEventObserved {
		record.SpawnToFirstEventMs = millis(task.Result.Timing.SpawnToFirstEvent)
	}
	if record.Status != protocolcli.TaskStatusSuccess &&
		(task.Result.Error != nil || record.Status == protocolcli.TaskStatusFailed) {
		err := taskError(task.Result.Error)
		record.Error = &err
	}
	return record
}

// taskStatus maps the canonical verdict onto the v2 vocabulary. The spellings
// are identical by design; a verdict outside it never reached a bucket in the
// reduction either, and is reported as skipped rather than invented into one of
// the three verdicts that carry meaning.
func taskStatus(status jobs.TaskStatus) string {
	switch status {
	case jobs.TaskStatusSuccess:
		return protocolcli.TaskStatusSuccess
	case jobs.TaskStatusFailed:
		return protocolcli.TaskStatusFailed
	case jobs.TaskStatusCanceled:
		return protocolcli.TaskStatusCanceled
	default:
		return protocolcli.TaskStatusSkipped
	}
}

// taskReuse maps the canonical provenance onto the v2 vocabulary, which spells
// "the task executed" explicitly instead of v1's empty string.
func taskReuse(reuse jobs.ReuseKind) string {
	switch reuse {
	case jobs.ReuseLocalCache:
		return protocolcli.TaskReuseLocalCache
	case jobs.ReuseRemoteCache:
		return protocolcli.TaskReuseRemoteCache
	case jobs.ReuseCoalesced:
		return protocolcli.TaskReuseCoalesced
	default:
		return protocolcli.TaskReuseNone
	}
}

// taskError projects a task's failure onto the envelope error shape. The class
// vocabulary is closed ("usage" | "auth" | "api" | "signal" | "failure"), so a
// task's own extension-defined code (e.g. "TEST_FAILED") stays out of it — a
// task failure is always the "failure" class.
func taskError(err *jobs.JobError) protocolcli.ResultError {
	message := "task failed"
	if err != nil && err.Message != "" {
		message = err.Message
	}
	return protocolcli.ResultError{Code: errorClassFailure, Message: message}
}

func diagnostics(in []jobs.TaskDiagnostic) []protocolcli.Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]protocolcli.Diagnostic, 0, len(in))
	for _, d := range in {
		out = append(out, protocolcli.Diagnostic{
			Severity: diagnosticSeverity(d.Severity),
			Message:  orUnknown(d.Message),
			Code:     d.Code,
			File:     d.File,
			Line:     nonNegative(d.Line),
			Column:   nonNegative(d.Column),
		})
	}
	return out
}

// diagnosticSeverity closes the severity vocabulary the way the renderers
// already present it: anything that is not an error or a warning is
// informational.
func diagnosticSeverity(severity string) string {
	switch severity {
	case "error", "warning":
		return severity
	default:
		return "info"
	}
}

// abortSource maps the abort source onto the v2 vocabulary. An abort whose
// source was never recorded is reported as a signal: v2 requires the member
// whenever the outcome is aborted, and "a supervisor stopped this" is the
// weaker of the two claims.
func abortSource(by string) string {
	if by == jobs.AbortUser {
		return protocolcli.AbortedByUser
	}
	return protocolcli.AbortedBySignal
}

// abortMessage phrases an abort for the envelope error, matching the wording
// every v1 renderer already uses (output.abortDescription).
func abortMessage(by string) string {
	switch by {
	case jobs.AbortUser:
		return "session interrupted by user"
	case jobs.AbortSignal:
		return "session interrupted by signal"
	default:
		return "session interrupted"
	}
}

// executionID names the physical execution a result came out of, or "" when
// nothing was spawned.
func executionID(execution *jobs.Execution) string {
	if execution == nil {
		return ""
	}
	return execution.ID
}

func millis(d time.Duration) int64 { return nonNegative64(d.Milliseconds()) }

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func nonNegative64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
