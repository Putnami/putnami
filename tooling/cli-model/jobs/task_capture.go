package jobs

// UsesDeclaredCapture reports whether this job can use a task-owned cache
// entry. It must be decidable BEFORE the task runs, so it reads nothing but the
// declaration and the job's own shape.
//
// A v3 declaration is the whole answer, in every command. Until an earlier fix, the
// `package` command was excluded unless a task made an explicit
// all-or-nothing assertion about one exact output shape, because every packager
// also read-merge-wrote a channel index at the package root that no task could
// own: a cache hit restored the artifact and skipped the merge, so publish saw
// a channel that was never recorded and silently skipped. Each packager now
// records its channel INSIDE the output it owns, so there is nothing left
// outside a declaration for a restore to miss, and no command needs a rule of
// its own.
func UsesDeclaredCapture(job *ScheduledJob) bool {
	return TaskDeclarationOf(job) != nil
}
