package scheduler

import (
	"sync"
	"sync/atomic"
	"time"

	model "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/sdk/extension/procstat"
)

// OrchestrationSnapshot is the settled orchestration report of a run.
type OrchestrationSnapshot = model.OrchestrationSnapshot

// OrchestrationClock times the engine phases of one run. The engine creates
// it when the run starts and hands it to the scheduler, which settles the
// run's orchestration report from it (Config.Orchestration). Its methods are
// safe for concurrent use; a nil clock records nothing.
type OrchestrationClock struct {
	mu     sync.Mutex
	phases []model.OrchestrationPhaseWall
	// base is what the process-wide ledgers held when the clock started, nil
	// for the first run of the process, which reports everything since the
	// process started: its bootstrap is part of its orchestration. A later run
	// in the same process, as cache verify and an impact plan start, reports
	// its own share only.
	base *ledgerReading
}

// ledgerReading is one reading of the process-wide ledgers and CPU.
type ledgerReading struct {
	engine         procstat.Snapshot
	jobs           procstat.Reported
	self, children time.Duration
	measured       bool
}

func readLedgers() ledgerReading {
	self, selfOK := procstat.SelfCPU()
	children, childrenOK := procstat.ChildrenCPU()
	return ledgerReading{
		engine: procstat.Take(), jobs: procstat.TakeReported(),
		self: self, children: children, measured: selfOK && childrenOK,
	}
}

// clockStarted is set once a clock of this process has started.
var clockStarted atomic.Bool

// NewOrchestrationClock returns a clock with no phase.
func NewOrchestrationClock() *OrchestrationClock {
	clock := &OrchestrationClock{}
	if clockStarted.Swap(true) {
		reading := readLedgers()
		clock.base = &reading
	}
	return clock
}

// Phase records that the run spent the wall since started in phase. A phase
// entered twice sums its walls.
func (c *OrchestrationClock) Phase(phase string, started time.Time) {
	if c == nil {
		return
	}
	elapsed := time.Since(started)
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.phases {
		if c.phases[i].Phase == phase {
			c.phases[i].Wall += elapsed
			return
		}
	}
	c.phases = append(c.phases, model.OrchestrationPhaseWall{Phase: phase, Wall: elapsed})
}

// Snapshot settles the run's orchestration report: the phases the clock
// recorded, the processes this CLI process started and those its job processes
// reported, and the CPU of the CLI process and of the children it waited for.
// It reads the process-wide ledgers (go.putnami.dev/sdk/extension/procstat),
// which count from the start of the CLI process.
func (c *OrchestrationClock) Snapshot() *OrchestrationSnapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	phases := append([]model.OrchestrationPhaseWall(nil), c.phases...)
	c.mu.Unlock()

	now := readLedgers()
	base := ledgerReading{}
	if c.base != nil {
		base = *c.base
	}
	snapshot := &OrchestrationSnapshot{
		Phases:              phases,
		Engine:              ledgerOf(now.engine.Kinds, base.engine.Kinds, now.engine.Git, base.engine.Git),
		Jobs:                ledgerOf(now.jobs.Kinds, base.jobs.Kinds, now.jobs.Git, base.jobs.Git),
		ExtensionCPU:        now.jobs.SelfCPU - base.jobs.SelfCPU,
		UnreportedProcesses: now.jobs.Unreported - base.jobs.Unreported,
	}
	if now.measured {
		snapshot.CPUMeasured = true
		snapshot.EngineCPU = now.self - base.self
		snapshot.RunCPU = now.self + now.children - base.self - base.children
	}
	return snapshot
}

// ledgerOf is the ledger between two readings, by kind and git subcommand.
func ledgerOf(kinds, baseKinds []procstat.KindCount, git, baseGit map[string]int64) model.ProcessLedger {
	ledger := model.ProcessLedger{Git: map[string]int64{}}
	for i, kind := range kinds {
		count := model.ProcessCount{Kind: string(kind.Kind), Starts: kind.Starts, CPU: kind.CPU, Measured: kind.Measured}
		if i < len(baseKinds) {
			count.Starts -= baseKinds[i].Starts
			count.CPU -= baseKinds[i].CPU
			count.Measured -= baseKinds[i].Measured
		}
		ledger.Kinds = append(ledger.Kinds, count)
	}
	for subcommand, calls := range git {
		if calls -= baseGit[subcommand]; calls > 0 {
			ledger.Git[subcommand] = calls
		}
	}
	return ledger
}
