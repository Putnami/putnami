package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The inferred-capture path is gone. This ratchet derives every first-party
// pipeline node from the checked-in manifests so a future task cannot quietly
// reintroduce an undeclared cache entry.

// firstPartyExtensionManifests are the manifests whose tasks this workspace
// schedules. The extension name lives in the owning project's package metadata
// rather than in the manifest, so it travels beside the path.
var firstPartyExtensionManifests = []struct{ name, path string }{
	{"@putnami/go", "go/extension/putnami.extension.json"},
	{"@putnami/python", "python/extension/putnami.extension.json"},
	{"@putnami/typescript", "typescript/extension/putnami.extension.json"},
	{"@putnami/clientgen", "tooling/clientgen-extension/putnami.extension.json"},
	{"@putnami/scaffold", "tooling/scaffold/putnami.extension.json"},
}

func TestFirstPartyCaptureReachability(t *testing.T) {
	jobs := firstPartyScheduledJobs(t)
	if len(jobs) == 0 {
		t.Fatal("no first-party jobs were derived; the ratchet would be vacuously green")
	}

	declared, cacheable := 0, 0
	for _, job := range jobs {
		name := firstPartyJobName(job)
		if taskDeclarationOf(job) == nil {
			t.Errorf("%s has no declares block; task-owned caching requires an explicit contract", name)
			continue
		}
		declared++

		if !isCacheEnabled(job, CacheBypass{}) {
			continue
		}
		cacheable++
		if !usesDeclaredCapture(job) {
			t.Errorf("%s is cache-enabled without task-owned declared capture", name)
		}
	}
	if declared == 0 {
		t.Fatal("no first-party job declares its outputs; the ratchet would be vacuously green")
	}
	if cacheable == 0 {
		t.Fatal("no first-party job is cache-enabled; the ratchet would be vacuously green")
	}
}

// ---------------------------------------------------------------------------
// manifest → plan nodes
// ---------------------------------------------------------------------------

// firstPartyScheduledJobs derives one plan node per (command, pipeline step) of
// every first-party manifest.
//
// It walks the pipeline DIRECTLY rather than through ExpandPipeline, on purpose:
// expansion evaluates each step's `if` against command params, and half of these
// steps are gated on a flag (params.fix, params.docker, params.npm, ...). An
// inventory that only saw one side of every flag would silently under-report the
// legacy surface. Every step a manifest CAN schedule counts.
//
// The fields it mirrors from ExpandPipeline are exactly the ones the capture
// predicates read: the step job name (where jobCommandName finds the command),
// the task's write resources, its effective cache enablement, and its noOutput
// policy.
func firstPartyScheduledJobs(t *testing.T) []*ScheduledJob {
	t.Helper()
	root := findJobsRepoRoot(t)

	var jobs []*ScheduledJob
	for _, entry := range firstPartyExtensionManifests {
		path := filepath.Join(root, entry.path)
		manifest, err := extension.LoadManifest(path)
		if err != nil {
			t.Fatalf("LoadManifest(%s): %v", entry.path, err)
		}
		desc := extension.Resolve(manifest, filepath.Dir(path))
		desc.Name = entry.name

		commands := make([]string, 0, len(manifest.Commands))
		for name := range manifest.Commands {
			commands = append(commands, name)
		}
		sort.Strings(commands)

		for _, cmdName := range commands {
			for _, step := range manifest.Commands[cmdName].Run {
				task, ok := manifest.Tasks[step.Task]
				if !ok {
					continue
				}
				jobs = append(jobs, firstPartyScheduledJob(desc, cmdName, step, task))
			}
		}
	}
	return jobs
}

func firstPartyScheduledJob(
	desc *extension.ExtensionDescription,
	cmdName string,
	step extension.PipelineStep,
	task extension.TaskDefinition,
) *ScheduledJob {
	policy := &extension.TaskCachePolicy{}
	if task.Cache != nil {
		policy.Enabled = task.Cache.Enabled
		policy.Deterministic = task.Cache.Deterministic
		policy.VersionAware = task.Cache.VersionAware
		policy.NoOutput = task.Cache.NoOutput
	}
	stepCopy := step
	return &ScheduledJob{
		Project: &workspace.Project{ID: "/inventory", Name: "inventory", Path: "inventory"},
		Extension: &extension.ExtensionDescription{
			Name:  desc.Name,
			Tasks: desc.Tasks,
		},
		JobDef: &extension.JobDefinition{
			ExtensionName:   desc.Name,
			Name:            extension.StepJobName(cmdName, step.ID),
			CommandName:     cmdName,
			StepID:          step.ID,
			Cache:           task.Cache.IsEnabled(),
			Writes:          task.Writes,
			Reads:           task.Reads,
			TaskCachePolicy: policy,
		},
		Step: &stepCopy,
	}
}

func firstPartyJobName(job *ScheduledJob) string {
	return fmt.Sprintf("%s %s", job.Extension.Name, job.JobDef.Name)
}

// findJobsRepoRoot locates the workspace root so the inventory can read the REAL
// manifests. It skips rather than fails when the tree is unavailable, so a
// packaged-source test run stays green.
func findJobsRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("could not find the Putnami repo root from %s", start)
		}
		dir = parent
	}
}
