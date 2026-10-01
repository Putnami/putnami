package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func statJob(projectID, jobName string) *ScheduledJob {
	return &ScheduledJob{
		Project: &workspace.Project{ID: projectID, Name: projectID, Path: projectID},
		JobDef: &extension.JobDefinition{
			Name:        jobName,
			CommandName: commandFromJobName(jobName),
		},
	}
}

func successResult(wall, cpu time.Duration) *JobResult {
	return &JobResult{Status: "success", Duration: wall, CPUTime: cpu}
}

func TestTaskStats_RecordAndEMA(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	job := statJob("/cli", "lint")

	job.CPUBudget = 3
	job.CPUBudgetHistoryEligible = true
	job.CPUBudgetUnweightedCeiling = 3
	store.record(job, successResult(80*time.Second, 80*time.Second))
	job.CPUBudget = 2
	store.record(job, successResult(40*time.Second, 40*time.Second))

	stat := store.entries[taskStatKey(job)]
	if stat == nil {
		t.Fatal("expected a recorded stat")
	}
	if stat.Samples != 2 {
		t.Fatalf("samples = %d, want 2", stat.Samples)
	}
	// EMA with α=0.5: 0.5*40000 + 0.5*80000 = 60000.
	if stat.WallMs != 60000 || stat.CPUMs != 60000 {
		t.Fatalf("ema = wall %.0f cpu %.0f, want 60000/60000", stat.WallMs, stat.CPUMs)
	}
	if stat.MaxUnweightedConcurrency != 3 {
		t.Fatalf("max unweighted concurrency = %d, want retained ceiling 3", stat.MaxUnweightedConcurrency)
	}
}

func TestTaskStats_IgnoresCacheHitsFailuresAndCancels(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	job := statJob("/cli", "lint")

	store.record(job, &JobResult{Status: "success", Duration: time.Second, CacheHit: true})
	store.record(job, &JobResult{Status: "failed", Duration: time.Second})
	store.record(job, &JobResult{Status: "canceled", Duration: time.Second})
	store.record(job, &JobResult{Status: "success"}) // zero duration

	if len(store.entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(store.entries))
	}
}

func TestTaskStats_SaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := loadTaskStats(root)
	job := statJob("/cli", "test")
	job.CPUBudget = 8
	job.CPUBudgetHistoryEligible = true
	job.CPUBudgetUnweightedCeiling = 8
	result := successResult(13*time.Second, 90*time.Second)
	result.Execution = &Execution{Concurrency: 8, MaxRSSBytes: 900 * 1024 * 1024}
	store.record(job, result)
	store.save()

	reloaded := loadTaskStats(root)
	stat := reloaded.entries[taskStatKey(job)]
	if stat == nil {
		t.Fatal("expected stat to survive a save/load round trip")
	}
	if stat.WallMs != 13000 || stat.CPUMs != 90000 || stat.MaxUnweightedConcurrency != 8 || stat.Samples != 1 {
		t.Fatalf("reloaded stat = %+v", stat)
	}
	observation := stat.Observations["8x1"]
	if observation == nil || observation.GrantedConcurrency != 8 || observation.MaxRSSBytes != 900*1024*1024 ||
		observation.Samples != 1 || observation.Confidence != 0.25 || observation.Saturation < 0.8 || observation.Saturation > 1 {
		t.Fatalf("reloaded concurrency-conditioned observation = %+v", observation)
	}
}

func TestTaskStats_RecordsRSSWithObservationConcurrency(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	job := statJob("/cli", "test")
	first := successResult(10*time.Second, 15*time.Second)
	first.Execution = &Execution{Concurrency: 8, MaxRSSBytes: 700 * 1024 * 1024}
	store.record(job, first)
	second := successResult(8*time.Second, 16*time.Second)
	second.Execution = &Execution{Concurrency: 4, MaxRSSBytes: 500 * 1024 * 1024}
	store.record(job, second)

	stat := store.entries[taskStatKey(job)]
	if len(stat.Observations) != 2 {
		t.Fatalf("observations = %+v, want separate 4-core and 8-core profiles", stat.Observations)
	}
	if got := stat.Observations["8x1"]; got == nil || got.MaxRSSBytes != 700*1024*1024 || got.GrantedConcurrency != 8 {
		t.Fatalf("8-core observation = %+v", got)
	}
	if got := stat.Observations["4x1"]; got == nil || got.MaxRSSBytes != 500*1024*1024 || got.GrantedConcurrency != 4 {
		t.Fatalf("4-core observation = %+v", got)
	}
}

