package finalize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/treefingerprint"

	"go.putnami.dev/tooling/contributor/internal/childenv"
)

// Git is the narrow repository boundary the finalizer needs. It deliberately
// has no staging or ordinary commit operation: ready publication consumes a
// clean, already committed candidate.
type Git interface {
	Branch() (string, error)
	Head() (string, error)
	BaseRef(string) (string, error)
	BaseSHA(string) (string, error)
	Clean() (bool, error)
	Staged() (bool, error)
	Ahead(string) (bool, error)
	ChangedPaths(string) ([]string, error)
	CommitMessages(string) ([]CommitMessage, error)
	Seed(context.Context, string) error
	Push(context.Context, string) error
	Fingerprint() (string, error)
}

type CommitMessage struct {
	Text  string
	Empty bool
}

type NativeGit struct{ Root string }

func (g NativeGit) run(args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("missing read-only git operation")
	}
	switch args[0] {
	case "branch", "rev-parse", "check-ref-format", "merge-base", "status", "diff", "rev-list", "log", "diff-tree":
	default:
		return nil, fmt.Errorf("unsupported read-only git operation %q", args[0])
	}
	// Read-only preflight must not start configured fsmonitor code before the
	// collaboration provider has prepared its credential holder.
	// Repository log.showSignature must not start a configured verifier during
	// this pre-provider read. The override applies only to this invocation.
	cmd := exec.Command("git", append([]string{"-C", g.Root, "--no-pager", "-c", "core.fsmonitor=false", "-c", "log.showSignature=false"}, args...)...)
	cmd.Env = gitEnvironment(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	answer, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return answer, nil
}

// gitWriteTimeout bounds each repository-writing git command, so a stalled
// remote, a credential prompt or a hook that never exits aborts the run
// instead of wedging it. It is generous enough not to interrupt a
// legitimately large push. A variable only so a test can shorten it.
var gitWriteTimeout = 5 * time.Minute

// gitWriteWaitDelay bounds how long a killed write may keep its output open
// through a hook or helper process that outlives it.
const gitWriteWaitDelay = 5 * time.Second

