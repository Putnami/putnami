package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "checkout", "-b", "main"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create initial commit
	file := filepath.Join(dir, "README.md")
	if err := os.WriteFile(file, []byte("# Test"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "initial")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func addOriginMainRef(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
}

func commitNewFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", "add "+name)
}

func TestCurrentBranch(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	branch, err := CurrentBranch(dir)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "main" {
		t.Errorf("CurrentBranch = %q, want %q", branch, "main")
	}
}

func TestCurrentBranch_UnbornRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")

	branch, err := CurrentBranch(dir)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "main" {
		t.Errorf("CurrentBranch = %q, want %q", branch, "main")
	}
}

func TestHeadSHA(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	got, err := HeadSHA(dir)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	want := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	if got != want {
		t.Errorf("HeadSHA = %q, want %q", got, want)
	}
}

func TestHeadTree(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	first, err := HeadTree(dir)
	if err != nil {
		t.Fatalf("HeadTree: %v", err)
	}
	if want := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD^{tree}")); first != want {
		t.Errorf("HeadTree = %q, want %q", first, want)
	}
	// A new commit with the same content keeps the tree, as a squash-merge
	// or a rebase does.
	runGit(t, dir, "commit", "--allow-empty", "-m", "same content")
	if again, err := HeadTree(dir); err != nil || again != first {
		t.Errorf("HeadTree after an empty commit = %q, %v; want %q", again, err, first)
	}
	if _, err := HeadTree(t.TempDir()); err == nil {
		t.Error("HeadTree outside git returned a tree")
	}
}

