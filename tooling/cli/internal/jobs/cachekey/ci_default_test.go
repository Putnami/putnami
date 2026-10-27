package cachekey

import (
	"testing"

	"go.putnami.dev/cli/model/extension"
	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/protocol/features/spectest"
)

const ciDefaultRequirement = "a-flag-takes-its-ci-default-when-ci-is-set"

// fixFlagJob is a lint job whose fix flag defaults on and off in CI.
func fixFlagJob() *extension.JobDefinition {
	return &extension.JobDefinition{
		Name:        "lint~golangci-lint",
		CommandName: "lint",
		Flags: map[string]extension.FlagDefinition{
			"fix":     {Type: "boolean", Default: true, CIDefault: false},
			"timeout": {Type: "string"},
		},
	}
}

func TestMergeParamLayers_TheCIDefaultReplacesTheDefaultWhenCIIsSet(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", ciDefaultRequirement, "the-ci-default-replaces-the-default-when-ci-is-set")
	for _, tc := range []struct {
		ci   string
		want bool
	}{{"true", false}, {"1", false}, {"", true}, {"  ", true}} {
		t.Setenv("CI", tc.ci)
		params := MergeParamLayers(nil, fixFlagJob(), nil, &workspace.Project{}, "@putnami/go", "lint", nil, nil)
		if got := params["fix"]; got != tc.want {
			t.Errorf("CI=%q: fix = %v, want %v", tc.ci, got, tc.want)
		}
	}
}

func TestMergeParamLayers_AnExplicitFlagWinsOverTheCIDefault(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", ciDefaultRequirement, "an-explicit-flag-wins-over-the-ci-default")
	t.Setenv("CI", "true")
	params := MergeParamLayers(nil, fixFlagJob(), nil, &workspace.Project{}, "@putnami/go", "lint", nil, extension.ParamMap{"fix": true})
	if got := params["fix"]; got != true {
		t.Errorf("fix = %v, want the explicit --fix", got)
	}
}

func TestTaskCacheParamNames_AFlagWithACIDefaultKeysOnlyTheTasksThatDeclareIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", ciDefaultRequirement, "a-flag-with-a-ci-default-keys-only-the-tasks-that-declare-it")
	undeclared := &jobmodel.ScheduledJob{
		Project:   &workspace.Project{ID: "/app"},
		Extension: &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef:    fixFlagJob(),
	}
	names := taskCacheParamNames(undeclared)
	if _, keyed := names["fix"]; keyed {
		t.Errorf("a task that does not declare fix is keyed on it: %v", names)
	}
	if _, keyed := names["timeout"]; !keyed {
		t.Errorf("a flag without a ciDefault no longer keys the task: %v", names)
	}

	declared := fixFlagJob()
	declared.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Params: []string{"fix"}}}
	names = taskCacheParamNames(&jobmodel.ScheduledJob{
		Project:   &workspace.Project{ID: "/app"},
		Extension: &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef:    declared,
	})
	if _, keyed := names["fix"]; !keyed {
		t.Errorf("a task that declares fix is not keyed on it: %v", names)
	}
}
