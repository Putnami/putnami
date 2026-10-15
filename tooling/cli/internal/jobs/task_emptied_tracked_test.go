package jobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
)

// An output that declares emptied owns its path whole, so the engine never
// empties one that holds a file the Git index tracks (ADR 0068). The cases run
// the real scheduler over a fixture project whose task owns the emptied
// directory .gen/clientgen and writes nothing there.

// committedContractRel is the tracked file the cases put in the output, as a
// workspace-relative slash path.
const committedContractRel = emptyingProject + "/.gen/clientgen/config.json"

// newTrackedEmptiedFixture is the emptying fixture whose task owns the emptied
// project directory .gen/clientgen and preserves its kept subpath. With git,
// the workspace is a Git work tree that ignores .gen; without it, no
// directory above the workspace is searched for one. It returns the fixture
// and the absolute path of the output.
func newTrackedEmptiedFixture(t *testing.T, git bool) (*emptyingFixture, string) {
	t.Helper()
	isolateTreeKeyedGit(t)
	f := newEmptyingFixture(t, false)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(f.ws.Root))
	f.planned[0].Extension.Tasks[emptyingTask] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"clientgen": {
				Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen/clientgen",
				OptionalEmpty: true, Emptied: true, Preserves: []string{"kept"},
			},
		},
	}}
	if git {
		writeTestFile(t, filepath.Join(f.ws.Root, ".gitignore"), ".gen/\n")
		treeKeyedGit(t, f.ws.Root, "init", "-q", "-b", "main")
	}
	return f, filepath.Join(f.gen, "clientgen")
}

// heldTrackedFile is what the refusal says about a tracked file in the
// output's worktree: the file, and the command that moves it out.
var heldTrackedFile = []string{
	"holds the Git-tracked file " + committedContractRel,
	"an emptied output owns its path whole, so move the committed files out of it with git mv",
}

// refused fails unless the session's one task failed before its subprocess
// started, with the error that names the output and its path and says each of
// want.
func (f *emptyingFixture) refused(t *testing.T, leg string, result *SchedulerResult, want ...string) {
	t.Helper()
	row := f.rows(t, leg, result)[0]
	if result.Success || row.Status != "failed" || row.CacheHit || row.Error == nil {
		t.Fatalf("%s: row = %+v (success=%v), want a task that failed without a hit", leg, row, result.Success)
	}
	for _, want := range append([]string{`declared output "clientgen" (` + emptyingProject + `/.gen/clientgen)`}, want...) {
		if !strings.Contains(row.Error.Message, want) {
			t.Errorf("%s: error %q does not say %q", leg, row.Error.Message, want)
		}
	}
}

// The execution fails before its subprocess starts and leaves every file in
// the output, the ignored one included, and so does a tracked file a build of
// an older engine or a move without git mv already deleted, since the index
// still tracks it: the error then says the file is missing from the worktree
// and names the git rm --cached that stages its removal. A tracked file under
// a kept subpath is never removed, so it does not count. Once the index no
// longer tracks the file, the execution empties the output as before.
func TestAnEmptiedOutputThatHoldsATrackedFileIsNeverEmptied(t *testing.T) {
	spectest.Proves(t, emptyingFeature, "emptied-output-keeps-tracked-files", "an-emptied-output-that-holds-a-tracked-file-is-never-emptied")
	f, clientgen := newTrackedEmptiedFixture(t, true)
	emptyings := recordEmptyings(t)
	committed := filepath.Join(clientgen, "config.json")
	writeFileAt(t, committed, "{\"targets\":[\"go\"]}\n")
	writeFileAt(t, filepath.Join(clientgen, "built.json"), "{}\n")
	writeFileAt(t, filepath.Join(clientgen, "kept", "note.txt"), "kept\n")
	treeKeyedGit(t, f.ws.Root, "add", "-f", committedContractRel, emptyingProject+"/.gen/clientgen/kept/note.txt")

	f.refused(t, "execution over a tracked file", f.run(t, SchedulerConfig{Retry: 2}), heldTrackedFile...)
	assertFilesIn(t, "execution over a tracked file", clientgen, "built.json", "config.json", "kept/note.txt")

	if err := os.Remove(committed); err != nil {
		t.Fatal(err)
	}
	f.refused(t, "execution over a tracked file deleted from the worktree", f.run(t, SchedulerConfig{}),
		committedContractRel+" is still tracked by Git but missing from the worktree",
		"run git rm --cached "+committedContractRel)
	assertFilesIn(t, "execution over a tracked file deleted from the worktree", clientgen, "built.json", "kept/note.txt")

	if got := invocationLog(t, f.logPath); len(got) != 0 {
		t.Fatalf("invocations = %v, want none: the tool ran over a tracked file", got)
	}
	if got := emptyings(); len(got) != 0 {
		t.Fatalf("emptied %v, want nothing while the output holds a tracked file", got)
	}
	if got := countTaskEntryDescriptors(t, f.storeRoot); got != 0 {
		t.Fatalf("entries stored = %d, want none", got)
	}

	// The migration takes the file out of the index. An untracked or ignored
	// file in the output is emptied as before; the kept subpath stays.
	writeFileAt(t, committed, "{\"targets\":[\"go\"]}\n")
	treeKeyedGit(t, f.ws.Root, "rm", "-q", "--cached", committedContractRel)
	writeFileAt(t, filepath.Join(clientgen, "untracked.json"), "{}\n")
	f.executed(t, "execution after the migration", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "execution after the migration", clientgen, "kept/note.txt")
	if got := emptyings(); len(got) != 1 {
		t.Fatalf("emptied %v, want the one execution after the migration", got)
	}
}

