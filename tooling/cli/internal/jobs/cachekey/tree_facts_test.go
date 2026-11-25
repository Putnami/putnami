package cachekey

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const treeFactsFeature = "cli/job-planning-execution"

// treeFactsRepo is a repository with two projects of one version line, app
// and lib, tagged v1.0.0, that ignores app/dist. Its attributes drop one file
// of app from an archive and rewrite another.
func treeFactsRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main\n")
	writeFileAt(t, filepath.Join(root, "lib", "lib.go"), "package lib\n")
	writeFileAt(t, filepath.Join(root, "other", "other.go"), "package other\n")
	writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\n")
	writeFileAt(t, filepath.Join(root, ".gitattributes"), "app/dropped.txt export-ignore\napp/subst.txt export-subst\n")
	writeFileAt(t, filepath.Join(root, "app", "dropped.txt"), "kept\n")
	writeFileAt(t, filepath.Join(root, "app", "subst.txt"), "$Format:%H$\n")
	treeKeyedGit(t, root, "add", "-A")
	treeKeyedGit(t, root, "commit", "-q", "-m", "feat: the workspace")
	treeKeyedGit(t, root, "tag", "-a", "v1.0.0", "-m", "release 1.0.0")
	writeFileAt(t, filepath.Join(root, "app", "dist", "out.js"), "built\n")
	return root
}

func TestTreeFactsRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string { return treeFactsRepo(t) })
}

// treeFactsJob is a job of project whose task declares the runtime inputs.
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

func newRunCache(t *testing.T) *store.CacheManager {
	t.Helper()
	return store.NewCacheManager(store.NewLocalStore(t.TempDir()))
}

func keyOf(t *testing.T, ws *workspace.Workspace, job *jobmodel.ScheduledJob, cache *store.CacheManager) string {
	t.Helper()
	hash, err := ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("key of %s: %v", job.Key(), err)
	}
	return hash
}

// A run reads each fact once, whatever the number of tasks that declare it,
// and the key and the context of each task answer from that read.
func TestARunReadsEachTreeFactOnce(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-run-reads-each-tree-fact-once")
	root := treeFactsRepo(t)
	ws := treeFactsWorkspace(root)
	var trackedReads, ignoredReads atomic.Int32
	savedTracked, savedIgnored := trackedPaths, ignoredDirectories
	trackedPaths = func(dir string) ([]string, error) { trackedReads.Add(1); return savedTracked(dir) }
	ignoredDirectories = func(dir string) ([]string, error) { ignoredReads.Add(1); return savedIgnored(dir) }
	t.Cleanup(func() { trackedPaths, ignoredDirectories = savedTracked, savedIgnored })

	cache := newRunCache(t)
	for _, project := range ws.Projects {
		job := treeFactsJob(project, extproto.RuntimeInputTrackedFiles, extproto.RuntimeInputIgnoredDirectories)
		keyOf(t, ws, job, cache)
		facts := RunTreeFacts(cache, root)
		if tracked, err := facts.TrackedUnder(TreeDirectory(job)); err != nil || len(tracked) == 0 {
			t.Fatalf("%s: tracked files %v, %v", project.ID, tracked, err)
		}
		if _, err := facts.IgnoredUnder(TreeDirectory(job)); err != nil {
			t.Fatal(err)
		}
	}
	if trackedReads.Load() != 1 || ignoredReads.Load() != 1 {
		t.Fatalf("a run of three tasks read the index %d times and the ignored directories %d times, want once each",
			trackedReads.Load(), ignoredReads.Load())
	}
}

