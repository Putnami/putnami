package jobs

// CanUseCache reports whether a planned job's task definition permits cache
// reuse. Invocation-level settings such as --no-cache are deliberately not
// considered here, so planning and display code can share the task-level rule.
func CanUseCache(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	// Every cacheable task is captured through its declared, task-owned entry.
	// An incomplete contract still executes, but cannot safely publish a cache
	// entry: nothing states what the task owns, so nothing can prove a restore
	// reproduces it.
	if !UsesDeclaredCapture(job) {
		return false
	}
	if !job.JobDef.Cache {
		return false
	}
	if job.JobDef.TaskCachePolicy != nil && !job.JobDef.TaskCachePolicy.IsEnabled() {
		return false
	}
	// Step-level cache override
	if job.Step != nil && job.Step.Cache != nil && job.Step.Cache.Enabled != nil {
		return *job.Step.Cache.Enabled
	}
	return true
}
