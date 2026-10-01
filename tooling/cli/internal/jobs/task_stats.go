package jobs

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	taskStatsVersion            = 3
	previousTaskStatsVersion    = 2
	legacyTaskStatsVersion      = 1
	maxTaskResourceObservations = 8

	// taskStatsAlpha is the EMA smoothing factor: half the estimate renews on
	// every observation, so the stats track real shifts within a couple of
	// runs without one outlier rewriting history.
	taskStatsAlpha = 0.5

	// learnedWeightCap bounds the history-derived boost so one pathological
	// task cannot monopolize the machine against everything running with it.
	learnedWeightCap = 8.0

	// learnedWeightMinPeers is the minimum number of projects (including this
	// one) that must have history for the same job name before "unbalanced vs
	// its peers" means anything.
	learnedWeightMinPeers = 2
)

// taskStat is the persisted execution history of one (project, job) pair.
// CPUMs is the EMA of the subprocess tree's CPU time — unlike wall time it
// measures the work itself, so a task that ran throttled does not look
// cheaper than it is and the boost cannot oscillate with past budgets.
// WallMs is the EMA of wall-clock duration, used to dispatch long-pole jobs
// first. MaxUnweightedConcurrency retains the largest successful
// history-backed singleton ceiling before configured or learned weight is
// applied. That weight-independent floor protects known long poles while still
// allowing a weight change to affect the next comparable grant. Cold fallbacks
// and batch/class grants are deliberately excluded.
type taskStat struct {
	CPUMs                    float64                             `json:"cpuMs"`
	WallMs                   float64                             `json:"wallMs"`
	MaxUnweightedConcurrency int                                 `json:"maxUnweightedConcurrency,omitempty"`
	Samples                  int                                 `json:"samples"`
	UpdatedAt                string                              `json:"updatedAt,omitempty"`
	Observations             map[string]*taskResourceObservation `json:"observations,omitempty"`
}

type taskStatsFile struct {
	Version int                  `json:"version"`
	Tasks   map[string]*taskStat `json:"tasks"`
}

// taskStatsStore holds machine-local execution history across runs. It is an
// execution hint store only: entries never enter cache keys, and every load
// or save failure degrades to "no history" rather than failing the build.
type taskStatsStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]*taskStat
	dirty   bool
}

func taskStatsPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".putnami", "stats", "tasks.json")
}

// loadTaskStats reads the workspace's task history. Missing or corrupt files
// yield an empty store.
func loadTaskStats(workspaceRoot string) *taskStatsStore {
	store := &taskStatsStore{
		path:    taskStatsPath(workspaceRoot),
		entries: make(map[string]*taskStat),
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		return store
	}
	var file taskStatsFile
	if err := json.Unmarshal(data, &file); err != nil ||
		(file.Version != taskStatsVersion && file.Version != previousTaskStatsVersion && file.Version != legacyTaskStatsVersion) {
		return store
	}
	migratingLegacy := file.Version == legacyTaskStatsVersion
	migratingPrevious := file.Version == previousTaskStatsVersion
	for key, stat := range file.Tasks {
		if stat == nil || stat.Samples <= 0 || !validResourceFloat(stat.WallMs) || !validResourceFloat(stat.CPUMs) {
			continue
		}
		if migratingLegacy {
			// Version 1 persisted the post-weight grant as maxConcurrency. Its
			// configured/learned multiplier cannot be reconstructed, so retain the
			// measured EMAs but deliberately drop the ambiguous floor.
			stat.MaxUnweightedConcurrency = 0
		} else if stat.MaxUnweightedConcurrency < 0 {
			stat.MaxUnweightedConcurrency = 0
		}
		if migratingLegacy || migratingPrevious {
			// v1/v2 did not retain RSS with its observation ceiling. Keep their
			// useful CPU/wall evidence, but do not fabricate a memory profile.
			stat.Observations = nil
		} else {
			stat.Observations = sanitizeTaskObservations(stat.Observations)
		}
		store.entries[key] = stat
	}
	store.dirty = migratingLegacy || migratingPrevious
	return store
}

