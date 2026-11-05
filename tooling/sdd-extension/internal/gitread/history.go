package gitread

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	sdkgit "go.putnami.dev/sdk/extension/gitread"
)

// MergeBase returns the best common ancestor of base and HEAD: the commit a
// branch started from, or the last base commit it merged. A base that names no
// commit is read as its origin counterpart, origin/<base>, as the impacted
// selection reads it. A base that names nothing, or that shares no commit with
// HEAD, is an error.
func MergeBase(repoRoot, base string) (string, error) {
	resolved, err := sdkgit.ResolveCommit(repoRoot, base)
	if err != nil && !strings.HasPrefix(base, "origin/") {
		if remote, remoteErr := sdkgit.ResolveCommit(repoRoot, "origin/"+base); remoteErr == nil {
			resolved, err = remote, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w; fetch the base branch, or pass origin/<branch>", err)
	}
	output, stderr, err := sdkgit.RunCapture(repoRoot, "merge-base", resolved, "HEAD")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(stderr) == "" {
			return "", fmt.Errorf("find the merge base of %s and HEAD: they share no commit; fetch the whole history of both", base)
		}
		return "", fmt.Errorf("find the merge base of %s and HEAD: %w: %s", base, err, strings.TrimSpace(stderr))
	}
	sha := strings.ToLower(strings.TrimSpace(output))
	if !sdkgit.ValidObjectID(sha) {
		return "", fmt.Errorf("find the merge base of %s and HEAD: git returned invalid object ID", base)
	}
	return sha, nil
}

// RepositoryLocation returns the top directory of the repository that holds
// dir, and the path of dir relative to it, with a trailing slash, or "" when
// dir is the top.
func RepositoryLocation(dir string) (top, prefix string, err error) {
	output, err := sdkgit.Run(dir, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		return "", "", fmt.Errorf("locate the directory in its repository: %w", err)
	}
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) == 1 {
		lines = append(lines, "")
	}
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" {
		return "", "", fmt.Errorf("locate the directory in its repository: unexpected git output %q", output)
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), nil
}
