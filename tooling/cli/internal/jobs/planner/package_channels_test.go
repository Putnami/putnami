package planner

import (
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/repotest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func loadRealPlannerExtension(t *testing.T, dir, name string) *extension.ExtensionDescription {
	t.Helper()
	ext := extension.LoadExtensionFromDir(filepath.Join(findJobsRepoRoot(t), dir, "extension"), name)
	if ext == nil {
		t.Fatalf("load real %s extension", name)
	}
	return ext
}

// The channels of a packaging command are the boolean flags its steps are
// planned on, read from the real manifests: a negated flag, a string flag and
// a flag no step reads are not channels, and publish, which publishes to a
// registry, offers none.
func TestPackageChannels_AreTheFlagsAStepIsPlannedOn(t *testing.T) {
	repotest.Require(t)
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-package-run-with-no-channel-on-says-which-option-to-set", "the-channels-of-a-packaging-command")
	cases := []struct {
		dir, name, command string
		want               []string
	}{
		{"go", "@putnami/go", "package", []string{"archives", "deployment", "docker", "go"}},
		{"typescript", "@putnami/typescript", "package", []string{"archives", "deployment", "docker", "npm"}},
		{"rust", "@putnami/rust", "package", []string{"archives", "crates", "docker"}},
		{"python", "@putnami/python", "package", []string{"docker"}},
		{"go", "@putnami/go", "publish", nil},
		{"go", "@putnami/go", "build", nil},
	}
	for _, tc := range cases {
		ext := loadRealPlannerExtension(t, tc.dir, tc.name)
		if got := PackageChannels(ext.Jobs[tc.command]); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("PackageChannels(%s %s) = %v, want %v", tc.name, tc.command, got, tc.want)
		}
	}
}