func TestTaskStats_RecordsPhysicalBatchContext(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	a := statJob("/a", "test")
	b := statJob("/b", "test")
	work := []taskWork{{job: a}, {job: b}}
	aggregate := &JobResult{
		Status:    "success",
		Duration:  2 * time.Second,
		CPUTime:   3 * time.Second,
		Execution: &Execution{Concurrency: 4, MaxRSSBytes: 1000 * 1024 * 1024},
	}
	results := map[string]*JobResult{
		a.Key(): {Status: "success"},
		b.Key(): {Status: "success"},
	}
	attributeSharedExecution(work, results, aggregate)
	for _, job := range []*ScheduledJob{a, b} {
		store.record(job, results[job.Key()])
		observation := store.entries[taskStatKey(job)].Observations["4x2"]
		if observation == nil || observation.BatchSize != 2 || observation.MaxRSSBytes != 1000*1024*1024 {
			t.Fatalf("%s batch observation = %+v, want the shared physical RSS with batchSize=2", job.Key(), observation)
		}
	}
}

func TestTaskResourceProfileNearestObservationUsesStableConservativeTies(t *testing.T) {
	t.Parallel()
	profile := taskResourceProfile{Observations: []taskResourceObservation{
		{GrantedConcurrency: 2, BatchSize: 3, MaxRSSBytes: 200},
		{GrantedConcurrency: 6, BatchSize: 1, MaxRSSBytes: 600},
	}}
	observation, ok := profile.NearestObservation(4, 2)
	if !ok || observation.GrantedConcurrency != 6 {
		t.Fatalf("equal-distance observation = %+v, want higher concurrency", observation)
	}

	profile = taskResourceProfile{Observations: []taskResourceObservation{
		{GrantedConcurrency: 4, BatchSize: 1, MaxRSSBytes: 400},
		{GrantedConcurrency: 4, BatchSize: 3, MaxRSSBytes: 800},
	}}
	observation, ok = profile.NearestObservation(4, 2)
	if !ok || observation.BatchSize != 3 {
		t.Fatalf("equal-distance batch observation = %+v, want larger batch", observation)
	}
}

// RFC3339Nano drops trailing zeros from the fractional second, so ".15Z" sorts
// BEFORE ".1Z" as text while being the later instant. Eviction must compare
// instants, or a same-second write drops the freshest observation it just made.
func TestPruneTaskObservations_EvictsOldestInstantNotSmallestString(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	observations := map[string]*taskResourceObservation{}
	for i := range maxTaskResourceObservations + 1 {
		// 100ms renders as ".1Z"; 150ms as ".15Z", which is later but sorts first.
		offset := 150 * time.Millisecond
		if i == 0 {
			offset = 100 * time.Millisecond
		}
		key := taskObservationKey(i+1, 1)
		observations[key] = &taskResourceObservation{
			GrantedConcurrency: i + 1,
			BatchSize:          1,
			Samples:            1,
			UpdatedAt:          base.Add(offset).Format(time.RFC3339Nano),
		}
	}

	pruneTaskObservations(observations)

	if len(observations) != maxTaskResourceObservations {
		t.Fatalf("retained %d observations, want %d", len(observations), maxTaskResourceObservations)
	}
	if _, kept := observations[taskObservationKey(1, 1)]; kept {
		t.Fatal("eviction kept the oldest instant and dropped a newer one: timestamps compared as text")
	}
}

func TestPruneTaskObservations_EvictsUnstampedObservationsFirst(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	observations := map[string]*taskResourceObservation{
		taskObservationKey(1, 1): {GrantedConcurrency: 1, BatchSize: 1, Samples: 1},
	}
	for i := 1; i <= maxTaskResourceObservations; i++ {
		observations[taskObservationKey(i+1, 1)] = &taskResourceObservation{
			GrantedConcurrency: i + 1,
			BatchSize:          1,
			Samples:            1,
			UpdatedAt:          stamp,
		}
	}

	pruneTaskObservations(observations)

	if _, kept := observations[taskObservationKey(1, 1)]; kept {
		t.Fatal("an observation with no timestamp survived eviction over stamped peers")
	}
}