func TestResolveCommitWorktreeCleanAndDiffCommitFiles(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	base, err := ResolveCommit(dir, "HEAD")
	if err != nil {
		t.Fatalf("ResolveCommit(base): %v", err)
	}
	clean, err := WorktreeClean(dir)
	if err != nil || !clean {
		t.Fatalf("WorktreeClean initial = %t, %v; want clean", clean, err)
	}

	// --name-only -z must preserve a newline in a valid Git path exactly. A
	// Windows file name cannot contain a newline, so there the name carries
	// the other bytes Git quotes without -z: a space and a non-ASCII letter.
	name := "changed\nname.txt"
	if runtime.GOOS == "windows" {
		name = "changed n\u00e4me.txt"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("change"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "--", name)
	runGit(t, dir, "commit", "-m", "change")
	head, err := ResolveCommit(dir, "HEAD")
	if err != nil {
		t.Fatalf("ResolveCommit(head): %v", err)
	}
	files, err := DiffCommitFiles(dir, base, head)
	if err != nil {
		t.Fatalf("DiffCommitFiles: %v", err)
	}
	if !reflect.DeepEqual(files, []string{name}) {
		t.Fatalf("DiffCommitFiles = %#v, want %#v", files, []string{name})
	}

	if err := os.WriteFile(filepath.Join(dir, "untracked"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err = WorktreeClean(dir)
	if err != nil || clean {
		t.Fatalf("WorktreeClean dirty = %t, %v; want dirty", clean, err)
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

func TestDiffCommitFiles_DisablesRenameDetection(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	from := "apps/from/file.go"
	to := "packages/to/file.go"
	if err := os.MkdirAll(filepath.Join(dir, "apps", "from"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, from), []byte("package moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "--", from)
	runGit(t, dir, "commit", "-m", "add source file")
	base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	if err := os.MkdirAll(filepath.Join(dir, "packages", "to"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "mv", "--", from, to)
	runGit(t, dir, "commit", "-m", "move source file")
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	want := []string{from, to}
	for _, configuredRenames := range []string{"true", "false"} {
		runGit(t, dir, "config", "diff.renames", configuredRenames)
		files, err := DiffCommitFiles(dir, base, head)
		if err != nil {
			t.Fatalf("DiffCommitFiles with diff.renames=%s: %v", configuredRenames, err)
		}
		if !reflect.DeepEqual(files, want) {
			t.Errorf("DiffCommitFiles with diff.renames=%s = %#v, want %#v", configuredRenames, files, want)
		}
	}
}

func TestCommitExists(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	sha := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	if !CommitExists(dir, sha) {
		t.Fatalf("CommitExists(%q) = false, want true", sha)
	}
	if CommitExists(dir, strings.Repeat("f", 40)) {
		t.Fatal("CommitExists(nonexistent sha) = true, want false")
	}
	if CommitExists(dir, "main") {
		t.Fatal("CommitExists(branch name) = true, want false")
	}
}

func TestCommitReachableFromHead(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	if !CommitReachableFromHead(dir, base) {
		t.Fatalf("CommitReachableFromHead(base) = false, want true")
	}

	if err := os.WriteFile(filepath.Join(dir, "future.txt"), []byte("future"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "future.txt")
	runGit(t, dir, "commit", "-m", "future")
	future := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "reset", "--hard", base)

	if CommitReachableFromHead(dir, future) {
		t.Fatal("CommitReachableFromHead(descendant) = true, want false")
	}
	if CommitReachableFromHead(dir, strings.Repeat("f", 40)) {
		t.Fatal("CommitReachableFromHead(nonexistent sha) = true, want false")
	}
}

func TestRemoteURL(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "git@github.com:Putnami/putnami.git")
	if got := RemoteURL(dir, "origin"); got != "git@github.com:Putnami/putnami.git" {
		t.Fatalf("RemoteURL = %q, want origin URL", got)
	}
	if got := RemoteURL(dir, "missing"); got != "" {
		t.Fatalf("RemoteURL missing = %q, want empty", got)
	}
}

func TestCurrentBranch_OtherBranch(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	cmd := exec.Command("git", "checkout", "-b", "feature")
	cmd.Dir = dir
	cmd.CombinedOutput()

	branch, err := CurrentBranch(dir)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "feature" {
		t.Errorf("CurrentBranch = %q, want %q", branch, "feature")
	}
}

func TestIsMainBranch_Main(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	if !IsMainBranch(dir) {
		t.Error("IsMainBranch = false, want true")
	}
}

func TestIsMainBranch_Feature(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	cmd := exec.Command("git", "checkout", "-b", "feature")
	cmd.Dir = dir
	cmd.CombinedOutput()

	if IsMainBranch(dir) {
		t.Error("IsMainBranch = true, want false")
	}
}

func TestIsMainBranch_InvalidRepo(t *testing.T) {
	t.Parallel()
	if IsMainBranch(t.TempDir()) {
		t.Error("IsMainBranch on non-repo should return false")
	}
}

func TestCommonDir(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	common := CommonDir(dir)
	if common == "" {
		t.Fatal("CommonDir returned empty for a git repo")
	}
	if !filepath.IsAbs(common) {
		t.Errorf("CommonDir = %q, want absolute path", common)
	}
	if filepath.Base(common) != ".git" {
		t.Errorf("CommonDir = %q, want a path ending in .git", common)
	}
}

func TestCommonDir_WorktreesAgree(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	// A linked worktree must resolve to the SAME common dir as the main
	// checkout — this is what lets every worktree of a repo share one store.
	wt := filepath.Join(t.TempDir(), "linked")
	cmd := exec.Command("git", "worktree", "add", "-b", "wt-branch", wt)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	main := CommonDir(dir)
	linked := CommonDir(wt)
	if main == "" || linked == "" {
		t.Fatalf("CommonDir empty: main=%q linked=%q", main, linked)
	}
	if main != linked {
		t.Errorf("worktrees disagree on common dir: main=%q linked=%q", main, linked)
	}
}

func TestCommonDir_NonRepo(t *testing.T) {
	t.Parallel()
	if got := CommonDir(t.TempDir()); got != "" {
		t.Errorf("CommonDir on non-repo = %q, want empty", got)
	}
}

func TestIgnoredDirs(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("generated/\nnode_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A fully-ignored directory with content on disk.
	if err := os.MkdirAll(filepath.Join(dir, "generated", "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "generated", "client", "index.ts"), []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A tracked, non-ignored directory.
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	ignored := IgnoredDirs(dir)
	if !ignored["generated"] {
		t.Errorf("IgnoredDirs missing %q; got %v", "generated", ignored)
	}
	if ignored["src"] {
		t.Errorf("IgnoredDirs should not include tracked dir %q; got %v", "src", ignored)
	}
}

func TestIgnoredDirs_InvalidRepo(t *testing.T) {
	t.Parallel()
	if got := IgnoredDirs(t.TempDir()); got != nil {
		t.Errorf("IgnoredDirs on non-repo = %v, want nil", got)
	}
}

// rootLine is the implicit line of a repository whose scopes declare none.
func rootLine() LineSpec { return LineSpec{TagPattern: "v{version}"} }

func TestGetVersionInfo_UntaggedLineStartsAtZero(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	info, err := GetVersionInfo(dir, rootLine())
	if err != nil {
		t.Fatalf("GetVersionInfo: %v", err)
	}
	if info.Base != "0.0.0" {
		t.Errorf("Base = %q, want 0.0.0: a line with no reachable tag has never released", info.Base)
	}
	if info.Tagged || info.Tag != "" {
		t.Errorf("Tagged/Tag = %v/%q, want an untagged commit", info.Tagged, info.Tag)
	}
	if info.Branch != "main" || info.SHA == "" {
		t.Errorf("Branch/SHA = %q/%q, want the tree state", info.Branch, info.SHA)
	}
	if info.Full != "0.0.0-"+info.Suffix {
		t.Errorf("Full = %q, want base-suffix", info.Full)
	}
	if info.IsDirty {
		t.Error("IsDirty should be false for a clean repo")
	}
}

func TestGetVersionInfo_TaggedCommitTakesTheTagVersion(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/version-from-git", "tag-sets-the-version", "tagged-commit-uses-the-tag-version")
	dir := initGitRepo(t)
	gitDo(t, dir, "tag", "-a", "ts/v0.4.0", "-m", "release")

	info, err := GetVersionInfo(dir, LineSpec{ScopePath: "typescript", TagPattern: "ts/v{version}"})
	if err != nil {
		t.Fatalf("GetVersionInfo: %v", err)
	}
	if !info.Tagged || info.Tag != "ts/v0.4.0" {
		t.Fatalf("Tagged/Tag = %v/%q, want the line tag at HEAD", info.Tagged, info.Tag)
	}
	if info.Base != "0.4.0" || info.Full != "0.4.0" {
		t.Errorf("Base/Full = %q/%q, want the tag's version with no suffix", info.Base, info.Full)
	}
	if info.Line != "typescript" {
		t.Errorf("Line = %q, want typescript", info.Line)
	}
}

// Another line's tag on the same commit belongs to that line, not to this one:
// a repository releases several lines from one merge commit.
func TestGetVersionInfo_AnotherLinesTagDoesNotTagThisLine(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	gitDo(t, dir, "tag", "-a", "go/v0.4.0", "-m", "release")

	info, err := GetVersionInfo(dir, LineSpec{ScopePath: "typescript", TagPattern: "ts/v{version}"})
	if err != nil {
		t.Fatalf("GetVersionInfo: %v", err)
	}
	if info.Tagged {
		t.Fatalf("Tagged = true for tag %q, want the go line's tag ignored", info.Tag)
	}
}

func TestGetVersionInfo_TwoTagsOfOneLineAtHeadAreRefused(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	gitDo(t, dir, "tag", "-a", "ts/v0.4.0", "-m", "a")
	gitDo(t, dir, "tag", "-a", "ts/v0.5.0", "-m", "b")

	_, err := GetVersionInfo(dir, LineSpec{ScopePath: "typescript", TagPattern: "ts/v{version}"})
	if err == nil || !strings.Contains(err.Error(), "--scope") {
		t.Fatalf("err = %v, want an ambiguity refusal naming --scope", err)
	}
}

// The advance is computed from the conventional commits that touch the line,
// and a pre-release is always at least a patch above the last tag (D9, R8).
func TestGetVersionInfo_AdvanceFollowsTheConventionalCommits(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/version-from-git", "conventional-advance", "docs-commit-still-advances-a-patch")
	spectest.Proves(t, "cli/version-from-git", "conventional-advance", "feat-advances-minor-before-one")
	for _, testCase := range []struct {
		name, subject, want string
	}{
		{"docs still advances a patch", "docs: rewrite the guide", "0.4.1"},
		{"fix advances a patch", "fix(ts): stop the leak", "0.4.1"},
		{"feat advances a minor", "feat(ts): add the thing", "0.5.0"},
		{"breaking is a minor before 1.0", "feat(ts)!: change the thing", "0.5.0"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			dir := initGitRepo(t)
			gitDo(t, dir, "tag", "-a", "ts/v0.4.0", "-m", "release")
			writeCommit(t, dir, "typescript/src.ts", testCase.subject)

			info, err := GetVersionInfo(dir, LineSpec{
				ScopePath: "typescript", TagPattern: "ts/v{version}", Pathspecs: []string{"typescript"}})
			if err != nil {
				t.Fatalf("GetVersionInfo: %v", err)
			}
			if info.Base != testCase.want {
				t.Errorf("Base = %q, want %q", info.Base, testCase.want)
			}
		})
	}
}

// A commit outside the line's pathspecs does not advance it, but the line is
// still a patch above its last tag: the commit exists, so the two builds must
// not publish under one number.
func TestGetVersionInfo_CommitsOutsideTheLineDoNotAdvanceIt(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	gitDo(t, dir, "tag", "-a", "ts/v0.4.0", "-m", "release")
	writeCommit(t, dir, "go/src.go", "feat(go): add the thing")

	info, err := GetVersionInfo(dir, LineSpec{
		ScopePath: "typescript", TagPattern: "ts/v{version}", Pathspecs: []string{"typescript"}})
	if err != nil {
		t.Fatalf("GetVersionInfo: %v", err)
	}
	if info.Base != "0.4.1" {
		t.Errorf("Base = %q, want the patch floor 0.4.1", info.Base)
	}
}

// A breaking change is a MAJOR once the line has released 1.0: the major number
// is a compatibility promise from then on.
func TestGetVersionInfo_BreakingIsMajorAfterOne(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/version-from-git", "conventional-advance", "breaking-advances-minor-before-one-major-after")
	dir := initGitRepo(t)
	gitDo(t, dir, "tag", "-a", "ts/v1.2.0", "-m", "release")
	writeCommit(t, dir, "typescript/src.ts", "feat(ts)!: change the thing")

	info, err := GetVersionInfo(dir, LineSpec{
		ScopePath: "typescript", TagPattern: "ts/v{version}", Pathspecs: []string{"typescript"}})
	if err != nil {
		t.Fatalf("GetVersionInfo: %v", err)
	}
	if info.Base != "2.0.0" {
		t.Errorf("Base = %q, want 2.0.0", info.Base)
	}
}

func TestGetVersionInfo_DirtyRepo(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := GetVersionInfo(dir, rootLine())
	if err != nil {
		t.Fatalf("GetVersionInfo: %v", err)
	}
	if !info.IsDirty {
		t.Error("IsDirty should be true for a dirty repo")
	}
	if info.Suffix == info.SHA {
		t.Error("Suffix should include the dirty hash, not just the SHA")
	}
}

func TestGetVersionInfo_ShallowCloneIsRefused(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/version-from-git", "full-clone-required", "shallow-clone-is-refused")
	origin := initGitRepo(t)
	writeCommit(t, origin, "second.txt", "chore: second")
	shallow := filepath.Join(t.TempDir(), "shallow")
	cmd := exec.Command("git", "clone", "--depth", "1", "file://"+origin, shallow)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("shallow clone unavailable in this environment: %v\n%s", err, out)
	}

	_, err := GetVersionInfo(shallow, rootLine())
	if err == nil || !strings.Contains(err.Error(), "--unshallow") {
		t.Fatalf("err = %v, want the full-clone refusal", err)
	}
}

func TestGetVersionInfo_InvalidRepo(t *testing.T) {
	t.Parallel()
	if _, err := GetVersionInfo(t.TempDir(), rootLine()); err == nil {
		t.Error("GetVersionInfo on a non-repo should fail")
	}
}

// gitDo runs one git command in dir and fails the test when it does not.
func gitDo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z", "GIT_AUTHOR_DATE=2026-01-02T03:04:05Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeCommit writes one file and commits it with the given subject.
func writeCommit(t *testing.T, dir, relPath, subject string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(subject), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, dir, "add", ".")
	gitDo(t, dir, "commit", "-m", subject)
}

// resolveBaseline exercises the no-epic-branches resolution order these tests
// pin. Production has one caller and it passes the workspace's epic branches.
func resolveBaseline(repoRoot, explicit string) (string, error) {
	resolved, err := ResolveBaselineDetailed(repoRoot, explicit, nil)
	return resolved.Ref, err
}

func TestResolveBaseline_ExplicitWins(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	got, err := resolveBaseline(dir, "release/v2")
	if err != nil {
		t.Fatalf("ResolveBaseline: %v", err)
	}
	if got != "release/v2" {
		t.Errorf("ResolveBaseline = %q, want explicit ref", got)
	}
}

// TestResolveBaseline_PushedBranchPrefersTrunkOverOwnUpstream pins a
// fix: an earlier default preferred the upstream tracking ref, and on a
// pushed branch that ref already contains HEAD — the committed diff was empty
// and --impacted selected nothing exactly where a review diff exists.
func TestResolveBaseline_PushedBranchPrefersTrunkOverOwnUpstream(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	runGit(t, dir, "config", "remote.origin.url", ".")
	runGit(t, dir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, dir, "checkout", "-b", "feature")
	commitNewFile(t, dir, "feature.txt")
	runGit(t, dir, "update-ref", "refs/remotes/origin/feature", "HEAD")
	runGit(t, dir, "config", "branch.feature.remote", "origin")
	runGit(t, dir, "config", "branch.feature.merge", "refs/heads/feature")

	resolved, err := ResolveBaselineDetailed(dir, "", nil)
	if err != nil {
		t.Fatalf("ResolveBaselineDetailed: %v", err)
	}
	if resolved.Ref != "origin/main" {
		t.Errorf("ResolveBaselineDetailed = %q, want origin/main", resolved.Ref)
	}
	if resolved.Source != BaselineSourceTrunk {
		t.Errorf("source = %q, want %q", resolved.Source, BaselineSourceTrunk)
	}
}

func TestResolveBaseline_BranchTrackingTrunkStillResolvesTrunk(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	runGit(t, dir, "checkout", "-b", "feature")
	runGit(t, dir, "config", "branch.feature.remote", "origin")
	runGit(t, dir, "config", "branch.feature.merge", "refs/heads/main")

	got, err := resolveBaseline(dir, "")
	if err != nil {
		t.Fatalf("ResolveBaseline: %v", err)
	}
	if got != "origin/main" {
		t.Errorf("ResolveBaseline = %q, want origin/main", got)
	}
}

func TestResolveBaseline_UpstreamIsLastResort(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "branch", "-M", "trunk") // no main/master, no origin/HEAD
	runGit(t, dir, "config", "remote.origin.url", ".")
	runGit(t, dir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, dir, "update-ref", "refs/remotes/origin/trunk", "HEAD")
	runGit(t, dir, "checkout", "-b", "feat")
	runGit(t, dir, "config", "branch.feat.remote", "origin")
	runGit(t, dir, "config", "branch.feat.merge", "refs/heads/trunk")
	commitNewFile(t, dir, "feat.txt") // local work: the upstream has something to measure

	resolved, err := ResolveBaselineDetailed(dir, "", nil)
	if err != nil {
		t.Fatalf("ResolveBaselineDetailed: %v", err)
	}
	if resolved.Ref != "origin/trunk" {
		t.Errorf("ResolveBaselineDetailed = %q, want origin/trunk", resolved.Ref)
	}
	if resolved.Source != BaselineSourceUpstream {
		t.Errorf("source = %q, want %q", resolved.Source, BaselineSourceUpstream)
	}
}

// TestResolveBaseline_RenamedBranchNeverItsPushedCounterpart pins the review
// finding that a local branch renamed away from its remote name
// (alice/topic tracking origin/topic) defeats the name comparison, yet the
// upstream is still the branch's pushed counterpart sitting at HEAD — diffing
// against it selects no committed work. The ancestry signal catches it with no
// push config at all.
func TestResolveBaseline_RenamedBranchNeverItsPushedCounterpart(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "branch", "-M", "alice/topic") // no trunk candidates at all
	runGit(t, dir, "config", "remote.origin.url", ".")
	runGit(t, dir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	commitNewFile(t, dir, "topic.txt")
	runGit(t, dir, "update-ref", "refs/remotes/origin/topic", "HEAD") // pushed counterpart, different name
	runGit(t, dir, "config", "branch.alice/topic.remote", "origin")
	runGit(t, dir, "config", "branch.alice/topic.merge", "refs/heads/topic")

	if got, err := resolveBaseline(dir, ""); err == nil {
		t.Fatalf("ResolveBaseline = %q, want error: origin/topic is this branch's pushed counterpart", got)
	}
}

// The push-destination signal covers the counterpart pushed BEFORE new local
// commits landed: the upstream no longer contains HEAD, so ancestry alone
// would accept it, but @{push} names it as where this branch publishes —
// measuring against your own stale push is still self-measurement.
func TestResolveBaseline_StalePushedCounterpartStillExcluded(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "branch", "-M", "alice/topic")
	runGit(t, dir, "config", "remote.origin.url", ".")
	runGit(t, dir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, dir, "config", "push.default", "upstream")
	runGit(t, dir, "update-ref", "refs/remotes/origin/topic", "HEAD") // pushed at the old tip
	runGit(t, dir, "config", "branch.alice/topic.remote", "origin")
	runGit(t, dir, "config", "branch.alice/topic.merge", "refs/heads/topic")
	commitNewFile(t, dir, "newer.txt") // upstream now behind HEAD

	if got, err := resolveBaseline(dir, ""); err == nil {
		t.Fatalf("ResolveBaseline = %q, want error: origin/topic is this branch's push destination", got)
	}
}

func TestResolveBaseline_UpstreamNeverThisBranchItself(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "branch", "-M", "feature") // no trunk candidates at all
	runGit(t, dir, "config", "remote.origin.url", ".")
	runGit(t, dir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, dir, "update-ref", "refs/remotes/origin/feature", "HEAD")
	runGit(t, dir, "config", "branch.feature.remote", "origin")
	runGit(t, dir, "config", "branch.feature.merge", "refs/heads/feature")

	if got, err := resolveBaseline(dir, ""); err == nil {
		t.Fatalf("ResolveBaseline = %q, want error: a branch is never measured against itself", got)
	}
}

func TestResolveBaseline_DetachedHeadUsesTrunk(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	runGit(t, dir, "checkout", "--detach")

	resolved, err := ResolveBaselineDetailed(dir, "", nil)
	if err != nil {
		t.Fatalf("ResolveBaselineDetailed: %v", err)
	}
	if resolved.Ref != "origin/main" || resolved.Source != BaselineSourceTrunk {
		t.Errorf("ResolveBaselineDetailed = %q (%s), want origin/main (trunk)", resolved.Ref, resolved.Source)
	}
}

func TestResolveBaselineDetailed_NearestEpicWins(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	runGit(t, dir, "checkout", "-b", "epic/store")
	commitNewFile(t, dir, "epic.txt")
	runGit(t, dir, "update-ref", "refs/remotes/origin/epic/store", "HEAD")
	runGit(t, dir, "checkout", "-b", "feat")
	commitNewFile(t, dir, "feat.txt")

	resolved, err := ResolveBaselineDetailed(dir, "", []string{"epic/*"})
	if err != nil {
		t.Fatalf("ResolveBaselineDetailed: %v", err)
	}
	if resolved.Ref != "origin/epic/store" {
		t.Errorf("ResolveBaselineDetailed = %q, want origin/epic/store", resolved.Ref)
	}
	if resolved.Source != BaselineSourceEpic {
		t.Errorf("source = %q, want %q", resolved.Source, BaselineSourceEpic)
	}
}

func TestResolveBaselineDetailed_EpicTieKeepsTrunk(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	// The epic exists but sits at the same commit as trunk: it adds nothing, so
	// the tie keeps the trunk.
	runGit(t, dir, "update-ref", "refs/remotes/origin/epic/store", "HEAD")
	runGit(t, dir, "checkout", "-b", "feat")

	resolved, err := ResolveBaselineDetailed(dir, "", []string{"epic/*"})
	if err != nil {
		t.Fatalf("ResolveBaselineDetailed: %v", err)
	}
	if resolved.Ref != "origin/main" || resolved.Source != BaselineSourceTrunk {
		t.Errorf("ResolveBaselineDetailed = %q (%s), want origin/main (trunk)", resolved.Ref, resolved.Source)
	}
}

func TestResolveBaselineDetailed_EpicBranchItselfMeasuresAgainstTrunk(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	runGit(t, dir, "checkout", "-b", "epic/store")
	commitNewFile(t, dir, "epic.txt")
	runGit(t, dir, "update-ref", "refs/remotes/origin/epic/store", "HEAD")

	resolved, err := ResolveBaselineDetailed(dir, "", []string{"epic/*"})
	if err != nil {
		t.Fatalf("ResolveBaselineDetailed: %v", err)
	}
	if resolved.Ref != "origin/main" || resolved.Source != BaselineSourceTrunk {
		t.Errorf("ResolveBaselineDetailed = %q (%s), want origin/main (trunk): an epic is a branch like any other", resolved.Ref, resolved.Source)
	}
}

func TestResolveBaseline_OriginHeadFallback(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	runGit(t, dir, "checkout", "-b", "feature")

	got, err := resolveBaseline(dir, "")
	if err != nil {
		t.Fatalf("ResolveBaseline: %v", err)
	}
	if got != "origin/main" {
		t.Errorf("ResolveBaseline = %q, want origin/main", got)
	}
}

func TestResolveBaseline_LocalMainFallback(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "checkout", "-b", "feature")

	got, err := resolveBaseline(dir, "")
	if err != nil {
		t.Fatalf("ResolveBaseline: %v", err)
	}
	if got != "main" {
		t.Errorf("ResolveBaseline = %q, want main", got)
	}
}

func TestResolveBaseline_ErrorWhenNoCandidateExists(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "branch", "-M", "feature")

	if got, err := resolveBaseline(dir, ""); err == nil {
		t.Fatalf("ResolveBaseline = %q, want error", got)
	}
}

func TestResolveTrunk_OriginHeadWinsOverUpstream(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	runGit(t, dir, "update-ref", "refs/remotes/origin/feature", "HEAD")
	runGit(t, dir, "checkout", "-b", "feature")
	runGit(t, dir, "config", "branch.feature.remote", "origin")
	runGit(t, dir, "config", "branch.feature.merge", "refs/heads/feature")

	got, err := ResolveTrunk(dir)
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	if got != "origin/main" {
		t.Errorf("ResolveTrunk = %q, want origin/main", got)
	}
}

func TestResolveTrunk_OriginMainFallback(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, dir, "checkout", "-b", "feature")

	got, err := ResolveTrunk(dir)
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	if got != "origin/main" {
		t.Errorf("ResolveTrunk = %q, want origin/main", got)
	}
}

func TestResolveTrunk_LocalMainFallback(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "checkout", "-b", "feature")

	got, err := ResolveTrunk(dir)
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	if got != "main" {
		t.Errorf("ResolveTrunk = %q, want main", got)
	}
}

