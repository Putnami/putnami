package cachekey

import (
	"reflect"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const (
	emptyingFeature = "cli/job-planning-execution"
)

const (
	emptyingRequirement = "executed-directory-output-starts-empty"
)

// emptyingShapesJob declares one output of every shape: a required directory
// at each of the three durable roots and at the default root, an optional
// directory under the command-output root, and an optional project and
// workspace directory that declare emptied, which are emptied, and the shapes
// that are not.
func emptyingShapesJob() *jobmodel.ScheduledJob {
	return declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"lib": dirDeclaration(extension.OutputRootCommandOutput, "lib", false),
			"gen": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen",
				Excludes: []string{".gen/migration-bundle"}, Preserves: []string{"handwritten"},
			},
			"shared":  dirDeclaration(extension.OutputRootWorkspace, "dist/shared", false),
			"default": {Kind: extension.OutputKindDirectory, Path: "generated"},
			"archives": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput, Path: "archives",
				OptionalEmpty: true, Excludes: []string{"archives/ceded"}, Preserves: []string{"kept"},
			},
			"optional":       dirDeclaration(extension.OutputRootProject, "described", true),
			"optionalShared": dirDeclaration(extension.OutputRootWorkspace, "dist/optional", true),
			"emptiedOptional": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "contract",
				OptionalEmpty: true, Emptied: true, Excludes: []string{"contract/ceded"}, Preserves: []string{"kept"},
			},
			"emptiedShared": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootWorkspace, Path: "dist/contracts",
				OptionalEmpty: true, Emptied: true,
			},
			"tool":    fileDeclaration(extension.OutputRootCommandOutput, "bin/tool", true),
			"client":  {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput", OptionalEmpty: true},
			"scratch": {Kind: extension.OutputKindDirectory, Scope: extensionproto.OutputScopeInvocation, Path: "scratch", OptionalEmpty: true},
			"socket":  {Kind: extensionproto.OutputKindRuntimeFile, Scope: extensionproto.OutputScopeInvocation, Path: "socket", OptionalEmpty: true},
			"escape":  {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "../outside", OptionalEmpty: true},
			"root":    {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".", OptionalEmpty: true},
			"unknown": {Kind: extension.OutputKindDirectory, Root: "elsewhere", Path: "out", OptionalEmpty: true},
		},
	})
}

// The emptied set is every durable directory output whose path the
// declaration states and that is required, lives under the command-output
// root, or declares emptied, in id order. An optionalEmpty directory under the
// project or the workspace root that does not declare emptied, a file, a
// pathFrom output, an invocation-scoped output, a path that escapes or names
// its root, and an unknown root are left out. The key carries the ids of
// exactly that set.
func TestEmptiedOutputsAreRequiredCommandOutputOrFlaggedDirectoriesWithAStatedPath(t *testing.T) {
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "a-path-from-output-is-left-as-it-stands")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-project-or-workspace-output-is-left-as-it-stands")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-optional-command-output-directory-is-emptied")
	spectest.Proves(t, emptyingFeature, emptyingRequirement, "an-emptied-optional-project-or-workspace-output-is-emptied")
	job := emptyingShapesJob()

	want := []string{"archives", "default", "emptiedOptional", "emptiedShared", "gen", "lib", "shared"}
	if got := EmptiedOutputIDs(job); !reflect.DeepEqual(got, want) {
		t.Fatalf("EmptiedOutputIDs = %v, want %v", got, want)
	}
	rels := map[string]string{}
	for _, declared := range EmptiedOutputDeclarations(job) {
		rels[declared.ID] = declared.Rel
	}
	if want := map[string]string{
		"archives": "archives", "default": "generated", "emptiedOptional": "contract", "emptiedShared": "dist/contracts",
		"gen": ".gen", "lib": "lib", "shared": "dist/shared",
	}; !reflect.DeepEqual(rels, want) {
		t.Fatalf("emptied paths = %v, want %v", rels, want)
	}

	undeclared := &jobmodel.ScheduledJob{
		Project:   &workspace.Project{ID: "/p", Name: "p", Path: "p"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: "lint"},
	}
	if got := EmptiedOutputIDs(undeclared); got != nil {
		t.Fatalf("a task with no declaration empties %v", got)
	}
}
