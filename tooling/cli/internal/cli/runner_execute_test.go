package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	runner "go.putnami.dev/protocol/runner"
)

// verifyCommitCheckout binds a version 2 request to the checkout of its
// commit: outside a repository, on another HEAD or over a modified tracked
// file it refuses and names why. Untracked files do not count.
func TestVerifyCommitCheckoutBindsTheNamedCommit(t *testing.T) {
	t.Parallel()
	if err := verifyCommitCheckout(t.TempDir(), strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "needs a Git checkout") {
		t.Fatalf("outside a repository: %v", err)
	}
	dir := treeCommandRepo(t)
	for index := range 6 {
		writeTestFile(t, filepath.Join(dir, fmt.Sprintf("file-%d.txt", index)), "committed\n")
	}
	runAliasGit(t, dir, "add", "-A")
	runAliasGit(t, dir, "commit", "-m", "files")
	head := strings.TrimSpace(gitTestOutput(t, dir, "rev-parse", "HEAD"))

	writeTestFile(t, filepath.Join(dir, "untracked.txt"), "new\n")
	if err := verifyCommitCheckout(dir, head); err != nil {
		t.Fatalf("a clean checkout with an untracked file was refused: %v", err)
	}
	other := strings.Repeat("b", 40)
	if err := verifyCommitCheckout(dir, other); err == nil || err.Error() != "the checkout's HEAD is "+head+", but the request names source.commit "+other {
		t.Fatalf("another HEAD: %v", err)
	}
	for index := range 6 {
		writeTestFile(t, filepath.Join(dir, fmt.Sprintf("file-%d.txt", index)), "edited\n")
	}
	err := verifyCommitCheckout(dir, head)
	if err == nil || !strings.Contains(err.Error(), "modifies 6 tracked file(s): file-0.txt, file-1.txt, file-2.txt, file-3.txt, file-4.txt, and 1 more") {
		t.Fatalf("modified tracked files: %v", err)
	}
}

// The adapter projects either version onto the engine request: a version 1
// request keeps its frozen projects and version snapshot, a version 2 request
// leaves both to the engine, which plans and stamps its checkout.
func TestBoundRequestProjectsEitherVersion(t *testing.T) {
	t.Parallel()
	invocation := runner.InvocationBlock{Commands: []string{"build"}, Providers: []string{"install"},
		Flags: runner.ExecutionFlags{Output: "json", ResourceBudgets: map[string]int{"db": 1}}}
	snapshot := runner.BoundRequest{Snapshot: &runner.ExecutionRequest{
		Invocation: invocation,
		Source:     runner.SourceBlock{Versions: []runner.LineVersion{{Line: "", Base: "1.0.0", Full: "1.0.0"}}},
		Selection:  runner.SelectionBlock{Projects: []string{"/app", "/lib"}},
	}}
	commit := runner.BoundRequest{Commit: &runner.CommitRequest{Invocation: invocation,
		Selection: runner.RequestedSelection{Mode: runner.SelectionModeProjects, Projects: []string{"app"}}}}

	if global := boundGlobalFlags(snapshot); global.Projects != "/app,/lib" || global.Where != "remote" || global.Output != "json" || global.ResourceBudgets["db"] != 1 {
		t.Fatalf("version 1 flags = %+v", global)
	}
	if global := boundGlobalFlags(commit); global.Projects != "" || global.Where != "remote" || global.Output != "json" || strings.Join(global.Providers, ",") != "install" {
		t.Fatalf("version 2 flags = %+v", global)
	}
	if version := boundVersionSnapshot(snapshot); version == nil || version.Full != "1.0.0" {
		t.Fatalf("version 1 snapshot = %+v", version)
	}
	if version := boundVersionSnapshot(commit); version != nil {
		t.Fatalf("version 2 snapshot = %+v; want the engine to stamp its checkout", version)
	}
	if portable := boundPortableExecution(commit); portable.Commit != commit.Commit {
		t.Fatal("a version 2 request is not bound as one")
	}
	if portable := boundPortableExecution(snapshot); portable.Commit != nil || strings.Join(portable.Request.Selection.Projects, ",") != "/app,/lib" {
		t.Fatal("a version 1 request is not bound frozen")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitTestOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	out, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
