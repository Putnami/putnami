package jobs

import (
	"os"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestResolveMaxWorkers_ExplicitNumericWins(t *testing.T) {
	planned := makePlannedJobs(20, "build~generate")
	got := workersFor(SchedulerConfig{
		MaxParallel:     7,
		MaxParallelMode: parallelModeEco,
	}, planned, 10, 0)
	if got != 7 {
		t.Fatalf("workers = %d, want 7", got)
	}
}

func TestResolveMaxWorkers_DefaultAutoUsesLogicalCPUHeadroom(t *testing.T) {
	planned := makePlannedJobs(100, "build~generate")
	got := workersFor(SchedulerConfig{}, planned, 10, 0)
	if got != 30 {
		t.Fatalf("workers = %d, want 30 for light auto plan", got)
	}
}

func TestResolveMaxWorkers_NormalizesModeAndCPU(t *testing.T) {
	planned := makePlannedJobs(100, "build~generate")
	got := workersFor(SchedulerConfig{MaxParallelMode: " unknown "}, planned, 0, 0)
	if got != 3 {
		t.Fatalf("workers = %d, want auto mode on one logical CPU", got)
	}
}

func TestResolveMaxWorkers_Modes(t *testing.T) {
	planned := makePlannedJobs(100, "build~generate")
	tests := []struct {
		mode string
		want int
	}{
		{parallelModeEco, 8},
		{parallelModeAuto, 30},
		{parallelModeMax, 30},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			got := workersFor(SchedulerConfig{MaxParallelMode: tt.mode}, planned, 10, 0)
			if got != tt.want {
				t.Fatalf("workers = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveMaxWorkers_HeavyPlanBacksOffAuto(t *testing.T) {
	planned := makePlannedJobs(100, "test~test")
	got := workersFor(SchedulerConfig{MaxParallelMode: parallelModeAuto}, planned, 10, 0)
	if got != 15 {
		t.Fatalf("workers = %d, want 15", got)
	}
}

func TestResolveMaxWorkers_HeavyPlanBacksOffMax(t *testing.T) {
	planned := makePlannedJobs(100, "test~test")
	got := workersFor(SchedulerConfig{MaxParallelMode: parallelModeMax}, planned, 10, 0)
	if got != 20 {
		t.Fatalf("workers = %d, want 20", got)
	}
}

func TestResolveMaxWorkers_MixedPlanKeepsAutoBase(t *testing.T) {
	planned := append(makePlannedJobs(50, "test~test"), makePlannedJobs(50, "build~generate")...)
	got := workersFor(SchedulerConfig{MaxParallelMode: parallelModeAuto}, planned, 10, 0)
	if got != 20 {
		t.Fatalf("workers = %d, want base auto workers for mixed plan", got)
	}
}

func TestResolveMaxWorkers_MixedHeavyPlanBacksOffMax(t *testing.T) {
	planned := append(makePlannedJobs(30, "test~test"), makePlannedJobs(70, "build~generate")...)
	got := workersFor(SchedulerConfig{MaxParallelMode: parallelModeMax}, planned, 10, 0)
	if got != 20 {
		t.Fatalf("workers = %d, want bounded max workers for mixed heavy plan", got)
	}
}

func TestResolveMaxWorkers_MemoryCap(t *testing.T) {
	planned := makePlannedJobs(100, "lint~golangci-lint")
	got := workersFor(SchedulerConfig{MaxParallelMode: parallelModeAuto}, planned, 10, 4*1024*1024*1024)
	if got != 2 {
		t.Fatalf("workers = %d, want memory cap of 2 after stable headroom", got)
	}
}

func TestResolveMaxWorkers_MemoryCapByMode(t *testing.T) {
	planned := makePlannedJobs(100, "build~generate")
	tests := []struct {
		mode string
		want int
	}{
		{parallelModeEco, 3},
		{parallelModeAuto, 6},
		{parallelModeMax, 8},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			got := workersFor(SchedulerConfig{MaxParallelMode: tt.mode}, planned, 16, 4*1024*1024*1024)
			if got != tt.want {
				t.Fatalf("workers = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveMaxWorkers_ClampsToPlannedJobs(t *testing.T) {
	planned := makePlannedJobs(2, "build~generate")
	got := workersFor(SchedulerConfig{MaxParallelMode: parallelModeMax}, planned, 10, 0)
	if got != 2 {
		t.Fatalf("workers = %d, want 2", got)
	}
}

func TestResolveMaxWorkers_EmptyPlanStillReturnsWorker(t *testing.T) {
	got := workersFor(SchedulerConfig{MaxParallelMode: parallelModeEco}, nil, 10, 0)
	if got != 1 {
		t.Fatalf("workers = %d, want 1", got)
	}
}

func TestApplyCPUBudget(t *testing.T) {
	goJob := func(budget int) *ScheduledJob {
		return &ScheduledJob{
			Extension: &extension.ExtensionDescription{Name: goExtensionName},
			JobDef:    &extension.JobDefinition{Name: "test~test"},
			CPUBudget: budget,
		}
	}

	t.Run("sets GOMAXPROCS for go jobs", func(t *testing.T) {
		t.Setenv("GOMAXPROCS", "") // ensure unset path
		os.Unsetenv("GOMAXPROCS")
		env := applyCPUBudget([]string{"PATH=/bin"}, goJob(3))
		if !slices.Contains(env, "GOMAXPROCS=3") {
			t.Fatalf("expected GOMAXPROCS=3 in env, got %v", env)
		}
		if !slices.Contains(env, "PUTNAMI_CPU_BUDGET=3") {
			t.Fatalf("expected PUTNAMI_CPU_BUDGET=3 in env, got %v", env)
		}
	})

	t.Run("no budget is a no-op", func(t *testing.T) {
		os.Unsetenv("GOMAXPROCS")
		env := applyCPUBudget([]string{"PATH=/bin"}, goJob(0))
		if hasEnvPrefix(env, "GOMAXPROCS=") {
			t.Fatalf("did not expect GOMAXPROCS, got %v", env)
		}
	})

	t.Run("non-go job gets only the generic budget", func(t *testing.T) {
		os.Unsetenv("GOMAXPROCS")
		job := &ScheduledJob{
			Extension: &extension.ExtensionDescription{Name: "@putnami/typescript"},
			JobDef:    &extension.JobDefinition{Name: "test~test"},
			CPUBudget: 3,
		}
		env := applyCPUBudget([]string{"PATH=/bin"}, job)
		if hasEnvPrefix(env, "GOMAXPROCS=") {
			t.Fatalf("did not expect GOMAXPROCS for non-go job, got %v", env)
		}
		if !slices.Contains(env, "PUTNAMI_CPU_BUDGET=3") {
			t.Fatalf("expected PUTNAMI_CPU_BUDGET=3 in env, got %v", env)
		}
	})

	t.Run("existing GOMAXPROCS wins", func(t *testing.T) {
		t.Setenv("GOMAXPROCS", "2")
		env := applyCPUBudget([]string{"PATH=/bin"}, goJob(3))
		if hasEnvPrefix(env, "GOMAXPROCS=") {
			t.Fatalf("should not override operator-set GOMAXPROCS, got %v", env)
		}
	})

	t.Run("job env GOMAXPROCS wins", func(t *testing.T) {
		os.Unsetenv("GOMAXPROCS")
		env := applyCPUBudget([]string{"PATH=/bin", "GOMAXPROCS=2"}, goJob(3))
		if !slices.Contains(env, "GOMAXPROCS=2") {
			t.Fatalf("expected existing job env GOMAXPROCS=2 to be preserved, got %v", env)
		}
		if slices.Contains(env, "GOMAXPROCS=3") {
			t.Fatalf("should not append scheduler budget when job env sets GOMAXPROCS, got %v", env)
		}
	})

	t.Run("inherited PUTNAMI_CPU_BUDGET from an outer putnami is overridden", func(t *testing.T) {
		os.Unsetenv("GOMAXPROCS")
		env := applyCPUBudget([]string{"PATH=/bin", "PUTNAMI_CPU_BUDGET=10"}, goJob(3))
		// os/exec keeps the last duplicate, so the scheduler's own value must
		// come after the inherited one.
		last := ""
		for _, e := range env {
			if strings.HasPrefix(e, "PUTNAMI_CPU_BUDGET=") {
				last = e
			}
		}
		if last != "PUTNAMI_CPU_BUDGET=3" {
			t.Fatalf("expected scheduler budget to win over inherited value, got %v", env)
		}
	})
}

func hasEnvPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func TestHeavyJobPermille(t *testing.T) {
	planned := append(makePlannedJobs(2, "test~test"), makePlannedJobs(3, "build~generate")...)
	if got := heavyJobPermille(planned); got != 400 {
		t.Fatalf("heavy permille = %d, want 400", got)
	}
	if got := heavyJobPermille(nil); got != 0 {
		t.Fatalf("empty heavy permille = %d, want 0", got)
	}
}

// TestIsHeavyJob covers the two declaration routes that survive: the
// per-step `heavy` override and the command's heavy trait.
func TestIsHeavyJob(t *testing.T) {
	heavyStep := func(name string, heavy bool) *ScheduledJob {
		job := makePlannedJobs(1, name)[0]
		job.JobDef.Heavy = &heavy
		return job
	}
	traitJob := func(name string, heavy bool) *ScheduledJob {
		job := makePlannedJobs(1, name)[0]
		job.JobDef.Heavy = nil
		job.JobDef.Traits = extensionproto.CommandTraits{Heavy: heavy}
		return job
	}

	tests := []struct {
		name string
		job  *ScheduledJob
		want bool
	}{
		{name: "declared heavy step", job: heavyStep("test~test", true), want: true},
		{name: "declared light step", job: heavyStep("test~test", false), want: false},
		{name: "command heavy trait", job: traitJob("build~generate", true), want: true},
		{name: "no declaration", job: traitJob("build~generate", false), want: false},
		{name: "step override beats the trait", job: func() *ScheduledJob {
			job := traitJob("build~generate", true)
			light := false
			job.JobDef.Heavy = &light
			return job
		}(), want: false},
		{name: "nil job", job: nil, want: false},
		{name: "job without a definition", job: &ScheduledJob{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHeavyJob(tt.job); got != tt.want {
				t.Fatalf("isHeavyJob = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsHeavyJob_NoNameHeuristic is the deletion's regression pin.
//
// Every one of these names WAS classified heavy by a substring match, and every
// one of them is now declared `heavy: true` by the manifest that owns it. An
// undeclared step wearing one of those names — a third-party extension's
// "lint~check", say — must be light, because scheduling reads declarations and
// a name is not a declaration.
func TestIsHeavyJob_NoNameHeuristic(t *testing.T) {
	for _, name := range []string{
		"test~test",
		"lint~golangci-lint",
		"lint~staticcheck",
		"build~types",
		"build~cross-compile",
		"lint~check",
	} {
		t.Run(name, func(t *testing.T) {
			job := makePlannedJobs(1, name)[0]
			job.JobDef.Heavy = nil
			job.JobDef.Traits = extensionproto.CommandTraits{}
			if isHeavyJob(job) {
				t.Fatalf("undeclared step %q was classified heavy — the name heuristic C5 deleted "+
					"is back", name)
			}
		})
	}
}

func TestMemoryWorkerCapDisabledWithoutMemory(t *testing.T) {
	if got := memoryWorkerCap(parallelModeAuto, 0, heavyJobPermille(makePlannedJobs(1, "test~test"))); got != 0 {
		t.Fatalf("memory cap = %d, want disabled cap", got)
	}
	if got := memoryWorkerCap(parallelModeAuto, 512, heavyJobPermille(makePlannedJobs(1, "test~test"))); got != 0 {
		t.Fatalf("sub-MiB memory cap = %d, want disabled cap", got)
	}
}

func TestResolveParallelDecision_AutoFieldsWithMemoryCap(t *testing.T) {
	planned := makePlannedJobs(100, "lint~golangci-lint")
	d := resolveParallelDecisionWithHardware(SchedulerConfig{MaxParallelMode: parallelModeAuto}, planned, 10, 4*1024*1024*1024)

	if d.Mode != parallelModeAuto {
		t.Errorf("mode = %q, want %q", d.Mode, parallelModeAuto)
	}
	if d.Workers != 2 {
		t.Errorf("workers = %d, want 2 (memory-capped with headroom)", d.Workers)
	}
	if d.LogicalCPU != 10 {
		t.Errorf("logicalCPU = %d, want 10", d.LogicalCPU)
	}
	if d.CPUCapacity != 10 {
		t.Errorf("cpuCapacity = %d, want 10", d.CPUCapacity)
	}
	if d.MemoryTotalMiB != 4096 {
		t.Errorf("memoryTotalMiB = %d, want 4096", d.MemoryTotalMiB)
	}
	if d.MemoryUsableMiB != 3072 {
		t.Errorf("memoryUsableMiB = %d, want 3072 after fixed headroom", d.MemoryUsableMiB)
	}
	if d.MemoryCapWorkers != 2 {
		t.Errorf("memoryCapWorkers = %d, want 2", d.MemoryCapWorkers)
	}
	if d.PlannedJobs != 100 {
		t.Errorf("plannedJobs = %d, want 100", d.PlannedJobs)
	}
	if d.HeavyJobPermille != 1000 {
		t.Errorf("heavyJobPermille = %d, want 1000", d.HeavyJobPermille)
	}
}

func TestResolveParallelDecision_NumericOverridesTuning(t *testing.T) {
	planned := makePlannedJobs(20, "build~generate")
	d := resolveParallelDecisionWithHardware(SchedulerConfig{MaxParallel: 5}, planned, 0, 0)

	if d.Mode != parallelModeNumeric {
		t.Errorf("mode = %q, want %q", d.Mode, parallelModeNumeric)
	}
	if d.Workers != 5 {
		t.Errorf("workers = %d, want 5", d.Workers)
	}
	if d.LogicalCPU != 1 {
		t.Errorf("logicalCPU = %d, want 1 (normalized)", d.LogicalCPU)
	}
	if d.MemoryCapWorkers != 0 {
		t.Errorf("memoryCapWorkers = %d, want 0 for numeric mode", d.MemoryCapWorkers)
	}
}

func TestResolveParallelDecision_UncappedReportsNoCap(t *testing.T) {
	planned := makePlannedJobs(100, "build~generate")
	d := resolveParallelDecisionWithHardware(SchedulerConfig{MaxParallelMode: parallelModeAuto}, planned, 10, 0)

	if d.Workers != 30 {
		t.Errorf("workers = %d, want 30", d.Workers)
	}
	if d.MemoryTotalMiB != 0 {
		t.Errorf("memoryTotalMiB = %d, want 0 when memory unknown", d.MemoryTotalMiB)
	}
	if d.MemoryCapWorkers != 0 {
		t.Errorf("memoryCapWorkers = %d, want 0 when uncapped", d.MemoryCapWorkers)
	}
}

func TestStableMemoryCapacityUsesPhysicalOrCgroupLimitWithHeadroom(t *testing.T) {
	const eightGiB = uint64(8 * 1024 * 1024 * 1024)
	root := fixtureRunner(t, map[string]string{
		"proc/self/cgroup":                "0::/runner\n",
		"sys/fs/cgroup/runner/memory.max": "2147483648\n",
	})
	capacity := stableMemoryCapacityBytes(root, eightGiB)
	if capacity != 2*1024*1024*1024 {
		t.Fatalf("stable capacity = %d, want 2 GiB cgroup limit", capacity)
	}
	if usable := usableMemoryBytes(capacity); usable != 1536*1024*1024 {
		t.Fatalf("usable memory = %d, want fixed 25%% headroom", usable)
	}

	unlimited := fixtureRunner(t, map[string]string{
		"proc/self/cgroup":         "0::/\n",
		"sys/fs/cgroup/memory.max": "max\n",
	})
	if got := stableMemoryCapacityBytes(unlimited, eightGiB); got != eightGiB {
		t.Fatalf("unlimited cgroup capacity = %d, want physical RAM %d", got, eightGiB)
	}
}

func TestStableMemoryCapacitySupportsCgroupV1(t *testing.T) {
	const physical = uint64(8 * 1024 * 1024 * 1024)
	root := fixtureRunner(t, map[string]string{
		"proc/self/cgroup": "5:memory:/docker/abc\n",
		"sys/fs/cgroup/memory/docker/abc/memory.limit_in_bytes": "3221225472\n",
	})
	if got := stableMemoryCapacityBytes(root, physical); got != 3*1024*1024*1024 {
		t.Fatalf("v1 stable capacity = %d, want 3 GiB", got)
	}
}

// workersFor resolves just the selected worker count, the value most of these
// tests assert on.
func workersFor(cfg SchedulerConfig, planned []*ScheduledJob, logicalCPU int, totalMemoryBytes uint64) int {
	return resolveParallelDecisionWithHardware(cfg, planned, logicalCPU, totalMemoryBytes).Workers
}

// declaredHeavySteps mirrors the `heavy: true` steps the SHIPPED first-party
// manifests declare, so these worker-tuning tests keep exercising the same
// heavy/light plan mixes they always did.
//
// It is a fixture of manifest DECLARATIONS, not a classifier: an earlier
// change deleted core's name-substring heuristic, so a planned job is heavy
// exactly when its step says so. Building the fixture from the declarations is
// what keeps these tests measuring the tuning arithmetic rather than a
// classification that no longer exists.
var declaredHeavySteps = map[string]bool{
	"test~test":             true, // go, typescript, python
	"lint~golangci-lint":    true, // go
	"lint~staticcheck":      true, // go
	"build~cross-compile":   true, // go
	"package~cross-compile": true, // go
	"build~types":           true, // typescript
	"lint~check":            true, // typescript
	"package~types":         true, // typescript
}

func makePlannedJobs(count int, jobName string) []*ScheduledJob {
	jobs := make([]*ScheduledJob, 0, count)
	heavy := declaredHeavySteps[jobName]
	for i := 0; i < count; i++ {
		def := &extension.JobDefinition{
			Name:        jobName,
			CommandName: commandFromJobName(jobName),
		}
		if heavy {
			declared := true
			def.Heavy = &declared
		}
		jobs = append(jobs, &ScheduledJob{
			Project: &workspace.Project{
				ID:   "/project",
				Name: "project",
				Path: "project",
			},
			JobDef: def,
		})
	}
	return jobs
}

func commandFromJobName(jobName string) string {
	for i, r := range jobName {
		if r == '~' {
			return jobName[:i]
		}
	}
	return jobName
}
