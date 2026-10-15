package cachekey

import (
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func contractWorkspace(t *testing.T, extensionName string) (*workspace.Workspace, *workspace.Project) {
	t.Helper()
	proj := &workspace.Project{ID: "/app", Name: "app", Path: "packages/app", Extensions: []string{extensionName}}
	ws := workspace.NewWorkspace("/ws", nil, []*workspace.Project{proj})
	ws.Name = "contract-ws"
	return ws, proj
}

// TestResolveDeclaredOutput_PortRefsCompareByPort pins the unresolved half of
// the contract: an output whose path comes from a task output port is compared
// by port under the resolved root, and is never comparable to a literal path.
func TestResolveDeclaredOutput_PortRefsCompareByPort(t *testing.T) {
	t.Parallel()
	ws, proj := contractWorkspace(t, "@test/decl")
	job := &jobmodel.ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "@test/decl"},
		JobDef:    &extension.JobDefinition{Name: "build~step"},
	}

	port, ok := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, PathFrom: "clientDir",
	}, ws)
	if !ok {
		t.Fatal("port-backed output did not resolve")
	}
	if port.Resolved() {
		t.Errorf("port-backed ref %+v claims to be resolved", port)
	}

	samePort, _ := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, PathFrom: "clientDir",
	}, ws)
	if !extension.OutputsOverlap(port, samePort) {
		t.Error("the same port under the same root does not overlap itself")
	}

	literal, _ := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "clients",
	}, ws)
	if extension.OutputsOverlap(port, literal) {
		t.Error("an unresolved port was compared against a literal path")
	}

	otherPort, _ := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, PathFrom: "docsDir",
	}, ws)
	if extension.OutputsOverlap(port, otherPort) {
		t.Error("two different ports overlap")
	}
}

// TestResolveDeclaredOutput_SkipsUnownablePaths pins that a path the contract
// cannot own (a glob, an escape) yields no ownership claim at all: guessing at
// the region such a declaration covers would be worse than saying nothing, and
// manifest validation reports the path itself.
func TestResolveDeclaredOutput_SkipsUnownablePaths(t *testing.T) {
	t.Parallel()
	ws, proj := contractWorkspace(t, "@test/decl")
	job := &jobmodel.ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "@test/decl"},
		JobDef:    &extension.JobDefinition{Name: "build~step"},
	}
	for _, path := range []string{"dist/**/*.js", "../outside", "."} {
		if _, ok := resolveDeclaredOutput(job, extension.DeclaredOutput{
			Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: path,
		}, ws); ok {
			t.Errorf("unownable path %q produced an ownership claim", path)
		}
	}
	if _, ok := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: "invented-root", Path: "dist",
	}, ws); ok {
		t.Error("an unknown output root produced an ownership claim")
	}
}
