package scheduler

import (
	"sync"
	"time"
)

// taskWaitsLedger accumulates, per task key, the wall the scheduler spends on
// a task outside its subprocess: computing its key, waiting for a lock and
// restoring an entry. The steps run on worker goroutines before the task's
// result exists, so they are kept here and stamped on the result when it is
// recorded (stampWaits).
type taskWaitsLedger struct {
	mu    sync.Mutex
	waits map[string]*TaskWaits
}

// entry is job's waits, nil for a job that has no task key: a scheduler
// built by hand around a bare job measures nothing.
func (l *taskWaitsLedger) entry(job *ScheduledJob) *TaskWaits {
	if job == nil || job.Project == nil || job.JobDef == nil {
		return nil
	}
	if l.waits == nil {
		l.waits = map[string]*TaskWaits{}
	}
	key := job.Key()
	w := l.waits[key]
	if w == nil {
		w = &TaskWaits{}
		l.waits[key] = w
	}
	return w
}

// key records one key computation of job that started at started.
func (l *taskWaitsLedger) key(job *ScheduledJob, started time.Time) {
	elapsed := time.Since(started)
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := l.entry(job); w != nil {
		w.AddKey(elapsed)
	}
}

// lockWait records one lock wait of job that started at started.
func (l *taskWaitsLedger) lockWait(job *ScheduledJob, started time.Time) {
	elapsed := time.Since(started)
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := l.entry(job); w != nil {
		w.AddLockWait(elapsed)
	}
}

// restore records one restore of job that started at started.
func (l *taskWaitsLedger) restore(job *ScheduledJob, started time.Time) {
	elapsed := time.Since(started)
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := l.entry(job); w != nil {
		w.AddRestore(elapsed)
	}
}

// stampWaits moves what the ledger holds for job onto its result.
func (l *taskWaitsLedger) stampWaits(job *ScheduledJob, result *JobResult) {
	if result == nil || job == nil || job.Project == nil || job.JobDef == nil {
		return
	}
	l.mu.Lock()
	w := l.waits[job.Key()]
	delete(l.waits, job.Key())
	l.mu.Unlock()
	if w != nil {
		result.Waits.Merge(*w)
	}
}
