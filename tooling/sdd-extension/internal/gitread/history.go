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
