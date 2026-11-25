package scheduler

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// candidatesJob is a job of project whose task keys the inputs and declares
// the runtime inputs; cached says whether its cache is enabled.
func candidatesJob(project *workspace.Project, key extension.TaskCacheKey, cached bool) *jobmodel.ScheduledJob {
	ext := &extension.ExtensionDescription{Name: "@test/ext", Tasks: map[string]extension.TaskDefinition{"check": {}}}
	def := extension.JobDefinition{
		Name: "validate~check", ExtensionName: ext.Name, Cache: cached,
		TaskCachePolicy: &extension.TaskCachePolicy{Key: &key},
	}
	return &jobmodel.ScheduledJob{Project: project, Extension: ext,
		Step: &extension.PipelineStep{ID: "check", Task: "check"}, JobDef: &def}
}

func candidatePaths(files []protocoljob.CandidateFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path+" "+file.Mode)
	}
	return paths
}

// The candidate cut a task declares is the one a `git:` input keys: tracked
// paths with their index modes and untracked paths no ignore rule excludes,
// an untracked file a file rule ignores included in neither. The key holds the
// cut under the project, so adding a candidate there moves it and an ignored
// file or a candidate elsewhere does not; a task that declares nothing keeps
// its key.
func TestACandidateCutIsKeyedAndCarried(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-candidate-cut-is-keyed-and-carried")
	root := treeFactsRepo(t)
	writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\n*.log\n")
	writeFileAt(t, filepath.Join(root, "app", "run.sh"), "#!/bin/sh\n")
	treeKeyedGit(t, root, "add", "app/run.sh", ".gitignore")
	treeKeyedGit(t, root, "update-index", "--chmod=+x", "app/run.sh")
	writeFileAt(t, filepath.Join(root, "app", "vendor", "new.py"), "x = 1\n")
	writeFileAt(t, filepath.Join(root, "app", "debug.log"), "secret\n")
	ws := treeFactsWorkspace(root)
	app := ws.ProjectByID("/app")
	declaring := candidatesJob(app, extension.TaskCacheKey{Files: []string{"main.go"},
		Runtime: []string{extproto.RuntimeInputCandidateFiles}}, true)
	silent := candidatesJob(app, extension.TaskCacheKey{Files: []string{"main.go"}}, true)

	tree := jobTree(cachekey.RunTreeFacts(newTreeFactsCache(t), root), ws, declaring, nil)
	if tree == nil || !slices.Equal(tree.CandidateDirectories, []string{"app"}) {
		t.Fatalf("tree = %+v, want the candidate cut of app", tree)
	}
	want := []string{"app/main.go 100644", "app/run.sh 100755", "app/vendor/new.py untracked"}
	if got := candidatePaths(tree.CandidateFiles); !slices.Equal(got, want) {
		t.Fatalf("candidateFiles = %v, want %v", got, want)
	}
	if got := jobTree(cachekey.RunTreeFacts(newTreeFactsCache(t), root), ws, silent, nil); got != nil {
		t.Fatalf("a task that declares no fact got %+v", got)
	}

	before := treeFactKeyOf(t, ws, declaring, newTreeFactsCache(t))
	silentBefore := treeFactKeyOf(t, ws, silent, newTreeFactsCache(t))
	writeFileAt(t, filepath.Join(root, "app", "more.log"), "ignored\n")
	writeFileAt(t, filepath.Join(root, "other", "new.go"), "package other\n")
	if after := treeFactKeyOf(t, ws, declaring, newTreeFactsCache(t)); after != before {
		t.Error("an ignored file under the project, or a candidate outside it, moved the key")
	}
	writeFileAt(t, filepath.Join(root, "app", "vendor", "more.py"), "y = 2\n")
	if after := treeFactKeyOf(t, ws, declaring, newTreeFactsCache(t)); after == before {
		t.Error("a new candidate under the project kept the key")
	}
	if silentAfter := treeFactKeyOf(t, ws, silent, newTreeFactsCache(t)); silentAfter != silentBefore {
		t.Error("a task that declares no fact moved its key with the cut")
	}
}

