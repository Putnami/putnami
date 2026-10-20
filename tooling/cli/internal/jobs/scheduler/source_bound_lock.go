package scheduler

import (
	"slices"
	"sync"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// lockSourceBoundProjects takes the source-bound lock of the project of every
// source-bound task in jobs, and returns the function that releases them.
//
// A source-bound task stores its entry at the binding its project's stamp
// names when it starts, and its outputs embed the binding the stamp names when
// its subprocess reads it. Its preparation rewrites that stamp
// (restampSourceBinding). Two source-bound tasks of one project can run in one
// session, such as the describe of a build and the describe of a test --race,
// and the task-output keys do not exclude tasks of the same session. Without
// this lock the second task could rewrite the stamp while the first one runs,
// and the first would store its entry at a binding its outputs do not name.
// executeJobGroup therefore holds the lock from the preparation through the
// capture: the stamp rewrite, the reading of the binding, the subprocess, its
// retries, and the capture of the entry. A task that is not source-bound does
// not take it.
//
// The caller takes it after its task-output locks, and its holder waits for no
// task-output lock and no other source-bound lock, so the wait ends when the
// holder's group ends. A group takes each of its projects once, in path order.
func (s *Scheduler) lockSourceBoundProjects(jobs []*ScheduledJob) func() {
	var paths []string
	for _, job := range jobs {
		if job != nil && job.Project != nil && cachekey.TaskIsSourceBound(job) {
			paths = append(paths, job.Project.Path)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	held := make([]*sync.Mutex, 0, len(paths))
	for _, path := range paths {
		lock := s.sourceBoundLock(path)
		lock.Lock()
		held = append(held, lock)
	}
	return func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].Unlock()
		}
	}
}

// sourceBoundLock returns the source-bound lock of the project at path,
// creating it on first use.
func (s *Scheduler) sourceBoundLock(path string) *sync.Mutex {
	s.sourceBoundLocksMu.Lock()
	defer s.sourceBoundLocksMu.Unlock()
	if s.sourceBoundLocks == nil {
		s.sourceBoundLocks = make(map[string]*sync.Mutex)
	}
	lock := s.sourceBoundLocks[path]
	if lock == nil {
		lock = &sync.Mutex{}
		s.sourceBoundLocks[path] = lock
	}
	return lock
}