func TestResolveTrunk_ErrorWhenNoCandidateExists(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "branch", "-M", "feature")

	if got, err := ResolveTrunk(dir); err == nil {
		t.Fatalf("ResolveTrunk = %q, want error", got)
	}
}

func TestDiffFiles(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	// Create a feature branch and add a file
	cmd := exec.Command("git", "checkout", "-b", "feature")
	cmd.Dir = dir
	cmd.CombinedOutput()

	newFile := filepath.Join(dir, "new.go")
	if err := os.WriteFile(newFile, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "add", "new.go")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "add new.go")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	files, err := DiffFiles(dir, "main")
	if err != nil {
		t.Fatalf("DiffFiles: %v", err)
	}
	found := false
	for _, f := range files {
		if f == "new.go" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DiffFiles = %v, expected to contain 'new.go'", files)
	}
}

func TestDiffFiles_DisablesRenameDetection(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	from := "apps/from/file.go"
	to := "packages/to/file.go"
	if err := os.MkdirAll(filepath.Join(dir, "apps", "from"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, from), []byte("package moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "--", from)
	runGit(t, dir, "commit", "-m", "add source file")
	runGit(t, dir, "checkout", "-b", "feature")

	if err := os.MkdirAll(filepath.Join(dir, "packages", "to"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "mv", "--", from, to)
	runGit(t, dir, "commit", "-m", "move source file")

	want := []string{from, to}
	for _, configuredRenames := range []string{"true", "false"} {
		runGit(t, dir, "config", "diff.renames", configuredRenames)
		files, err := DiffFiles(dir, "main")
		if err != nil {
			t.Fatalf("DiffFiles with diff.renames=%s: %v", configuredRenames, err)
		}
		if !reflect.DeepEqual(files, want) {
			t.Errorf("DiffFiles with diff.renames=%s = %#v, want %#v", configuredRenames, files, want)
		}
	}
}

func TestDiffFiles_FallsBackToOriginBranchForBareBaseline(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	addOriginMainRef(t, dir)
	runGit(t, dir, "checkout", "-b", "feature")
	runGit(t, dir, "branch", "-D", "main")

	newFile := filepath.Join(dir, "new.go")
	if err := os.WriteFile(newFile, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "new.go")
	runGit(t, dir, "commit", "-m", "add new.go")

	files, err := DiffFiles(dir, "main")
	if err != nil {
		t.Fatalf("DiffFiles: %v", err)
	}
	found := false
	for _, f := range files {
		if f == "new.go" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DiffFiles = %v, expected to contain 'new.go'", files)
	}
}

func TestDiffFiles_UntrackedFiles(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)

	// Create an untracked file
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := DiffFiles(dir, "main")
	if err != nil {
		t.Fatalf("DiffFiles: %v", err)
	}

	found := false
	for _, f := range files {
		if f == "untracked.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DiffFiles = %v, expected to contain 'untracked.txt'", files)
	}
}

// DiffWorkingTree states the diff's evidence: the merge base it measured
// against, and which changed paths exist only on this machine. A committed
// change is not uncommitted; a staged, an unstaged and an untracked one are.
func TestDiffWorkingTree_NamesTheBaseAndTheUncommittedPaths(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-m", "add tracked")
	base := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "checkout", "-b", "feature")
	commitNewFile(t, dir, "committed.txt")

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := DiffWorkingTree(dir, "main")
	if err != nil {
		t.Fatalf("DiffWorkingTree: %v", err)
	}
	if diff.Base != base {
		t.Errorf("base = %q, want the merge base %q", diff.Base, base)
	}
	for _, want := range []string{"committed.txt", "tracked.txt", "staged.txt", "untracked.txt"} {
		if !slices.Contains(diff.Files, want) {
			t.Errorf("files = %v, missing %s", diff.Files, want)
		}
	}
	uncommitted := slices.Clone(diff.Uncommitted)
	slices.Sort(uncommitted)
	if want := []string{"staged.txt", "tracked.txt", "untracked.txt"}; !slices.Equal(uncommitted, want) {
		t.Errorf("uncommitted = %v, want %v", diff.Uncommitted, want)
	}
	files, err := DiffFiles(dir, "main")
	if err != nil || !slices.Equal(files, diff.Files) {
		t.Errorf("DiffFiles = %v, %v; want DiffWorkingTree's files %v", files, err, diff.Files)
	}

	// A clean tree has no uncommitted paths.
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "commit the rest")
	clean, err := DiffWorkingTree(dir, "main")
	if err != nil {
		t.Fatalf("DiffWorkingTree on a clean tree: %v", err)
	}
	if clean.Uncommitted != nil {
		t.Errorf("uncommitted on a clean tree = %v, want nil", clean.Uncommitted)
	}
}