func TestTaskStats_DoesNotRetainColdFallbackAsProvenConcurrency(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	job := statJob("/cli", "test")
	scheduler := &Scheduler{cpuAlloc: newCPUAllocator(10, "")}
	scheduler.acquireGroupCPUBudget([]taskWork{{job: job}})
	if job.CPUBudget != 10 || job.CPUBudgetHistoryEligible {
		t.Fatalf("cold grant = %d eligible=%v, want whole-machine fallback excluded from history", job.CPUBudget, job.CPUBudgetHistoryEligible)
	}

	result := successResult(88*time.Second, 65*time.Second)
	result.Execution = &Execution{Concurrency: 10}
	store.record(job, result)
	if got := store.entries[taskStatKey(job)].MaxUnweightedConcurrency; got != 0 {
		t.Fatalf("cold fallback persisted as max concurrency %d", got)
	}

	applyTaskTuning([]*ScheduledJob{job}, store)
	job.LearnedCPUWeight = 8
	scheduler.acquireGroupCPUBudget([]taskWork{{job: job}})
	if job.CPUBudget != 8 || !job.CPUBudgetHistoryEligible {
		t.Fatalf("history-backed long-pole grant = %d eligible=%v, want stable 8 and persistable", job.CPUBudget, job.CPUBudgetHistoryEligible)
	}
}

func TestTaskStats_ConfigWeightChangeDoesNotRatchetHistoricalDemand(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	weight := 4.0
	job := statJob("/cli", "lint")
	job.Project.Config = &wsproto.ProjectConfig{
		Tasks: map[string]wsproto.ProjectTaskTuning{"lint": {CPUWeight: &weight}},
	}
	store.entries[taskStatKey(job)] = &taskStat{
		CPUMs:   1_000,
		WallMs:  1_000,
		Samples: 1,
	}
	scheduler := &Scheduler{cpuAlloc: newCPUAllocator(8, "")}

	applyTaskTuning([]*ScheduledJob{job}, store)
	scheduler.acquireGroupCPUBudget([]taskWork{{job: job}})
	if job.CPUBudget != 4 {
		t.Fatalf("weighted budget = %d, want 4", job.CPUBudget)
	}
	store.record(job, successResult(time.Second, time.Second))
	if got := store.entries[taskStatKey(job)].MaxUnweightedConcurrency; got != 1 {
		t.Fatalf("persisted unweighted ceiling = %d, want 1", got)
	}

	weight = 1
	applyTaskTuning([]*ScheduledJob{job}, store)
	scheduler.acquireGroupCPUBudget([]taskWork{{job: job}})
	if job.CPUBudget != 1 {
		t.Fatalf("budget after weight 4 -> 1 = %d, want unweighted demand 1", job.CPUBudget)
	}
}

func TestTaskStats_LearnedWeightChangeDoesNotRatchetHistoricalDemand(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	job := statJob("/cli", "lint")
	store.entries[taskStatKey(job)] = &taskStat{CPUMs: 4_000, WallMs: 4_000, Samples: 1}
	for _, key := range []string{"/a|lint", "/b|lint"} {
		store.entries[key] = &taskStat{CPUMs: 1_000, WallMs: 1_000, Samples: 1}
	}
	scheduler := &Scheduler{cpuAlloc: newCPUAllocator(8, "")}

	applyTaskTuning([]*ScheduledJob{job}, store)
	if job.LearnedCPUWeight != 4 {
		t.Fatalf("initial learned weight = %v, want 4", job.LearnedCPUWeight)
	}
	scheduler.acquireGroupCPUBudget([]taskWork{{job: job}})
	if job.CPUBudget != 4 {
		t.Fatalf("learned-weight budget = %d, want 4", job.CPUBudget)
	}
	store.record(job, successResult(4*time.Second, 4*time.Second))

	store.entries["/a|lint"].CPUMs = 4_000
	store.entries["/b|lint"].CPUMs = 4_000
	applyTaskTuning([]*ScheduledJob{job}, store)
	if job.LearnedCPUWeight != 0 {
		t.Fatalf("adapted learned weight = %v, want neutral", job.LearnedCPUWeight)
	}
	scheduler.acquireGroupCPUBudget([]taskWork{{job: job}})
	if job.CPUBudget != 1 {
		t.Fatalf("budget after learned weight 4 -> 1 = %d, want unweighted demand 1", job.CPUBudget)
	}
}