func validResourceFloat(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func observationConfidence(samples int) float64 {
	if samples <= 0 {
		return 0
	}
	return min(float64(samples)/4, 1)
}

func sanitizeTaskObservations(observations map[string]*taskResourceObservation) map[string]*taskResourceObservation {
	if len(observations) == 0 {
		return nil
	}
	clean := make(map[string]*taskResourceObservation, min(len(observations), maxTaskResourceObservations))
	keys := make([]string, 0, len(observations))
	for key := range observations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		observation := observations[key]
		if observation == nil || observation.GrantedConcurrency <= 0 || observation.Samples <= 0 ||
			observation.MaxRSSBytes < 0 || !validResourceFloat(observation.CPUMs) ||
			!validResourceFloat(observation.WallMs) || !validResourceFloat(observation.Saturation) {
			continue
		}
		copy := *observation
		if copy.BatchSize <= 0 {
			copy.BatchSize = 1
		}
		copy.Confidence = observationConfidence(copy.Samples)
		copy.Saturation = min(max(copy.Saturation, 0), 1)
		clean[taskObservationKey(copy.GrantedConcurrency, copy.BatchSize)] = &copy
	}
	pruneTaskObservations(clean)
	if len(clean) == 0 {
		return nil
	}
	return clean
}

func pruneTaskObservations(observations map[string]*taskResourceObservation) {
	for len(observations) > maxTaskResourceObservations {
		oldestKey := ""
		for key, observation := range observations {
			if oldestKey == "" ||
				observationOlder(observation.UpdatedAt, observations[oldestKey].UpdatedAt, key, oldestKey) {
				oldestKey = key
			}
		}
		delete(observations, oldestKey)
	}
}

// observationOlder reports whether a is the better eviction candidate.
//
// The stamps are compared as INSTANTS, never as text: RFC3339Nano drops
// trailing zeros from the fractional second, so "…:00.15Z" sorts before
// "…:00.1Z" lexicographically while being the later instant. An absent or
// unparseable stamp is treated as oldest — it predates observation recording or
// is corrupt, and either way it is the right thing to drop first. Equal instants
// fall back to key order so eviction never depends on map iteration.
func observationOlder(aStamp, bStamp, aKey, bKey string) bool {
	aTime, aOK := observationTime(aStamp)
	bTime, bOK := observationTime(bStamp)
	if aOK != bOK {
		return !aOK
	}
	if aOK && !aTime.Equal(bTime) {
		return aTime.Before(bTime)
	}
	return aKey < bKey
}

