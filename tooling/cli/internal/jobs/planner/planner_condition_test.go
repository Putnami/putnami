package planner

import (
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestPlan_GoLibraryCompileUsesInvocationAndProjectFacts exercises the real
// first-party manifest. A synthetic condition here could stay green while the
// shipped Go extension drifted back to an unconditional compile.
func TestPlan_GoLibraryCompileUsesInvocationAndProjectFacts(t *testing.T) {
	t.Parallel()
	ext := loadGoPlannerExtension(t)

	tests := []struct {
		name     string
		typ      string
		commands []string
		options  string
		want     bool
	}{
		{
			name:     "library combined validation omits redundant compile",
			typ:      "library",
			commands: []string{"lint", "test", "build"},
			want:     false,
		},
		{
			name:     "library standalone build retains compile evidence",
			typ:      "library",
			commands: []string{"build"},
			want:     true,
		},
		{
			name:     "application combined validation retains binary build",
			typ:      "application",
			commands: []string{"lint", "test", "build"},
			want:     true,
		},
		{
			name:     "explicit platform retains library compile",
			typ:      "library",
			commands: []string{"lint", "test", "build"},
			options:  `{"options":{"build":{"target":"linux/amd64"}}}`,
			want:     true,
		},
		{
			name:     "explicit build tags retain library compile",
			typ:      "library",
			commands: []string{"lint", "test", "build"},
			options:  `{"options":{"build":{"tags":"integration"}}}`,
			want:     true,
		},
		{
			name:     "build-only race retains plain compile",
			typ:      "library",
			commands: []string{"lint", "test", "build"},
			options:  `{"options":{"build":{"race":true}}}`,
			want:     true,
		},
		{
			name:     "test-only race retains plain compile",
			typ:      "library",
			commands: []string{"lint", "test", "build"},
			options:  `{"options":{"test":{"race":true}}}`,
			want:     true,
		},
		{
			name:     "provider-qualified test-only race retains plain compile",
			typ:      "library",
			commands: []string{"lint", "test", "build"},
			options:  `{"options":{"@putnami/go:test":{"race":true}}}`,
			want:     true,
		},
		{
			name:     "matching build and test race still delegates compile",
			typ:      "library",
			commands: []string{"lint", "test", "build"},
			options:  `{"options":{"@putnami/go":{"race":true}}}`,
			want:     false,
		},
		{
			name:     "explicit false cgo retains library compile",
			typ:      "library",
			commands: []string{"lint", "test", "build"},
			options:  `{"options":{"build":{"cgo":false}}}`,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			planned := planGoProject(t, ext, tt.typ, tt.commands, tt.options)
			got := planKeySet(planned)["/subject:build~compile"]
			if got != tt.want {
				t.Errorf("build~compile planned = %t, want %t; keys=%v", got, tt.want, planKeySet(planned))
			}
			// Infra survives compile pruning and keeps a valid dependency set;
			// Plan's DAG validator has already rejected any dangling edge.
			if !planKeySet(planned)["/subject:build~infra"] {
				t.Error("build~infra disappeared with compile pruning")
			}
		})
	}
}

func TestPlan_PlannerConditionCommandSetIsOrderIndependent(t *testing.T) {
	t.Parallel()
	ext := loadGoPlannerExtension(t)
	first := planKeySet(planGoProject(t, ext, "library", []string{"lint", "test", "build"}, ""))
	second := planKeySet(planGoProject(t, ext, "library", []string{"build", "lint", "test"}, ""))
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("command order changed plan:\nfirst=%v\nsecond=%v", first, second)
	}
}

func loadGoPlannerExtension(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	root := findJobsRepoRoot(t)
	ext := extension.LoadExtensionFromDir(filepath.Join(root, "go", "extension"), "@putnami/go")
	if ext == nil {
		t.Fatal("load real @putnami/go extension")
	}
	return ext
}

