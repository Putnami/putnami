package scheduler

import (
	"sync"
	"time"

	model "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/sdk/extension/procstat"
)

// OrchestrationSnapshot is the settled orchestration report of a run.
type OrchestrationSnapshot = model.OrchestrationSnapshot

// OrchestrationClock times the engine phases of one run. The engine creates
// it before its first phase and hands it to the scheduler, which settles the
// run's orchestration report from it (Config.Orchestration). Its methods are
// safe for concurrent use; a nil clock records nothing.
type OrchestrationClock struct {
	mu     sync.Mutex
	phases []model.OrchestrationPhaseWall
}

// NewOrchestrationClock returns a clock with no phase.
func NewOrchestrationClock() *OrchestrationClock { return &OrchestrationClock{} }

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

	engine := procstat.Take()
	jobs := procstat.TakeReported()
	snapshot := &OrchestrationSnapshot{
		Phases:              phases,
		Engine:              ledgerOf(engine.Kinds, engine.Git),
		Jobs:                ledgerOf(jobs.Kinds, jobs.Git),
		ExtensionCPU:        jobs.SelfCPU,
		UnreportedProcesses: jobs.Unreported,
	}
	self, selfOK := procstat.SelfCPU()
	children, childrenOK := procstat.ChildrenCPU()
	if selfOK && childrenOK {
		snapshot.CPUMeasured = true
		snapshot.EngineCPU = self
		snapshot.RunCPU = self + children
	}
	return snapshot
}

func ledgerOf(kinds []procstat.KindCount, git map[string]int64) model.ProcessLedger {
	ledger := model.ProcessLedger{Git: git}
	for _, kind := range kinds {
		ledger.Kinds = append(ledger.Kinds, model.ProcessCount{Kind: string(kind.Kind), Starts: kind.Starts, CPU: kind.CPU})
	}
	return ledger
}
