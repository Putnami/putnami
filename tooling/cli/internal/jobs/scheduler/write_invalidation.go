package scheduler

// invalidateWritten records that a task of the run wrote paths, absolute
// paths an execution, a restore or a stamp wrote: the run's read of the
// candidate cut is marked stale first, then every memoized input digest the
// paths can move is dropped. In this order a key computed between the two
// steps reads the cut again and memoizes nothing the drop keeps, so no later
// key reuses a digest of the cut before the write (protocols/job ADR 0003).
func (s *Scheduler) invalidateWritten(paths []string) {
	if len(paths) == 0 {
		return
	}
	s.tree.InvalidateCut(paths)
	betweenInvalidations()
	s.cache.InvalidateFileHashesUnder(paths)
}

// invalidateEverything is invalidateWritten for a task that may have
// rewritten anything in the workspace: a source rewriter.
func (s *Scheduler) invalidateEverything() {
	if s.ws != nil {
		s.tree.InvalidateCut([]string{s.ws.Root})
	}
	betweenInvalidations()
	s.cache.InvalidateFileHashes()
}

// betweenInvalidations runs between the two steps of an invalidation. It is
// a variable only so a test can compute a key there, as another worker can.
var betweenInvalidations = func() {}
