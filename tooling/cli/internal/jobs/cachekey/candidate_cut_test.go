package cachekey

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// cutJob is a job of project whose task keys key and declares the runtime
// inputs of key.Runtime.
func cutJob(project *workspace.Project, key extension.TaskCacheKey) *jobmodel.ScheduledJob {
	ext := &extension.ExtensionDescription{Name: "@test/ext", Tasks: map[string]extension.TaskDefinition{"check": {}}}
	def := extension.JobDefinition{Name: "lint~check", ExtensionName: ext.Name, Cache: true,
		TaskCachePolicy: &extension.TaskCachePolicy{Key: &key}}
	return &jobmodel.ScheduledJob{Project: project, Extension: ext,
		Step: &extension.PipelineStep{ID: "check", Task: "check"}, JobDef: &def}
}

// candidateIdentity is the candidateFiles pair of the job's key, and the
// pair the digest of what the job's context carries would make.
func candidateIdentity(t *testing.T, ws *workspace.Workspace, job *jobmodel.ScheduledJob, facts *TreeFacts) (keyed, carried string) {
	t.Helper()
	identity, err := treeFactIdentity(ws, job, nil, facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range identity {
		if strings.HasPrefix(pair, extproto.RuntimeInputCandidateFiles+"=") {
			keyed = pair
		}
	}
	directories := CandidateDirectories(ws, job)
	files, found, err := facts.CandidatesUnder(job.Key(), directories)
	if err != nil || !found {
		t.Fatalf("context cut: %v, found %v", err, found)
	}
	return keyed, extproto.RuntimeInputCandidateFiles + "=" + candidatesDigest(directories, files)
}

// The key holds exactly the cut the context carries: a change of the index
// mode alone, or a candidate added in a closure member alone, moves the key,
// and after each the key is the digest of what the context carries.
func TestTheKeyHoldsTheCutTheContextCarries(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-candidate-cut-is-keyed-and-carried")
	root := treeFactsRepo(t)
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	ws := testWorkspace(root, lib, app, &workspace.Project{ID: "/other", Name: "other", Path: "other"})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	job := cutJob(app, extension.TaskCacheKey{Files: []string{"main.go"}, ClosureFiles: []string{"git:**/*"},
		Runtime: []string{extproto.RuntimeInputCandidateFiles}})

	fresh := func() *TreeFacts { return NewTreeFacts(root) }
	keyed, carried := candidateIdentity(t, ws, job, fresh())
	if keyed == "" || keyed != carried {
		t.Fatalf("key %q, context %q: want the digest of the context", keyed, carried)
	}
	for _, step := range []struct {
		name string
		edit func()
	}{
		{"the index mode alone", func() { treeKeyedGit(t, root, "update-index", "--chmod=+x", "app/main.go") }},
		{"an untracked candidate of a closure member", func() { writeFileAt(t, filepath.Join(root, "lib", "new.go"), "package lib\n") }},
		{"staging it", func() { treeKeyedGit(t, root, "add", "lib/new.go") }},
	} {
		step.edit()
		after, carriedAfter := candidateIdentity(t, ws, job, fresh())
		if after == keyed {
			t.Errorf("%s kept the key", step.name)
		}
		if after != carriedAfter {
			t.Errorf("%s: key %q, context %q: want the digest of the context", step.name, after, carriedAfter)
		}
		// The workspace's `git:**` digest, listed from the same read, is the
		// one Git lists.
		if viaGit, viaCut, gitErr, cutErr := cutDigests(t, root); gitErr != nil || cutErr != nil || viaGit != viaCut {
			t.Errorf("%s: the workspace digest from the cut %q (%v) is not the Git one %q (%v)", step.name, viaCut, cutErr, viaGit, gitErr)
		}
		keyed = after
	}
}

// A run reads the cut once, and again only after a write of the run that can
// change it: a key and the context of its job carry the read the key took,
// and the next key reads the cut again. A write under an ignored .putnami,
// where command outputs live, reads nothing again; under one Git does not
// ignore, it does.
func TestAWriteOfTheRunReadsTheCutAgain(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	root := treeFactsRepo(t)
	// The store and the command outputs live under .putnami, which the
	// workspace ignores.
	writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\n.putnami\n")
	writeFileAt(t, filepath.Join(root, ".putnami", "cache.json"), "{}\n")
	ws := treeFactsWorkspace(root)
	var reads atomic.Int32
	saved := candidateFiles
	candidateFiles = func(dir string) ([]protocoljob.CandidateFile, error) { reads.Add(1); return saved(dir) }
	t.Cleanup(func() { candidateFiles = saved })
	job := cutJob(ws.ProjectByID("/app"), extension.TaskCacheKey{Files: []string{"main.go"},
		Runtime: []string{extproto.RuntimeInputCandidateFiles}})
	facts := NewTreeFacts(root)
	facts.Prime([]*jobmodel.ScheduledJob{job})
	keyed, _ := candidateIdentity(t, ws, job, facts)

	generated := filepath.Join(root, "app", "gen", "doc.md")
	writeFileAt(t, generated, "# generated\n")
	facts.InvalidateCut([]string{filepath.Join(root, ".putnami", "out", "app")})
	if keyedAgain, _ := candidateIdentity(t, ws, job, facts); keyedAgain != keyed || reads.Load() != 1 {
		t.Fatalf("a write under .putnami read the cut again (%d reads)", reads.Load())
	}
	facts.InvalidateCut([]string{filepath.Join(root, "app", "gen")})
	// The context still carries the read the key took.
	if files, _, _ := facts.CandidatesUnder(job.Key(), []string{"app"}); slices.ContainsFunc(files, func(f protocoljob.CandidateFile) bool {
		return f.Path == "app/gen/doc.md"
	}) {
		t.Fatal("the context of a keyed job left the read its key took")
	}
	after, carried := candidateIdentity(t, ws, job, facts)
	if after == keyed || after != carried || reads.Load() != 2 {
		t.Fatalf("after the write: key %q (before %q), context %q, %d reads; want a new read the context carries", after, keyed, carried, reads.Load())
	}
	// A job whose key took no read, such as an uncached one, carries the
	// current read.
	uncached := cutJob(ws.ProjectByID("/app"), extension.TaskCacheKey{Runtime: []string{extproto.RuntimeInputCandidateFiles}})
	uncached.JobDef.Cache = false
	uncached.JobDef.Name = "package~stage"
	files, _, err := facts.CandidatesUnder(uncached.Key(), []string{"app"})
	if err != nil || !slices.ContainsFunc(files, func(f protocoljob.CandidateFile) bool { return f.Path == "app/gen/doc.md" }) {
		t.Fatalf("an uncached job does not see the file a task of the run wrote: %v %v", files, err)
	}
}

// A .putnami directory Git does not ignore holds candidates, so a write under
// it reads the cut again.
func TestAWriteUnderAPutnamiDirectoryGitListsReadsTheCutAgain(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	root := treeFactsRepo(t)
	writeFileAt(t, filepath.Join(root, ".putnami", "cache.json"), "{}\n")
	var reads atomic.Int32
	saved := candidateFiles
	candidateFiles = func(dir string) ([]protocoljob.CandidateFile, error) { reads.Add(1); return saved(dir) }
	t.Cleanup(func() { candidateFiles = saved })
	facts := NewTreeFacts(root)
	facts.currentCut()
	facts.InvalidateCut([]string{filepath.Join(root, ".putnami", "out")})
	facts.currentCut()
	if reads.Load() != 2 {
		t.Fatalf("a write under a .putnami Git lists read the cut %d times, want twice", reads.Load())
	}
}

// A write of the run reads the cut again only for a key whose directories it
// reaches, at, below or above them: a key of another project keeps the
// current read, whose listing there the write did not change. A write under
// .git, where Git keeps its own exclude rules, reaches every key. A write
// under .putnami asks Git whether .putnami is excluded only once a key it
// reaches asks for the cut, here the workspace's.
func TestAWriteReadsTheCutAgainOnlyForTheKeysItReaches(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	root := treeFactsRepo(t)
	writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\n.putnami\n")
	ws := treeFactsWorkspace(root)
	var reads, asks atomic.Int32
	savedFiles, savedIgnores := candidateFiles, ignoresDirectory
	candidateFiles = func(dir string) ([]protocoljob.CandidateFile, error) { reads.Add(1); return savedFiles(dir) }
	ignoresDirectory = func(root, rel string) bool { asks.Add(1); return savedIgnores(root, rel) }
	t.Cleanup(func() { candidateFiles, ignoresDirectory = savedFiles, savedIgnores })
	key := extension.TaskCacheKey{Files: []string{"main.go"}, Runtime: []string{extproto.RuntimeInputCandidateFiles}}
	app, lib := cutJob(ws.ProjectByID("/app"), key), cutJob(ws.ProjectByID("/lib"), key)
	facts := NewTreeFacts(root)
	facts.Prime([]*jobmodel.ScheduledJob{app, lib})
	appKeyed, _ := candidateIdentity(t, ws, app, facts)
	libKeyed, _ := candidateIdentity(t, ws, lib, facts)

	writeFileAt(t, filepath.Join(root, "app", "gen", "doc.md"), "# generated\n")
	facts.InvalidateCut([]string{filepath.Join(root, "app", "gen"), filepath.Join(root, ".putnami", "out", "app")})
	if again, carried := candidateIdentity(t, ws, lib, facts); again != libKeyed || again != carried || reads.Load() != 1 {
		t.Fatalf("a write under app read the cut again for lib's key (%d reads)", reads.Load())
	}
	if asks.Load() != 0 {
		t.Fatal("a write under .putnami asked Git before a key it reaches asked for the cut")
	}
	if again, carried := candidateIdentity(t, ws, app, facts); again == appKeyed || again != carried || reads.Load() != 2 {
		t.Fatalf("a write under app kept app's key (%d reads)", reads.Load())
	}
	// A write above the directory reaches it.
	facts.InvalidateCut([]string{root})
	if candidateIdentity(t, ws, lib, facts); reads.Load() != 3 {
		t.Fatalf("a write of the workspace root kept lib's read (%d reads)", reads.Load())
	}
	// A write under .git reaches every key.
	facts.InvalidateCut([]string{filepath.Join(root, ".git", "info", "exclude")})
	if candidateIdentity(t, ws, lib, facts); reads.Load() != 4 {
		t.Fatalf("a write under .git kept lib's read (%d reads)", reads.Load())
	}
	// The workspace's cut is reached by a write under .putnami, which Git
	// excludes: it asks once and reads nothing again.
	facts.InvalidateCut([]string{filepath.Join(root, ".putnami", "out", "lib")})
	facts.currentCut()
	if reads.Load() != 4 || asks.Load() != 1 {
		t.Fatalf("a write under an excluded .putnami: %d reads, %d asks; want 4 and 1", reads.Load(), asks.Load())
	}
	// The excluded writes are dropped: another one asks nothing more, and a
	// write outside .putnami still reaches the key.
	facts.InvalidateCut([]string{filepath.Join(root, ".putnami", "out", "other"), filepath.Join(root, "lib", "gen")})
	if candidateIdentity(t, ws, lib, facts); reads.Load() != 5 || asks.Load() != 1 {
		t.Fatalf("after the drop: %d reads, %d asks; want 5 and 1", reads.Load(), asks.Load())
	}
}

// A key that keeps the current read after a write holds the cut a fresh read
// gives: a .gitignore written above the key's directory, at the workspace root,
// and a write a symbolic link leads into the key's directory both reach it.
func TestAKeptReadIsTheCutAFreshReadGives(t *testing.T) {
	spectest.Proves(t, treeFactsFeature, "candidate-cut-and-history-follow-the-key", "a-write-of-the-run-reads-the-cut-again")
	root := treeFactsRepo(t)
	writeFileAt(t, filepath.Join(root, ".gitignore"), "dist/\n.putnami\n")
	writeFileAt(t, filepath.Join(root, "lib", "extra.txt"), "untracked\n")
	if err := os.MkdirAll(filepath.Join(root, "lib", "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "lib", "out"), filepath.Join(root, "app", "out")); err != nil {
		t.Fatal(err)
	}
	ws := treeFactsWorkspace(root)
	key := extension.TaskCacheKey{Files: []string{"lib.go"}, Runtime: []string{extproto.RuntimeInputCandidateFiles}}
	lib := cutJob(ws.ProjectByID("/lib"), key)
	facts := NewTreeFacts(root)
	facts.Prime([]*jobmodel.ScheduledJob{lib})
	candidateIdentity(t, ws, lib, facts)
	for _, step := range []struct {
		name  string
		write func() string
	}{
		{"a .gitignore at the workspace root", func() string {
			path := filepath.Join(root, ".gitignore")
			writeFileAt(t, path, "dist/\n.putnami\nextra.txt\n")
			return path
		}},
		{"a repository nested at the directory", func() string {
			treeKeyedGit(t, filepath.Join(root, "lib"), "init", "-q")
			return filepath.Join(root, "lib", ".git")
		}},
		{"a write through a link into the directory", func() string {
			path := filepath.Join(root, "app", "out", "gen.txt")
			writeFileAt(t, path, "generated\n")
			return path
		}},
	} {
		facts.InvalidateCut([]string{step.write()})
		kept, carried := candidateIdentity(t, ws, lib, facts)
		fresh, _ := candidateIdentity(t, ws, lib, NewTreeFacts(root))
		if kept != fresh || carried != fresh {
			t.Errorf("after %s: key %q, context %q, a fresh read %q", step.name, kept, carried, fresh)
		}
	}
}

// reachedPath maps a write to the directory whose cut it can change.
func TestReachedPath(t *testing.T) {
	for rel, want := range map[string]string{
		"lib/out/x.js":               "lib/out/x.js",
		".gitignore":                 ".",
		"packages/.gitignore":        "packages",
		".git/info/exclude":          ".",
		".GIT":                       ".",
		"packages/.git/info/exclude": "packages",
		"packages/.git":              "packages",
		"Lib/Out":                    "lib/out",
	} {
		if got := reachedPath(rel); got != want {
			t.Errorf("reachedPath(%q) = %q, want %q", rel, got, want)
		}
	}
}
