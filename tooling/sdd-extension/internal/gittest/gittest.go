// Package gittest runs git for the tests of this module, isolated from the
// git configuration of the machine that runs them.
//
// A test that commits must not read the global or system configuration: a
// global commit.gpgsign, core.hooksPath or init.defaultBranch changes what the
// commit does, and the test then fails on one machine and passes on another.
package gittest

import (
	"os"
	"os/exec"
	"strings"
)

// The identity every fixture commit carries.
const (
	Name  = "Test"
	Email = "test@test.com"
)

// The branch git init creates. Fixed here because git's own default depends on
// its version and on the configuration this package hides.
const DefaultBranch = "main"

// Env returns the environment of a fixture git command: the process
// environment without its GIT_ variables, which can point git at another
// configuration or repository, plus an empty global and system configuration,
// the fixture identity and the default branch.
func Env() []string {
	environ := os.Environ()
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
		"GIT_AUTHOR_NAME="+Name,
		"GIT_AUTHOR_EMAIL="+Email,
		"GIT_COMMITTER_NAME="+Name,
		"GIT_COMMITTER_EMAIL="+Email,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=init.defaultBranch",
		"GIT_CONFIG_VALUE_0="+DefaultBranch,
	)
}

// Command returns a git command that runs in dir with Env. An empty dir runs
// it in the current directory.
func Command(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...) //nolint:gosec // fixture arguments from the test itself
	cmd.Dir = dir
	cmd.Env = Env()
	return cmd
}
