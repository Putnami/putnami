// Package gittest builds hermetic git repositories for tests: a fresh
// repository in a temporary directory, with no global or system git
// configuration and no network.
package gittest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Repo is a repository under construction.
type Repo struct {
	t   testing.TB
	Dir string
	env []string
}

// New initializes an empty repository on branch main.
func New(t testing.TB) *Repo {
	t.Helper()
	dir := t.TempDir()
	home := t.TempDir()
	repo := &Repo{t: t, Dir: dir, env: append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)}
	repo.Git("init", "-q", "-b", "main")
	repo.Git("config", "commit.gpgsign", "false")
	// A merge checks the committer identity even with --no-commit, and a CI
	// runner has none; Commit and Merge still set their own.
	repo.Git("config", "user.name", "gittest")
	repo.Git("config", "user.email", "gittest@example.test")
	return repo
}

// Git runs a git command in the repository and returns its trimmed output.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	return r.run(nil, nil, args...)
}

func (r *Repo) run(extraEnv []string, stdin []byte, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(append([]string{}, r.env...), extraEnv...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// Shell runs a command line from the repository root, the way a reader
// reproduces a marker's evidence.
func (r *Repo) Shell(line string) string {
	r.t.Helper()
	cmd := exec.Command("sh", "-c", line)
	cmd.Dir = r.Dir
	cmd.Env = r.env
	out, err := cmd.Output()
	// git grep exits 1 when nothing matches, which is a valid reading.
	var exitErr *exec.ExitError
	if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		r.t.Fatalf("%s: %v", line, err)
	}
	return string(out)
}

// Write creates or replaces a file, creating its directories.
func (r *Repo) Write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.Dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// Commit stages everything and commits it as author at the given time, and
// returns the commit id.
func (r *Repo) Commit(message, author string, at time.Time) string {
	r.t.Helper()
	r.Git("add", "-A")
	r.run(authorEnv(author, at), nil, "commit", "-q", "--allow-empty", "-m", message)
	return r.Git("rev-parse", "HEAD")
}

// Merge merges a branch into the current one with a merge commit.
func (r *Repo) Merge(branch, message, author string, at time.Time) string {
	r.t.Helper()
	r.run(authorEnv(author, at), nil, "merge", "-q", "--no-ff", "-m", message, branch)
	return r.Git("rev-parse", "HEAD")
}

// Import feeds a git fast-import stream, then checks out main.
func (r *Repo) Import(stream []byte) {
	r.t.Helper()
	r.run(nil, stream, "fast-import", "--quiet")
	r.Git("reset", "-q", "--hard", "main")
}

func authorEnv(author string, at time.Time) []string {
	date := fmt.Sprintf("%d +0000", at.Unix())
	name, _, _ := strings.Cut(author, "@")
	return []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + author, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + author, "GIT_COMMITTER_DATE=" + date,
	}
}
