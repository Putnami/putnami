package treefacts

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/gittest"
)

// contextFacts is a context whose tree covers the project app of the
// workspace at root.
func contextFacts(root string) *Facts {
	return From(&job.Context{WorkspaceRoot: root, Tree: &job.Tree{
		Directory:          "app",
		Repository:         &job.TreeRepository{State: job.RepositoryShallow},
		TrackedFiles:       []string{"app/main.go", "app/sub/a.go"},
		IgnoredDirectories: []string{"app/dist", "app/sub/out"},
		ReleaseBaseline:    &job.ReleaseBaseline{State: job.ReleaseBaselineTagged, Pattern: "v{version}", Tag: "v1.0.0", Directory: "/x/app"},
	}})
}

// A job whose context carries the facts reads them without Git: with no git
// on PATH every answer still comes back, relative to the directory asked.
func TestFactsAnswerFromTheContextWithoutGit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	facts := contextFacts(root)

	tracked, err := facts.TrackedFiles(filepath.Join(root, "app"))
	if err != nil || !reflect.DeepEqual(tracked, []string{"main.go", "sub/a.go"}) {
		t.Errorf("TrackedFiles(app) = %v, %v", tracked, err)
	}
	tracked, err = facts.TrackedFiles(filepath.Join(root, "app", "sub"))
	if err != nil || !reflect.DeepEqual(tracked, []string{"a.go"}) {
		t.Errorf("TrackedFiles(app/sub) = %v, %v", tracked, err)
	}
	if ignored := facts.IgnoredDirectories(filepath.Join(root, "app")); !reflect.DeepEqual(ignored, map[string]bool{"dist": true, "sub/out": true}) {
		t.Errorf("IgnoredDirectories(app) = %v", ignored)
	}
	if state, err := facts.Repository(); err != nil || state != job.RepositoryShallow {
		t.Errorf("Repository() = %q, %v", state, err)
	}
	if baseline, ok := facts.ReleaseBaseline(); !ok || baseline.Tag != "v1.0.0" {
		t.Errorf("ReleaseBaseline() = %+v, %v", baseline, ok)
	}
}

// A question the context does not cover is asked of Git: a directory outside
// the tree's directory, a fact the task did not declare, a context without a
// tree. Without a release baseline there is no answer.
func TestFactsFallBackToGit(t *testing.T) {
	root := gittest.Repo(t)
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	git, err := From(nil).TrackedFiles(root)
	if err != nil || len(git) == 0 {
		t.Fatalf("TrackedFiles from Git = %v, %v; want the repository's files", git, err)
	}
	outside, err := contextFacts(root).TrackedFiles(root)
	if err != nil || !reflect.DeepEqual(outside, git) {
		t.Errorf("TrackedFiles outside the tree's directory = %v, %v; want Git's %v", outside, err, git)
	}
	if state, err := From(&job.Context{WorkspaceRoot: root}).Repository(); err != nil || state != job.RepositoryComplete {
		t.Errorf("Repository() from Git = %q, %v", state, err)
	}
	if _, ok := From(&job.Context{WorkspaceRoot: root}).ReleaseBaseline(); ok {
		t.Error("a context without a tree answered a release baseline")
	}
	if ignored := From(nil).IgnoredDirectories(root); ignored == nil {
		t.Error("IgnoredDirectories from Git = nil, want a set")
	}
	unmanaged := t.TempDir()
	if tracked, err := From(nil).TrackedFiles(unmanaged); err != nil || tracked == nil || len(tracked) != 0 {
		t.Errorf("TrackedFiles outside a work tree = %#v, %v; want the empty answer", tracked, err)
	}
	if state, _, err := RepositoryState(unmanaged); err != nil || state != job.RepositoryUnmanaged {
		t.Errorf("RepositoryState outside a work tree = %q, %v", state, err)
	}
	// A repository git refuses to read is a failure, never unmanaged.
	refused := t.TempDir()
	gittest.Run(t, refused, "init", "-q")
	if err := os.WriteFile(filepath.Join(refused, ".git", "config"), []byte("[core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state, _, err := RepositoryState(refused); err == nil {
		t.Errorf("RepositoryState of a refused repository = %q with no error", state)
	}
	if tracked, err := From(nil).TrackedFiles(refused); err == nil {
		t.Errorf("TrackedFiles of a refused repository = %v with no error", tracked)
	}
}