func TestTaskStats_LoadMigratesV1WithoutAmbiguousWeightedFloor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := taskStatsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"tasks":{"/cli|lint":{"cpuMs":1000,"wallMs":1000,"maxConcurrency":4,"samples":2}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	store := loadTaskStats(root)
	stat := store.entries["/cli|lint"]
	if stat == nil || stat.CPUMs != 1_000 || stat.WallMs != 1_000 || stat.Samples != 2 {
		t.Fatalf("migrated stat = %+v, want measured history retained", stat)
	}
	if stat.MaxUnweightedConcurrency != 0 {
		t.Fatalf("migrated unweighted ceiling = %d, want ambiguous v1 weighted floor dropped", stat.MaxUnweightedConcurrency)
	}
	store.save()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "maxConcurrency") {
		t.Fatalf("migrated stats retain legacy weighted floor: %s", data)
	}
	var migrated taskStatsFile
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Version != taskStatsVersion {
		t.Fatalf("migrated version = %d, want %d", migrated.Version, taskStatsVersion)
	}
}

func TestTaskStats_LoadMigratesV2WithoutInventingResourceContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := taskStatsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	v2 := `{"version":2,"tasks":{"/cli|test":{"cpuMs":65000,"wallMs":88000,"maxUnweightedConcurrency":8,"samples":3}}}`
	if err := os.WriteFile(path, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}

	store := loadTaskStats(root)
	stat := store.entries["/cli|test"]
	if stat == nil || stat.CPUMs != 65000 || stat.WallMs != 88000 ||
		stat.MaxUnweightedConcurrency != 8 || stat.Samples != 3 {
		t.Fatalf("migrated v2 stat = %+v, want usable CPU/wall/ceiling evidence", stat)
	}
	if len(stat.Observations) != 0 {
		t.Fatalf("v2 migration fabricated RSS/concurrency observations: %+v", stat.Observations)
	}
	store.save()
	var migrated taskStatsFile
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Version != taskStatsVersion {
		t.Fatalf("migrated version = %d, want %d", migrated.Version, taskStatsVersion)
	}
}