func (g NativeGit) runWrite(ctx context.Context, args ...string) error {
	if len(args) == 0 || args[0] != "commit" && args[0] != "push" {
		return fmt.Errorf("unsupported repository-writing git operation")
	}
	ctx, cancel := context.WithTimeout(ctx, gitWriteTimeout)
	defer cancel()
	// Commit hooks and push transport helpers may execute repository code.
	// The executable holds no run credential, so it has nothing to mark.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.Root}, args...)...)
	cmd.Env = gitEnvironment(os.Environ())
	cmd.WaitDelay = gitWriteWaitDelay
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("timed out after %s", gitWriteTimeout)
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// gitEnvironment is the environment of every git command: the repository
// selectors and the host channel are removed.
func gitEnvironment(input []string) []string {
	return childenv.Repository(input)
}

func (g NativeGit) one(args ...string) (string, error) {
	answer, err := g.run(args...)
	return strings.TrimSpace(string(answer)), err
}
func (g NativeGit) Branch() (string, error) { return g.one("branch", "--show-current") }
func (g NativeGit) Head() (string, error)   { return g.one("rev-parse", "HEAD") }
func (g NativeGit) BaseRef(base string) (string, error) {
	if base == "" || strings.HasPrefix(base, "-") || strings.ContainsAny(base, " \t\n\r") {
		return "", fmt.Errorf("invalid base branch %q", base)
	}
	if _, err := g.run("check-ref-format", "--branch", base); err != nil {
		return "", fmt.Errorf("invalid base branch %q: %w", base, err)
	}
	if _, err := g.run("rev-parse", "--verify", "-q", "refs/remotes/origin/"+base); err == nil {
		return "origin/" + base, nil
	}
	if _, err := g.run("rev-parse", "--verify", "-q", base); err != nil {
		return "", err
	}
	return base, nil
}
func (g NativeGit) BaseSHA(base string) (string, error) { return g.one("merge-base", base, "HEAD") }
func (g NativeGit) Clean() (bool, error) {
	answer, err := g.run("status", "--porcelain", "--untracked-files=all", "--ignore-submodules=all")
	return len(answer) == 0, err
}
func (g NativeGit) Staged() (bool, error) {
	answer, err := g.run("diff", "--no-ext-diff", "--cached", "--name-only", "-z")
	return len(answer) > 0, err
}
func (g NativeGit) Ahead(base string) (bool, error) {
	answer, err := g.one("rev-list", "--count", base+"..HEAD")
	return answer != "0", err
}
func (g NativeGit) ChangedPaths(base string) ([]string, error) {
	answer, err := g.run("diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	return gitread.SplitNUL(string(answer)), nil
}
func (g NativeGit) CommitMessages(base string) ([]CommitMessage, error) {
	answer, err := g.one("rev-list", "--no-merges", base+"..HEAD")
	if err != nil {
		return nil, err
	}
	var result []CommitMessage
	for _, sha := range strings.Fields(answer) {
		message, err := g.one("log", "-1", "--format=%B", sha)
		if err != nil {
			return nil, err
		}
		paths, err := g.run("diff-tree", "--no-ext-diff", "--root", "--no-commit-id", "--name-only", "-r", "-z", sha)
		if err != nil {
			return nil, err
		}
		result = append(result, CommitMessage{Text: message, Empty: len(paths) == 0})
	}
	return result, nil
}
func (g NativeGit) Seed(ctx context.Context, title string) error {
	return g.runWrite(ctx, "commit", "--allow-empty", "-m", title)
}

// PushRefusedError reports a push origin refused before it wrote anything:
// origin's branch is not a fast-forward of HEAD, and no lease on the head
// this checkout last pushed allows replacing it.
type PushRefusedError struct {
	Branch string
	// Pushed is the head this checkout last pushed; empty when it has none.
	Pushed string
	Err    error
}

func (e *PushRefusedError) Error() string {
	if e.Pushed == "" {
		return fmt.Sprintf("origin refused %s: HEAD does not contain origin's head, and the reflog of refs/remotes/origin/%s records no push from this checkout to lease on, so the command only fast-forwards; fetch, make HEAD contain origin/%s (for example git rebase origin/%s), then run again: %v", e.Branch, e.Branch, e.Branch, e.Branch, e.Err)
	}
	return fmt.Sprintf("origin refused %s: HEAD does not contain origin's head, and origin no longer holds %s, the head this checkout last pushed: it holds commits this checkout did not push; fetch, make HEAD contain origin/%s (for example git rebase origin/%s), then run again: %v", e.Branch, e.Pushed, e.Branch, e.Branch, e.Err)
}

func (e *PushRefusedError) Unwrap() error { return e.Err }

// Push publishes the branch to origin. A push that fast-forwards origin's
// branch goes through as is. When origin refuses it as not a fast-forward,
// as after a rebase, the push is repeated with a lease pinned to the head
// this checkout last pushed: it replaces origin's branch only while origin
// still holds that head. Someone else's push, or a deletion, moves origin
// off that head, even when a fetch moved the remote-tracking ref along, and
// the push is refused with a PushRefusedError.
func (g NativeGit) Push(ctx context.Context, branch string) error {
	err := g.runWrite(ctx, "push", "-u", "origin", branch)
	if err == nil || !strings.Contains(err.Error(), "[rejected]") {
		return err
	}
	pushed, readErr := g.lastPushed(branch)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if pushed == "" {
		return &PushRefusedError{Branch: branch, Err: err}
	}
	err = g.runWrite(ctx, "push", "--force-with-lease=refs/heads/"+branch+":"+pushed, "-u", "origin", branch)
	if err != nil && strings.Contains(err.Error(), "[rejected]") {
		return &PushRefusedError{Branch: branch, Pushed: pushed, Err: err}
	}
	return err
}

// lastPushed is the head this checkout last pushed to origin's branch, read
// from the reflog of its remote-tracking ref: git records each push there as
// "update by push", and a fetch under another subject. It is empty when this
// checkout never pushed the branch, or when its reflog no longer holds a push.
func (g NativeGit) lastPushed(branch string) (string, error) {
	tracking := "refs/remotes/origin/" + branch
	if _, err := g.run("rev-parse", "--verify", "-q", tracking); err != nil {
		return "", nil
	}
	entries, err := g.one("log", "-g", "--format=%H%x09%gs", tracking, "--")
	if err != nil {
		return "", err
	}
	for _, entry := range strings.Split(entries, "\n") {
		sha, subject, found := strings.Cut(entry, "\t")
		if found && subject == "update by push" {
			return sha, nil
		}
	}
	return "", nil
}
func (g NativeGit) Fingerprint() (string, error) {
	result, err := treefingerprint.Compute(g.Root)
	return result.Fingerprint, err
}
