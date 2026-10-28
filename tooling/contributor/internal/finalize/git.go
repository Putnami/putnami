package finalize

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

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
	Seed(string) error
	Push(string) error
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

func (g NativeGit) runWrite(args ...string) error {
	if len(args) == 0 || args[0] != "commit" && args[0] != "push" {
		return fmt.Errorf("unsupported repository-writing git operation")
	}
	// Commit hooks and push transport helpers may execute repository code.
	// The executable holds no run credential, so it has nothing to mark.
	cmd := exec.Command("git", append([]string{"-C", g.Root}, args...)...)
	cmd.Env = gitEnvironment(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
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
	return splitNUL(answer), nil
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
func (g NativeGit) Seed(title string) error {
	return g.runWrite("commit", "--allow-empty", "-m", title)
}
func (g NativeGit) Push(branch string) error {
	return g.runWrite("push", "-u", "origin", branch)
}
func (g NativeGit) Fingerprint() (string, error) {
	result, err := treefingerprint.Compute(g.Root)
	return result.Fingerprint, err
}

func splitNUL(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}
