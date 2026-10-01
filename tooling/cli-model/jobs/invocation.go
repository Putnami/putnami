package jobs

// IsFinalizerJob reports whether the plan node is a `runOn: finally` step with
// the relation the contract requires alongside it.
func IsFinalizerJob(job *ScheduledJob) bool {
	return job != nil && job.Step != nil && job.Step.IsFinalizer() && job.Step.Finalizes != nil
}
