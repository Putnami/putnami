package gitread

import (
	"fmt"
	"strings"
)

// HeadSHA returns the full SHA for HEAD.
//
// Copied from tooling/cli/internal/git/git.go. The feature engine stamps it on
// every worktree revision, so a report can be joined back to the exact tree it
// described.
func HeadSHA(repoRoot string) (string, error) {
	output, err := run(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// ResolveBaseline chooses the ref the architecture ratchet compares against
// when the caller passed none. An explicit ref is returned unchanged so a typo
// or an intentionally unusual ref surfaces from the later read rather than from
// a silent substitution.
//
// Order: explicit → trunk (origin/HEAD, origin/main, local main/master) →
// upstream tracking ref (skipped when it is this branch's own remote
// counterpart) → error.
//
// Copied from `ResolveBaselineDetailed` in tooling/cli/internal/git/git.go,
// minus the epic-branch tier and minus the ResolvedBaseline tier label. The
// architecture engine passes a nil epic-branch list and discards the tier, so
// both are dead weight here. The error message is kept verbatim: it is what an
// operator reads.
func ResolveBaseline(repoRoot, explicit string) (string, error) {
	if ref := strings.TrimSpace(explicit); ref != "" {
		return ref, nil
	}
	if trunk := trunkRef(repoRoot); trunk != "" {
		return trunk, nil
	}
	currentBranch := currentBranchName(repoRoot)
	if ref := upstreamRef(repoRoot); ref != "" && !upstreamIsSelfCounterpart(repoRoot, ref, currentBranch) {
		return ref, nil
	}
	return "", fmt.Errorf("resolve git baseline: no explicit baseline, origin/HEAD, origin/main, local main/master, or usable upstream tracking ref was found (a branch is never measured against itself) — pass --baseline or set the workspace config baseline")
}

// trunkRef finds the repository trunk: origin/HEAD, origin/main, then a local
// main/master.
func trunkRef(repoRoot string) string {
	if ref := originHeadRef(repoRoot); ref != "" {
		return ref
	}
	if refExists(repoRoot, "origin/main") {
		return "origin/main"
	}
	for _, ref := range []string{"main", "master"} {
		if refExists(repoRoot, ref) {
			return ref
		}
	}
	return ""
}

// upstreamIsSelfCounterpart reports whether the upstream tracking ref is this
// branch's own published counterpart rather than a base branch it forked from.
// Any of three signals disqualifies it as a baseline: it names the branch, it
// is the branch's push destination, or it already contains HEAD — the last one
// catching counterparts no git config can see.
func upstreamIsSelfCounterpart(repoRoot, ref, currentBranch string) bool {
	if refNamesBranch(ref, currentBranch) {
		return true
	}
	if push := symbolicAbbrevRef(repoRoot, "@{push}"); push != "" && push == ref {
		return true
	}
	return isAncestorOf(repoRoot, "HEAD", ref)
}

// refNamesBranch reports whether a remote-tracking ref (e.g. "origin/feat")
// points at the given local branch name. Branch names may contain slashes, so
// only the remote segment is stripped before comparing.
func refNamesBranch(ref, branch string) bool {
	if branch == "" {
		return false
	}
	parts := strings.SplitN(ref, "/", 2)
	return len(parts) == 2 && parts[1] == branch
}

// currentBranchName treats detached HEAD as no branch at all, because the tier
// that excludes "the branch itself" has nothing to exclude there.
func currentBranchName(repoRoot string) string {
	branch, err := currentBranch(repoRoot)
	if err != nil || branch == "HEAD" {
		return ""
	}
	return branch
}

func currentBranch(repoRoot string) (string, error) {
	if output, err := run(repoRoot, "symbolic-ref", "--short", "HEAD"); err == nil {
		return strings.TrimSpace(output), nil
	}
	output, err := run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func upstreamRef(repoRoot string) string {
	return symbolicAbbrevRef(repoRoot, "@{u}")
}

func originHeadRef(repoRoot string) string {
	if ref := symbolicAbbrevRef(repoRoot, "origin/HEAD"); ref != "" && ref != "origin/HEAD" {
		return ref
	}
	output, err := run(repoRoot, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	ref := strings.TrimPrefix(strings.TrimSpace(output), "refs/remotes/")
	if ref == "" || ref == "origin/HEAD" {
		return ""
	}
	return ref
}

func symbolicAbbrevRef(repoRoot, ref string) string {
	output, err := run(repoRoot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

func refExists(repoRoot, ref string) bool {
	_, err := run(repoRoot, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// isAncestorOf reports whether ancestor is an ancestor of descendant.
func isAncestorOf(repoRoot, ancestor, descendant string) bool {
	_, err := run(repoRoot, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}