// A Go service whose package job has every channel off is a gap; a channel
// turned on by options.package, by the run's flag or by the project's publish
// list closes it.
func TestInactiveChannels_NamesAProjectWithEveryChannelOff(t *testing.T) {
	repotest.Require(t)
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-package-run-with-no-channel-on-says-which-option-to-set", "a-project-with-every-channel-off")
	ext := loadGoPlannerExtension(t)
	cases := []struct {
		name    string
		options string
		publish []string
		flags   extension.ParamMap
		gap     bool
	}{
		{name: "no channel", gap: true},
		{name: "a channel turned off", options: `{"options":{"package":{"docker":false}}}`, gap: true},
		{name: "options.package", options: `{"options":{"package":{"docker":true}}}`},
		{name: "the extension's options", options: `{"options":{"@putnami/go":{"archives":true}}}`},
		{name: "the run's flag", flags: extension.ParamMap{"docker": true}},
		{name: "the publish list", publish: []string{"go"}},
		{name: "an image project's undeclared image parameter", options: `{"options":{"package":{"image":true}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeProjectFile(t, root, "api", "go.mod", "module example.com/api\n\ngo 1.25\n")
			writeProjectFile(t, root, "api", "main.go", "package main\n\nfunc main() {}\n")
			if tc.options != "" {
				writeProjectFile(t, root, "api", wsproto.ConfigFilename, tc.options)
			}
			project := &workspace.Project{
				ID: "/api", Name: "api", Path: "api", Type: "application",
				Extensions: []string{"@putnami/go"}, Publish: tc.publish,
				Config: wsproto.LoadProjectConfig(filepath.Join(root, "api")),
			}
			ws := testWorkspace(root, project)
			gaps := InactiveChannels(ws, []string{"package"}, []*workspace.Project{project},
				[]*extension.ExtensionDescription{ext}, tc.flags, nil, nil)
			if !tc.gap {
				if len(gaps) != 0 {
					t.Fatalf("gaps = %+v, want none", gaps)
				}
				return
			}
			want := []ChannelGap{{Project: project, Command: "package", Channels: []string{"archives", "deployment", "docker", "go"}}}
			if !reflect.DeepEqual(gaps, want) {
				t.Fatalf("gaps = %+v, want %+v", gaps, want)
			}
		})
	}
}

// A command that is not a packaging command, and a project no packaging job
// matches, are never gaps.
func TestInactiveChannels_IgnoresOtherCommandsAndUnmatchedProjects(t *testing.T) {
	repotest.Require(t)
	t.Parallel()
	ext := loadGoPlannerExtension(t)
	root := t.TempDir()
	writeProjectFile(t, root, "api", "go.mod", "module example.com/api\n\ngo 1.25\n")
	project := &workspace.Project{ID: "/api", Name: "api", Path: "api", Type: "application", Extensions: []string{"@putnami/go"}}
	other := &workspace.Project{ID: "/docs", Name: "docs", Path: "docs", Type: "library"}
	ws := testWorkspace(root, project, other)
	if gaps := InactiveChannels(ws, []string{"build", "publish"}, []*workspace.Project{project}, []*extension.ExtensionDescription{ext}, nil, nil, nil); len(gaps) != 0 {
		t.Fatalf("build, publish gaps = %+v, want none", gaps)
	}
	if gaps := InactiveChannels(ws, []string{"package"}, []*workspace.Project{other}, []*extension.ExtensionDescription{ext}, nil, nil, nil); len(gaps) != 0 {
		t.Fatalf("unmatched project gaps = %+v, want none", gaps)
	}
}

// A packaging job that offers no channel packages whatever its steps make, so
// a project it matches is never a gap, even when another packaging job it
// matches has every channel off.
func TestInactiveChannels_AJobWithNoChannelAlwaysPackages(t *testing.T) {
	repotest.Require(t)
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-package-run-with-no-channel-on-says-which-option-to-set", "a-project-with-every-channel-off")
	goExt := loadGoPlannerExtension(t)
	member := &extension.ExtensionDescription{
		Name:  "@test/member",
		Tasks: map[string]extension.TaskDefinition{"write": {Kind: "command", Command: "true"}},
		Jobs: map[string]*extension.JobDefinition{"package": {
			Name: "package", ExtensionName: "@test/member",
			Traits:        extension.CommandTraits{InjectPublishChannels: true},
			PipelineSteps: []extension.PipelineStep{{ID: "member", Task: "write"}},
		}},
	}
	root := t.TempDir()
	writeProjectFile(t, root, "api", "go.mod", "module example.com/api\n\ngo 1.25\n")
	writeProjectFile(t, root, "api", "main.go", "package main\n\nfunc main() {}\n")
	project := &workspace.Project{
		ID: "/api", Name: "api", Path: "api", Type: "application",
		Extensions: []string{"@putnami/go", "@test/member"},
	}
	ws := testWorkspace(root, project)
	if gaps := InactiveChannels(ws, []string{"package"}, []*workspace.Project{project},
		[]*extension.ExtensionDescription{goExt, member}, nil, nil, nil); len(gaps) != 0 {
		t.Fatalf("gaps = %+v, want none", gaps)
	}
}

// An entry names a project only when it spells its name, ID or path, or is
// "." inside it: a glob or a subtraction names none, even of one project.
func TestNamedProjects_NamesOnlyAProjectAnEntrySpells(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-package-run-with-no-channel-on-says-which-option-to-set", "fail-when-named-warn-otherwise")
	api := &workspace.Project{ID: "/services/api", Name: "api", Path: "services/api"}
	web := &workspace.Project{ID: "/services/web", Name: "web", Path: "services/web"}
	ws := testWorkspace(t.TempDir(), api, web)
	cases := map[string][]string{
		"api":                {"/services/api"},
		"/services/api":      {"/services/api"},
		"services/api/":      {"/services/api"},
		"api,web":            {"/services/api", "/services/web"},
		"/services/...":      nil,
		"/services/...,-web": nil,
		"/services/api/...":  nil,
	}
	for selector, want := range cases {
		named := NamedProjects(ws, selector)
		var got []string
		for _, project := range []*workspace.Project{api, web} {
			if named[project.ID] {
				got = append(got, project.ID)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("NamedProjects(%q) = %v, want %v", selector, got, want)
		}
	}
}
