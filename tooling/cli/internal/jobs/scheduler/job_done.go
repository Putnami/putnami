package scheduler

// jobDone pairs a completed job with its result.
type jobDone struct {
	job    *ScheduledJob
	result *JobResult
}

// jobGroup is one scheduler dispatch. A singleton preserves the historical
// execution path; multiple jobs are an opportunistic same-key batch formed
// only from work that was already ready.
type jobGroup struct {
	jobs        []*ScheduledJob
	reservation resourceReservation
}

// jobGroupDone returns every original per-project result from one dispatch.
// The coordinator still completes DAG nodes, renderer rows, and session rows
// individually so batching never changes public job accounting.
type jobGroupDone struct {
	jobs        []jobDone
	reservation resourceReservation
}
