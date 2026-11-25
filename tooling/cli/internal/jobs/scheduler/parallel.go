package scheduler

import "strings"

const (
	parallelModeEco     = "eco"
	parallelModeAuto    = "auto"
	parallelModeMax     = "max"
	parallelModeNumeric = "numeric"
)

// resolveParallelDecisionWithHardware sizes the worker pool. CPU capacity is
// the logical CPUs clamped to cpuQuota, the cgroup bandwidth limit in cores (0
// when there is none). Every finite dispatch reserves at least one CPU of that
// capacity, so auto and max run one worker per CPU of capacity and eco half of
// them, and each serve step adds one; an explicit count wins, and usable
// memory may lower the capacity's share.
func resolveParallelDecisionWithHardware(
	cfg SchedulerConfig,
	planned []*ScheduledJob,
	logicalCPU, cpuQuota int,
	totalMemoryBytes uint64,
) ParallelDecision {
	if logicalCPU < 1 {
		logicalCPU = 1
	}
	capacity := logicalCPU
	if cpuQuota > 0 {
		capacity = min(capacity, cpuQuota)
	}

	heavyPermille := heavyJobPermille(planned)

	decision := ParallelDecision{
		LogicalCPU:       logicalCPU,
		CPUCapacity:      capacity,
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
	workers := workersForCapacity(mode, capacity)

	// The cap is computed against USABLE memory, not the machine total, so it
	// agrees with the resource pool the coordinator admits against.
	if cap := memoryWorkerCap(mode, usableMemoryBytes(totalMemoryBytes), heavyPermille); cap > 0 {
		decision.MemoryCapWorkers = cap
		if workers > cap {
			workers = cap
		}
	}

	decision.Mode = mode
	decision.Workers = clampWorkers(workers+serveStepCount(cfg, planned), len(planned))
	return decision
}

// serveStepCount is the planned serve steps (SchedulerConfig.ServeSteps). A
// serve step holds its worker until the run is interrupted and reserves no
// CPU, so each one adds a worker beside those the capacity sizes.
func serveStepCount(cfg SchedulerConfig, planned []*ScheduledJob) int {
	count := 0
	for _, job := range planned {
		if job != nil && cfg.ServeSteps[job.Key()] {
			count++
		}
	}
	return count
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

// workersForCapacity is the worker count of a mode for a CPU capacity: a
// worker beyond the capacity would wait for admission, never run.
func workersForCapacity(mode string, capacity int) int {
	if mode == parallelModeEco {
		return max(1, (capacity+1)/2)
	}
	return max(1, capacity)
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

// isHeavyJob answers from the declaration only: the step's heavy flag, the
// command's heavy trait, the project's cpuWeight overrides and the measured EMA
// history; a job's display name is never consulted.
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
