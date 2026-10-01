package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- GetVersionInfo ----

func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	runGit("init")
	runGit("checkout", "-b", "main")
	runGit("config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0644)
	runGit("add", ".")
	runGit("commit", "-m", "initial")
	return dir
}

func TestGetVersionInfo_InGitRepo(t *testing.T) {
	dir := initTestRepo(t)

	info, err := GetVersionInfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.SHA == "" {
		t.Error("expected non-empty SHA")
	}
	if len(info.SHA) < 4 {
		t.Errorf("expected SHA of at least 4 chars, got %q", info.SHA)
	}
	if info.Branch != "main" {
		t.Errorf("expected branch 'main', got %q", info.Branch)
	}
	if info.IsDirty {
		t.Error("expected clean repo")
	}
	if info.Suffix == "" {
		t.Error("expected non-empty suffix")
	}
	if !strings.HasSuffix(info.Suffix, "-"+info.SHA) || len(info.Suffix) != len("20060102150405")+1+len(info.SHA) {
		t.Errorf("expected suffix <commitTime>-<sha> for clean repo, got suffix=%q sha=%q", info.Suffix, info.SHA)
	}
	for _, char := range info.Suffix[:14] {
		if char < '0' || char > '9' {
			t.Fatalf("suffix %q does not start with a 14-digit UTC commit time", info.Suffix)
		}
	}
}

func TestGetVersionInfo_DirtyRepo(t *testing.T) {
	dir := initTestRepo(t)

	// Make repo dirty
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("modified"), 0644)

	info, err := GetVersionInfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !info.IsDirty {
		t.Error("expected dirty repo")
	}
	if strings.HasSuffix(info.Suffix, "-"+info.SHA) {
		t.Errorf("expected a dirty hash after the SHA for a dirty repo, got %q", info.Suffix)
	}
	if !strings.Contains(info.Suffix, "-"+info.SHA+"-") {
		t.Errorf("expected suffix <commitTime>-<sha>-<dirtyHash> for dirty repo, got %q", info.Suffix)
	}
}

func TestGetVersionInfo_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	// GetVersionInfo uses exec.Run which returns error differently
	// In a non-git dir, the git rev-parse command will fail
	info, err := GetVersionInfo(dir)
	// Either err is non-nil or info has empty SHA
	if err == nil && info != nil && info.SHA != "" {
		t.Error("expected error or empty SHA for non-git directory")
	}
}

// ---- sha256Hash ----

func TestSha256Hash_Deterministic(t *testing.T) {
	input := "hello world"
	a := sha256Hash(input)
	b := sha256Hash(input)
	if a != b {
		t.Errorf("expected deterministic output, got %q and %q", a, b)
	}
}

func TestSha256Hash_Length(t *testing.T) {
	got := sha256Hash("test input")
	if len(got) != 7 {
		t.Errorf("expected length 7, got %d (%q)", len(got), got)
	}
}

func TestSha256Hash_DifferentInputs(t *testing.T) {
	a := sha256Hash("input one")
	b := sha256Hash("input two")
	if a == b {
		t.Errorf("expected different hashes for different inputs, both got %q", a)
	}
}

// ---- gitCmd ----