func observationTime(stamp string) (time.Time, bool) {
	if stamp == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func taskObservationKey(concurrency, batchSize int) string {
	return strconv.Itoa(concurrency) + "x" + strconv.Itoa(max(batchSize, 1))
}

// taskStatKey identifies a job's history across runs: stable for a given
// project and job name, independent of cache keys and plan composition.
func taskStatKey(job *ScheduledJob) string {
	return job.Project.ID + "|" + job.DisplayName()
}

// record folds a real execution into the history. Cache hits, failures, and
// cancellations are excluded: hits did no work, and aborted runs would teach
// the store durations the task does not have. The pre-weight ceiling is
// retained only when recommendation marked the singleton grant as derived from
// prior history.
func (s *taskStatsStore) record(job *ScheduledJob, result *JobResult) {
	if s == nil || job == nil || result == nil {
		return
	}
	if result.Status != "success" || result.CacheHit || result.Coalesced || result.Duration <= 0 {
		return
	}

	wallMs := float64(result.Duration.Milliseconds())
	cpuMs := float64(result.CPUTime.Milliseconds())
	if cpuMs < 0 {
		cpuMs = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := taskStatKey(job)
	stat := s.entries[key]
	if stat == nil || stat.Samples <= 0 {
		stat = &taskStat{CPUMs: cpuMs, WallMs: wallMs}
	} else {
		stat.CPUMs = taskStatsAlpha*cpuMs + (1-taskStatsAlpha)*stat.CPUMs
		stat.WallMs = taskStatsAlpha*wallMs + (1-taskStatsAlpha)*stat.WallMs
	}
	if job.CPUBudgetHistoryEligible {
		if job.CPUBudgetUnweightedCeiling > stat.MaxUnweightedConcurrency {
			stat.MaxUnweightedConcurrency = job.CPUBudgetUnweightedCeiling
		}
	}
	stat.Samples++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	stat.UpdatedAt = now
	if result.Execution != nil && result.Execution.Concurrency > 0 {
		concurrency := result.Execution.Concurrency
		batchSize := max(result.Execution.BatchSize, 1)
		if stat.Observations == nil {
			stat.Observations = make(map[string]*taskResourceObservation)
		}
		observationKey := taskObservationKey(concurrency, batchSize)
		observation := stat.Observations[observationKey]
		if observation == nil || observation.Samples <= 0 {
			observation = &taskResourceObservation{
				CPUMs:              cpuMs,
				WallMs:             wallMs,
				GrantedConcurrency: concurrency,
				BatchSize:          batchSize,
			}
		} else {
			observation.CPUMs = taskStatsAlpha*cpuMs + (1-taskStatsAlpha)*observation.CPUMs
			observation.WallMs = taskStatsAlpha*wallMs + (1-taskStatsAlpha)*observation.WallMs
		}
		observation.MaxRSSBytes = max(observation.MaxRSSBytes, result.Execution.MaxRSSBytes)
		observedParallelism := 0.0
		if wallMs > 0 {
			observedParallelism = cpuMs / wallMs
		}
		saturation := min(max(observedParallelism/float64(concurrency), 0), 1)
		if observation.Samples <= 0 {
			observation.Saturation = saturation
		} else {
			observation.Saturation = taskStatsAlpha*saturation + (1-taskStatsAlpha)*observation.Saturation
		}
		observation.Samples++
		observation.Confidence = observationConfidence(observation.Samples)
		observation.UpdatedAt = now
		stat.Observations[observationKey] = observation
		pruneTaskObservations(stat.Observations)
	}
	s.entries[key] = stat
	s.dirty = true
}

// save persists the history atomically (temp file + rename). Best-effort: a
// failure loses nothing but the next run's hints.
func (s *taskStatsStore) save() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(taskStatsFile{Version: taskStatsVersion, Tasks: s.entries}, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tasks-*.json.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	s.dirty = false
}

// applyTaskTuning resolves each planned job's CPU weight and expected work
// before workers start: the configured weight from project config or the
// pipeline step, then the learned boost, expected wall time, and expected CPU
// work and successful unweighted ceiling from run history. Wall time drives
// dispatch; CPU/wall demand, the current weight, and the historical ceiling
// drive allocation.
func applyTaskTuning(planned []*ScheduledJob, stats *taskStatsStore) {
	for _, job := range planned {
		if job == nil {
			continue
		}
		job.CPUBudget = 0
		job.CPUBudgetHistoryEligible = false
		job.CPUBudgetUnweightedCeiling = 0
		job.CPUWeight = resolveConfigCPUWeight(job)
		job.LearnedCPUWeight = 0
		job.ExpectedWallMs = 0
		job.ExpectedCPUWorkMs = 0
		job.HistoricalCPUCeiling = 0
		job.ResourceProfile = taskResourceProfile{}
	}
	if stats == nil {
		return
	}

	stats.mu.Lock()
	defer stats.mu.Unlock()

	// Group historical CPU costs by job name so each task is compared against
	// tasks of the same nature on other projects (every lint vs every lint).
	cpuByName := make(map[string][]float64)
	for key, stat := range stats.entries {
		_, name, ok := strings.Cut(key, "|")
		if !ok || stat.CPUMs <= 0 {
			continue
		}
		cpuByName[name] = append(cpuByName[name], stat.CPUMs)
	}

	for _, job := range planned {
		if job == nil {
			continue
		}
		stat := stats.entries[taskStatKey(job)]
		if stat == nil {
			continue
		}
		job.ExpectedWallMs = int64(stat.WallMs)
		job.ExpectedCPUWorkMs = stat.CPUMs
		job.HistoricalCPUCeiling = max(stat.MaxUnweightedConcurrency, 0)
		job.ResourceProfile = resourceProfileFromStat(stat)

		peers := cpuByName[job.DisplayName()]
		if len(peers) < learnedWeightMinPeers || stat.CPUMs <= 0 {
			continue
		}
		med := median(peers)
		if med <= 0 {
			continue
		}
		boost := stat.CPUMs / med
		if boost <= 1 {
			continue
		}
		job.LearnedCPUWeight = min(boost, learnedWeightCap)
	}
}

func resourceProfileFromStat(stat *taskStat) taskResourceProfile {
	if stat == nil || len(stat.Observations) == 0 {
		return taskResourceProfile{}
	}
	profile := taskResourceProfile{Observations: make([]taskResourceObservation, 0, len(stat.Observations))}
	for _, observation := range stat.Observations {
		if observation == nil {
			continue
		}
		profile.Observations = append(profile.Observations, *observation)
	}
	sort.Slice(profile.Observations, func(i, j int) bool {
		if profile.Observations[i].GrantedConcurrency != profile.Observations[j].GrantedConcurrency {
			return profile.Observations[i].GrantedConcurrency < profile.Observations[j].GrantedConcurrency
		}
		return profile.Observations[i].BatchSize < profile.Observations[j].BatchSize
	})
	return profile
}

// resolveConfigCPUWeight returns the configured weight for a job: the
// project's putnami.json tasks entry (full step name first, then command
// name), falling back to the pipeline step's cpuWeight. Zero means
// unconfigured.
func resolveConfigCPUWeight(job *ScheduledJob) float64 {
	if job.Project != nil && job.Project.Config != nil {
		tasks := job.Project.Config.Tasks
		if len(tasks) > 0 {
			for _, key := range []string{job.DisplayName(), job.CommandName()} {
				if tuning, ok := tasks[key]; ok && tuning.CPUWeight != nil && *tuning.CPUWeight > 0 {
					return *tuning.CPUWeight
				}
			}
		}
	}
	if job.JobDef != nil && job.JobDef.CPUWeight != nil && *job.JobDef.CPUWeight > 0 {
		return *job.JobDef.CPUWeight
	}
	return 0
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// applyCriticalPathTuning stamps every planned job's CriticalPathMs: its own
// expected wall time plus the longest expected-wall chain among its transitive
// scheduling dependents. Dispatch must sort on the chain, not own duration —
// the sub-second links that unblock a session's long pole otherwise sort
// behind hundreds of unrelated medium jobs and delay the whole run by their
// combined queueing (measured ~30s on the repo's own l,t,b --all gate).
//
// Runs after applyTaskTuning so ExpectedWallMs is resolved. A cold store
// leaves every ExpectedWallMs at zero, so every chain is zero and dispatch
// keeps its cold-history key order. Edges whose predecessor is outside the
// plan are ignored, matching the DAG the scheduler actually gates on.
func applyCriticalPathTuning(planned []*ScheduledJob) {
	jobs := make(map[string]*ScheduledJob, len(planned))
	dependents := make(map[string][]string, len(planned))
	for _, job := range planned {
		if job == nil {
			continue
		}
		jobs[job.Key()] = job
	}
	for _, job := range planned {
		if job == nil {
			continue
		}
		for _, pred := range job.SchedulingPredecessors() {
			if _, ok := jobs[pred]; ok {
				dependents[pred] = append(dependents[pred], job.Key())
			}
		}
	}

	// Longest path to a sink, memoized. The planner validates acyclicity, but a
	// malformed plan must degrade to a bounded answer, not a hang: nodes on the
	// current stack are scored zero, which simply breaks the offending cycle.
	memo := make(map[string]int64, len(jobs))
	onStack := make(map[string]bool, len(jobs))
	var chain func(key string) int64
	chain = func(key string) int64 {
		if v, ok := memo[key]; ok {
			return v
		}
		if onStack[key] {
			return 0
		}
		onStack[key] = true
		var longest int64
		for _, dep := range dependents[key] {
			longest = max(longest, chain(dep))
		}
		delete(onStack, key)
		v := jobs[key].ExpectedWallMs + longest
		memo[key] = v
		return v
	}
	for key, job := range jobs {
		job.CriticalPathMs = chain(key)
	}
}

// sortPendingLongestFirst orders the dispatch queue by the expected critical
// path descending — own duration plus the longest chain it unblocks — so both
// historical long poles and the cheap links ahead of them start before the
// short tail. Ties fall back to own expected duration, then key order for
// determinism. Jobs without history sort last in key order — exactly the
// previous behavior for a cold store.
func sortPendingLongestFirst(pending []*ScheduledJob) {
	sort.SliceStable(pending, func(i, j int) bool {
		if pending[i].CriticalPathMs != pending[j].CriticalPathMs {
			return pending[i].CriticalPathMs > pending[j].CriticalPathMs
		}
		if pending[i].ExpectedWallMs != pending[j].ExpectedWallMs {
			return pending[i].ExpectedWallMs > pending[j].ExpectedWallMs
		}
		return pending[i].Key() < pending[j].Key()
	})
}
