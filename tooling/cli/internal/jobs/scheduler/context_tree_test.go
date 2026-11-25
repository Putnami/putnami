package scheduler

import (
	"path/filepath"
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const treeFactsFeature = "cli/job-planning-execution"

// treeFactsRepo is a repository with the projects app, lib and other, that
// ignores dist directories.
func treeFactsRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main\n")
	writeFileAt(t, filepath.Join(root, "lib", "lib.go"), "package lib\n")
	writeFileAt(t, filepath.Join(root, "other", "other.go"), "package other\n")
	writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\n")
	treeKeyedGit(t, root, "add", "-A")
	treeKeyedGit(t, root, "commit", "-q", "-m", "feat: the workspace")
	writeFileAt(t, filepath.Join(root, "app", "dist", "out.js"), "built\n")
	return root
}

// treeFactsJob is a job of project whose task reads main.go and declares the
// runtime inputs.
func treeFactsJob(project *workspace.Project, inputs ...string) *jobmodel.ScheduledJob {
	ext := &extension.ExtensionDescription{Name: "@test/ext", Tasks: map[string]extension.TaskDefinition{"check": {}}}
	def := extension.JobDefinition{
		Name: "lint~check", ExtensionName: ext.Name, Cache: true,
		TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Files: []string{"main.go"}, Runtime: inputs}},
	}
	return &jobmodel.ScheduledJob{Project: project, Extension: ext,
		Step: &extension.PipelineStep{ID: "check", Task: "check"}, JobDef: &def}
}

func treeFactsWorkspace(root string) *workspace.Workspace {
	return testWorkspace(root,
		&workspace.Project{ID: "/app", Name: "app", Path: "app"},
		&workspace.Project{ID: "/lib", Name: "lib", Path: "lib"},
		&workspace.Project{ID: "/other", Name: "other", Path: "other"})
}

func newTreeFactsCache(t *testing.T) *store.CacheManager {
	t.Helper()
	return store.NewCacheManager(store.NewLocalStore(t.TempDir()))
}

func treeFactKeyOf(t *testing.T, ws *workspace.Workspace, job *jobmodel.ScheduledJob, cache *store.CacheManager) string {
	t.Helper()
	hash, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("key of %s: %v", job.Key(), err)
	}
	return hash
}

// A task receives a tree fact only when it declares it, and then its key
// holds the fact: the context carries the listing bounded to the project, and
// adding a file to the index or a directory to the ignored set under the
// project moves the key, while the same change elsewhere does not. A task
// that declares nothing gets no tree and keeps its key.
func TestADeclaredTreeFactIsKeyedAndCarried(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-declared-tree-fact-is-keyed-and-carried")
	root := treeFactsRepo(t)
	ws := treeFactsWorkspace(root)
	app := ws.ProjectByID("/app")
	declaring := treeFactsJob(app, extproto.RuntimeInputTrackedFiles, extproto.RuntimeInputIgnoredDirectories, extproto.RuntimeInputRepository)
	silent := treeFactsJob(app)

	cache := newTreeFactsCache(t)
	tree := jobTree(cachekey.RunTreeFacts(cache, root), ws, declaring)
	want := &protocoljob.Tree{
		Directory:          "app",
		Repository:         &protocoljob.TreeRepository{State: putnamigit.RepositoryComplete},
		TrackedFiles:       []string{"app/main.go"},
		IgnoredDirectories: []string{"app/dist"},
	}
	assertTree(t, tree, want)
	if got := jobTree(cachekey.RunTreeFacts(cache, root), ws, silent); got != nil {
		t.Fatalf("a task that declares no fact got %+v", got)
	}

	before := treeFactKeyOf(t, ws, declaring, newTreeFactsCache(t))
	silentBefore := treeFactKeyOf(t, ws, silent, newTreeFactsCache(t))

	// Outside the project: no key moves.
	writeFileAt(t, filepath.Join(root, "other", "new.go"), "package other\n")
	treeKeyedGit(t, root, "add", "other/new.go")
	writeFileAt(t, filepath.Join(root, "other", "dist", "x"), "x\n")
	if after := treeFactKeyOf(t, ws, declaring, newTreeFactsCache(t)); after != before {
		t.Error("a file tracked outside the project moved the key")
	}

	// A directory ignored under the project moves it.
	writeFileAt(t, filepath.Join(root, "app", "sub", "dist", "x"), "x\n")
	ignored := treeFactKeyOf(t, ws, declaring, newTreeFactsCache(t))
	if ignored == before {
		t.Error("a directory ignored under the project kept the key")
	}
	// So does a file the index starts tracking under it, though the task's
	// file patterns read no file.
	writeFileAt(t, filepath.Join(root, "app", "extra.txt"), "x\n")
	treeKeyedGit(t, root, "add", "app/extra.txt")
	if tracked := treeFactKeyOf(t, ws, declaring, newTreeFactsCache(t)); tracked == ignored {
		t.Error("a file tracked under the project kept the key")
	}
	if silentAfter := treeFactKeyOf(t, ws, silent, newTreeFactsCache(t)); silentAfter != silentBefore {
		t.Error("a task that declares no fact moved its key with the tree")
	}

	// A workspace task covers the workspace.
	workspaceJob := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputIgnoredDirectories)
	workspaceJob.SelectedProjects = ws.Projects
	got := jobTree(cachekey.RunTreeFacts(newTreeFactsCache(t), root), ws, workspaceJob)
	if got == nil || got.Directory != "." || !slices.Contains(got.IgnoredDirectories, "app/dist") ||
		!slices.Contains(got.IgnoredDirectories, "other/dist") {
		t.Fatalf("workspace task tree = %+v, want the ignored directories of the workspace", got)
	}
}