// The files a line's last tag holds are extracted once per tag commit for
// every project of the line that declares releaseBaseline, and each project's
// context names its directory as the tag holds it.
func TestAReleaseBaselineIsExtractedOncePerTagCommit(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-release-baseline-is-extracted-once-per-tag-commit")
	root := treeFactsRepo(t)
	// A change after the tag that the baseline must not show.
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main\n\nconst Changed = true\n")
	treeKeyedGit(t, root, "commit", "-q", "-am", "feat(app)!: change")
	ws := treeFactsWorkspace(root)
	app := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputReleaseBaseline, extproto.RuntimeInputReleaseBaselineFiles)
	lib := treeFactsJob(ws.ProjectByID("/lib"), extproto.RuntimeInputReleaseBaseline, extproto.RuntimeInputReleaseBaselineFiles)

	var archives atomic.Int32
	saved := writeFilesAtCommit
	writeFilesAtCommit = func(root, commit string, paths []string, target string) error {
		archives.Add(1)
		return saved(root, commit, paths, target)
	}
	t.Cleanup(func() { writeFilesAtCommit = saved })

	cache := newRunCache(t)
	facts := RunTreeFacts(cache, root)
	facts.SetPlanned(ws, []*jobmodel.ScheduledJob{app, lib})
	keyOf(t, ws, app, cache)
	baselineOf := func(job *jobmodel.ScheduledJob) (putnamigit.ReleaseBaseline, string) {
		t.Helper()
		dir := filepath.Join(root, job.Project.Path)
		baseline, err := facts.ReleaseBaseline(dir, LineTagPattern(ws, ""))
		if err != nil {
			t.Fatal(err)
		}
		extracted, err := facts.BaselineDirectory(dir, LineTagPattern(ws, ""), baseline)
		if err != nil {
			t.Fatal(err)
		}
		return baseline, extracted
	}
	appBaseline, appDir := baselineOf(app)
	libBaseline, libDir := baselineOf(lib)
	if archives.Load() != 1 {
		t.Fatalf("two projects of one line extracted the tag %d times, want once", archives.Load())
	}
	for name, baseline := range map[string]putnamigit.ReleaseBaseline{"app": appBaseline, "lib": libBaseline} {
		if baseline.State != putnamigit.ReleaseBaselineTagged || baseline.Tag != "v1.0.0" {
			t.Fatalf("%s baseline = %+v, want the tagged baseline", name, baseline)
		}
	}
	if appDir == "" || libDir == "" {
		t.Fatalf("baseline directories = %q, %q; want both", appDir, libDir)
	}
	if !appBaseline.Breaking || libBaseline.Breaking {
		t.Errorf("breaking = app %v, lib %v; want only app's commit since the tag to declare a break",
			appBaseline.Breaking, libBaseline.Breaking)
	}
	// The files are the blobs the tag's tree names: no export attribute
	// drops or rewrites one.
	for name, want := range map[string]string{"main.go": "package main\n", "dropped.txt": "kept\n", "subst.txt": "$Format:%H$\n"} {
		data, err := os.ReadFile(filepath.Join(appDir, name))
		if err != nil || string(data) != want {
			t.Fatalf("app/%s at the tag = %q (%v), want %q", name, data, err, want)
		}
	}
	facts.Close()
	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Errorf("the extraction outlived the run: %v", err)
	}
}

// A Git failure is not a fact: the key of a task that declares the fact fails,
// so the task runs uncached, and the fact is not answered.
func TestAGitFailureIsNotATreeFact(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-declared-tree-fact-is-keyed-and-carried")
	root := treeFactsRepo(t)
	ws := treeFactsWorkspace(root)
	failure := errors.New("git cannot run")
	savedTracked, savedIgnored := trackedPaths, ignoredDirectories
	trackedPaths = func(string) ([]string, error) { return nil, failure }
	ignoredDirectories = func(string) ([]string, error) { return nil, failure }
	t.Cleanup(func() { trackedPaths, ignoredDirectories = savedTracked, savedIgnored })

	for _, input := range []string{extproto.RuntimeInputTrackedFiles, extproto.RuntimeInputIgnoredDirectories} {
		cache := newRunCache(t)
		job := treeFactsJob(ws.ProjectByID("/app"), input)
		if _, err := ComputeJobCacheHash(ws, job, nil, nil, cache, nil); !errors.Is(err, failure) {
			t.Errorf("%s: key error = %v, want the Git failure", input, err)
		}
	}
	facts := RunTreeFacts(newRunCache(t), root)
	if paths, err := facts.TrackedUnder("app"); err == nil {
		t.Errorf("tracked files = %v with no error, want the failure", paths)
	}
	if paths, err := facts.IgnoredUnder("app"); err == nil {
		t.Errorf("ignored directories = %v with no error, want the failure", paths)
	}
}

// A project reached through a symbolic link is placed by the path Git names
// it by, so its release baseline directory holds the tagged files; a project
// whose directory the tag holds as a link names no directory and fails.
func TestAReleaseBaselineThroughALinkIsExtracted(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-release-baseline-is-extracted-once-per-tag-commit")
	if runtime.GOOS == "windows" {
		t.Skip("the fixture needs a symbolic link")
	}
	root := t.TempDir()
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	writeFileAt(t, filepath.Join(root, "shared", "app", "main.go"), "package tagged\n")
	if err := os.Symlink(filepath.Join("shared", "app"), filepath.Join(root, "app")); err != nil {
		t.Fatal(err)
	}
	treeKeyedGit(t, root, "add", "-A")
	treeKeyedGit(t, root, "commit", "-q", "-m", "feat: the workspace")
	treeKeyedGit(t, root, "tag", "-a", "v1.0.0", "-m", "release 1.0.0")
	ws := testWorkspace(root, &workspace.Project{ID: "/app", Name: "app", Path: "app"})
	job := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputReleaseBaseline, extproto.RuntimeInputReleaseBaselineFiles)

	facts := RunTreeFacts(newRunCache(t), root)
	facts.SetPlanned(ws, []*jobmodel.ScheduledJob{job})
	t.Cleanup(facts.Close)
	dir := filepath.Join(root, "app")
	baseline, err := facts.ReleaseBaseline(dir, LineTagPattern(ws, ""))
	if err != nil || baseline.State != putnamigit.ReleaseBaselineTagged {
		t.Fatalf("baseline = %+v, %v; want tagged", baseline, err)
	}
	extracted, err := facts.BaselineDirectory(dir, LineTagPattern(ws, ""), baseline)
	if err != nil {
		t.Fatalf("baseline directory: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(extracted, "main.go")); err != nil || string(data) != "package tagged\n" {
		t.Fatalf("main.go in %s = %q (%v), want the tagged content", extracted, data, err)
	}

	// The tag holds the link itself, not a directory, at a project path.
	linked := baseline
	linked.Prefix = "app"
	if got, err := facts.BaselineDirectory(filepath.Join(root, "other"), LineTagPattern(ws, ""), linked); err == nil {
		t.Fatalf("a path the tag holds as a link named the directory %q, want an error", got)
	}
}