// Without a merge base the diff falls back to the ref; the recorded base is
// still the commit that ref names, never the ref's name.
func TestDiffWorkingTree_ResolvesAFallbackBaseToACommit(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "checkout", "--orphan", "unrelated")
	runGit(t, dir, "rm", "-rf", "--cached", ".")
	commitNewFile(t, dir, "orphan.txt")
	want := strings.TrimSpace(runGit(t, dir, "rev-parse", "main"))

	diff, err := DiffWorkingTree(dir, "main")
	if err != nil {
		t.Fatalf("DiffWorkingTree: %v", err)
	}
	if diff.Base != want {
		t.Errorf("base = %q, want main's commit %q", diff.Base, want)
	}
}

func TestSplitLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"a\nb\nc", 3},
		{"a\n\nb\n", 2},
		{"  \n  a  \n  ", 1},
	}

	for _, tt := range tests {
		got := splitLines(tt.input)
		if len(got) != tt.want {
			t.Errorf("splitLines(%q) len = %d, want %d (got %v)", tt.input, len(got), tt.want, got)
		}
	}
}

func TestDedupStrings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input []string
		want  int
	}{
		{nil, 0},
		{[]string{"a", "b", "a"}, 2},
		{[]string{"a", "b", "c"}, 3},
		{[]string{"x", "x", "x"}, 1},
	}

	for _, tt := range tests {
		got := dedupStrings(tt.input)
		if len(got) != tt.want {
			t.Errorf("dedupStrings(%v) len = %d, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestDedupStrings_PreservesOrder(t *testing.T) {
	t.Parallel()
	input := []string{"c", "a", "b", "a", "c"}
	got := dedupStrings(input)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Errorf("got %v, want [c a b] (preserves first occurrence order)", got)
	}
}

// TestGetVersionInfo_SuffixIsOrderedByCommitTime pins the pre-release suffix
// shape: a fixed-width UTC commit time, then the SHA, so a newer commit sorts
// after an older one as one semver identifier and the SHA stays the last
// segment of a clean build.
func TestGetVersionInfo_SuffixIsOrderedByCommitTime(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	first, err := GetVersionInfo(dir, rootLine())
	if err != nil {
		t.Fatalf("GetVersionInfo: %v", err)
	}
	if matched, _ := regexp.MatchString(`^[0-9]{14}-[0-9a-f]{7,}$`, first.Suffix); !matched {
		t.Fatalf("suffix = %q, want <yyyymmddHHMMSS>-<sha>", first.Suffix)
	}
	if !strings.HasSuffix(first.Suffix, "-"+first.SHA) || first.Full != "0.0.0-"+first.Suffix {
		t.Fatalf("suffix/full = %q/%q, want the SHA last and Full = base-suffix", first.Suffix, first.Full)
	}
	if again, againErr := GetVersionInfo(dir, rootLine()); againErr != nil || again.Suffix != first.Suffix {
		t.Fatalf("suffix is not stable for one commit: %q vs %q", first.Suffix, again.Suffix)
	}

	// A later commit, committed later, sorts after the first one whatever its
	// SHA happens to start with.
	if err := os.WriteFile(filepath.Join(dir, "next.txt"), []byte("next"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"git", "add", "."}, {"git", "commit", "-m", "next"}} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2099-01-02T03:04:05Z", "GIT_AUTHOR_DATE=2099-01-02T03:04:05Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	second, err := GetVersionInfo(dir, rootLine())
	if err != nil {
		t.Fatalf("GetVersionInfo: %v", err)
	}
	if !strings.HasPrefix(second.Suffix, "20990102030405-") {
		t.Fatalf("suffix = %q, want the UTC committer time first", second.Suffix)
	}
	if !(first.Full < second.Full) {
		t.Fatalf("versions are not ordered by commit time: %q !< %q", first.Full, second.Full)
	}
	if got := OrderedSuffix(time.Date(2026, 9, 2, 17, 30, 0, 0, time.UTC), "8d5edb751", "a1b2c3d"); got != "20260902173000-8d5edb751-a1b2c3d" {
		t.Fatalf("OrderedSuffix = %q", got)
	}
}