// Outside a work tree the facts are answers: the repository is unmanaged and
// the listings are empty, so the task keeps a key.
func TestTreeFactsOutsideAWorkTreeAreEmptyAnswers(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-declared-tree-fact-is-keyed-and-carried")
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main\n")
	ws := treeFactsWorkspace(root)
	job := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputTrackedFiles, extproto.RuntimeInputIgnoredDirectories, extproto.RuntimeInputRepository)
	cache := newTreeFactsCache(t)
	treeFactKeyOf(t, ws, job, cache)
	assertTree(t, jobTree(cachekey.RunTreeFacts(cache, root), ws, job), &protocoljob.Tree{
		Directory:          "app",
		Repository:         &protocoljob.TreeRepository{State: putnamigit.RepositoryUnmanaged},
		TrackedFiles:       []string{},
		IgnoredDirectories: []string{},
	})
}

func assertTree(t *testing.T, got, want *protocoljob.Tree) {
	t.Helper()
	if got == nil {
		t.Fatalf("tree = nil, want %+v", want)
	}
	if got.Directory != want.Directory {
		t.Errorf("directory = %q, want %q", got.Directory, want.Directory)
	}
	if (got.Repository == nil) != (want.Repository == nil) || got.Repository != nil && *got.Repository != *want.Repository {
		t.Errorf("repository = %+v, want %+v", got.Repository, want.Repository)
	}
	equal := func(name string, a, b []string) {
		if (a == nil) != (b == nil) || len(a) != len(b) {
			t.Errorf("%s = %#v, want %#v", name, a, b)
			return
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s = %#v, want %#v", name, a, b)
				return
			}
		}
	}
	equal("trackedFiles", got.TrackedFiles, want.TrackedFiles)
	equal("ignoredDirectories", got.IgnoredDirectories, want.IgnoredDirectories)
}

// A scheduled job's key and its context come from one read: a change to the
// index after the key was computed reaches neither the key of that run nor the
// context the job receives.
func TestAJobsKeyAndContextComeFromOneRead(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-run-reads-each-tree-fact-once")
	root := treeFactsRepo(t)
	ws := treeFactsWorkspace(root)
	job := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputTrackedFiles)
	cache := newTreeFactsCache(t)
	key := treeFactKeyOf(t, ws, job, cache)

	writeFileAt(t, filepath.Join(root, "app", "staged.go"), "package main\n")
	treeKeyedGit(t, root, "add", "app/staged.go")

	tree := jobTree(cachekey.RunTreeFacts(cache, root), ws, job)
	if tree == nil || !slices.Equal(tree.TrackedFiles, []string{"app/main.go"}) {
		t.Fatalf("context tracked files = %+v, want the listing the key read", tree)
	}
	if again := treeFactKeyOf(t, ws, job, cache); again != key {
		t.Error("the key of one run moved between two computations")
	}
	if next := treeFactKeyOf(t, ws, job, newTreeFactsCache(t)); next == key {
		t.Error("a new run kept the key of a listing that changed")
	}
}

// The release baseline reaches a task that declares it with no directory: the
// tag's files are extracted only for a task that also declares
// releaseBaselineFiles.
func TestTheBaselineFilesAreExtractedOnlyWhenAskedFor(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-release-baseline-is-extracted-once-per-tag-commit")
	root := treeFactsRepo(t)
	treeKeyedGit(t, root, "tag", "-a", "v1.0.0", "-m", "release 1.0.0")
	ws := treeFactsWorkspace(root)
	baselineOnly := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputReleaseBaseline)
	withFiles := treeFactsJob(ws.ProjectByID("/lib"), extproto.RuntimeInputReleaseBaseline, extproto.RuntimeInputReleaseBaselineFiles)
	facts := cachekey.RunTreeFacts(newTreeFactsCache(t), root)
	facts.SetPlanned(ws, []*jobmodel.ScheduledJob{baselineOnly, withFiles})
	t.Cleanup(facts.Close)

	only := jobTree(facts, ws, baselineOnly)
	if only == nil || only.ReleaseBaseline == nil || only.ReleaseBaseline.State != protocoljob.ReleaseBaselineTagged || only.ReleaseBaseline.Directory != "" {
		t.Fatalf("baseline-only tree = %+v, want the tagged baseline with no directory", only)
	}
	if entries, _ := filepath.Glob(filepath.Join(store.ResolveScratchRoot(root), "release-baselines-*")); len(entries) != 0 {
		t.Fatalf("a task that did not ask for the files extracted %v", entries)
	}
	with := jobTree(facts, ws, withFiles)
	if with == nil || with.ReleaseBaseline == nil || with.ReleaseBaseline.Directory == "" {
		t.Fatalf("tree with files = %+v, want the directory", with)
	}
	if entries, _ := filepath.Glob(filepath.Join(store.ResolveScratchRoot(root), "release-baselines-*")); len(entries) != 1 {
		t.Fatalf("extractions = %v, want the one the task asked for", entries)
	}
}
