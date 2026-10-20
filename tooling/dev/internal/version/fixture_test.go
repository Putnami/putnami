package version

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wire "go.putnami.dev/protocol/extension/hostrpc"
)

// fakeHost answers version.lines with result or err, and records the scopes
// it was asked for.
type fakeHost struct {
	result wire.VersionLinesResult
	err    error

	mu     sync.Mutex
	scopes []string
}

func (h *fakeHost) VersionLines(_ context.Context, scope string) (wire.VersionLinesResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scopes = append(h.scopes, scope)
	return h.result, h.err
}

// asked returns the scopes the host was asked for.
func (h *fakeHost) asked() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.scopes...)
}

// outcome is what one command run answered.
type outcome struct {
	code           int
	stdout, stderr string
}

// runGet runs `version get args` against host in root with output mode mode.
func runGet(t *testing.T, root string, host Lines, mode protocolcli.OutputMode, args ...string) outcome {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := Command{Root: root, Output: mode, Host: host, Stdout: &stdout, Stderr: &stderr}
	code := command.Get(t.Context(), args)
	return outcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// runTag runs `version tag args` against host in root.
func runTag(t *testing.T, root string, host Lines, args ...string) outcome {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := Command{Root: root, Host: host, Stdout: &stdout, Stderr: &stderr}
	code := command.Tag(t.Context(), args)
	return outcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// gitEnv isolates a test's git from the machine's configuration.
var gitEnv = []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}

// git runs git in dir and returns its trimmed standard output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes content at the slash-separated path rel under dir.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// releaseRepo builds a repository with two commits that touch tooling/, whose
// local configuration names the human who releases and signs nothing. The
// command's own git reads that local configuration, which wins over the
// machine's. It returns the repository and its commits, oldest first.
func releaseRepo(t *testing.T) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not installed: %v", err)
	}
	dir := t.TempDir()
	hooks := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	for key, value := range map[string]string{
		"user.name": "Release Human", "user.email": "human@example.invalid",
		"commit.gpgsign": "false", "tag.gpgsign": "false", "tag.forceSignAnnotated": "false",
		"core.hooksPath": hooks,
	} {
		git(t, dir, "config", key, value)
	}
	writeFile(t, dir, "tooling/cli/main.go", "package main\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "feat: the workspace")
	writeFile(t, dir, "tooling/cli/fix.go", "package main\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "fix(cli): stop the leak", "-m", "The body says why.")
	return dir, strings.Fields(git(t, dir, "rev-list", "--reverse", "HEAD"))
}

// toolingLine is the untagged tooling line of releaseRepo as the host
// answers it: its proposed release and its commits, newest first.
func toolingLine(commits []string) wire.VersionLine {
	return wire.VersionLine{
		Line: "tooling", TagPattern: "tooling/v{version}", Version: "0.0.1-20260101000000-" + commits[1][:7],
		Release: &wire.LineRelease{Version: "0.0.1", Tag: "tooling/v0.0.1"},
		Commits: []wire.LineCommit{
			{SHA: commits[1], Subject: "fix(cli): stop the leak", Body: "The body says why."},
			{SHA: commits[0], Subject: "feat: the workspace"},
		},
	}
}

// releaseHost answers releaseRepo's tooling line, bound to HEAD.
func releaseHost(commits []string) *fakeHost {
	return &fakeHost{result: wire.VersionLinesResult{SourceRevision: commits[len(commits)-1], Lines: []wire.VersionLine{toolingLine(commits)}}}
}

// assertNothingWritten fails when the repository moved off head, carries a
// tag other than the ones it had, or has a change in its tree.
func assertNothingWritten(t *testing.T, dir, head string, tags ...string) {
	t.Helper()
	if got := git(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s, want %s", got, head)
	}
	if got := git(t, dir, "tag", "--list"); got != strings.Join(tags, "\n") {
		t.Errorf("tags %q, want %q", got, strings.Join(tags, "\n"))
	}
	if got := git(t, dir, "status", "--porcelain"); got != "" {
		t.Errorf("the tree changed: %q", got)
	}
}
