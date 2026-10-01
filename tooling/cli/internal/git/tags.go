package git

import (
	"fmt"
	"os"
	"strings"
)

// IsShallow reports whether the repository is a shallow clone. A shallow clone
// carries neither the whole commit history nor, usually, the tags, so the
// version of a line cannot be derived from it: the answer would be a plausible
// number computed from the fraction of history that happened to be fetched.
func IsShallow(repoRoot string) (bool, error) {
	output, err := run(repoRoot, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, fmt.Errorf("read repository depth: %w", err)
	}
	return strings.TrimSpace(output) == "true", nil
}

// TagsAtHead returns every tag that points at HEAD, in git's order. A commit
// carrying its line's tag publishes that line's cohort, so this is the question
// "is this commit a release of some line?" asked before any line is named.
func TagsAtHead(repoRoot string) ([]string, error) {
	output, err := run(repoRoot, "tag", "--points-at", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read the tags at HEAD: %w", err)
	}
	return splitLines(output), nil
}

// LastReachableTag returns the nearest tag reachable from HEAD that matches one
// complete line tag pattern, and the commit it points at. Matching only the
// literal prefix is insufficient for patterns with a suffix: a nearer tag that
// the line could never render would otherwise become its version baseline.
// ok is false when the line has no reachable tag, which is an ordinary state —
// a line yields 0.0.0 until it is tagged for the first time — and never an
// error. A git failure is an error, even when it exits as the untagged line
// does.
func LastReachableTag(repoRoot, tagPattern string) (tag, commit string, ok bool, err error) {
	if strings.Count(tagPattern, lineTagPlaceholder) != 1 || strings.ContainsAny(tagPattern, " \t\n*?[") || strings.HasPrefix(tagPattern, "-") {
		return "", "", false, fmt.Errorf("read the last tag: %q is not a usable line tag pattern", tagPattern)
	}
	match := strings.Replace(tagPattern, lineTagPlaceholder, "*", 1)
	output, stderr, runErr := runCapture(repoRoot, "describe", "--tags", "--abbrev=0", "--match", match)
	if runErr != nil {
		describeErr := fmt.Errorf("git describe (in %s): %w: %s", repoRoot, runErr, strings.TrimSpace(stderr))
		return "", "", false, describeUntagged(repoRoot, match, describeErr)
	}
	tag = strings.TrimSpace(output)
	if version, matches := versionFromLineTag(tag, tagPattern); tag == "" || !matches || version == "" {
		return "", "", false, nil
	}
	commit, err = ResolveCommit(repoRoot, "refs/tags/"+tag)
	if err != nil {
		return "", "", false, err
	}
	return tag, commit, true, nil
}

// describeUntagged decides whether a failed `git describe` is the untagged
// line, and returns nil only then. Describe exits non-zero when nothing
// matches, but also when it cannot run or read the history, and its last
// message does not tell the two apart: on a history with a missing object it
// prints the untagged line's own "No tags can describe". Read as untagged, that
// failure would silently version the line at 0.0.0.
//
// So the tags HEAD reaches decide. The line is untagged only when `git tag
// --merged` answers cleanly with none: it exits 0 on that same history and
// names the unreadable object on stderr only, so any stderr is a failure. The
// GIT_TRACE* debug settings write to stderr too, so this call runs without
// them: set to debug git, they must not turn every untagged line into an error.
func describeUntagged(repoRoot, match string, describeErr error) error {
	reachable, stderr, err := runCaptureEnv(repoRoot, withoutGitTrace(os.Environ()),
		"tag", "--list", "--merged", "HEAD", "--", match)
	switch tags := splitLines(reachable); {
	case err != nil:
		return fmt.Errorf("read the last tag: %w; git tag --merged: %w: %s", describeErr, err, strings.TrimSpace(stderr))
	case strings.TrimSpace(stderr) != "":
		return fmt.Errorf("read the last tag: %w; git tag --merged: %s", describeErr, strings.TrimSpace(stderr))
	case len(tags) > 0:
		return fmt.Errorf("read the last tag: %w; HEAD reaches %s", describeErr, strings.Join(tags, ", "))
	}
	return nil
}

// withoutGitTrace is env without git's GIT_TRACE* settings.
func withoutGitTrace(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, "GIT_TRACE") {
			kept = append(kept, entry)
		}
	}
	return kept
}

// Commit is one commit of a line's history: what conventional-commit parsing
// reads, plus the SHA a changelog bullet cites.
type Commit struct {
	SHA     string
	Subject string
	Body    string
}

// commitRecordSeparator is ASCII RS. A commit body is arbitrary text, newlines
// included, so records are separated by a byte no message carries and fields by
// NUL for the same reason.
const commitRecordSeparator = "\x1e"

// CommitsSince returns the commits between fromCommit (exclusive) and HEAD that
// touch one of pathspecs, newest first. An empty fromCommit walks the whole
// history, which is the untagged line's case; empty pathspecs walk the whole
// tree, which is a line that owns the repository root.
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
	output, err := run(repoRoot, args...)
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

// TreeState reads the git state of the working tree alone: the commit, the
// branch, the ordered suffix, and whether anything is uncommitted.
//
// It is the half of a version that a run must capture BEFORE its own hooks and
// codegen touch the tree. The other half — the line's base version —
// depends only on tags and history, so it is computed later, by GetVersionInfo.
//
// The commit is HEAD's unless PUTNAMI_SOURCE_REVISION (SourceRevisionEnv)
// names the commit the run is bound to. Then SHA and the suffix's sha half are
// that commit's first 12 characters (sourceRevisionShortLength), whether or not
// the repository has the commit, so one bound commit has one version on every
// runner. The time half is that commit's committer time when the repository
// has it, else PUTNAMI_SOURCE_COMMIT_TIME (SourceCommitTimeEnv), which is
// required there. The override names WHICH COMMIT the publication is bound
// to; the tree that is built and hashed is always HEAD's, so Branch, IsDirty
// and the dirty hash stay the checkout's.
//
// Unset, the output is byte-identical to reading HEAD, with git's
// abbreviation. Binding to HEAD itself keeps HEAD's time but carries the fixed
// 12-character SHA, so it can differ from the unset output in length.
func TreeState(repoRoot string) (*VersionInfo, error) {
	branch, err := CurrentBranch(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("read the current branch: %w", err)
	}
	sha, committedAt, err := boundCommit(repoRoot)
	if err != nil {
		return nil, err
	}
	dirty := dirtyHash(repoRoot)
	return &VersionInfo{
		SHA:     sha,
		Branch:  branch,
		Suffix:  OrderedSuffix(committedAt, sha, dirty),
		IsDirty: dirty != "",
	}, nil
}
