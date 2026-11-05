package git

import (
	"fmt"
	"strings"

	"go.putnami.dev/sdk/extension/gitread"
)

// TagsAtHead returns every tag that points at HEAD, in git's order. A commit
// carrying its line's tag publishes that line's cohort, so this is the question
// "is this commit a release of some line?" asked before any line is named.
func TagsAtHead(repoRoot string) ([]string, error) {
	output, err := gitread.Run(repoRoot, "tag", "--points-at", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read the tags at HEAD: %w", err)
	}
	return splitLines(output), nil
}

// CommitFiles returns the repository-relative paths one commit changes, limited
// to pathspecs. A rename lists both of its paths, so a file moved out of a
// project still counts as a change to that project. A root commit lists every
// file it adds; a merge commit lists none, as it changes nothing of its own.
// Paths are read NUL-separated, so no name is quoted or split.
func CommitFiles(repoRoot, sha string, pathspecs []string) ([]string, error) {
	args := []string{"diff-tree", "-r", "--root", "--no-commit-id", "--name-only", "--no-renames", "-z", "--end-of-options", sha}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	output, err := gitread.Run(repoRoot, args...)
	if err != nil {
		return nil, fmt.Errorf("read the files of %s: %w", sha, err)
	}
	var files []string
	for _, file := range strings.Split(output, "\x00") {
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
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
	branch, err := gitread.CurrentBranch(repoRoot)
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