func planGoProject(
	t *testing.T,
	ext *extension.ExtensionDescription,
	projectType string,
	commands []string,
	options string,
) []*jobmodel.ScheduledJob {
	t.Helper()
	root := t.TempDir()
	writeProjectFile(t, root, "subject", "go.mod", "module example.com/subject\n\ngo 1.25\n")
	source := "package subject\n"
	if projectType == "application" {
		source = "package main\n\nfunc main() {}\n"
	}
	writeProjectFile(t, root, "subject", "subject.go", source)
	if options != "" {
		writeProjectFile(t, root, "subject", wsproto.ConfigFilename, options)
	}

	project := &workspace.Project{
		ID:         "/subject",
		Name:       "subject",
		Path:       "subject",
		Type:       projectType,
		Extensions: []string{"@putnami/go"},
		Config:     wsproto.LoadProjectConfig(filepath.Join(root, "subject")),
	}
	ws := testWorkspace(root, project)
	planned, err := Plan(ws, commands, []*workspace.Project{project},
		[]*extension.ExtensionDescription{ext}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan(%v, type=%s, options=%s): %v", commands, projectType, options, err)
	}
	return planned
}

// routedImagePackagerName is an extension that packages a Go workload's OCI
// image in place of @putnami/go. Its package step claims the same bin and
// docker directories as Go's cross-compile and docker steps, as such a
// packager does, so the planner refuses any plan that keeps both.
const routedImagePackagerName = "@test/oci"

func routedImagePackager() *extension.ExtensionDescription {
	directory := func(path string) extension.DeclaredOutput {
		return extension.DeclaredOutput{
			Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput,
			Path: path, OptionalEmpty: true,
		}
	}
	return &extension.ExtensionDescription{
		Name: routedImagePackagerName,
		Jobs: map[string]*extension.JobDefinition{
			"package": {
				ExtensionName: routedImagePackagerName, Name: "package", Kind: "command",
				Traits:        extension.CommandTraits{InjectPublishChannels: true},
				PipelineSteps: []extension.PipelineStep{{ID: "oci-image", Task: "oci-image", If: "params.docker"}},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"oci-image": {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{
				Outputs: map[string]extension.DeclaredOutput{"bin": directory("bin"), "docker": directory("docker")},
			}},
		},
	}
}

// planGoWorkload plans commands for one Go application whose putnami.json is
// config, with the given extensions installed for it.
func planGoWorkload(
	t *testing.T,
	config string,
	commands []string,
	extensions ...*extension.ExtensionDescription,
) ([]*jobmodel.ScheduledJob, error) {
	t.Helper()
	root := t.TempDir()
	writeProjectFile(t, root, "subject", "go.mod", "module example.com/subject\n\ngo 1.25\n")
	writeProjectFile(t, root, "subject", "main.go", "package main\n\nfunc main() {}\n")
	writeProjectFile(t, root, "subject", wsproto.ConfigFilename, config)
	names := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		names = append(names, ext.Name)
	}
	project := &workspace.Project{
		ID: "/subject", Name: "subject", Path: "subject", Type: "application",
		Extensions: names, Config: wsproto.LoadProjectConfig(filepath.Join(root, "subject")),
	}
	project.Publish = project.Config.Publish
	ws := testWorkspace(root, project)
	return Plan(ws, commands, []*workspace.Project{project}, extensions, nil, nil, nil, nil)
}

// plannedSteps lists the step ids one extension planned for one command.
func plannedSteps(planned []*jobmodel.ScheduledJob, extensionName, command string) []string {
	var steps []string
	for _, job := range planned {
		if job.Extension != nil && job.Extension.Name == extensionName && job.CommandName() == command {
			steps = append(steps, job.StepID())
		}
	}
	sort.Strings(steps)
	return steps
}

func findStep(planned []*jobmodel.ScheduledJob, extensionName, command, step string) *jobmodel.ScheduledJob {
	for _, job := range planned {
		if job.Extension != nil && job.Extension.Name == extensionName && job.CommandName() == command && job.StepID() == step {
			return job
		}
	}
	return nil
}

