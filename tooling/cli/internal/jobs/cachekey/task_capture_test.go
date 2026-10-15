package cachekey

import (
	"os"
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const captureTestProject = "proj"

// declaredJob builds a plan node whose manifest task carries a v3 declaration.
// The declaration hangs off the extension's task table (never off JobDefinition),
// exactly as extension.Resolve leaves it, so the tests exercise the same lookup
// the scheduler does.
func declaredJob(name, task string, declaration *extension.TaskDeclaration, writes ...extension.ResourceRef) *jobmodel.ScheduledJob {
	return &jobmodel.ScheduledJob{
		Project: &workspace.Project{ID: "/" + captureTestProject, Name: captureTestProject, Path: captureTestProject},
		Extension: &extension.ExtensionDescription{
			Name:  "@test/ext",
			Tasks: map[string]extension.TaskDefinition{task: {Declares: declaration}},
		},
		JobDef: &extension.JobDefinition{Name: name, Cache: true, Writes: writes},
		Step:   &extension.PipelineStep{Task: task},
	}
}

func dirDeclaration(root, path string, optional bool) extension.DeclaredOutput {
	return extension.DeclaredOutput{Kind: extension.OutputKindDirectory, Root: root, Path: path, OptionalEmpty: optional}
}

func fileDeclaration(root, path string, optional bool) extension.DeclaredOutput {
	return extension.DeclaredOutput{Kind: extension.OutputKindFile, Root: root, Path: path, OptionalEmpty: optional}
}

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A v2 source writer still executes uncached: no inferred directory capture
// remains after the v3 cutover.
func TestV2SourceWriterStaysUncachedWithoutDeclaration(t *testing.T) {
	sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
	job := declaredJob("lint~fix", "fix", nil, sources)
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	if IsCacheEnabled(job, CacheBypass{}) {
		t.Fatal("a v2 source writer reached a cache path without a declaration")
	}
}

// Capture resolves every declared command-output path, including one at the
// root of the directory. It used to refuse a closed set of "unowned merge
// point" paths; a later fix gave those paths owners, so the refusal would now reject
// the declaration that fixed them. Who may own a path is decided where
// ownership is decided — the manifest validator and validatePlanContract.
func TestResolveDeclaredOutputsResolvesAnOwnedCommandOutputFile(t *testing.T) {
	for _, path := range []string{"metadata.json", "npm/metadata.json"} {
		job := declaredJob("package~owned", "owned", &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"manifest": fileDeclaration(extension.OutputRootCommandOutput, path, false),
			},
		})
		plans, err := ResolveDeclaredOutputs(WorkspaceRoots{Root: "/ws"}, job, nil)
		if err != nil {
			t.Fatalf("declared command-output file %q failed to resolve: %v", path, err)
		}
		if len(plans) != 1 || plans[0].Spec.Path != path {
			t.Fatalf("resolved %+v, want the declared path %q", plans, path)
		}
	}
}

func TestResolveDeclaredOutputsResolvesRootsAbsolutely(t *testing.T) {
	job := declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"cmd":  dirDeclaration(extension.OutputRootCommandOutput, "lib", false),
			"proj": dirDeclaration(extension.OutputRootProject, ".gen", false),
			"ws":   fileDeclaration(extension.OutputRootWorkspace, "bun.lock", false),
		},
	})
	plans, err := ResolveDeclaredOutputs(WorkspaceRoots{Root: "/ws"}, job, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := map[string]string{}
	for _, plan := range plans {
		got[plan.Spec.ID] = plan.Src
	}
	want := map[string]string{
		"cmd":  filepath.Join("/ws", ".putnami", "out", captureTestProject, "build", "lib"),
		"proj": filepath.Join("/ws", captureTestProject, ".gen"),
		"ws":   filepath.Join("/ws", "bun.lock"),
	}
	for id, wantPath := range want {
		if got[id] != wantPath {
			t.Errorf("output %q resolved to %q, want %q", id, got[id], wantPath)
		}
	}
}

// Capture and ownership must honor the SAME carve-outs. A cede on a pathFrom
// output is undecidable from the manifest — validation rejects it, and the
// ownership refs drop it — so capture ignores it too. Honoring it on one side
// only is how a subtree ends up owned by one task and captured by neither.
func TestDeclaredCarveOutsAreIgnoredOnAPortPath(t *testing.T) {
	port := extension.DeclaredOutput{
		Kind:     extension.OutputKindDirectory,
		PathFrom: "clientOutputs",
		Excludes: []string{"clients/go/vendor"},
	}
	if got := declaredExcludesOf(port); got != nil {
		t.Errorf("declaredExcludesOf(port output) = %v, want nothing", got)
	}
	if got := outputRelativeExcludes("clients/go", declaredExcludesOf(port)); got != nil {
		t.Errorf("capture honored %v on a runtime-reported path the ownership check drops", got)
	}

	// Control: the same carve-out on a literal path IS honored and is rebased
	// onto the output, so the assertions above are about the port rather than
	// about carve-outs in general.
	literal := extension.DeclaredOutput{
		Kind:     extension.OutputKindDirectory,
		Root:     extension.OutputRootProject,
		Path:     ".gen",
		Excludes: []string{".gen/migration-bundle"},
	}
	got := outputRelativeExcludes(".gen", declaredExcludesOf(literal))
	if len(got) != 1 || got[0] != "migration-bundle" {
		t.Errorf("literal-path carve-out = %v, want the output-relative subpath", got)
	}
}
