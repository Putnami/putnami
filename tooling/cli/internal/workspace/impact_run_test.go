package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/git"
)

func impactNameSet(ps []*Project) map[string]bool {
	m := make(map[string]bool, len(ps))
	for _, p := range ps {
		m[p.Name] = true
	}
	return m
}

// gitInitWithBaseline creates a git repo at dir with one committed file and a
// "main" branch, so git.DiffWorkingTree has a valid baseline to diff against.
// Untracked files created afterwards are reported as changes.
func gitInitWithBaseline(t *testing.T, dir string) {
	t.Helper()
	runGitForImpact(t, dir, "init")
	runGitForImpact(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForImpact(t, dir, "add", "-A")
	runGitForImpact(t, dir, "commit", "-m", "baseline")
	runGitForImpact(t, dir, "branch", "-M", "main")
}

func TestGitInitWithBaselineIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		dir := t.TempDir()
		gitInitWithBaseline(t, dir)
		return dir
	})
}

func runGitForImpact(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := gittest.Command(dir, args...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestImpactedProjects_DirectAndTransitive(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "changed-files-select-owners-and-transitive-dependents")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
		{ID: "/packages/utils", Name: "utils", Path: "packages/utils"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	// Change a file in lib (untracked). app depends on lib, so both are impacted.
	libFile := filepath.Join(tmp, "packages", "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ImpactedProjects(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedProjects: %v", err)
	}
	names := impactNameSet(got)
	if !names["lib"] || !names["app"] {
		t.Errorf("expected lib (direct) and app (transitive) impacted, got %v", names)
	}
	if names["utils"] {
		t.Errorf("utils should not be impacted, got %v", names)
	}
}

// The selection carries the trace the shared calculation produced, so a caller
// that sees "lib and app" can also see that lib was seeded by the changed file
// and app was reached from lib over a dependency edge.
func TestImpactedSelection_TraceExplainsEverySelectedProject(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "the-selection-explains-every-project-it-holds")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
		{ID: "/packages/utils", Name: "utils", Path: "packages/utils"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}
	libFile := filepath.Join(tmp, "packages", "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	selection, err := ImpactedSelectionForBaseline(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	if len(selection.Projects) != 2 {
		t.Fatalf("selected %v, want lib and app", selection.Projects)
	}
	for _, p := range selection.Projects {
		_, seeded := selection.Trace.Seeds[p.ID]
		_, propagated := selection.Trace.Edges[p.ID]
		if seeded == propagated {
			t.Errorf("%s: seeded=%v propagated=%v, want exactly one explanation", p.ID, seeded, propagated)
		}
	}
	wantSeeds := []ImpactSeed{{File: "packages/lib/lib.go", Kind: ImpactSeedPathOwner, Via: "packages/lib"}}
	if got := selection.Trace.Seeds["/packages/lib"]; !slices.Equal(got, wantSeeds) {
		t.Errorf("seeds of lib = %+v, want %+v", got, wantSeeds)
	}
	if got, want := selection.Trace.Edges["/packages/app"], (ImpactEdge{From: "/packages/lib", Kind: ImpactEdgeDependency}); got != want {
		t.Errorf("edge into app = %+v, want %+v", got, want)
	}
	if explanation := selection.TraceExplanation(); len(explanation) != 3 || explanation[0] != "  --impacted: 1 changed file(s) reached 1 project(s) directly; propagation added 1 (dependency 1)" {
		t.Errorf("explanation = %q", explanation)
	}
}

