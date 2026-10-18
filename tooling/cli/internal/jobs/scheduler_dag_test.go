package jobs

import (
	"sync"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// makeJob builds a minimal ScheduledJob for DAG tests.
func makeJob(projectName, jobName string, dependsOn []string) *ScheduledJob {
	return &ScheduledJob{
		Project:   &workspace.Project{ID: "/" + projectName, Name: projectName, Path: projectName},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: jobName},
		DependsOn: dependsOn,
	}
}

// --- Scheduler.shouldSkip ---

func makeScheduler(continueOnError bool) *Scheduler {
	return &Scheduler{
		cfg: SchedulerConfig{ContinueOnError: continueOnError},
	}
}

func TestShouldSkip_NoDeps(t *testing.T) {
	s := makeScheduler(false)
	job := makeJob("app", "build", nil)
	results := map[string]*JobResult{}
	var mu sync.Mutex
	if s.shouldSkip(job, results, &mu) {
		t.Error("job with no deps should not be skipped")
	}
}

func TestShouldSkip_DepFailed(t *testing.T) {
	s := makeScheduler(false)
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	results := map[string]*JobResult{
		lib.Key(): {Status: "failed"},
	}
	var mu sync.Mutex
	if !s.shouldSkip(app, results, &mu) {
		t.Error("job whose dependency failed should be skipped")
	}
}

func TestShouldSkip_DepSucceeded(t *testing.T) {
	s := makeScheduler(false)
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	results := map[string]*JobResult{
		lib.Key(): {Status: "success"},
	}
	var mu sync.Mutex
	if s.shouldSkip(app, results, &mu) {
		t.Error("job with successful dependency should not be skipped")
	}
}

func TestShouldSkip_ContinueOnError(t *testing.T) {
	// When ContinueOnError=true, shouldSkip always returns false
	s := makeScheduler(true)
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	results := map[string]*JobResult{
		lib.Key(): {Status: "failed"},
	}
	var mu sync.Mutex
	if s.shouldSkip(app, results, &mu) {
		t.Error("shouldSkip should return false when ContinueOnError=true")
	}
}