// Prime reads each declared work-tree fact before the run's first task: a
// directory a task creates afterwards is in neither the fact nor the key.
func TestPrimeReadsTheTreeFactsBeforeTheFirstTask(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-run-reads-each-tree-fact-once")
	root := treeFactsRepo(t)
	ws := treeFactsWorkspace(root)
	job := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputIgnoredDirectories, extproto.RuntimeInputTrackedFiles)
	cache := newRunCache(t)
	facts := RunTreeFacts(cache, root)
	facts.Prime([]*jobmodel.ScheduledJob{job})
	before := keyOf(t, ws, job, cache)

	// A task of the run writes an ignored directory and stages a file.
	writeFileAt(t, filepath.Join(root, "app", "later", "dist", "x"), "x\n")
	writeFileAt(t, filepath.Join(root, "app", "staged.go"), "package main\n")
	treeKeyedGit(t, root, "add", "app/staged.go")

	ignored, err := facts.IgnoredUnder("app")
	if err != nil || slices.Contains(ignored, "app/later/dist") {
		t.Errorf("ignored directories = %v, %v; want the run's first read", ignored, err)
	}
	tracked, err := facts.TrackedUnder("app")
	if err != nil || slices.Contains(tracked, "app/staged.go") {
		t.Errorf("tracked files = %v, %v; want the run's first read", tracked, err)
	}
	if after := keyOf(t, ws, job, cache); after != before {
		t.Error("the key of one run moved with a change a task made")
	}
}

// releaseBaselineFiles adds nothing to a key: the directory it names is
// derived from the baseline the key already holds, so a task that asks for the
// files keys as the task that reads only the baseline.
func TestReleaseBaselineFilesMovesNoKey(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "tree-facts-follow-the-key", "a-release-baseline-is-extracted-once-per-tag-commit")
	root := treeFactsRepo(t)
	ws := treeFactsWorkspace(root)
	baseline := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputReleaseBaseline)
	files := treeFactsJob(ws.ProjectByID("/app"), extproto.RuntimeInputReleaseBaseline, extproto.RuntimeInputReleaseBaselineFiles)
	if a, b := keyOf(t, ws, baseline, newRunCache(t)), keyOf(t, ws, files, newRunCache(t)); a != b {
		t.Fatalf("releaseBaselineFiles moved the key (%s != %s)", a, b)
	}
}

// A lint keyed on the Go lint tasks' source patterns keeps its key when the
// project builds into its ignored output directories: the key's file walk
// never enters dist, its patterns leave .gen out, bin holds no Go file, and
// the task declares no tree fact that lists the directories Git ignores.
func TestABuildOutputDirectoryDoesNotMoveTheGoLintKey(t *testing.T) {
	root := treeFactsRepo(t)
	writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\nbin/\n.gen/\n")
	ws := treeFactsWorkspace(root)
	ext := &extension.ExtensionDescription{Name: "@test/ext", Tasks: map[string]extension.TaskDefinition{"check": {}}}
	def := extension.JobDefinition{
		Name: "lint~check", ExtensionName: ext.Name, Cache: true,
		TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{
			Files: []string{"**/*.go", "!**/.gen/**", "go.mod"}}},
	}
	job := &jobmodel.ScheduledJob{Project: ws.ProjectByID("/app"), Extension: ext,
		Step: &extension.PipelineStep{ID: "check", Task: "check"}, JobDef: &def}
	if err := os.RemoveAll(filepath.Join(root, "app", "dist")); err != nil {
		t.Fatal(err)
	}
	fresh := keyOf(t, ws, job, newRunCache(t))
	writeFileAt(t, filepath.Join(root, "app", "dist", "gen.go"), "package dist\n")
	writeFileAt(t, filepath.Join(root, "app", ".gen", "api", "api.go"), "package api\n")
	writeFileAt(t, filepath.Join(root, "app", "bin", "app"), "binary\n")
	if built := keyOf(t, ws, job, newRunCache(t)); built != fresh {
		t.Fatalf("building into dist, .gen and bin moved the lint key (%s != %s)", built, fresh)
	}
}
