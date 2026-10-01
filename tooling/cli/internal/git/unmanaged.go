package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrNoProgram reports that no usable git program is on PATH.
var ErrNoProgram = errors.New("no git program is on PATH")

// ErrNotRepository reports that a directory is outside every git repository.
var ErrNotRepository = errors.New("not a git repository")

// UnmanagedError says why Git does not manage a directory and what to run so it
// does. It wraps ErrNoProgram or ErrNotRepository and renders as one line, so a
// command that needs git history refuses with "<command> needs git history:"
// followed by it, and nothing else.
type UnmanagedError struct {
	// Dir is the directory Git does not manage.
	Dir string
	// Cause is ErrNoProgram or ErrNotRepository.
	Cause error
}

func (e *UnmanagedError) Error() string {
	if errors.Is(e.Cause, ErrNoProgram) {
		return fmt.Sprintf("no git program is on PATH; install git, then run git init in %s and commit", e.Dir)
	}
	return fmt.Sprintf("%s is not a git repository; run git init there and commit", e.Dir)
}

func (e *UnmanagedError) Unwrap() error { return e.Cause }

// Unmanaged reports whether Git manages dir. It returns nil when dir is inside
// a repository, and an *UnmanagedError when no git program is on PATH or dir is
// outside every repository.
//
// It also returns nil when the probe fails for any other reason — a corrupt
// repository, a refused ownership check, a timeout — because reporting those as
// "no repository" would hide the real cause. A caller that asks after a git
// command failed keeps that command's error when the answer is nil. A caller
// that asks before any git command, as a run's cache-key source state does
// when the run captured no tree state, reads nil as "Git manages dir": the git
// commands that follow fail with their own detail.
func Unmanaged(dir string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return &UnmanagedError{Dir: dir, Cause: ErrNoProgram}
	}
	// LC_ALL=C pins the wording of git's refusal, which is the only signal that
	// separates "not a repository" from every other exit 128.
	_, stderr, err := runCaptureEnv(dir, append(os.Environ(), "LC_ALL=C"), "rev-parse", "--git-dir")
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return &UnmanagedError{Dir: dir, Cause: ErrNoProgram}
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return nil
	}
	switch {
	case strings.Contains(stderr, "not a git repository"):
		return &UnmanagedError{Dir: dir, Cause: ErrNotRepository}
	case isDeveloperToolsStub(stderr):
		return &UnmanagedError{Dir: dir, Cause: ErrNoProgram}
	}
	return nil
}

// isDeveloperToolsStub recognizes the placeholder macOS installs at
// /usr/bin/git before the developer tools are: it is on PATH, runs no git, and
// exits with one of these two notices.
func isDeveloperToolsStub(stderr string) bool {
	return strings.Contains(stderr, "xcrun: error: invalid active developer path") ||
		strings.Contains(stderr, "xcode-select: note: No developer tools were found")
}
