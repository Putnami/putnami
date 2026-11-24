package jobs

import "time"

// TaskWaits is the wall the scheduler spent on one task outside its
// subprocess. Each step is observed or not: a step the task never entered
// stays unobserved, which is not the same as a step that took no time.
type TaskWaits struct {
	Key, LockWait, Restore                     time.Duration
	KeyObserved, LockObserved, RestoreObserved bool
}

// AddKey adds the wall of one key computation.
func (w *TaskWaits) AddKey(d time.Duration) { w.Key += d; w.KeyObserved = true }

// AddLockWait adds the wall of one lock wait.
func (w *TaskWaits) AddLockWait(d time.Duration) { w.LockWait += d; w.LockObserved = true }

// AddRestore adds the wall of one restore.
func (w *TaskWaits) AddRestore(d time.Duration) { w.Restore += d; w.RestoreObserved = true }

// Merge adds every observed step of other.
func (w *TaskWaits) Merge(other TaskWaits) {
	if other.KeyObserved {
		w.AddKey(other.Key)
	}
	if other.LockObserved {
		w.AddLockWait(other.LockWait)
	}
	if other.RestoreObserved {
		w.AddRestore(other.Restore)
	}
}

// Any reports whether the task entered any of the three steps.
func (w TaskWaits) Any() bool { return w.KeyObserved || w.LockObserved || w.RestoreObserved }

// OrchestrationPhaseWall is the wall of one engine phase.
type OrchestrationPhaseWall struct {
	Phase string
	Wall  time.Duration
}

// ProcessCount is the processes of one kind and the CPU of those measured.
// Measured counts the processes whose CPU is in CPU: zero means no CPU was
// measured, which is not a CPU of zero.
type ProcessCount struct {
	Kind     string
	Starts   int64
	CPU      time.Duration
	Measured int64
}

// ProcessLedger is the processes one origin started, by kind in canonical
// order, and its git calls by subcommand.
type ProcessLedger struct {
	Kinds []ProcessCount
	Git   map[string]int64
}

// OrchestrationSnapshot is the settled orchestration report of a run.
type OrchestrationSnapshot struct {
	// Phases are the engine phases the run entered, in the order entered.
	Phases []OrchestrationPhaseWall
	// Engine is what the CLI process started; Jobs what the job processes
	// reported they started.
	Engine, Jobs ProcessLedger
	// CPUMeasured is false where the platform measures no process CPU: every
	// CPU member below is then zero and means nothing.
	CPUMeasured bool
	// EngineCPU is the CLI process's own CPU; RunCPU adds every child it
	// waited for.
	EngineCPU, RunCPU time.Duration
	// ExtensionCPU is the CPU job processes reported for themselves.
	ExtensionCPU time.Duration
	// UnreportedProcesses counts the job processes that reported no CPU.
	UnreportedProcesses int64
}

// Of returns the entry of kind in l.
func (l ProcessLedger) Of(kind string) ProcessCount {
	for _, entry := range l.Kinds {
		if entry.Kind == kind {
			return entry
		}
	}
	return ProcessCount{Kind: kind}
}

// OrchestrationCPU is the orchestration CPU of s: the engine, the extension
// processes without their children, the control processes, the CLI starts
// made by jobs, and the git calls of both origins. A CLI the engine starts,
// such as a workspace hook, is counted among its processes and not here.
func (s *OrchestrationSnapshot) OrchestrationCPU() (engine, extension, control, cli, git time.Duration) {
	return s.EngineCPU, s.ExtensionCPU,
		s.Engine.Of("control").CPU,
		s.Jobs.Of("cli").CPU,
		s.Engine.Of("git").CPU + s.Jobs.Of("git").CPU
}
