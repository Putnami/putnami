package planner

import (
	"path/filepath"
	"reflect"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A planned job exposes the publish channels its task declares, read from the
// real Go manifest. The project declares only the go channel, so the docker
// step is not planned, and the package jobs publish requires declare none.
func TestPlan_APublishJobExposesItsTasksPublishChannels(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "publish-tasks-declare-their-channels", "a-planned-job-exposes-its-tasks-publish-channels")
	ext := loadGoPlannerExtension(t)
	root := t.TempDir()
	writeProjectFile(t, root, "subject", "go.mod", "module example.com/subject\n\ngo 1.25\n")
	writeProjectFile(t, root, "subject", "subject.go", "package subject\n")
	project := &workspace.Project{
		ID:         "/subject",
		Name:       "subject",
		Path:       "subject",
		Type:       "library",
		Extensions: []string{"@putnami/go"},
		Publish:    []string{"go"},
		Config:     wsproto.LoadProjectConfig(filepath.Join(root, "subject")),
	}
	planned, err := Plan(testWorkspace(root, project), []string{"publish"}, []*workspace.Project{project},
		[]*extension.ExtensionDescription{ext}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan(publish): %v", err)
	}

	got := map[string][]string{}
	for _, job := range planned {
		if declaration := jobmodel.TaskDeclarationOf(job); declaration != nil && declaration.PublishChannels != nil {
			got[job.Key()] = declaration.PublishChannels
		}
	}
	if want := map[string][]string{"/subject:publish~go": {"go"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planned publish channels = %v, want %v; keys=%v", got, want, planKeySet(planned))
	}
}
