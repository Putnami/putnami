package jobs

// TaskResourceObservation is one bounded, concurrency-conditioned resource
// profile. RSS and useful CPU occupancy both depend on the subprocess ceiling,
// so measurements taken under different grants are never averaged together.
// Confidence is persisted for inspectability but recomputed from Samples on
// load; corrupt confidence can therefore never make admission optimistic.
type TaskResourceObservation struct {
	CPUMs              float64 `json:"cpuMs"`
	WallMs             float64 `json:"wallMs"`
	MaxRSSBytes        int64   `json:"maxRssBytes,omitempty"`
	GrantedConcurrency int     `json:"grantedConcurrency"`
	BatchSize          int     `json:"batchSize"`
	Samples            int     `json:"samples"`
	Confidence         float64 `json:"confidence"`
	Saturation         float64 `json:"saturation"`
	UpdatedAt          string  `json:"updatedAt,omitempty"`
}

// TaskResourceProfile is the in-memory, deterministically ordered copy placed
// on a planned job. The on-disk map is bounded by maxTaskResourceObservations;
// sorting here makes recommendation independent of JSON/map iteration order.
type TaskResourceProfile struct {
	Observations []TaskResourceObservation
}

// NearestObservation returns the concurrency- and batch-conditioned evidence
// closest to the requested subprocess shape. Ties prefer the higher observed
// ceiling, then the larger observed batch, whose RSS are the conservative
// choices. The profile slice is sorted, so this decision is stable across
// map/JSON order.
func (p TaskResourceProfile) NearestObservation(concurrency, batchSize int) (TaskResourceObservation, bool) {
	if len(p.Observations) == 0 {
		return TaskResourceObservation{}, false
	}
	best := p.Observations[0]
	bestDistance := absInt(best.GrantedConcurrency - concurrency)
	bestBatchDistance := absInt(best.BatchSize - batchSize)
	for _, observation := range p.Observations[1:] {
		distance := absInt(observation.GrantedConcurrency - concurrency)
		batchDistance := absInt(observation.BatchSize - batchSize)
		if distance < bestDistance ||
			(distance == bestDistance && batchDistance < bestBatchDistance) ||
			(distance == bestDistance && batchDistance == bestBatchDistance && observation.GrantedConcurrency > best.GrantedConcurrency) ||
			(distance == bestDistance && batchDistance == bestBatchDistance && observation.GrantedConcurrency == best.GrantedConcurrency && observation.BatchSize > best.BatchSize) {
			best, bestDistance, bestBatchDistance = observation, distance, batchDistance
		}
	}
	return best, true
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// resolveConfigTimeoutMs returns a positive project timeout override, if one
// exists. It deliberately mirrors CPU-weight precedence: a full task-step key
// wins over its command key, but a step that only tunes cpuWeight still lets the
// command entry supply timeoutMs. Non-positive values are rejected while
// loading project config and remain ignored here as a defensive fallback for
// hand-built plans and callers outside workspace discovery.
func resolveConfigTimeoutMs(job *ScheduledJob) (int, bool) {
	if job == nil || job.JobDef == nil || job.Project == nil || job.Project.Config == nil {
		return 0, false
	}
	for _, key := range []string{job.DisplayName(), job.CommandName()} {
		tuning, ok := job.Project.Config.Tasks[key]
		if !ok || tuning.TimeoutMs == nil || *tuning.TimeoutMs <= 0 {
			continue
		}
		return *tuning.TimeoutMs, true
	}
	return 0, false
}