func TestGitCmd_InRepo(t *testing.T) {
	dir := initTestRepo(t)

	output, err := gitCmd(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != "main" {
		t.Errorf("expected 'main', got %q", output)
	}
}

func TestGitCmd_OutputTrimmed(t *testing.T) {
	dir := initTestRepo(t)

	output, err := gitCmd(dir, "log", "--oneline", "-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Output should not have trailing newline
	if output != strings.TrimRight(output, "\n") {
		t.Errorf("output should be trimmed")
	}
	if len(output) == 0 {
		t.Error("expected non-empty output")
	}
}

func TestGitCmd_NonZeroExitReturnsError(t *testing.T) {
	dir := initTestRepo(t)

	// An unknown subcommand exits non-zero; gitCmd must surface that as an
	// error rather than silently returning an empty string.
	out, err := gitCmd(dir, "this-is-not-a-git-command")
	if err == nil {
		t.Fatalf("expected error for failing git command, got output %q", out)
	}
	if out != "" {
		t.Errorf("expected empty output on failure, got %q", out)
	}
}

func TestGitCmd_NotAGitRepoReturnsError(t *testing.T) {
	dir := t.TempDir()

	out, err := gitCmd(dir, "status", "--porcelain")
	if err == nil {
		t.Fatalf("expected error for non-git directory, got output %q", out)
	}
}

// ---- GetVersionInfo error propagation ----

func TestGetVersionInfo_NonGitRepoErrors(t *testing.T) {
	dir := t.TempDir()

	info, err := GetVersionInfo(dir)
	if err == nil {
		t.Fatalf("expected error for non-git directory, got info=%+v", info)
	}
	if info != nil {
		t.Errorf("expected nil info on error, got %+v", info)
	}
}

// A tag on HEAD is not this file's business: which tag a commit is a release of
// is the orchestrator's answer, and this reader only derives the suffix.
func TestGetVersionInfo_IgnoresTags(t *testing.T) {
	dir := initTestRepo(t)
	cmd := exec.Command("git", "tag", "v1.0.0")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag failed: %v\n%s", err, out)
	}

	info, err := GetVersionInfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.SHA == "" || info.Suffix == "" {
		t.Errorf("expected the tree state to be populated, got %+v", info)
	}
}

// ---- PUTNAMI_SOURCE_REVISION ----

func gitRaw(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return string(out)
}

// A bound commit the repository has carries its first 12 characters and its
// own committer time: the suffix the CLI stamps for the same variable.
func TestGetVersionInfo_SourceRevisionInRepository(t *testing.T) {
	dir := initTestRepo(t)
	head := strings.TrimSpace(gitRaw(t, dir, "rev-parse", "HEAD"))
	seconds, err := strconv.ParseInt(strings.TrimSpace(gitRaw(t, dir, "show", "-s", "--format=%ct", "HEAD")), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(SourceRevisionEnv, head)
	t.Setenv(SourceCommitTimeEnv, "")

	info, err := GetVersionInfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.SHA != head[:12] {
		t.Errorf("SHA = %q, want the first 12 characters of the bound commit %q", info.SHA, head[:12])
	}
	if want := time.Unix(seconds, 0).UTC().Format("20060102150405") + "-" + head[:12]; info.Suffix != want {
		t.Errorf("Suffix = %q, want %q", info.Suffix, want)
	}
	if info.Branch != "main" || info.IsDirty {
		t.Errorf("branch and dirty state stay the checkout's, got branch=%q dirty=%v", info.Branch, info.IsDirty)
	}
}

// A bound commit the repository lacks takes its time from
// PUTNAMI_SOURCE_COMMIT_TIME, and the checkout's dirty hash still follows.
func TestGetVersionInfo_SourceRevisionOutsideRepository(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SourceRevisionEnv, "0123456789abcdef0123456789abcdef01234567")
	t.Setenv(SourceCommitTimeEnv, "1767323045")

	info, err := GetVersionInfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "20260102030405-0123456789ab-" + sha256Hash(gitRaw(t, dir, "diff", "HEAD", "--no-color"))
	if info.Suffix != want || info.SHA != "0123456789ab" || !info.IsDirty {
		t.Errorf("got suffix=%q sha=%q dirty=%v, want suffix=%q sha=%q dirty=true", info.Suffix, info.SHA, info.IsDirty, want, "0123456789ab")
	}
}

func TestGetVersionInfo_SourceRevisionOutsideRepositoryRequiresTime(t *testing.T) {
	dir := initTestRepo(t)
	t.Setenv(SourceRevisionEnv, "0123456789abcdef0123456789abcdef01234567")
	for _, value := range []string{"", "0", "yesterday"} {
		t.Setenv(SourceCommitTimeEnv, value)
		if info, err := GetVersionInfo(dir); err == nil {
			t.Errorf("%s=%q: expected an error, got %+v", SourceCommitTimeEnv, value, info)
		}
	}
}

func TestGetVersionInfo_SourceRevisionMalformed(t *testing.T) {
	dir := initTestRepo(t)
	for _, value := range []string{"abc1234", "0123456789ABCDEF0123456789ABCDEF01234567", "HEAD"} {
		t.Setenv(SourceRevisionEnv, value)
		if info, err := GetVersionInfo(dir); err == nil {
			t.Errorf("%s=%q: expected an error, got %+v", SourceRevisionEnv, value, info)
		}
	}
}

// A blank variable is the same as an unset one: HEAD with git's abbreviation.
func TestGetVersionInfo_SourceRevisionBlankMeansHead(t *testing.T) {
	dir := initTestRepo(t)
	t.Setenv(SourceRevisionEnv, "  ")

	info, err := GetVersionInfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := strings.TrimSpace(gitRaw(t, dir, "rev-parse", "--short", "HEAD")); info.SHA != want {
		t.Errorf("SHA = %q, want HEAD's abbreviation %q", info.SHA, want)
	}
}

// The dirty hash is the CLI's: the first 7 hex characters of the SHA-256 of
// the untrimmed `git diff HEAD` output, present whenever `git status` reports
// a change, even one the diff does not show.
func TestGetVersionInfo_DirtyHashMatchesCLI(t *testing.T) {
	dir := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := GetVersionInfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "-" + info.SHA + "-" + sha256Hash(gitRaw(t, dir, "diff", "HEAD", "--no-color")); !strings.HasSuffix(info.Suffix, want) {
		t.Errorf("Suffix = %q, want it to end with %q", info.Suffix, want)
	}

	untracked := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(untracked, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = GetVersionInfo(untracked)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "-" + info.SHA + "-" + sha256Hash(""); !info.IsDirty || !strings.HasSuffix(info.Suffix, want) {
		t.Errorf("untracked change: got suffix=%q dirty=%v, want it to end with %q", info.Suffix, info.IsDirty, want)
	}
}