func TestTaskStats_LoadSanitizesCorruptResourceObservations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := taskStatsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(taskStatsFile{Version: taskStatsVersion, Tasks: map[string]*taskStat{
		"/cli|test": {
			WallMs: 1000, CPUMs: 1500, Samples: 1,
			Observations: map[string]*taskResourceObservation{
				"bad": {WallMs: 1000, CPUMs: 1000, MaxRSSBytes: -1, GrantedConcurrency: 8, Samples: 1, Confidence: 1},
				"4":   {WallMs: 1000, CPUMs: 1500, MaxRSSBytes: 500, GrantedConcurrency: 4, Samples: 1, Confidence: 99, Saturation: 4},
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	observations := loadTaskStats(root).entries["/cli|test"].Observations
	if len(observations) != 1 {
		t.Fatalf("sanitized observations = %+v, want only valid evidence", observations)
	}
	if got := observations["4x1"]; got == nil || got.Confidence != 0.25 || got.Saturation != 1 || got.BatchSize != 1 {
		t.Fatalf("sanitized observation = %+v, want recomputed confidence and clamped saturation", got)
	}
}

func TestTaskStats_LoadToleratesCorruptAndWrongVersion(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := taskStatsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if store := loadTaskStats(root); len(store.entries) != 0 {
		t.Fatalf("corrupt file should load empty, got %d entries", len(store.entries))
	}

	wrongVersion, _ := json.Marshal(taskStatsFile{Version: 99, Tasks: map[string]*taskStat{
		"/cli|lint": {WallMs: 1, CPUMs: 1, Samples: 1},
	}})
	if err := os.WriteFile(path, wrongVersion, 0o644); err != nil {
		t.Fatal(err)
	}
	if store := loadTaskStats(root); len(store.entries) != 0 {
		t.Fatalf("wrong version should load empty, got %d entries", len(store.entries))
	}
}

func TestTaskStats_LoadSanitizesNegativeUnweightedConcurrency(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := taskStatsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(taskStatsFile{Version: taskStatsVersion, Tasks: map[string]*taskStat{
		"/cli|test": {WallMs: 1, CPUMs: 1, MaxUnweightedConcurrency: -99, Samples: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if got := loadTaskStats(root).entries["/cli|test"].MaxUnweightedConcurrency; got != 0 {
		t.Fatalf("loaded max unweighted concurrency = %d, want invalid value sanitized", got)
	}
}

func TestTaskStats_SaveSkipsWhenClean(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := loadTaskStats(root)
	store.save()
	if _, err := os.Stat(taskStatsPath(root)); !os.IsNotExist(err) {
		t.Fatal("clean store should not write a stats file")
	}
}

func TestApplyTaskTuning_LearnedBoostAgainstPeers(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	// Five projects run "lint"; one is wildly more expensive than the others.
	cheap := []string{"/a", "/b", "/c", "/d"}
	for _, id := range cheap {
		store.record(statJob(id, "lint"), successResult(2*time.Second, 2*time.Second))
	}
	expensive := statJob("/cli", "lint")
	expensive.CPUBudget = 6
	expensive.CPUBudgetHistoryEligible = true
	expensive.CPUBudgetUnweightedCeiling = 6
	store.record(expensive, successResult(80*time.Second, 80*time.Second))

	planned := []*ScheduledJob{expensive, statJob("/a", "lint")}
	applyTaskTuning(planned, store)

	// Median cpu across {2000 ×4, 80000} = 2000 → raw boost 40, capped at 8.
	if planned[0].LearnedCPUWeight != learnedWeightCap {
		t.Fatalf("learned weight = %v, want cap %v", planned[0].LearnedCPUWeight, learnedWeightCap)
	}
	if planned[0].ExpectedWallMs != 80000 {
		t.Fatalf("expected wall = %d, want 80000", planned[0].ExpectedWallMs)
	}
	if planned[0].ExpectedCPUWorkMs != 80000 {
		t.Fatalf("expected CPU work = %v, want 80000", planned[0].ExpectedCPUWorkMs)
	}
	if planned[0].HistoricalCPUCeiling != 6 {
		t.Fatalf("historical CPU ceiling = %d, want 6", planned[0].HistoricalCPUCeiling)
	}
	// A median-priced peer gets no boost.
	if planned[1].LearnedCPUWeight != 0 {
		t.Fatalf("peer learned weight = %v, want 0 (no boost)", planned[1].LearnedCPUWeight)
	}
}

func TestApplyTaskTuning_NoBoostWithoutPeersOrHistory(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	solo := statJob("/cli", "lint")
	store.record(solo, successResult(80*time.Second, 80*time.Second))

	fresh := statJob("/new", "test")
	planned := []*ScheduledJob{solo, fresh}
	applyTaskTuning(planned, store)

	if solo.LearnedCPUWeight != 0 {
		t.Fatalf("learned weight = %v, want 0 with a single-project population", solo.LearnedCPUWeight)
	}
	if fresh.LearnedCPUWeight != 0 || fresh.ExpectedWallMs != 0 || fresh.ExpectedCPUWorkMs != 0 || fresh.HistoricalCPUCeiling != 0 {
		t.Fatalf("job without history should stay untuned, got %+v", fresh)
	}
}

func TestApplyTaskTuning_CPUWorkExpectationIsIndependentOfBudgetShapedWall(t *testing.T) {
	t.Parallel()
	store := loadTaskStats(t.TempDir())
	job := statJob("/cli", "test")

	// Simulate the same amount of CPU work first under a small budget and then
	// under a larger one. The faster wall changes dispatch history, but must not
	// lower the size signal that the next CPU grant consumes.
	store.record(job, successResult(120*time.Second, 60*time.Second))
	store.record(job, successResult(30*time.Second, 60*time.Second))
	applyTaskTuning([]*ScheduledJob{job}, store)

	if job.ExpectedWallMs != 75000 {
		t.Fatalf("expected wall = %d, want budget-shaped EMA 75000", job.ExpectedWallMs)
	}
	if job.ExpectedCPUWorkMs != 60000 {
		t.Fatalf("expected CPU work = %v, want policy-independent EMA 60000", job.ExpectedCPUWorkMs)
	}
}

func TestApplyTaskTuning_NilStoreStillResolvesConfigWeights(t *testing.T) {
	t.Parallel()
	weight := 4.0
	job := statJob("/cli", "lint")
	job.Project.Config = &wsproto.ProjectConfig{
		Tasks: map[string]wsproto.ProjectTaskTuning{"lint": {CPUWeight: &weight}},
	}
	applyTaskTuning([]*ScheduledJob{job}, nil)
	if job.CPUWeight != 4 {
		t.Fatalf("config weight = %v, want 4", job.CPUWeight)
	}
}

func TestApplyTaskTuning_ResetsRunScopedFields(t *testing.T) {
	t.Parallel()
	weight := 4.0
	job := statJob("/cli", "lint")
	job.CPUBudget = 10
	job.CPUBudgetHistoryEligible = true
	job.CPUBudgetUnweightedCeiling = 5
	job.CPUWeight = 99
	job.LearnedCPUWeight = 8
	job.ExpectedWallMs = 12345
	job.ExpectedCPUWorkMs = 54321
	job.HistoricalCPUCeiling = 7
	job.Project.Config = &wsproto.ProjectConfig{
		Tasks: map[string]wsproto.ProjectTaskTuning{"lint": {CPUWeight: &weight}},
	}

	applyTaskTuning([]*ScheduledJob{job}, nil)

	if job.CPUBudget != 0 {
		t.Fatalf("cpu budget = %d, want reset to 0 before execution", job.CPUBudget)
	}
	if job.CPUBudgetHistoryEligible {
		t.Fatal("CPU budget history eligibility was not reset")
	}
	if job.CPUBudgetUnweightedCeiling != 0 {
		t.Fatalf("CPU budget unweighted ceiling = %d, want reset to 0", job.CPUBudgetUnweightedCeiling)
	}
	if job.CPUWeight != 4 {
		t.Fatalf("config weight = %v, want recomputed weight 4", job.CPUWeight)
	}
	if job.LearnedCPUWeight != 0 {
		t.Fatalf("learned weight = %v, want reset to 0 without history", job.LearnedCPUWeight)
	}
	if job.ExpectedWallMs != 0 {
		t.Fatalf("expected wall = %d, want reset to 0 without history", job.ExpectedWallMs)
	}
	if job.ExpectedCPUWorkMs != 0 {
		t.Fatalf("expected CPU work = %v, want reset to 0 without history", job.ExpectedCPUWorkMs)
	}
	if job.HistoricalCPUCeiling != 0 {
		t.Fatalf("historical CPU ceiling = %d, want reset to 0 without history", job.HistoricalCPUCeiling)
	}
}

func TestResolveConfigCPUWeight_Precedence(t *testing.T) {
	t.Parallel()
	stepWeight := 2.0
	commandWeight := 3.0
	fullNameWeight := 5.0

	job := statJob("/cli", "lint~staticcheck")
	job.JobDef.CPUWeight = &stepWeight

	// Pipeline step weight applies when the project says nothing.
	if got := resolveConfigCPUWeight(job); got != 2 {
		t.Fatalf("weight = %v, want step weight 2", got)
	}

	// Command-name entry overrides the step.
	job.Project.Config = &wsproto.ProjectConfig{
		Tasks: map[string]wsproto.ProjectTaskTuning{"lint": {CPUWeight: &commandWeight}},
	}
	if got := resolveConfigCPUWeight(job); got != 3 {
		t.Fatalf("weight = %v, want command override 3", got)
	}

	// Full step-name entry beats the command-name entry.
	job.Project.Config.Tasks["lint~staticcheck"] = wsproto.ProjectTaskTuning{CPUWeight: &fullNameWeight}
	if got := resolveConfigCPUWeight(job); got != 5 {
		t.Fatalf("weight = %v, want step-name override 5", got)
	}

	// Invalid values are ignored.
	zero := 0.0
	job.Project.Config.Tasks = map[string]wsproto.ProjectTaskTuning{"lint": {CPUWeight: &zero}}
	job.JobDef.CPUWeight = nil
	if got := resolveConfigCPUWeight(job); got != 0 {
		t.Fatalf("weight = %v, want 0 (unconfigured)", got)
	}
}

func TestEffectiveCPUWeight_CombinesConfigAndLearned(t *testing.T) {
	t.Parallel()
	job := statJob("/cli", "lint")
	if got := job.EffectiveCPUWeight(); got != 1 {
		t.Fatalf("default weight = %v, want 1", got)
	}
	job.CPUWeight = 2
	job.LearnedCPUWeight = 3
	if got := job.EffectiveCPUWeight(); got != 6 {
		t.Fatalf("combined weight = %v, want 6", got)
	}
	job.LearnedCPUWeight = 0.5 // learned values <= 1 never penalize
	if got := job.EffectiveCPUWeight(); got != 2 {
		t.Fatalf("weight = %v, want 2", got)
	}
}

// The measured failure mode this guards: the sub-second config-merge that
// transitively unblocks a 90s test job must outrank an unrelated 5s job, or
// the long pole starts only after the whole medium tail has queued past it.
func TestApplyCriticalPathTuning_ChainOutranksOwnDuration(t *testing.T) {
	configMerge := statJob("/cli", "build~config-merge")
	configMerge.ExpectedWallMs = 40
	compile := statJob("/cli", "build~compile")
	compile.ExpectedWallMs = 800
	compile.DependsOn = []string{configMerge.Key()}
	longTest := statJob("/cli", "test~test")
	longTest.ExpectedWallMs = 90000
	longTest.DependsOn = []string{compile.Key()}
	medium := statJob("/other", "test~test")
	medium.ExpectedWallMs = 5000

	applyCriticalPathTuning([]*ScheduledJob{configMerge, compile, longTest, medium, nil})

	for _, tc := range []struct {
		job  *ScheduledJob
		want int64
	}{
		{longTest, 90000},
		{compile, 90800},
		{configMerge, 90840},
		{medium, 5000},
	} {
		if tc.job.CriticalPathMs != tc.want {
			t.Fatalf("%s CriticalPathMs = %d, want %d", tc.job.Key(), tc.job.CriticalPathMs, tc.want)
		}
	}

	pending := []*ScheduledJob{medium, configMerge}
	sortPendingLongestFirst(pending)
	if pending[0] != configMerge {
		t.Fatalf("dispatch order = %v, want the chain link %s first", keysOf(pending), configMerge.Key())
	}
}

func TestApplyCriticalPathTuning_SerializeEdgesAndForeignPredecessors(t *testing.T) {
	// A serialize successor gates on this job exactly like a functional one, so
	// its chain adds the same dispatch urgency.
	writer := statJob("/cli", "lint~golangci-lint")
	writer.ExpectedWallMs = 100
	reader := statJob("/cli", "test~test")
	reader.ExpectedWallMs = 60000
	reader.SerializeAfter = []string{writer.Key()}
	// A predecessor outside the plan (cached away) must be ignored, not scored.
	orphan := statJob("/cli", "build~infra")
	orphan.ExpectedWallMs = 200
	orphan.DependsOn = []string{"/gone:build~describe"}

	applyCriticalPathTuning([]*ScheduledJob{writer, reader, orphan})

	if writer.CriticalPathMs != 60100 {
		t.Fatalf("serialize predecessor CriticalPathMs = %d, want 60100", writer.CriticalPathMs)
	}
	if orphan.CriticalPathMs != 200 {
		t.Fatalf("orphan CriticalPathMs = %d, want its own 200", orphan.CriticalPathMs)
	}
}

func TestApplyCriticalPathTuning_MalformedCycleStaysBounded(t *testing.T) {
	// The planner validates acyclicity; a malformed plan must still terminate
	// with each node scoring at least its own duration.
	a := statJob("/a", "build~compile")
	a.ExpectedWallMs = 10
	b := statJob("/b", "build~compile")
	b.ExpectedWallMs = 20
	a.DependsOn = []string{b.Key()}
	b.DependsOn = []string{a.Key()}

	applyCriticalPathTuning([]*ScheduledJob{a, b})

	if a.CriticalPathMs < 10 || b.CriticalPathMs < 20 {
		t.Fatalf("cycle scores = %d/%d, want at least own durations 10/20", a.CriticalPathMs, b.CriticalPathMs)
	}
}

func TestSortPendingLongestFirst(t *testing.T) {
	short := statJob("/a", "lint")
	short.ExpectedWallMs = 100
	long := statJob("/b", "test")
	long.ExpectedWallMs = 90000
	unknownB := statJob("/y", "build")
	unknownA := statJob("/x", "build")

	pending := []*ScheduledJob{unknownB, short, unknownA, long}
	sortPendingLongestFirst(pending)

	wantOrder := []string{long.Key(), short.Key(), unknownA.Key(), unknownB.Key()}
	for i, want := range wantOrder {
		if pending[i].Key() != want {
			t.Fatalf("pending[%d] = %s, want %s (full order: %v)", i, pending[i].Key(), want, keysOf(pending))
		}
	}
}

func keysOf(jobs []*ScheduledJob) []string {
	keys := make([]string, 0, len(jobs))
	for _, j := range jobs {
		keys = append(keys, j.Key())
	}
	return keys
}