// The record a run keeps states the diff a second machine would need to
// reproduce the selection: the merge base, every changed file, which of them no
// commit records, and every seed and edge. A root file the run's own
// bootstrap rewrote is exactly the shape that made a hosted plan differ from a
// local one: uncommitted, and at the workspace root.
func TestImpactedSelection_TraceRecordStatesTheDiffAndEveryReason(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "the-selection-explains-every-project-it-holds")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)
	runGitForImpact(t, tmp, "checkout", "-b", "feature")

	projects := []*Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}
	write := func(rel string) {
		t.Helper()
		path := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rel+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("packages/lib/lib.go")
	runGitForImpact(t, tmp, "add", "-A")
	runGitForImpact(t, tmp, "commit", "-m", "change lib")
	write("putnami.lock.json")

	selection, err := ImpactedSelectionForBaseline(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	record := selection.TraceRecord()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var got ImpactTraceRecord
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode record %s: %v", encoded, err)
	}
	mergeBase, err := gitread.ResolveCommit(tmp, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Baseline != "main" || got.DiffBase != mergeBase {
		t.Errorf("baseline = %q, diffBase = %q; want main at %s", got.Baseline, got.DiffBase, mergeBase)
	}
	if want := []string{"packages/lib/lib.go", "putnami.lock.json"}; !slices.Equal(got.ChangedFiles, want) {
		t.Errorf("changedFiles = %v, want %v", got.ChangedFiles, want)
	}
	if want := []string{"putnami.lock.json"}; !slices.Equal(got.UncommittedFiles, want) || !slices.Equal(got.UnownedRootFiles, want) {
		t.Errorf("uncommittedFiles = %v, unownedRootFiles = %v; want both %v", got.UncommittedFiles, got.UnownedRootFiles, want)
	}
	if len(got.Seeds) != 1 || got.Seeds[0].Project != "/packages/lib" || got.Seeds[0].File != "packages/lib/lib.go" || got.Seeds[0].Kind != "path-owner" {
		t.Errorf("seeds = %+v, want lib seeded by its own file", got.Seeds)
	}
	if len(got.Edges) != 1 || got.Edges[0].Project != "/packages/app" || got.Edges[0].From != "/packages/lib" || got.Edges[0].Kind != "dependency" {
		t.Errorf("edges = %+v, want app reached from lib", got.Edges)
	}
}

func TestImpactedProjects_RenameAcrossProjectsIncludesBothOwners(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "renames-and-assets-select-every-owner")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	from := "apps/from/source.go"
	to := "packages/to/source.go"
	if err := os.MkdirAll(filepath.Join(tmp, "apps", "from"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, from), []byte("package moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForImpact(t, tmp, "add", "--", from)
	runGitForImpact(t, tmp, "commit", "-m", "add source project file")
	runGitForImpact(t, tmp, "checkout", "-b", "feature")

	if err := os.MkdirAll(filepath.Join(tmp, "packages", "to"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitForImpact(t, tmp, "mv", "--", from, to)
	runGitForImpact(t, tmp, "commit", "-m", "move project file")
	// This is the configuration that would otherwise cause Git to return only
	// the destination path for a pure rename.
	runGitForImpact(t, tmp, "config", "diff.renames", "true")

	projects := []*Project{
		{ID: "/apps/from", Name: "from", Path: "apps/from"},
		{ID: "/packages/to", Name: "to", Path: "packages/to"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	got, err := ImpactedProjects(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedProjects: %v", err)
	}
	names := impactNameSet(got)
	if !names["from"] || !names["to"] {
		t.Errorf("expected both rename owners to be impacted, got %v", names)
	}
}

// TestResolveImpactedBaselineDetailed_EpicBranchesFromConfig pins the config
// threading: workspace epicBranches reach the git resolution, so a sub-branch
// of a configured epic measures against the epic, not the trunk.
func TestResolveImpactedBaselineDetailed_EpicBranchesFromConfig(t *testing.T) {
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)
	runGitForImpact(t, tmp, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGitForImpact(t, tmp, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGitForImpact(t, tmp, "checkout", "-b", "epic/store")
	if err := os.WriteFile(filepath.Join(tmp, "epic.txt"), []byte("epic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForImpact(t, tmp, "add", "epic.txt")
	runGitForImpact(t, tmp, "commit", "-m", "epic work")
	runGitForImpact(t, tmp, "update-ref", "refs/remotes/origin/epic/store", "HEAD")
	runGitForImpact(t, tmp, "checkout", "-b", "feat")

	ws := &Workspace{Root: tmp, Config: &wsproto.Config{EpicBranches: []string{"epic/*"}}}
	resolved, err := ResolveImpactedBaselineDetailed(ws, "")
	if err != nil {
		t.Fatalf("ResolveImpactedBaselineDetailed: %v", err)
	}
	if resolved.Ref != "origin/epic/store" {
		t.Errorf("resolved = %q, want origin/epic/store", resolved.Ref)
	}
	if resolved.Source != git.BaselineSourceEpic {
		t.Errorf("source = %q, want %q", resolved.Source, git.BaselineSourceEpic)
	}
}

func TestImpactedProjects_UsesWorkspaceConfiguredBaseline(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "safe-baseline", "the-workspace-configured-baseline-wins")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{{ID: "/packages/lib", Name: "lib", Path: "packages/lib"}}
	ws := &Workspace{
		Root:     tmp,
		Config:   &wsproto.Config{Baseline: "main"},
		Projects: projects,
		Graph:    BuildGraph(projects),
	}

	libFile := filepath.Join(tmp, "packages", "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, resolvedBaseline, err := ImpactedProjectsWithBaseline(ws, "")
	if err != nil {
		t.Fatalf("ImpactedProjectsWithBaseline: %v", err)
	}
	if resolvedBaseline != "main" {
		t.Errorf("resolved baseline = %q, want main", resolvedBaseline)
	}
	if names := impactNameSet(got); !names["lib"] {
		t.Errorf("expected lib impacted, got %v", names)
	}
}

func TestImpactedProjects_ResolvesOriginHeadWithoutLocalMain(t *testing.T) {
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)
	runGitForImpact(t, tmp, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGitForImpact(t, tmp, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGitForImpact(t, tmp, "checkout", "-b", "feature")
	runGitForImpact(t, tmp, "branch", "-D", "main")

	projects := []*Project{{ID: "/packages/lib", Name: "lib", Path: "packages/lib"}}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	libFile := filepath.Join(tmp, "packages", "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, resolvedBaseline, err := ImpactedProjectsWithBaseline(ws, "")
	if err != nil {
		t.Fatalf("ImpactedProjectsWithBaseline: %v", err)
	}
	if resolvedBaseline != "origin/main" {
		t.Errorf("resolved baseline = %q, want origin/main", resolvedBaseline)
	}
	if names := impactNameSet(got); !names["lib"] {
		t.Errorf("expected lib impacted, got %v", names)
	}
}

func TestImpactedProjects_AssetPathTriggersConsumer(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "renames-and-assets-select-every-owner")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{
		{ID: "/web", Name: "web", Path: "web", Config: &wsproto.ProjectConfig{
			Build: &wsproto.BuildConfig{Assets: []wsproto.BuildAsset{{From: "/shared/icons"}}},
		}},
		{ID: "/other", Name: "other", Path: "other"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	// Change a file under the shared asset dir that "web" consumes.
	assetFile := filepath.Join(tmp, "shared", "icons", "a.svg")
	if err := os.MkdirAll(filepath.Dir(assetFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assetFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ImpactedProjects(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedProjects: %v", err)
	}
	names := impactNameSet(got)
	if !names["web"] {
		t.Errorf("web should be impacted via its declared asset path, got %v", names)
	}
	if names["other"] {
		t.Errorf("other should not be impacted, got %v", names)
	}
}

func TestImpactedProjects_NoChangesReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{{ID: "/a", Name: "a", Path: "a"}}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	got, err := ImpactedProjects(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedProjects: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no impacted projects on a clean tree, got %v", impactNameSet(got))
	}
}

// A workspace-root change is the one diff shape whose selection cannot be read
// off the project count: the paths belong to no project, so a non-empty diff
// selects NOTHING and reports it exactly like a clean tree does.
//
// This is measured behavior, not a hypothesis. On this repository's downstream
// workspace (86 projects) a diff limited to the Go or TypeScript dependency
// manifests at the root selects 0 projects, and has at every version of this
// selector — the widening that would attribute such a file to projects lives
// behind ChangeImpactOptions, and `--impacted` passes it empty on purpose.
//
// What the selection cannot do, it must SAY. The test pins the evidence rather
// than the count, so "why did the gate verify nothing?" is answerable from the
// run itself.
func TestImpactedSelection_ReportsUnownedRootFiles(t *testing.T) {
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	writeImpactFile(t, tmp, "go.work.sum", "example.com/dep v1.0.0 h1:new=\n")
	writeImpactFile(t, tmp, "bun.lock", "{\"lockfileVersion\":1}\n")

	selection, err := ImpactedSelectionForBaseline(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	if len(selection.Projects) != 0 {
		t.Fatalf("root-only change selected %v, want none — the mapping attributes a root path to no project",
			impactNameSet(selection.Projects))
	}
	if len(selection.ChangedFiles) != 2 {
		t.Fatalf("changed files = %v, want the two root files", selection.ChangedFiles)
	}
	want := []string{"bun.lock", "go.work.sum"}
	if !slices.Equal(selection.UnownedRootFiles, want) {
		t.Errorf("unowned root files = %v, want %v (sorted, so the explanation is stable)",
			selection.UnownedRootFiles, want)
	}
}

// The under-selection that hides inside a NORMAL run: a dependency-manifest
// edit riding along with ordinary source changes. The projects owning the
// source are selected and the run looks entirely healthy, while the root file
// contributed nothing at all. The evidence must survive a non-empty selection,
// or the only case anybody notices is the one where the gate already looks
// suspicious.
func TestImpactedSelection_ReportsUnownedRootFilesBesideASelection(t *testing.T) {
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	writeImpactFile(t, tmp, filepath.Join("packages", "lib", "lib.go"), "package lib\n")
	writeImpactFile(t, tmp, "bun.lock", "{\"lockfileVersion\":1}\n")

	selection, err := ImpactedSelectionForBaseline(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	names := impactNameSet(selection.Projects)
	if !names["lib"] || !names["app"] {
		t.Fatalf("selected %v, want lib and its dependent app from the source change", names)
	}
	if !slices.Equal(selection.UnownedRootFiles, []string{"bun.lock"}) {
		t.Errorf("unowned root files = %v, want [bun.lock] — the root file rode along unattributed",
			selection.UnownedRootFiles)
	}
}

// Only ROOT paths are evidence. An unowned file inside a directory is ordinary
// — repository configuration, top-level docs — and naming those would bury the
// case that matters under a list nobody reads.
func TestImpactedSelection_UnownedNestedFilesAreNotReported(t *testing.T) {
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{{ID: "/packages/lib", Name: "lib", Path: "packages/lib"}}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	writeImpactFile(t, tmp, filepath.Join(".github", "workflows", "ci.yml"), "on: push\n")

	selection, err := ImpactedSelectionForBaseline(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	if len(selection.UnownedRootFiles) != 0 {
		t.Errorf("unowned root files = %v, want none for a nested path", selection.UnownedRootFiles)
	}
}

func writeImpactFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A run writes its own state into the tree: the build stamp
// <project>/.gen/version.json and the session files under .putnami/. Neither is
// a change the operator made, so the next --impacted run must not select the
// projects they sit in — whether or not .gitignore lists them.
func TestImpactedSelection_IgnoresThePathsPutnamiWrites(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-ignores-own-writes", "the-diff-leaves-out-what-putnami-and-the-run-wrote")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)

	projects := []*Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app"},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	writeImpactFile(t, tmp, filepath.Join("packages", "lib", "lib.go"), "package lib\n")
	writeImpactFile(t, tmp, filepath.Join("packages", "app", ".gen", "version.json"), "{\"buildTime\":\"now\"}\n")
	writeImpactFile(t, tmp, filepath.Join(".putnami", "sessions", "last.json"), "{}\n")

	selection, err := ImpactedSelectionForBaseline(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	if names := impactNameSet(selection.Projects); len(names) != 1 || !names["lib"] {
		t.Fatalf("selected %v, want only lib — app's untracked .gen/version.json is Putnami's own output", names)
	}
	if !slices.Equal(selection.ChangedFiles, []string{"packages/lib/lib.go"}) {
		t.Errorf("changed files = %v, want only the source edit", selection.ChangedFiles)
	}
	if len(selection.UnownedRootFiles) != 0 {
		t.Errorf("unowned root files = %v, want none — .putnami/ is not an operator change", selection.UnownedRootFiles)
	}
}

// A run's first-use install can write a file before the selection reads the
// tree: on an adopted repository, a lockfile the repository does not commit.
// The selection measures the tree the operator left — what was uncommitted
// when the run started — so that file selects nothing, while an edit the
// operator made before the run, and a path a commit since the base changed,
// still count.
func TestImpactedSelection_LeavesOutTheFilesTheRunWrote(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-ignores-own-writes", "the-diff-leaves-out-what-putnami-and-the-run-wrote")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)
	writeImpactFile(t, tmp, filepath.Join("packages", "web", "pnpm-lock.yaml"), "lockfileVersion: 9\n")
	runGitForImpact(t, tmp, "add", "-A")
	runGitForImpact(t, tmp, "commit", "-m", "web lock")

	projects := []*Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app"},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
		{ID: "/packages/web", Name: "web", Path: "packages/web"},
	}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	// The operator's edit, before the run.
	writeImpactFile(t, tmp, filepath.Join("packages", "lib", "lib.go"), "package lib\n")
	runStart, err := git.UncommittedPaths(tmp)
	if err != nil {
		t.Fatalf("UncommittedPaths: %v", err)
	}
	// The run's install, after: a new lockfile in app, and a rewrite of the
	// lockfile the base..HEAD commit changed in web.
	writeImpactFile(t, tmp, filepath.Join("packages", "app", "bun.lock"), "{}\n")
	writeImpactFile(t, tmp, filepath.Join("packages", "web", "pnpm-lock.yaml"), "lockfileVersion: 9.1\n")

	selection, err := ImpactedSelectionWithOptions(ws, "HEAD~1", ImpactedSelectionOptions{RunStart: runStart})
	if err != nil {
		t.Fatalf("ImpactedSelectionWithOptions: %v", err)
	}
	if names := impactNameSet(selection.Projects); len(names) != 2 || !names["lib"] || !names["web"] {
		t.Fatalf("selected %v, want lib (the operator's edit) and web (the commit) — not app (the run's lockfile)", names)
	}
	if !slices.Equal(selection.RunWrittenFiles, []string{"packages/app/bun.lock"}) {
		t.Errorf("run-written files = %v, want [packages/app/bun.lock]", selection.RunWrittenFiles)
	}
	if slices.Contains(selection.ChangedFiles, "packages/app/bun.lock") ||
		slices.Contains(selection.UncommittedFiles, "packages/app/bun.lock") {
		t.Errorf("changed %v / uncommitted %v still hold the run's lockfile", selection.ChangedFiles, selection.UncommittedFiles)
	}
	if note := selection.RunWrittenFilesExplanation(); note == "" {
		t.Error("the selection does not say it left the run's lockfile out")
	}

	// Without a recorded start, every uncommitted path counts.
	all, err := ImpactedSelectionWithOptions(ws, "HEAD~1", ImpactedSelectionOptions{})
	if err != nil {
		t.Fatalf("ImpactedSelectionWithOptions: %v", err)
	}
	if names := impactNameSet(all.Projects); !names["app"] {
		t.Errorf("with no recorded start, selected %v, want app too", names)
	}
}

// Every --impacted run names its base and where it came from, whatever the
// tier: the same change selects a handful of projects against HEAD~1 and the
// whole workspace against the trunk.
func TestImpactedSelection_BaselineExplanationNamesTheBaseAndItsOrigin(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "safe-baseline", "every-run-states-its-base-and-its-origin")
	tmp := t.TempDir()
	gitInitWithBaseline(t, tmp)
	head, err := gitread.ResolveCommit(tmp, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	writeImpactFile(t, tmp, "next.txt", "next\n")
	runGitForImpact(t, tmp, "add", "-A")
	runGitForImpact(t, tmp, "commit", "-m", "next")

	projects := []*Project{{ID: "/packages/lib", Name: "lib", Path: "packages/lib"}}
	ws := &Workspace{Root: tmp, Projects: projects, Graph: BuildGraph(projects)}

	explicit, err := ImpactedSelectionForBaseline(ws, "HEAD~1")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	want := "  --impacted: diffs against HEAD~1 at " + head[:7] + " (from --baseline)."
	if got := explicit.BaselineExplanation(); got != want {
		t.Errorf("explicit baseline line = %q, want %q", got, want)
	}

	ws.Config = &wsproto.Config{Baseline: "HEAD~1"}
	configured, err := ImpactedSelectionForBaseline(ws, "")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	if configured.BaselineSource != git.BaselineSourceConfig {
		t.Errorf("source = %q, want %q", configured.BaselineSource, git.BaselineSourceConfig)
	}
	if got := configured.BaselineExplanation(); !strings.Contains(got, "from the workspace config baseline") {
		t.Errorf("configured baseline line = %q, want it to name the workspace config", got)
	}

	ws.Config = nil
	runGitForImpact(t, tmp, "update-ref", "refs/remotes/origin/main", head)
	trunk, err := ImpactedSelectionForBaseline(ws, "")
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaseline: %v", err)
	}
	if got := trunk.BaselineExplanation(); !strings.Contains(got, "origin/main") || !strings.Contains(got, "remote trunk") {
		t.Errorf("trunk baseline line = %q, want it to name origin/main and the trunk tier", got)
	}
	if trunk.BaselineIsFallback() {
		t.Error("the remote trunk is not a fallback tier")
	}
}
