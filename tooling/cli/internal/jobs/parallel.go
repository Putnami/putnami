package jobs

import "strings"

const (
	parallelModeEco     = "eco"
	parallelModeAuto    = "auto"
	parallelModeMax     = "max"
	parallelModeNumeric = "numeric"

	mixedHeavyPlanPermille = 300
	heavyPlanPermille      = 650
)

func resolveParallelDecisionWithHardware(
	cfg SchedulerConfig,
	planned []*ScheduledJob,
	logicalCPU int,
	totalMemoryBytes uint64,
) ParallelDecision {
	if logicalCPU < 1 {
		logicalCPU = 1
	}

	// Compute the heavy-job share once; three tuning steps below consume it.
	heavyPermille := heavyJobPermille(planned)

	decision := ParallelDecision{
		LogicalCPU:       logicalCPU,
		CPUCapacity:      logicalCPU,
		MemoryTotalMiB:   int(totalMemoryBytes / (1024 * 1024)),
		MemoryUsableMiB:  int(usableMemoryBytes(totalMemoryBytes) / (1024 * 1024)),
		PlannedJobs:      len(planned),
		HeavyJobPermille: heavyPermille,
	}

	// An explicit numeric count overrides hardware tuning entirely.
	if cfg.MaxParallel > 0 {
		decision.Mode = parallelModeNumeric
		decision.Workers = clampWorkers(cfg.MaxParallel, len(planned))
		return decision
	}

	mode := normalizeParallelMode(cfg.MaxParallelMode)
	workers := baseWorkersForMode(mode, logicalCPU)
	workers = adjustWorkersForPlan(mode, workers, logicalCPU, heavyPermille)

	// The cap is computed against USABLE memory, not the machine total, so it
	// agrees with the resource pool the coordinator admits against. This lowers
	// the cap by the headroom fraction on a memory-bound host, and it is a second
	// reduction on top of admission itself — both bound the same plan, and only
	// the tighter one is ever visible in a run.
	if cap := memoryWorkerCap(mode, usableMemoryBytes(totalMemoryBytes), heavyPermille); cap > 0 {
		decision.MemoryCapWorkers = cap
		if workers > cap {
			workers = cap
		}
	}

	decision.Mode = mode
	decision.Workers = clampWorkers(workers, len(planned))
	return decision
}

func normalizeParallelMode(mode string) string {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	switch normalized {
	case parallelModeEco, parallelModeMax:
		return normalized
	default:
		return parallelModeAuto
	}
}

func baseWorkersForMode(mode string, logicalCPU int) int {
	switch mode {
	case parallelModeEco:
		return maxInt(1, minInt(8, (logicalCPU+1)/2))
	case parallelModeMax:
		return maxInt(1, logicalCPU*3)
	default:
		return maxInt(1, logicalCPU*2)
	}
}

func adjustWorkersForPlan(mode string, workers, logicalCPU, heavyPermille int) int {
	switch mode {
	case parallelModeEco:
		if heavyPermille < 200 {
			return maxInt(workers, minInt(logicalCPU, 8))
		}
		return workers
	case parallelModeMax:
		if heavyPermille >= mixedHeavyPlanPermille {
			return maxInt(logicalCPU, logicalCPU*2)
		}
		return workers
	default:
		if heavyPermille >= heavyPlanPermille {
			return maxInt(logicalCPU, logicalCPU+logicalCPU/2)
		}
		if heavyPermille < 200 {
			return maxInt(workers, logicalCPU*3)
		}
		return workers
	}
}

func heavyJobPermille(planned []*ScheduledJob) int {
	if len(planned) == 0 {
		return 0
	}
	heavy := 0
	for _, job := range planned {
		if isHeavyJob(job) {
			heavy++
		}
	}
	return heavy * 1000 / len(planned)
}

// isHeavyJob answers from the DECLARATION only.
//
// It used to fall back to matching the job's display name against a list of
// substrings — "golangci-lint", "build~types", "staticcheck", "build~
// cross-compile", "lint~check", plus test~test by command/step name. Every one
// of those is now declared as `heavy: true` on the owning manifest's step, so
// the fallback bought nothing but the failure mode it always had: a THIRD-PARTY
// extension whose expensive step happened to be called "lint~check" was tuned
// for as heavy, while one called "lint~vet" was not, and neither author could
// discover why. Scheduling reads what the manifest says.
//
// This deletes only the NAME heuristic. Declared per-step `heavy`, the command's
// heavy trait, the project's cpuWeight overrides and the measured EMA history in
// task_stats.go are all untouched — they are evidence, not guesswork.
func isHeavyJob(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	// A per-step heavy override wins, then the command's heavy trait (verb
	// default or manifest declaration).
	if job.JobDef.Heavy != nil {
		return *job.JobDef.Heavy
	}
	return job.JobDef.Traits.Heavy
}

func memoryWorkerCap(mode string, totalMemoryBytes uint64, heavyPermille int) int {
	if totalMemoryBytes == 0 {
		return 0
	}
	totalMiB := int(totalMemoryBytes / (1024 * 1024))
	if totalMiB <= 0 {
		return 0
	}

	// The cap estimates average memory pressure per worker. Heavy plans include
	// jobs like golangci-lint, TS declaration generation, and test runners, which
	// already fan out inside their toolchains.
	perWorkerMiB := 512 + heavyPermille*768/1000
	switch mode {
	case parallelModeEco:
		perWorkerMiB += 512
	case parallelModeMax:
		perWorkerMiB = maxInt(384, perWorkerMiB-128)
	}
	return maxInt(1, totalMiB/perWorkerMiB)
}

func totalMemoryBytes() uint64 {
	return totalMemoryBytesAt(runnerEnvironmentRoot, readFile)
}

// stableMemoryCapacityBytes returns the smaller of physical RAM and this
// process's cgroup memory limit. Both are allocation configuration, not live
// pressure, so identical machines/plans do not change ceilings because a
// co-tenant happened to consume memory at sampling time.
func stableMemoryCapacityBytes(root string, physical uint64) uint64 {
	sample := environmentSample{}
	if physical > 0 {
		sample.memoryCapacity = memoryCapacityReading{physicalBytes: physical, hasPhysical: true}
	}
	readCgroupMemoryFrom(root, &sample, readFile)
	effective, _, ok := sample.memoryCapacity.effectiveBytes()
	if !ok {
		return 0
	}
	return effective
}

func clampWorkers(workers, plannedCount int) int {
	if plannedCount <= 0 {
		return 1
	}
	if workers > plannedCount {
		workers = plannedCount
	}
	if workers < 1 {
		return 1
	}
	return workers
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
