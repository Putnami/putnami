package jobs

// JobCommandName returns the base command name for a job, stripping
// pipeline step suffixes (e.g., "build~transpile" → "build").
func JobCommandName(job *ScheduledJob) string {
	command, _ := JobCommandAndStep(job.JobDef.Name)
	return command
}
