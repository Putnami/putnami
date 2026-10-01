package gitread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitSourceBindingMatchesCleanWorktreeAndTracksOnlyBoundSource(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	project := filepath.Join(repo, "project")
	files := map[string]string{
		"project/app.go":                                 "package project\n",
		"project/tool.sh":                                "#!/bin/sh\n",
		"project/.gen/schema/capabilities.json":          "generated\n",
		"project/schema/capabilities.json":               "promoted\n",
		"project/schema/feature-evidence/framework.json": "evidence\n",
	}
	for relative, contents := range files {
		filename := filepath.Join(repo, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(project, "tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("app.go", filepath.Join(project, "app-link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "project")
	runGit(t, repo, "commit", "-m", "add project")

	worktree, err := ProjectSourceBinding(repo, project)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := CommitSourceBinding(repo, "HEAD", "project")
	if err != nil {
		t.Fatal(err)
	}
	if committed != worktree {
		t.Fatalf("commit/worktree source binding mismatch: %s != %s", committed, worktree)
	}

	for relative, contents := range map[string]string{
		"project/.gen/schema/capabilities.json":          "changed generated\n",
		"project/schema/capabilities.json":               "changed promoted\n",
		"project/schema/feature-evidence/framework.json": "changed evidence\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(relative)), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "add", "project")
	runGit(t, repo, "commit", "-m", "change excluded outputs")
	excludedOnly, err := CommitSourceBinding(repo, "HEAD", "project")
	if err != nil {
		t.Fatal(err)
	}
	if excludedOnly != committed {
		t.Fatalf("excluded output changes moved binding: %s -> %s", committed, excludedOnly)
	}

	if err := os.WriteFile(filepath.Join(project, "app.go"), []byte("package project\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "project/app.go")
	runGit(t, repo, "commit", "-m", "change source")
	changed, err := CommitSourceBinding(repo, "HEAD", "project")
	if err != nil {
		t.Fatal(err)
	}
	if changed == excludedOnly {
		t.Fatal("authored source change did not move commit binding")
	}
}

func TestResolveCommitRejectsUnsafeAndAmbiguousRevisionArguments(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	sha := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	resolved, err := ResolveCommit(repo, sha)
	if err != nil || resolved != strings.ToLower(sha) {
		t.Fatalf("ResolveCommit(full SHA) = %q, %v", resolved, err)
	}

	runGit(t, repo, "branch", "shared", sha)
	runGit(t, repo, "tag", "shared", sha)
	if _, err := ResolveCommit(repo, "shared"); err == nil {
		t.Fatal("ambiguous short ref resolved without an error")
	}

	for _, revision := range []string{"", " HEAD", "HEAD ", "-c", "HEAD\nmain", strings.Repeat("x", 1025)} {
		if _, err := ResolveCommit(repo, revision); err == nil {
			t.Errorf("unsafe revision %q resolved without an error", revision)
		}
	}
}

func TestTreeEntryAtTreatsGitPathspecSyntaxLiterally(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	relative := "project/:(glob)*.txt"
	filename := filepath.Join(repo, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("literal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "--", ":(literal)"+relative)
	runGit(t, repo, "commit", "-m", "literal pathspec filename")
	sha := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))

	entry, exists, err := TreeEntryAt(repo, sha, relative)
	if err != nil || !exists {
		t.Fatalf("TreeEntryAt(literal path) = %+v, %t, %v", entry, exists, err)
	}
	data, err := ReadTreeBlob(repo, entry.ObjectID, 100)
	if err != nil || string(data) != "literal\n" {
		t.Fatalf("ReadTreeBlob(literal path) = %q, %v", data, err)
	}
}
