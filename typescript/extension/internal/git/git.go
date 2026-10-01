// Package git provides git version metadata for the TypeScript extension.
package git

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/errs"
)

const (
	// SourceRevisionEnv names the commit a publication is bound to when it is
	// not the checkout's HEAD: a full lowercase hex commit id, 40 or 64
	// characters. Empty or blank means HEAD. It is the variable
	// tooling/cli/internal/git reads under the same name.
	SourceRevisionEnv = "PUTNAMI_SOURCE_REVISION"
	// SourceCommitTimeEnv is the committer time of SourceRevisionEnv's commit
	// in unix seconds. It is read only when the repository does not have that
	// commit, and it is required there.
	SourceCommitTimeEnv = "PUTNAMI_SOURCE_COMMIT_TIME"

	// sourceRevisionShortLength is how many leading characters of
	// SourceRevisionEnv the suffix carries: a fixed length, so one bound
	// commit has one version on every runner, whether or not the repository
	// has the commit.
	sourceRevisionShortLength = 12

	commitTimeLayout = "20060102150405"
)

// VersionInfo holds git version metadata.
//
// It carries no tag. Which tag a commit is a release of, and which channels a
// publication advances, are the orchestrator's answers: this file exists only
// so a direct extension invocation — one the CLI did not stamp — can still
// derive the ordered suffix for a fallback version.
type VersionInfo struct {
	SHA     string
	Branch  string
	IsDirty bool
	Suffix  string
}

// GetVersionInfo reads git state from the workspace root and builds the
// ordered suffix <commitTime>-<sha>[-<dirtyHash>] exactly as
// tooling/cli/internal/git TreeState does.
//
// The commit is HEAD's, with git's abbreviation, unless SourceRevisionEnv
// names the commit the publication is bound to. Then SHA is that commit's
// first 12 characters, and the time is its committer time when the repository
// has the commit, else SourceCommitTimeEnv. Branch, IsDirty and the dirty hash
// always describe the checkout.
func GetVersionInfo(workspaceRoot string) (*VersionInfo, error) {
	info := &VersionInfo{}

	sha, committedAt, err := boundCommit(workspaceRoot)
	if err != nil {
		return nil, err
	}
	info.SHA = sha

	branch, err := gitCmd(workspaceRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolving branch: %w", err)
	}
	info.Branch = branch

	// A failing `git status` is an error, never a clean tree: a clean
	// pre-release must not be stamped for uncommitted code.
	status, err := gitCmd(workspaceRoot, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("checking working tree status: %w", err)
	}
	info.IsDirty = status != ""

	info.Suffix = committedAt.UTC().Format(commitTimeLayout) + "-" + info.SHA
	if info.IsDirty {
		// The hash covers the untrimmed diff output, and a dirty tree always
		// carries one, even when the diff is empty (untracked files only).
		diff, err := gitRun(workspaceRoot, "diff", "HEAD", "--no-color")
		if err != nil {
			return nil, fmt.Errorf("computing dirty diff: %w", err)
		}
		info.Suffix += "-" + sha256Hash(diff)
	}

	return info, nil
}

// boundCommit answers the short SHA and committer time the suffix is built
// from: HEAD's, unless SourceRevisionEnv names the bound commit.
func boundCommit(workspaceRoot string) (string, time.Time, error) {
	revision := strings.TrimSpace(os.Getenv(SourceRevisionEnv))
	if revision == "" {
		sha, err := gitCmd(workspaceRoot, "rev-parse", "--short", "HEAD")
		if err != nil {
			return "", time.Time{}, err
		}
		committedAt, err := commitTime(workspaceRoot, "HEAD")
		if err != nil {
			return "", time.Time{}, fmt.Errorf("reading commit time: %w", err)
		}
		return sha, committedAt, nil
	}
	if !validSourceRevision(revision) {
		return "", time.Time{}, fmt.Errorf("read the source revision: %s=%q is not a full lowercase hex commit id (40 or 64 characters)", SourceRevisionEnv, revision)
	}
	sha := revision[:sourceRevisionShortLength]
	// ^{commit} refuses a tree or blob id; a failure means the repository does
	// not have the commit.
	if committedAt, err := commitTime(workspaceRoot, revision+"^{commit}"); err == nil {
		return sha, committedAt, nil
	}
	raw := strings.TrimSpace(os.Getenv(SourceCommitTimeEnv))
	if raw == "" {
		return "", time.Time{}, fmt.Errorf("read the source revision: %s is required when %s names a commit the repository does not have", SourceCommitTimeEnv, SourceRevisionEnv)
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return "", time.Time{}, fmt.Errorf("read the source revision: %s=%q is not a unix time in seconds", SourceCommitTimeEnv, raw)
	}
	return sha, time.Unix(seconds, 0).UTC(), nil
}

// validSourceRevision accepts exactly a full-length lowercase hex object id:
// 40 characters for SHA-1, 64 for SHA-256.
func validSourceRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	for _, r := range revision {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func commitTime(workspaceRoot, revision string) (time.Time, error) {
	output, err := gitCmd(workspaceRoot, "show", "-s", "--format=%ct", "--end-of-options", revision)
	if err != nil {
		return time.Time{}, err
	}
	seconds, err := strconv.ParseInt(output, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, fmt.Errorf("%q is not a unix timestamp", output)
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)[:7]
}

// gitCmd runs git and returns its trimmed stdout.
func gitCmd(dir string, args ...string) (string, error) {
	stdout, err := gitRun(dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// gitRun runs git and returns its stdout unchanged. A non-zero exit is an
// error carrying git's stderr, never an empty success. Each call is bounded
// by a 30-second timeout.
func gitRun(dir string, args ...string) (string, error) {
	result, err := exec.Run("git", args, exec.Dir(dir), exec.Timeout(30*time.Second))
	if err != nil {
		return "", err
	}
	if !result.Success {
		stderr := strings.TrimSpace(result.Stderr)
		if stderr == "" {
			stderr = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return "", errs.Newf(errs.CodeGitFailed, "git %s: %s", strings.Join(args, " "), stderr).WithCategory(errs.CategoryInfra)
	}
	return result.Stdout, nil
}
