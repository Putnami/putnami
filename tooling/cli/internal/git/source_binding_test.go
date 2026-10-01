package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	protocaps "go.putnami.dev/protocol/capabilities"
)

// projectSourceBinding drops the subprocess count these binding tests do not
// assert on.
func projectSourceBinding(repoRoot, projectRoot string) (string, error) {
	binding, _, err := ProjectSourceBindingMeasured(repoRoot, projectRoot)
	return binding, err
}

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
	if !hostStatsExecBit {
		// Windows stores no executable bit on disk, so the index carries it.
		runGit(t, repo, "update-index", "--chmod=+x", "project/tool.sh")
	}
	runGit(t, repo, "commit", "-m", "add project")

	first, err := projectSourceBinding(repo, project)
	if err != nil {
		t.Fatalf("ProjectSourceBinding: %v", err)
	}
	cleanAgain, err := projectSourceBinding(repo, project)
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
	second, err := projectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("generated/promoted/ignored-only changes moved binding: %s -> %s", first, second)
	}
	if err := os.WriteFile(filepath.Join(project, "tracked-ignored.txt"), []byte("changed tracked despite ignore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trackedIgnored, err := projectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if trackedIgnored == second {
		t.Fatal("a tracked file must remain bound after an ignore rule is added")
	}

	if err := os.WriteFile(filepath.Join(project, "untracked.go"), []byte("package project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := projectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if third == trackedIgnored {
		t.Fatal("non-ignored untracked source must change the binding")
	}
	if hostStatsExecBit {
		if err := os.Chmod(filepath.Join(project, "tool.sh"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		runGit(t, repo, "update-index", "--chmod=-x", "project/tool.sh")
	}
	modeChanged, err := projectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if modeChanged == third {
		t.Fatal("changing a tracked file from executable to regular must change the binding")
	}
	if err := os.Remove(filepath.Join(project, "tool.sh")); err != nil {
		t.Fatal(err)
	}
	deleted, err := projectSourceBinding(repo, project)
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
	symlinkChanged, err := projectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if symlinkChanged == deleted {
		t.Fatal("changing a tracked symlink target must change the binding")
	}
	if err := os.WriteFile(filepath.Join(project, "app.go"), []byte("package project\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fourth, err := projectSourceBinding(repo, project)
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
	gitlinkChanged, err := projectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	if gitlinkChanged == fourth {
		t.Fatal("advancing the visible worktree commit of a tracked gitlink must change the binding")
	}
	if err := os.RemoveAll(depPath); err != nil {
		t.Fatal(err)
	}
	gitlinkDeleted, err := projectSourceBinding(repo, project)
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
	first, err := projectSourceBinding(repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package root\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := projectSourceBinding(repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("repository-root project source change did not move binding")
	}
}

func TestProjectSourceBindingMeasuredCountsGitProcesses(t *testing.T) {
	repo := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "root.go"), []byte("package root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "root.go")
	runGit(t, repo, "commit", "-m", "add root project")

	measured, metrics, err := ProjectSourceBindingMeasured(repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := projectSourceBinding(repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	if measured != plain {
		t.Fatalf("measured binding %q differs from ordinary binding %q", measured, plain)
	}
	if metrics.SpawnedProcesses != 2 {
		t.Fatalf("spawned processes = %d, want the two ls-files calls", metrics.SpawnedProcesses)
	}
}

func TestProjectSourceBindingUsesIndexOIDForEmptyUninitializedGitlink(t *testing.T) {
	repo := initGitRepo(t)
	dependency := initGitRepo(t)
	project := filepath.Join(repo, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", dependency, "project/vendor/uninitialized")
	runGit(t, repo, "commit", "-m", "add gitlink")
	initialized, err := projectSourceBinding(repo, project)
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
	uninitialized, err := projectSourceBinding(repo, project)
	if err != nil {
		t.Fatalf("empty uninitialized gitlink: %v", err)
	}
	if uninitialized != initialized {
		t.Fatalf("index OID parity mismatch: %s != %s", uninitialized, initialized)
	}
	if err := os.WriteFile(filepath.Join(gitlink, "fake.txt"), []byte("not a worktree"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := projectSourceBinding(repo, project); err == nil || !strings.Contains(err.Error(), "exact Git worktree") {
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
	if _, err := projectSourceBinding(repo, project); err == nil || !strings.Contains(err.Error(), "unmerged index stage") {
		t.Fatalf("unmerged index error = %v", err)
	}
	// Repository-wide enumeration must not turn a conflict in one project into
	// an unavailable binding for every other project.
	unrelated := filepath.Join(repo, "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot, metrics, err := ReadSourceBindingSnapshot(repo)
	if err != nil || metrics.SpawnedProcesses != 2 {
		t.Fatalf("snapshot: metrics=%+v err=%v", metrics, err)
	}
	if _, _, err := snapshot.ProjectSourceBindingMeasured(unrelated); err != nil {
		t.Fatalf("unrelated conflict poisoned project binding: %v", err)
	}
	for _, root := range []string{project, repo} {
		if _, _, err := snapshot.ProjectSourceBindingMeasured(root); err == nil || !strings.Contains(err.Error(), "unmerged index stage") {
			t.Fatalf("conflicted binding %s: %v", root, err)
		}
	}
}

func TestSourceBindingSnapshotSharesEnumerationAcrossExactRoots(t *testing.T) {
	repo := initGitRepo(t)
	files := map[string]string{
		"README.md":          "# Test",
		"one/source.go":      "package one\n",
		"one/nested/new.go":  "package nested\n",
		"one-more/source.go": "package sibling\n",
		"literal [x]/new.go": "package literal\n",
	}
	for name, data := range files {
		absolute := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "add", "one/source.go", "one-more/source.go")
	snapshot, metrics, err := ReadSourceBindingSnapshot(repo)
	if err != nil || metrics.SpawnedProcesses != 2 {
		t.Fatalf("snapshot: metrics=%+v err=%v", metrics, err)
	}
	// Calls on the same immutable enumeration can be concurrent. Each result
	// must contain exactly that root's descendants, with project-relative paths.
	for _, root := range []string{".", "one", "one/nested", "one-more", "literal [x]"} {
		t.Run(root, func(t *testing.T) {
			t.Parallel()
			var records []protocaps.SourceBindingFile
			for name, data := range files {
				relative, ok := strings.CutPrefix(name, root+"/")
				if root == "." || ok {
					records = append(records, protocaps.SourceBindingFile{
						Path: relative, Mode: protocaps.SourceModeRegular, Digest: protocaps.SourceDigest([]byte(data)),
					})
				}
			}
			want, err := protocaps.ComputeSourceBinding(records)
			if err != nil {
				t.Fatal(err)
			}
			got, metrics, err := snapshot.ProjectSourceBindingMeasured(filepath.Join(repo, root))
			if err != nil || got != want || metrics.SpawnedProcesses != 0 {
				t.Fatalf("binding=%s want=%s metrics=%+v err=%v", got, want, metrics, err)
			}
		})
	}
}

func TestSourceBindingSnapshotReadsCurrentBytesAndRejectsEscapes(t *testing.T) {
	repo := initGitRepo(t)
	file := filepath.Join(repo, "source.go")
	if err := os.WriteFile(file, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := ReadSourceBindingSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := snapshot.ProjectSourceBindingMeasured(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, metrics, err := snapshot.ProjectSourceBindingMeasured(repo)
	if err != nil || before == after || metrics.SpawnedProcesses != 0 {
		t.Fatalf("snapshot retained file bytes: before=%s after=%s metrics=%+v err=%v", before, after, metrics, err)
	}
	if _, _, err := snapshot.ProjectSourceBindingMeasured(filepath.Dir(repo)); err == nil || !strings.Contains(err.Error(), "outside repository root") {
		t.Fatalf("escaping root error = %v", err)
	}
	if _, _, err := snapshot.ProjectSourceBindingMeasured(filepath.Join(repo, "missing")); err == nil || !strings.Contains(err.Error(), "inspect project root") {
		t.Fatalf("missing root error = %v", err)
	}
}
