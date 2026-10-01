package jobs

import (
	"sort"
	"sync"
	"time"
)

// ParallelDecision captures how the scheduler chose its worker count, plus the
// hardware and plan inputs that drove the choice. It answers "which mode was
// used, how many workers were selected, and why".
type ParallelDecision struct {
	Mode             string `json:"mode"`                      // auto | eco | max | numeric
	Workers          int    `json:"workers"`                   // selected worker count
	LogicalCPU       int    `json:"logicalCpu"`                // detected logical CPUs (>=1)
	CPUCapacity      int    `json:"cpuCapacity,omitempty"`     // quota-clamped admission capacity
	MemoryTotalMiB   int    `json:"memoryTotalMiB"`            // stable physical/cgroup capacity before headroom (0 when unknown)
	MemoryUsableMiB  int    `json:"memoryUsableMiB,omitempty"` // admission capacity after headroom
	MemoryCapWorkers int    `json:"memoryCapWorkers"`          // memory-derived cap (0 when uncapped/unknown)
	PlannedJobs      int    `json:"plannedJobs"`               // number of jobs in the plan
	HeavyJobPermille int    `json:"heavyJobPermille"`          // share of heavy jobs, per mille
}

// ReadyWaitByCommand aggregates, per command, the time jobs spent ready (all
// dependencies satisfied) but waiting for a free worker. High values signal
// worker starvation rather than dependency stalls.
type ReadyWaitByCommand struct {
	Command string `json:"command"`
	Jobs    int    `json:"jobs"`
	TotalMs int64  `json:"totalMs"`
	MaxMs   int64  `json:"maxMs"`
}

// CriticalPathNode is one job on the critical (longest-duration) dependency
// chain.
type CriticalPathNode struct {
	Job        string `json:"job"`
	Command    string `json:"command"`
	DurationMs int64  `json:"durationMs"`
}

// CriticalPath is the longest dependency chain by cumulative job duration — the
// chain that bounds the minimum achievable makespan regardless of worker count.
type CriticalPath struct {
	DurationMs int64              `json:"durationMs"`
	Chain      []CriticalPathNode `json:"chain"`
}

// JobCPUBudget is one CPU budget recorded at spawn time: the singleton or
// shared-batch key, effective weight, expected CPU work, and cores the
// subprocess was allowed. Values are deterministic for a fixed plan, machine,
// and history; batch-compatible jobs use their precomputed class ceiling even
// when runtime timing changes the actual member set.
type JobCPUBudget struct {
	Job           string  `json:"job"`
	Weight        float64 `json:"weight"`
	ExpectedCPUMs float64 `json:"expectedCpuMs"`
	Budget        int     `json:"budget"`
}

// JobResourceReservation is one dispatched job/group's scheduler-only
// admission claim. Retries reuse the same deterministic claim. Job is the
// sorted logical member key(s); internal reservation IDs are intentionally
// never published.
type JobResourceReservation struct {
	Job       string `json:"job"`
	CPU       int    `json:"cpu"`
	MemoryMiB int    `json:"memoryMiB,omitempty"`
}

// TuningReport is the machine-readable summary of scheduler auto-tuning
// decisions and DAG wait behavior for a single run.
type TuningReport struct {
	Parallel      ParallelDecision     `json:"parallel"`
	HeavyJobRatio float64              `json:"heavyJobRatio"`
	ReadyWait     []ReadyWaitByCommand `json:"readyWait,omitempty"`
	CriticalPath  *CriticalPath        `json:"criticalPath,omitempty"`
	// CPUBudgets lists the budgets granted to jobs that actually executed a
	// subprocess (cache hits take none), sorted by job key.
	CPUBudgets []JobCPUBudget `json:"cpuBudgets,omitempty"`
	// ResourceReservations is sorted by member key and bounded, so session
	// observability cannot grow without limit or depend on completion timing.
	ResourceReservations []JobResourceReservation `json:"resourceReservations,omitempty"`
}

// jobTiming records the timestamps used to derive ready wait for a single job.
type jobTiming struct {
	readyAt time.Time // all dependencies satisfied; queued for a worker
	startAt time.Time // a worker picked the job up
}

// schedulerMetrics collects per-job timing during a scheduler run. Its methods
// are safe for concurrent use: markReady/markEnd are driven from the
// coordinator goroutine and markStart from worker goroutines.
type schedulerMetrics struct {
	mu     sync.Mutex
	timing map[string]*jobTiming
}

func newSchedulerMetrics() *schedulerMetrics {
	return &schedulerMetrics{timing: make(map[string]*jobTiming)}
}

func (m *schedulerMetrics) entry(key string) *jobTiming {
	t := m.timing[key]
	if t == nil {
		t = &jobTiming{}
		m.timing[key] = t
	}
	return t
}

// markReady records when a job's dependencies are all satisfied. Only the first
// call per job takes effect, so re-queueing never resets the clock.
func (m *schedulerMetrics) markReady(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.entry(key); t.readyAt.IsZero() {
		t.readyAt = time.Now()
	}
}

// markStart records when a worker began executing a job.
func (m *schedulerMetrics) markStart(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.entry(key); t.startAt.IsZero() {
		t.startAt = time.Now()
	}
}

