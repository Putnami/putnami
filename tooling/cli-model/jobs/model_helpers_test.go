package jobs

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	protocoljob "go.putnami.dev/protocol/job"
)

func TestCacheStatsSnapshotActivityAndOverhead(t *testing.T) {
	var nilSnapshot *CacheStatsSnapshot
	if nilSnapshot.HasActivity() || nilSnapshot.OverheadMs() != 0 {
		t.Fatal("a nil cache snapshot must be inactive with no overhead")
	}

	snapshot := &CacheStatsSnapshot{SetupMs: 2, NegotiateMs: 3, RestoreMs: 5, UploadMs: 7}
	if snapshot.HasActivity() {
		t.Fatal("setup timings alone must not count as cache activity")
	}
	if got := snapshot.OverheadMs(); got != 17 {
		t.Fatalf("OverheadMs() = %d, want 17", got)
	}
	snapshot.KeysRequested = 1
	if !snapshot.HasActivity() {
		t.Fatal("a requested cache key must count as activity")
	}
}

func TestRunnerEnvironmentAllocatedMillicores(t *testing.T) {
	tests := []struct {
		name      string
		env       *RunnerEnvironment
		want      int
		wantQuota bool
	}{
		{name: "nil"},
		{name: "visible CPUs", env: &RunnerEnvironment{LogicalCPUs: 6}, want: 6000},
		{name: "quota", env: &RunnerEnvironment{LogicalCPUs: 8, Cgroup: &CgroupCPU{QuotaUs: 250_000, PeriodUs: 100_000}}, want: 2500, wantQuota: true},
		{name: "sub-millicore quota falls back", env: &RunnerEnvironment{LogicalCPUs: 2, Cgroup: &CgroupCPU{QuotaUs: 1, PeriodUs: 10_000}}, want: 2000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, fromQuota := tt.env.AllocatedMillicores()
			if got != tt.want || fromQuota != tt.wantQuota {
				t.Fatalf("AllocatedMillicores() = (%d, %v), want (%d, %v)", got, fromQuota, tt.want, tt.wantQuota)
			}
		})
	}
}

func TestExecutionCPUTime(t *testing.T) {
	var nilExecution *Execution
	if got := nilExecution.CPUTime(); got != 0 {
		t.Fatalf("nil CPUTime() = %s, want 0", got)
	}
	execution := &Execution{UserCPU: 3 * time.Second, SystemCPU: 250 * time.Millisecond}
	if got := execution.CPUTime(); got != 3250*time.Millisecond {
		t.Fatalf("CPUTime() = %s, want 3.25s", got)
	}
}

func TestJobCommandNameAndInvocationLocator(t *testing.T) {
	invocation := &protocoljob.Invocation{ID: "release-1"}
	job := &ScheduledJob{
		JobDef:     &extension.JobDefinition{Name: "build~compile"},
		Invocation: invocation,
	}
	if got := JobCommandName(job); got != "build" {
		t.Fatalf("JobCommandName() = %q, want build", got)
	}
	if got := job.InvocationLocator(); got != invocation {
		t.Fatalf("InvocationLocator() = %p, want %p", got, invocation)
	}
	var nilJob *ScheduledJob
	if nilJob.InvocationLocator() != nil {
		t.Fatal("nil job returned an invocation locator")
	}
}

func TestComputePlanMetrics(t *testing.T) {
	projectA := &workspace.Project{ID: "/a"}
	projectB := &workspace.Project{ID: "/b"}
	planned := []*ScheduledJob{
		{Project: projectA, JobDef: &extension.JobDefinition{Name: "build~compile"}},
		{Project: projectA, JobDef: &extension.JobDefinition{Name: "test"}, DependsOn: []string{"/a:build"}, SerializeAfter: []string{"/b:build"}},
		{Project: projectB, JobDef: &extension.JobDefinition{Name: "build"}, DependsOn: []string{"/a:build"}},
	}

	metrics := ComputePlanMetrics(planned)
	if metrics.Jobs != 3 || metrics.Edges != 3 || metrics.Projects != 2 {
		t.Fatalf("ComputePlanMetrics() = %+v, want 3 jobs, 3 edges, 2 projects", metrics)
	}
	if !reflect.DeepEqual(metrics.ByCommand, map[string]int{"build": 2, "test": 1}) {
		t.Fatalf("ByCommand = %#v", metrics.ByCommand)
	}
	if got := metrics.CommandsSorted(); !reflect.DeepEqual(got, []string{"build", "test"}) {
		t.Fatalf("CommandsSorted() = %v", got)
	}
}

func TestTaskDeclarationOf(t *testing.T) {
	declaration := &extension.TaskDeclaration{}
	ext := &extension.ExtensionDescription{Tasks: map[string]extension.TaskDefinition{
		"compile": {Declares: declaration},
	}}
	job := &ScheduledJob{Extension: ext, Step: &extension.PipelineStep{Task: "compile"}}
	if got := TaskDeclarationOf(job); got != declaration {
		t.Fatalf("TaskDeclarationOf() = %p, want %p", got, declaration)
	}

	for name, candidate := range map[string]*ScheduledJob{
		"nil":          nil,
		"no step":      {Extension: ext},
		"no extension": {Step: &extension.PipelineStep{Task: "compile"}},
		"unknown task": {Extension: ext, Step: &extension.PipelineStep{Task: "missing"}},
	} {
		t.Run(name, func(t *testing.T) {
			if TaskDeclarationOf(candidate) != nil {
				t.Fatal("TaskDeclarationOf() must fail closed")
			}
		})
	}
}

func TestDedupeSorted(t *testing.T) {
	if DedupeSorted(nil) != nil {
		t.Fatal("DedupeSorted(nil) must return nil")
	}
	items := []string{"z", "a", "z", "b", "a"}
	if got := DedupeSorted(items); !reflect.DeepEqual(got, []string{"a", "b", "z"}) {
		t.Fatalf("DedupeSorted() = %v", got)
	}
}

func TestBatchNumber(t *testing.T) {
	tests := []struct {
		value any
		want  float64
		ok    bool
	}{
		{value: float64(1.5), want: 1.5, ok: true},
		{value: float32(2.5), want: 2.5, ok: true},
		{value: int(3), want: 3, ok: true},
		{value: int64(4), want: 4, ok: true},
		{value: json.Number("5.5"), want: 5.5, ok: true},
		{value: json.Number("not-a-number")},
		{value: "6"},
	}
	for _, tt := range tests {
		got, ok := BatchNumber(tt.value)
		if got != tt.want || ok != tt.ok {
			t.Errorf("BatchNumber(%T(%v)) = (%v, %v), want (%v, %v)", tt.value, tt.value, got, ok, tt.want, tt.ok)
		}
	}
}

func TestTaskResourceProfileNearestObservation(t *testing.T) {
	if _, ok := (TaskResourceProfile{}).NearestObservation(2, 1); ok {
		t.Fatal("empty profile returned an observation")
	}
	profile := TaskResourceProfile{Observations: []TaskResourceObservation{
		{GrantedConcurrency: 1, BatchSize: 1},
		{GrantedConcurrency: 3, BatchSize: 1},
		{GrantedConcurrency: 3, BatchSize: 4},
	}}
	got, ok := profile.NearestObservation(2, 3)
	if !ok || got.GrantedConcurrency != 3 || got.BatchSize != 4 {
		t.Fatalf("NearestObservation() = (%+v, %v), want concurrency 3 batch 4", got, ok)
	}
}