// A task whose closure input holds a `git:` pattern receives the cut of each
// member of its dependency closure; a workspace task receives the whole cut.
func TestACandidateCutCoversTheKeyedClosure(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-candidate-cut-is-keyed-and-carried")
	root := treeFactsRepo(t)
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	other := &workspace.Project{ID: "/other", Name: "other", Path: "other"}
	ws := testWorkspace(root, lib, app, other)
	ws.Graph = workspace.BuildGraph(ws.Projects)

	closure := candidatesJob(app, extension.TaskCacheKey{Files: []string{"git:**/*"}, ClosureFiles: []string{"git:**/*"},
		Runtime: []string{extproto.RuntimeInputCandidateFiles}}, true)
	tree := jobTree(cachekey.RunTreeFacts(newTreeFactsCache(t), root), ws, closure, nil)
	if tree == nil || !slices.Equal(tree.CandidateDirectories, []string{"app", "lib"}) {
		t.Fatalf("tree = %+v, want the cut of app and lib", tree)
	}
	if got, want := candidatePaths(tree.CandidateFiles), []string{"app/main.go 100644", "lib/lib.go 100644"}; !slices.Equal(got, want) {
		t.Fatalf("candidateFiles = %v, want %v", got, want)
	}
	before := treeFactKeyOf(t, ws, closure, newTreeFactsCache(t))
	writeFileAt(t, filepath.Join(root, "lib", "more.go"), "package lib\n")
	if after := treeFactKeyOf(t, ws, closure, newTreeFactsCache(t)); after == before {
		t.Error("a new candidate in a closure member kept the key")
	}

	workspaceTask := candidatesJob(app, extension.TaskCacheKey{Files: []string{"git:**"},
		Runtime: []string{extproto.RuntimeInputCandidateFiles}}, true)
	workspaceTask.SelectedProjects = []*workspace.Project{app}
	if tree := jobTree(cachekey.RunTreeFacts(newTreeFactsCache(t), root), ws, workspaceTask, nil); tree == nil ||
		!slices.Equal(tree.CandidateDirectories, []string{"."}) || len(tree.CandidateFiles) != 5 {
		t.Fatalf("workspace task tree = %+v, want the whole cut", tree)
	}
}

// The workspace's cut reaches a project task through one file of the run,
// which reads back what the key's digest holds; a candidate anywhere in the
// workspace moves that key.
func TestTheWorkspaceCutIsOneFileOfTheRun(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "the-workspace-cut-is-one-file-of-the-run")
	root := treeFactsRepo(t)
	ws := treeFactsWorkspace(root)
	key := extension.TaskCacheKey{WorkspaceFiles: []string{"git:**"}, Runtime: []string{extproto.RuntimeInputWorkspaceCandidateFiles}}
	app := candidatesJob(ws.ProjectByID("/app"), key, true)
	lib := candidatesJob(ws.ProjectByID("/lib"), key, true)

	facts := cachekey.RunTreeFacts(newTreeFactsCache(t), root)
	appTree, libTree := jobTree(facts, ws, app, nil), jobTree(facts, ws, lib, nil)
	if appTree == nil || appTree.WorkspaceCandidates == nil || libTree == nil || *libTree.WorkspaceCandidates != *appTree.WorkspaceCandidates {
		t.Fatalf("trees = %+v, %+v, want one listing file for both", appTree, libTree)
	}
	files, err := protocoljob.ReadCandidateListing(appTree.WorkspaceCandidates)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore 100644", "app/main.go 100644", "lib/lib.go 100644", "other/other.go 100644"}
	if got := candidatePaths(files); !slices.Equal(got, want) {
		t.Fatalf("listing = %v, want %v", got, want)
	}
	before := treeFactKeyOf(t, ws, app, newTreeFactsCache(t))
	writeFileAt(t, filepath.Join(root, "other", "new.go"), "package other\n")
	if after := treeFactKeyOf(t, ws, app, newTreeFactsCache(t)); after == before {
		t.Error("a new candidate elsewhere in the workspace kept the key")
	}
	facts.Close()
	if _, err := protocoljob.ReadCandidateListing(appTree.WorkspaceCandidates); err == nil {
		t.Error("the listing file outlived the run")
	}
}

// The change history since the impacted baseline reaches an uncached job of
// an impacted run, and never a cached job or a run without a baseline.
func TestChangeHistoryReachesOnlyAnUncachedJob(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "history-reaches-only-an-uncached-job")
	root := treeFactsRepo(t)
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"), `{"name": "ws"}`+"\n")
	treeKeyedGit(t, root, "add", "putnami.workspace.json")
	treeKeyedGit(t, root, "commit", "-q", "-m", "chore: the workspace config")
	base := strings.TrimSpace(treeKeyedGit(t, root, "rev-parse", "HEAD"))
	treeKeyedGit(t, root, "checkout", "-q", "-b", "change")
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main // changed\n")
	treeKeyedGit(t, root, "commit", "-q", "-am", "feat(app)!: drop the old flag\n\nBREAKING CHANGE: the flag is gone")
	ws := treeFactsWorkspace(root)
	key := extension.TaskCacheKey{Runtime: []string{extproto.RuntimeInputChangeHistory}}
	uncached := candidatesJob(ws.ProjectByID("/app"), key, false)
	cached := candidatesJob(ws.ProjectByID("/app"), key, true)
	impacted := &protocoljob.Selection{Mode: protocoljob.SelectionModeImpacted, Baseline: "main"}

	facts := cachekey.RunTreeFacts(newTreeFactsCache(t), root)
	tree := jobTree(facts, ws, uncached, impacted)
	if tree == nil || tree.ChangeHistory == nil {
		t.Fatalf("tree = %+v, want the change history", tree)
	}
	history := tree.ChangeHistory
	if history.Baseline != "main" || history.Shallow || history.MergeBase != base || len(history.Commits) != 1 ||
		history.Commits[0].Subject != "feat(app)!: drop the old flag" || history.Commits[0].Body != "BREAKING CHANGE: the flag is gone" ||
		history.WorkspaceConfig == nil || *history.WorkspaceConfig != `{"name": "ws"}`+"\n" {
		t.Fatalf("history = %+v", history)
	}
	if got := jobTree(facts, ws, cached, impacted); got != nil {
		t.Errorf("a cached job got %+v", got)
	}
	if got := jobTree(facts, ws, uncached, &protocoljob.Selection{Mode: "all"}); got != nil {
		t.Errorf("a run without an impacted baseline got %+v", got)
	}
	silent := candidatesJob(ws.ProjectByID("/app"), extension.TaskCacheKey{}, false)
	if treeFactKeyOf(t, ws, uncached, newTreeFactsCache(t)) != treeFactKeyOf(t, ws, silent, newTreeFactsCache(t)) {
		t.Error("the change history moved a key")
	}
}

