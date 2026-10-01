package jobs

import (
	"path/filepath"
	"reflect"
	"testing"

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
) []*ScheduledJob {
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
		[]*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan(%v, type=%s, options=%s): %v", commands, projectType, options, err)
	}
	return planned
}