// TestPlan_GoPackageYieldsTheImageToTheRoutedPackager plans the real @putnami/go
// manifest for a workload whose publish route names another extension as the
// producer of its OCI image. The publication plans: Go keeps its deployment
// step and plans no docker or cross-compile step, the routed packager owns bin
// and docker alone, and Go's docker upload waits for that packager's step.
func TestPlan_GoPackageYieldsTheImageToTheRoutedPackager(t *testing.T) {
	t.Parallel()
	goExt := loadGoPlannerExtension(t)
	routed := `{"publish":["docker","deployment"],"options":{"@putnami/go:publish":{` +
		`"docker-package-publisher":"` + routedImagePackagerName + `","docker-package-step":"oci-image"}}}`

	planned, err := planGoWorkload(t, routed, []string{"publish"}, goExt, routedImagePackager())
	if err != nil {
		t.Fatalf("publish of a workload whose image another extension packages: %v", err)
	}
	goSteps := plannedSteps(planned, goExt.Name, "package")
	if !slices.Contains(goSteps, "deployment") {
		t.Errorf("Go package steps = %v, want the deployment step", goSteps)
	}
	for _, unwanted := range []string{"docker", "cross-compile", "image"} {
		if slices.Contains(goSteps, unwanted) {
			t.Errorf("Go package steps = %v, want no %q step: the routed packager owns the image", goSteps, unwanted)
		}
	}
	image := findStep(planned, routedImagePackagerName, "package", "oci-image")
	if image == nil {
		t.Fatalf("the routed packager's step is not planned; keys=%v", planKeySet(planned))
	}
	upload := findStep(planned, goExt.Name, "publish", "docker")
	if upload == nil {
		t.Fatalf("Go's docker upload is not planned; keys=%v", planKeySet(planned))
	}
	if !slices.Contains(upload.DependsOn, image.Key()) {
		t.Errorf("Go's docker upload depends on %v, want it to wait for %s", upload.DependsOn, image.Key())
	}

	// The archives channel still consumes Go's own binaries.
	archives := `{"publish":["archives","docker"],"options":{"@putnami/go:publish":{` +
		`"docker-package-publisher":"` + routedImagePackagerName + `","docker-package-step":"oci-image"}}}`
	planned, err = planGoWorkload(t, archives, []string{"publish"}, goExt)
	if err != nil {
		t.Fatalf("publish with archives and a routed image: %v", err)
	}
	goSteps = plannedSteps(planned, goExt.Name, "package")
	if !slices.Contains(goSteps, "archives") || !slices.Contains(goSteps, "cross-compile") || slices.Contains(goSteps, "docker") {
		t.Errorf("Go package steps with archives and a routed image = %v, want archives and cross-compile, no docker", goSteps)
	}
}

// TestPlan_GoPackageWithoutARoutedImageIsUnchanged pins that the image route
// changes nothing for an ordinary workload: with no route, or a route that
// names @putnami/go, every channel mix plans the same jobs and edges as the
// docker step gated on its channel alone.
func TestPlan_GoPackageWithoutARoutedImageIsUnchanged(t *testing.T) {
	t.Parallel()
	goExt := loadGoPlannerExtension(t)
	channelOnly := loadGoPlannerExtension(t)
	gated := false
	for i, step := range channelOnly.Jobs["package"].PipelineSteps {
		if step.ID == "docker" {
			channelOnly.Jobs["package"].PipelineSteps[i].If = "params.docker && !params.image"
			gated = true
		}
	}
	if !gated {
		t.Fatal("the Go package command has no docker step")
	}

	shape := func(planned []*jobmodel.ScheduledJob) map[string][]string {
		edges := make(map[string][]string, len(planned))
		for _, job := range planned {
			deps := append([]string(nil), job.DependsOn...)
			sort.Strings(deps)
			edges[job.Key()] = deps
		}
		return edges
	}
	for _, publish := range []string{`["docker"]`, `["docker","deployment"]`, `["archives"]`, `["go","docker"]`, `["deployment"]`} {
		for _, route := range []string{``, `,"options":{"@putnami/go:publish":{"docker-package-publisher":"@putnami/go"}}`} {
			for _, commands := range [][]string{{"package"}, {"publish"}} {
				config := `{"publish":` + publish + route + `}`
				want, err := planGoWorkload(t, config, commands, channelOnly)
				if err != nil {
					t.Fatalf("%v %s with the channel-only docker step: %v", commands, config, err)
				}
				got, err := planGoWorkload(t, config, commands, goExt)
				if err != nil {
					t.Fatalf("%v %s: %v", commands, config, err)
				}
				if !reflect.DeepEqual(shape(got), shape(want)) {
					t.Errorf("%v %s plans\n%v\nwant\n%v", commands, config, shape(got), shape(want))
				}
			}
		}
	}
}