// readyWaits returns the ready-wait duration per job (start - ready), only for
// jobs that became ready and then started.
func (m *schedulerMetrics) readyWaits() map[string]time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]time.Duration, len(m.timing))
	for key, t := range m.timing {
		if t.readyAt.IsZero() || t.startAt.IsZero() || t.startAt.Before(t.readyAt) {
			continue
		}
		out[key] = t.startAt.Sub(t.readyAt)
	}
	return out
}

// buildTuningReport assembles the run's tuning report from the parallel
// decision, the plan, the per-job results (for durations) and the measured
// ready waits.
func buildTuningReport(
	decision ParallelDecision,
	planned []*ScheduledJob,
	results map[string]*JobResult,
	readyWaits map[string]time.Duration,
	grants []cpuAllocation,
) *TuningReport {
	budgets := make([]JobCPUBudget, 0, len(grants))
	for _, g := range grants {
		budgets = append(budgets, JobCPUBudget{
			Job:           g.job,
			Weight:        g.weight,
			ExpectedCPUMs: g.expectedCPUMs,
			Budget:        g.budget,
		})
	}
	return &TuningReport{
		Parallel:      decision,
		HeavyJobRatio: float64(decision.HeavyJobPermille) / 1000.0,
		ReadyWait:     aggregateReadyWait(planned, readyWaits),
		CriticalPath:  computeCriticalPath(planned, results),
		CPUBudgets:    budgets,
	}
}

// aggregateReadyWait groups per-job ready waits by command, dropping commands
// whose aggregate rounds below a millisecond (measurement noise). The result is
// sorted by total wait descending, then command name, for stable output.
func aggregateReadyWait(planned []*ScheduledJob, waits map[string]time.Duration) []ReadyWaitByCommand {
	if len(waits) == 0 {
		return nil
	}

	commandOf := make(map[string]string, len(planned))
	for _, j := range planned {
		commandOf[j.Key()] = j.CommandName()
	}

	type agg struct {
		jobs  int
		total time.Duration
		max   time.Duration
	}
	byCommand := make(map[string]*agg)
	for key, wait := range waits {
		if wait <= 0 {
			continue
		}
		command := commandOf[key]
		if command == "" {
			command = "(unknown)"
		}
		a := byCommand[command]
		if a == nil {
			a = &agg{}
			byCommand[command] = a
		}
		a.jobs++
		a.total += wait
		if wait > a.max {
			a.max = wait
		}
	}

	out := make([]ReadyWaitByCommand, 0, len(byCommand))
	for command, a := range byCommand {
		if a.total.Milliseconds() == 0 {
			continue
		}
		out = append(out, ReadyWaitByCommand{
			Command: command,
			Jobs:    a.jobs,
			TotalMs: a.total.Milliseconds(),
			MaxMs:   a.max.Milliseconds(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalMs != out[j].TotalMs {
			return out[i].TotalMs > out[j].TotalMs
		}
		return out[i].Command < out[j].Command
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// computeCriticalPath finds the longest dependency chain weighted by measured
// job durations. It is deterministic given the plan and results, and ignores
// dependency keys that fall outside the plan (e.g. unmet upstream references).
func computeCriticalPath(planned []*ScheduledJob, results map[string]*JobResult) *CriticalPath {
	if len(planned) == 0 {
		return nil
	}

	durationMs := make(map[string]int64, len(planned))
	command := make(map[string]string, len(planned))
	dependsOn := make(map[string][]string, len(planned))
	inPlan := make(map[string]bool, len(planned))
	keys := make([]string, 0, len(planned))
	for _, j := range planned {
		key := j.Key()
		if inPlan[key] {
			continue
		}
		inPlan[key] = true
		keys = append(keys, key)
		command[key] = j.CommandName()
		dependsOn[key] = j.SchedulingPredecessors()
		if r := results[key]; r != nil {
			durationMs[key] = r.Duration.Milliseconds()
		}
	}

	finish := make(map[string]int64, len(keys))
	predecessor := make(map[string]string, len(keys))

	var compute func(key string) int64
	compute = func(key string) int64 {
		if f, ok := finish[key]; ok {
			return f
		}

		var best int64
		bestDep := ""
		for _, dep := range dependsOn[key] {
			if !inPlan[dep] {
				continue
			}
			if f := compute(dep); f > best {
				best = f
				bestDep = dep
			}
		}

		finish[key] = durationMs[key] + best
		predecessor[key] = bestDep
		return finish[key]
	}

	sort.Strings(keys) // deterministic argmax tie-breaking
	bestKey := ""
	var bestFinish int64 = -1
	for _, key := range keys {
		if f := compute(key); f > bestFinish {
			bestFinish = f
			bestKey = key
		}
	}
	if bestKey == "" || bestFinish <= 0 {
		return nil
	}

	var reversed []string
	for key := bestKey; key != ""; key = predecessor[key] {
		reversed = append(reversed, key)
	}
	chain := make([]CriticalPathNode, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		key := reversed[i]
		chain = append(chain, CriticalPathNode{
			Job:        key,
			Command:    command[key],
			DurationMs: durationMs[key],
		})
	}
	return &CriticalPath{DurationMs: bestFinish, Chain: chain}
}
