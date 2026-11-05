package gitread

import "strings"

// CurrentBranch returns the current git branch name, and "HEAD" on a detached
// HEAD. A branch with no commit yet is named too.
func CurrentBranch(repoRoot string) (string, error) {
	if output, err := Run(repoRoot, "symbolic-ref", "--short", "HEAD"); err == nil {
		return strings.TrimSpace(output), nil
	}
	output, err := Run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// CurrentBranchName is CurrentBranch minus the error: a resolution tier that
// excludes "the branch itself" reads a detached HEAD, or a directory git cannot
// read, as no branch at all, "".
func CurrentBranchName(repoRoot string) string {
	branch, err := CurrentBranch(repoRoot)
	if err != nil || branch == "HEAD" {
		return ""
	}
	return branch
}

// TrunkRef finds the repository trunk: origin/HEAD, origin/main, then a local
// main or master. It returns "" when none of them names a commit.
func TrunkRef(repoRoot string) string {
	if ref := OriginHeadRef(repoRoot); ref != "" {
		return ref
	}
	if RefExists(repoRoot, "origin/main") {
		return "origin/main"
	}
	for _, ref := range []string{"main", "master"} {
		if RefExists(repoRoot, ref) {
			return ref
		}
	}
	return ""
}

// OriginHeadRef returns the remote-tracking branch origin/HEAD points at, such
// as "origin/main", or "" when origin/HEAD is unset.
func OriginHeadRef(repoRoot string) string {
	if ref := SymbolicAbbrevRef(repoRoot, "origin/HEAD"); ref != "" && ref != "origin/HEAD" {
		return ref
	}
	output, err := Run(repoRoot, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	ref := strings.TrimPrefix(strings.TrimSpace(output), "refs/remotes/")
	if ref == "" || ref == "origin/HEAD" {
		return ""
	}
	return ref
}

// UpstreamRef returns the current branch's upstream tracking ref, such as
// "origin/feature", or "" when it has none.
func UpstreamRef(repoRoot string) string {
	return SymbolicAbbrevRef(repoRoot, "@{u}")
}

// SymbolicAbbrevRef returns the short name of the ref that ref resolves to, or
// "" when git cannot resolve it.
func SymbolicAbbrevRef(repoRoot, ref string) string {
	output, err := Run(repoRoot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// RefExists reports whether ref names a commit.
func RefExists(repoRoot, ref string) bool {
	_, err := Run(repoRoot, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// RefNamesBranch reports whether a remote-tracking ref (e.g. "origin/feat")
// points at the given local branch name. Branch names may contain slashes, so
// only the remote segment is stripped before comparing. No ref names the
// branch "".
func RefNamesBranch(ref, branch string) bool {
	if branch == "" {
		return false
	}
	parts := strings.SplitN(ref, "/", 2)
	return len(parts) == 2 && parts[1] == branch
}

// IsAncestorOf reports whether ancestor is an ancestor of descendant.
func IsAncestorOf(repoRoot, ancestor, descendant string) bool {
	_, err := Run(repoRoot, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// UpstreamIsSelfCounterpart reports whether the upstream tracking ref is this
// branch's own published counterpart rather than a base branch it forked from.
// Any of three signals disqualifies it as a baseline:
//
//   - it names the branch (origin/<branch>);
//   - it is the branch's push destination (@{push}), which catches a local
//     branch renamed away from its remote name (alice/topic pushing to
//     origin/topic);
//   - it already contains HEAD, so the committed diff would be empty, which
//     catches counterparts no git config can see (explicit-refspec pushes).
//
// The ancestry signal also fires on a freshly forked branch with no work yet,
// where the caller's "set a baseline" error beats an empty answer that cannot
// be told apart from self-measurement.
func UpstreamIsSelfCounterpart(repoRoot, ref, currentBranch string) bool {
	if RefNamesBranch(ref, currentBranch) {
		return true
	}
	if push := SymbolicAbbrevRef(repoRoot, "@{push}"); push != "" && push == ref {
		return true
	}
	return IsAncestorOf(repoRoot, "HEAD", ref)
}
