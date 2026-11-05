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

// commitRecordSeparator is ASCII RS. A commit body is arbitrary text, newlines
// included, so records are separated by a byte no message carries and fields by
// NUL for the same reason.
const commitRecordSeparator = "\x1e"

// IsShallow reports whether the repository is a shallow clone. A shallow clone
// carries neither the whole commit history nor, usually, the tags, so a range
// read from it can silently miss the commits beyond its depth.
//
// Where Git does not manage repoRoot there is no history at all, and the error
// wraps the one-line UnmanagedError naming what to run.
func IsShallow(repoRoot string) (bool, error) {
	output, err := Run(repoRoot, "rev-parse", "--is-shallow-repository")
	if err != nil {
		if unmanaged := Unmanaged(repoRoot); unmanaged != nil {
			return false, fmt.Errorf("version and publish need git history: %w", unmanaged)
		}
		return false, fmt.Errorf("read repository depth: %w", err)
	}
	return strings.TrimSpace(output) == "true", nil
}

// CommitsSince returns the commits between fromCommit (exclusive) and HEAD that
// touch one of pathspecs, newest first, merges included. fromCommit is resolved
// to one commit first, so a revision that names nothing is an error rather
// than an empty range. An empty fromCommit walks the whole history; empty
// pathspecs walk the whole tree.
func CommitsSince(repoRoot, fromCommit string, pathspecs []string) ([]Commit, error) {
	revision := "HEAD"
	if trimmed := strings.TrimSpace(fromCommit); trimmed != "" {
		resolved, err := ResolveCommit(repoRoot, trimmed)
		if err != nil {
			return nil, err
		}
		revision = resolved + "..HEAD"
	}
	args := []string{"log", "--format=%H%x00%s%x00%b" + commitRecordSeparator, "--no-show-signature", revision}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	output, err := Run(repoRoot, args...)
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