// An untracked nested repository is a candidate Git lists as its directory:
// the cut names it without a trailing slash, so the context it reaches
// validates, and a job that opens the cut at it asks Git.
func TestANestedRepositoryIsACleanCandidate(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-candidate-cut-is-keyed-and-carried")
	root := treeFactsRepo(t)
	writeFileAt(t, filepath.Join(root, "app", "nested", "x.go"), "package x\n")
	treeKeyedGit(t, filepath.Join(root, "app", "nested"), "init", "-q")
	ws := treeFactsWorkspace(root)
	job := candidatesJob(ws.ProjectByID("/app"), extension.TaskCacheKey{Files: []string{"main.go"},
		Runtime: []string{extproto.RuntimeInputCandidateFiles}}, true)
	tree := jobTree(cachekey.RunTreeFacts(newTreeFactsCache(t), root), ws, job, nil)
	if tree == nil {
		t.Fatal("no tree")
	}
	if got, want := candidatePaths(tree.CandidateFiles), []string{"app/main.go 100644", "app/nested untracked"}; !slices.Equal(got, want) {
		t.Fatalf("candidateFiles = %v, want %v", got, want)
	}
	if diags := protocoljob.ValidateCandidateFiles("tree.candidateFiles", tree.CandidateDirectories, tree.CandidateFiles); len(diags) != 0 {
		t.Fatalf("the cut does not validate: %v", diags)
	}
}

// A lint-docs task keys the workspace's cut through `git:**`; declaring
// workspaceCandidateFiles beside it adds the listing's digest, which moves the
// key only with what the cut lists. Over a sequence of edits the declaring key
// moves exactly when the plain key does, except when a file the cut already
// lists is staged: its index mode joins the listing.
func TestAWorkspaceCutDeclarationMovesTheKeyWithTheCut(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "the-workspace-cut-is-one-file-of-the-run")
	root := treeFactsRepo(t)
	ws := treeFactsWorkspace(root)
	app := ws.ProjectByID("/app")
	plain := candidatesJob(app, extension.TaskCacheKey{Files: []string{"README.md"}, WorkspaceFiles: []string{"git:**"}}, true)
	declaring := candidatesJob(app, extension.TaskCacheKey{Files: []string{"README.md"}, WorkspaceFiles: []string{"git:**"},
		Runtime: []string{extproto.RuntimeInputWorkspaceCandidateFiles}}, true)
	keys := func() (string, string) {
		return treeFactKeyOf(t, ws, plain, newTreeFactsCache(t)), treeFactKeyOf(t, ws, declaring, newTreeFactsCache(t))
	}
	plainBefore, declaringBefore := keys()
	if plainBefore == declaringBefore {
		t.Fatal("the declaration added nothing to the key")
	}
	for _, step := range []struct {
		name               string
		edit               func()
		plain, declaration bool
	}{
		{"an ignored file", func() { writeFileAt(t, filepath.Join(root, "app", "dist", "more.js"), "x\n") }, false, false},
		{"a new candidate", func() { writeFileAt(t, filepath.Join(root, "lib", "new.md"), "# new\n") }, true, true},
		{"an edited candidate", func() { writeFileAt(t, filepath.Join(root, "lib", "lib.go"), "package lib // edited\n") }, true, true},
		{"a staged candidate", func() { treeKeyedGit(t, root, "add", "lib/new.md") }, false, true},
	} {
		step.edit()
		plainAfter, declaringAfter := keys()
		if moved := plainAfter != plainBefore; moved != step.plain {
			t.Errorf("%s: the plain key moved = %v, want %v", step.name, moved, step.plain)
		}
		if moved := declaringAfter != declaringBefore; moved != step.declaration {
			t.Errorf("%s: the declaring key moved = %v, want %v", step.name, moved, step.declaration)
		}
		plainBefore, declaringBefore = plainAfter, declaringAfter
	}
}
