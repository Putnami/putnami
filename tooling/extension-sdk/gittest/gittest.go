// Package gittest runs the git of a test fixture, isolated from the git
// environment of the machine that runs the test.
//
// A fixture that reads the caller's git environment gives another repository
// on another machine. A GIT_ variable can name another identity, another
// repository or another configuration. A global configuration can sign
// commits, run hooks or name another default branch. Git also reads an ignore
// and an attributes file under XDG_CONFIG_HOME without any configuration, so a
// global ignore file can drop fixture files from a commit.
//
// Every test of the repository that runs git goes through this package. Only
// tests and the helper packages that only tests import use it; no product package
// imports it.
package gittest

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/gitread"
)

// The identity of a fixture commit that names none of its own.
const (
	Name  = "Test"
	Email = "test@test.com"
)

// DefaultBranch is the branch git init creates. Git's own default depends on
// its version and on the configuration this package hides.
const DefaultBranch = "main"

// Environ returns environ without its GIT_ variables, plus an empty global and
// system configuration, no global ignore or attributes file, and DefaultBranch
// as the branch git init creates. It sets no identity: a fixture that commits
// sets its own, in its repository configuration or in GIT_AUTHOR_ and
// GIT_COMMITTER_ variables it appends.
func Environ(environ []string) []string {
	env := make([]string, 0, len(environ)+12)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=init.defaultBranch",
		"GIT_CONFIG_VALUE_0="+DefaultBranch,
		"GIT_CONFIG_KEY_1=core.excludesFile",
		"GIT_CONFIG_VALUE_1="+os.DevNull,
		"GIT_CONFIG_KEY_2=core.attributesFile",
		"GIT_CONFIG_VALUE_2="+os.DevNull,
	)
}

// Env returns Environ of the process environment, plus Name and Email as the
// author and committer. A variable appended after them wins.
func Env() []string {
	return append(Environ(os.Environ()),
		"GIT_AUTHOR_NAME="+Name,
		"GIT_AUTHOR_EMAIL="+Email,
		"GIT_COMMITTER_NAME="+Name,
		"GIT_COMMITTER_EMAIL="+Email,
	)
}

// Command returns a git command that runs in dir with Env. An empty dir runs
// it in the current directory.
func Command(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command(gitread.Executable(), args...)
	cmd.Dir = dir
	cmd.Env = Env()
	return cmd
}

// Run runs git in dir with Env and returns its output without surrounding
// space. A failure ends the test with git's output.
func Run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := Command(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
