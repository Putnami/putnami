package gitread

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectSourceBindingTracksSourceAndExcludesGeneratedOutputs(t *testing.T) {
	repo := initGitRepo(t)
	project := filepath.Join(repo, "project")
	dependency := initGitRepo(t)
	if err := os.MkdirAll(filepath.Join(project, "schema", "feature-evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"project/app.go":                                 "package project\n",
		"project/tool.sh":                                "#!/bin/sh\nexit 0\n",
		"project/.gitignore":                             "ignored.txt\ntracked-ignored.txt\n",
		"project/ignored.txt":                            "ignored\n",
		"project/tracked-ignored.txt":                    "tracked despite ignore\n",
		"project/.gen/schema/capabilities.json":          "generated\n",
		"project/nested/.gen/generated.txt":              "nested generated\n",
		"project/schema/capabilities.json":               "promoted\n",
		"project/nested/schema/capabilities.json":        "nested promoted\n",
		"project/schema/feature-evidence/framework.json": "evidence\n",
		"project/nested/schema/feature-evidence/x.json":  "nested evidence\n",
	} {
		absolute := filepath.Join(repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(project, "tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("app.go", filepath.Join(project, "app-link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "clone", dependency, "project/vendor/dep")
	runGit(t, repo, "add", "project/app.go", "project/tool.sh", "project/app-link", "project/.gitignore", "project/vendor/dep")
	runGit(t, repo, "add", "-f", "project/tracked-ignored.txt", "project/.gen/schema/capabilities.json", "project/nested/.gen/generated.txt", "project/schema/capabilities.json", "project/nested/schema/capabilities.json", "project/schema/feature-evidence/framework.json", "project/nested/schema/feature-evidence/x.json")
	runGit(t, repo, "commit", "-m", "add project")

	first, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatalf("ProjectSourceBinding: %v", err)
	}
	cleanAgain, err := ProjectSourceBinding(repo, project)
	if err != nil || cleanAgain != first {
		t.Fatalf("clean worktree binding is not stable: %s / %s (%v)", first, cleanAgain, err)
	}
	if err := os.WriteFile(filepath.Join(project, ".gen", "schema", "capabilities.json"), []byte("changed generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "schema", "capabilities.json"), []byte("changed promoted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "schema", "feature-evidence", "framework.json"), []byte("changed evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "ignored.txt"), []byte("changed ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("generated/promoted/ignored-only changes moved binding: %s -> %s", first, second)
	}
	if err := os.WriteFile(filepath.Join(project, "tracked-ignored.txt"), []byte("changed tracked despite ignore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trackedIgnored, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if trackedIgnored == second {
		t.Fatal("a tracked file must remain bound after an ignore rule is added")
	}

	if err := os.WriteFile(filepath.Join(project, "untracked.go"), []byte("package project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if third == trackedIgnored {
		t.Fatal("non-ignored untracked source must change the binding")
	}
	if err := os.Chmod(filepath.Join(project, "tool.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	modeChanged, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if modeChanged == third {
		t.Fatal("changing a tracked file from executable to regular must change the binding")
	}
	if err := os.Remove(filepath.Join(project, "tool.sh")); err != nil {
		t.Fatal(err)
	}
	deleted, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if deleted == modeChanged {
		t.Fatal("deleting a tracked file must change the binding")
	}
	if err := os.Remove(filepath.Join(project, "app-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("untracked.go", filepath.Join(project, "app-link")); err != nil {
		t.Fatal(err)
	}
	symlinkChanged, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if symlinkChanged == deleted {
		t.Fatal("changing a tracked symlink target must change the binding")
	}
	if err := os.WriteFile(filepath.Join(project, "app.go"), []byte("package project\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fourth, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if fourth == symlinkChanged {
		t.Fatal("working bytes of a tracked source file must change the binding")
	}
	depPath := filepath.Join(project, "vendor", "dep")
	if err := os.WriteFile(filepath.Join(depPath, "next.txt"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, depPath, "add", "next.txt")
	runGit(t, depPath,
		"-c", "user.email=test@test.com",
		"-c", "user.name=Test",
		"-c", "commit.gpgsign=false",
		"commit", "-m", "advance dependency",
	)
	gitlinkChanged, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if gitlinkChanged == fourth {
		t.Fatal("advancing the visible worktree commit of a tracked gitlink must change the binding")
	}
	if err := os.RemoveAll(depPath); err != nil {
		t.Fatal(err)
	}
	gitlinkDeleted, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if gitlinkDeleted == gitlinkChanged {
		t.Fatal("an absent tracked gitlink must be treated as deleted")
	}
}

func TestProjectSourceBindingSupportsRepositoryRootProject(t *testing.T) {
	repo := initGitRepo(t)
	path := filepath.Join(repo, "root.go")
	if err := os.WriteFile(path, []byte("package root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "root.go")
	runGit(t, repo, "commit", "-m", "add root project")
	first, err := ProjectSourceBinding(repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package root\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := ProjectSourceBinding(repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("repository-root project source change did not move binding")
	}
}

// Core's TestProjectSourceBindingMeasuredCountsGitProcesses is deliberately not
// copied: this package does not export ProjectSourceBindingMeasured, because no
// SDD engine reads the subprocess count.

func TestProjectSourceBindingUsesIndexOIDForEmptyUninitializedGitlink(t *testing.T) {
	repo := initGitRepo(t)
	dependency := initGitRepo(t)
	project := filepath.Join(repo, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", dependency, "project/vendor/uninitialized")
	runGit(t, repo, "commit", "-m", "add gitlink")
	initialized, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	gitlink := filepath.Join(project, "vendor", "uninitialized")
	if err := os.RemoveAll(gitlink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gitlink, 0o755); err != nil {
		t.Fatal(err)
	}
	uninitialized, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatalf("empty uninitialized gitlink: %v", err)
	}
	if uninitialized != initialized {
		t.Fatalf("index OID parity mismatch: %s != %s", uninitialized, initialized)
	}
	if err := os.WriteFile(filepath.Join(gitlink, "fake.txt"), []byte("not a worktree"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ProjectSourceBinding(repo, project); err == nil || !strings.Contains(err.Error(), "exact Git worktree") {
		t.Fatalf("nonempty fake gitlink error = %v", err)
	}
}

func TestProjectSourceBindingRejectsUnmergedAndMalformedStageEntries(t *testing.T) {
	if _, _, _, _, ok := parseStageEntry("malformed"); ok {
		t.Fatal("malformed stage entry accepted")
	}
	repo := initGitRepo(t)
	project := filepath.Join(repo, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "conflict.txt")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "project/conflict.txt")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "checkout", "-b", "other")
	if err := os.WriteFile(path, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "commit", "-am", "other")
	runGit(t, repo, "checkout", "main")
	if err := os.WriteFile(path, []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "commit", "-am", "main")
	cmd := exec.Command("git", "merge", "other")
	cmd.Dir = repo
	if err := cmd.Run(); err == nil {
		t.Fatal("expected merge conflict")
	}
	if _, err := ProjectSourceBinding(repo, project); err == nil || !strings.Contains(err.Error(), "unmerged index stage") {
		t.Fatalf("unmerged index error = %v", err)
	}
}

// A Windows checkout has no executable bit, so it takes a tracked file's from
// the index. Its worktree binding then equals the one a Linux or macOS checkout
// of the same commit computes from its disk, and the binding of the commit
// itself: the SDD engine compares a binding it computes with one recorded on
// another host.
func TestProjectSourceBindingMatchesAcrossHostsWithAndWithoutAnExecutableBit(t *testing.T) {
	repo := initGitRepo(t)
	project := filepath.Join(repo, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(project, "tool.sh")
	for path, body := range map[string]string{tool: "#!/bin/sh\nexit 0\n", filepath.Join(project, "plain.txt"): "plain\n"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "config", "core.fileMode", "false")
	runGit(t, repo, "add", "project/tool.sh", "project/plain.txt")
	runGit(t, repo, "update-index", "--chmod=+x", "project/tool.sh")
	runGit(t, repo, "commit", "-m", "add project")
	committed, err := CommitSourceBinding(repo, "HEAD", "project")
	if err != nil {
		t.Fatalf("CommitSourceBinding: %v", err)
	}

	binding := func(perm os.FileMode, statsExecBit bool) string {
		t.Helper()
		if err := os.Chmod(tool, perm); err != nil {
			t.Fatal(err)
		}
		got, err := projectSourceBinding(repo, project, statsExecBit)
		if err != nil {
			t.Fatalf("projectSourceBinding(statsExecBit=%v): %v", statsExecBit, err)
		}
		return got
	}
	if posix := binding(0o755, true); posix != committed {
		t.Fatalf("a checkout with an executable bit: %s, want the commit's %s", posix, committed)
	}
	if windows := binding(0o644, false); windows != committed {
		t.Fatalf("a checkout without an executable bit: %s, want the commit's %s", windows, committed)
	}
	if runtime.GOOS != "windows" {
		if stale := binding(0o644, true); stale == committed {
			t.Fatal("a host with an executable bit ignored a cleared one")
		}
	}
}
