package gitread

import (
	"fmt"
	"strings"
)

// Commit is one commit message read from the history: its full ID, its subject
// as git renders it, and its body.
type Commit struct {
	SHA     string
	Subject string
	Body    string
}

// commitRecordSeparator ends each record CommitsSince reads. A body holds
// newlines, so records are separated by a byte no message carries and fields by
// NUL for the same reason. Copied from tooling/cli/internal/git/tags.go.
const commitRecordSeparator = "\x1e"

// IsShallow reports whether the repository is a shallow clone. A shallow clone
// does not carry the whole history, so a range read from it can silently miss
// the commits beyond its depth.
func IsShallow(repoRoot string) (bool, error) {
	output, err := run(repoRoot, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, fmt.Errorf("read repository depth: %w", err)
	}
	return strings.TrimSpace(output) == "true", nil
}

// CommitsSince returns every commit HEAD reaches that base does not, newest
// first, merges included. base is resolved to one commit first, so a revision
// that names nothing is an error rather than an empty range.
func CommitsSince(repoRoot, base string) ([]Commit, error) {
	resolved, err := ResolveCommit(repoRoot, base)
	if err != nil {
		return nil, err
	}
	revision := resolved + "..HEAD"
	output, err := run(repoRoot, "log", "--format=%H%x00%s%x00%b"+commitRecordSeparator, "--no-show-signature", revision)
	if err != nil {
		return nil, fmt.Errorf("read the commits of %s: %w", revision, err)
	}
	var commits []Commit
	for _, record := range strings.Split(output, commitRecordSeparator) {
		record = strings.TrimLeft(record, "\n")
		if strings.TrimSpace(record) == "" {
			continue
		}
		fields := strings.SplitN(record, "\x00", 3)
		if len(fields) != 3 {
			continue
		}
		commits = append(commits, Commit{
			SHA:     strings.TrimSpace(fields[0]),
			Subject: strings.TrimSpace(fields[1]),
			Body:    strings.TrimRight(fields[2], "\n"),
		})
	}
	return commits, nil
}

// MergeBase returns the best common ancestor of base and HEAD: the commit a
// branch started from, or the last base commit it merged. A base that names no
// commit is read as its origin counterpart, origin/<base>, as the impacted
// selection reads it. A base that names nothing, or that shares no commit with
// HEAD, is an error.
func MergeBase(repoRoot, base string) (string, error) {
	resolved, err := ResolveCommit(repoRoot, base)
	if err != nil && !strings.HasPrefix(base, "origin/") {
		if remote, remoteErr := ResolveCommit(repoRoot, "origin/"+base); remoteErr == nil {
			resolved, err = remote, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w; fetch the base branch, or pass origin/<branch>", err)
	}
	output, stderr, err := runCapture(repoRoot, "merge-base", resolved, "HEAD")
	if err != nil {
		if strings.TrimSpace(stderr) == "" {
			return "", fmt.Errorf("find the merge base of %s and HEAD: they share no commit; fetch the whole history of both", base)
		}
		return "", fmt.Errorf("find the merge base of %s and HEAD: %w: %s", base, err, strings.TrimSpace(stderr))
	}
	sha := strings.ToLower(strings.TrimSpace(output))
	if !validObjectID(sha) {
		return "", fmt.Errorf("find the merge base of %s and HEAD: git returned invalid object ID", base)
	}
	return sha, nil
}

// RepositoryLocation returns the top directory of the repository that holds
// dir, and the path of dir relative to it, with a trailing slash, or "" when
// dir is the top.
func RepositoryLocation(dir string) (top, prefix string, err error) {
	output, err := run(dir, "rev-parse", "--show-toplevel", "--show-prefix")
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