// A hit whose restore would empty an emptied output that holds a tracked file
// is not served: the restore refuses it before it writes anything, the task
// executes, and the execution fails as above. The file stays.
func TestAHitOverATrackedFileInAnEmptiedOutputIsNotServed(t *testing.T) {
	spectest.Proves(t, emptyingFeature, "emptied-output-keeps-tracked-files", "a-hit-over-a-tracked-file-in-an-emptied-output-is-not-served")
	f, clientgen := newTrackedEmptiedFixture(t, true)
	first := f.executed(t, "execution of an empty output", f.run(t, SchedulerConfig{}))[0]
	if got := f.storedOutput(t, first.CacheKey, "clientgen"); len(got) != 0 {
		t.Fatalf("the entry holds %v under clientgen, want the output recorded empty", got)
	}

	emptyings := recordEmptyings(t)
	writeFileAt(t, filepath.Join(clientgen, "config.json"), "{\"targets\":[\"go\"]}\n")
	treeKeyedGit(t, f.ws.Root, "add", "-f", committedContractRel)
	f.refused(t, "a hit over a tracked file", f.run(t, SchedulerConfig{}), heldTrackedFile...)
	assertFilesIn(t, "a hit over a tracked file", clientgen, "config.json")
	if got := emptyings(); len(got) != 0 {
		t.Fatalf("emptied %v, want nothing while the output holds a tracked file", got)
	}
	if got := invocationLog(t, f.logPath); len(got) != 1 {
		t.Fatalf("invocations = %v, want only the first execution", got)
	}
}

// Outside a Git work tree there is no index to ask, and the output is emptied
// as before: the scheduler asks once per emptied output and the answer is
// that Git does not manage the workspace.
func TestOutsideAGitWorkTreeAnEmptiedOutputIsEmptied(t *testing.T) {
	spectest.Proves(t, emptyingFeature, "emptied-output-keeps-tracked-files", "outside-a-git-work-tree-an-emptied-output-is-emptied")
	f, clientgen := newTrackedEmptiedFixture(t, false)
	if err := putnamigit.Unmanaged(f.ws.Root); err == nil {
		t.Fatalf("the fixture workspace %s is inside a Git work tree", f.ws.Root)
	}
	var unmanaged []error
	saved := trackedUnder
	trackedUnder = func(dir, rel string) ([]string, error) {
		paths, err := saved(dir, rel)
		var outside *putnamigit.UnmanagedError
		if errors.As(err, &outside) {
			unmanaged = append(unmanaged, err)
		}
		return paths, err
	}
	t.Cleanup(func() { trackedUnder = saved })

	writeFileAt(t, filepath.Join(clientgen, "config.json"), "{\"targets\":[\"go\"]}\n")
	f.executed(t, "execution outside Git", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "execution outside Git", clientgen)
	if len(unmanaged) != 1 {
		t.Fatalf("the scheduler asked Git %d times and was told it does not manage the workspace, want once", len(unmanaged))
	}
}

// A workspace with a Git entry at or above its root but no git program to read
// the index fails closed: the engine cannot tell whether the output holds a
// tracked file, so the execution fails before its subprocess starts and names
// the missing program. Without a Git entry the workspace is no work tree, and
// the output is emptied as before.
func TestAGitWorkTreeWithoutAGitProgramFailsClosed(t *testing.T) {
	spectest.Proves(t, emptyingFeature, "emptied-output-keeps-tracked-files", "a-git-work-tree-without-a-git-program-fails-closed")
	f, clientgen := newTrackedEmptiedFixture(t, true)
	mkdir, err := exec.LookPath("mkdir")
	if err != nil {
		t.Skip("the fixture tool needs mkdir")
	}
	bin := t.TempDir()
	if err := os.Symlink(mkdir, filepath.Join(bin, "mkdir")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := exec.LookPath("git"); err == nil {
		t.Fatal("git is still on PATH")
	}
	emptyings := recordEmptyings(t)
	writeFileAt(t, filepath.Join(clientgen, "config.json"), "{\"targets\":[\"go\"]}\n")

	f.refused(t, "execution in a work tree without git", f.run(t, SchedulerConfig{}),
		filepath.Join(f.ws.Root, ".git")+" marks a Git work tree, but no git program is on PATH",
		"install git")
	assertFilesIn(t, "execution in a work tree without git", clientgen, "config.json")
	if got := invocationLog(t, f.logPath); len(got) != 0 {
		t.Fatalf("invocations = %v, want none: the tool ran over an output nobody could check", got)
	}
	if got := emptyings(); len(got) != 0 {
		t.Fatalf("emptied %v, want nothing in a work tree without git", got)
	}

	if err := os.RemoveAll(filepath.Join(f.ws.Root, ".git")); err != nil {
		t.Fatal(err)
	}
	f.executed(t, "execution without git outside a work tree", f.run(t, SchedulerConfig{}))
	assertFilesIn(t, "execution without git outside a work tree", clientgen)
}
