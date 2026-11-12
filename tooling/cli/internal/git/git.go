// Package git provides helpers for shelling out to git.
package git

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/linetag"
)

// LineSpec names one version line to git: where it lives, how its tags are
// spelled, and which paths its commits touch.
type LineSpec struct {
	// ScopePath is the line's scope path, slash-separated and relative to the
	// repository root. "" is the root line.
	ScopePath string
	// TagPattern is the line's git tag pattern, carrying one {version}
	// placeholder (workspace.LineTagPattern renders it).
	TagPattern string
	// Pathspecs bound which commits advance the line: only those that touch one
	// of these paths are read. Empty means the whole tree.
	Pathspecs []string
	// Stable caps at a patch every commit that touches no stable project, by
	// the files it changes inside Pathspecs. Nil reads every commit as stable.
	Stable StableTest
}

// VersionInfo is one line's version at HEAD, derived from git alone.
type VersionInfo struct {
	Base    string // The line's version: the tag's on a tagged commit, else the computed next one
	Full    string // Base, plus "-" and Suffix when the commit is not tagged
	SHA     string // Short commit SHA
	Branch  string // Current branch name
	Suffix  string // Ordered suffix: <commitTime>-<sha>, plus -<dirtyHash> for a dirty tree
	IsDirty bool   // Whether the working tree has uncommitted changes
	Tagged  bool   // Whether HEAD carries this line's tag
	Line    string // The line's scope path ("" is the root line)
	Tag     string // The line tag at HEAD when Tagged, else ""
}

// CommitTimeLayout is the UTC layout of the version suffix's leading
// identifier: fixed width, so string order is time order.
const CommitTimeLayout = "20060102150405"

// OrderedSuffix renders the pre-release suffix of a build: the commit's
// committer time in UTC, then the short SHA, then the dirty hash when the
// tree has uncommitted changes.
//
// The suffix is one semver identifier (digits, letters, hyphens), so npm and
// Go compare it as a string: with the fixed-width timestamp first, a newer
// commit always sorts after an older one, and a native `@latest` follows the
// newest publication instead of a lexical accident of the SHA. The SHA stays
// the last hyphen-separated segment of a clean build, which is the marker
// Distribution uses to recognize a commit-stamped artifact.
func OrderedSuffix(committedAt time.Time, sha, dirtyHash string) string {
	suffix := committedAt.UTC().Format(CommitTimeLayout) + "-" + sha
	if dirtyHash != "" {
		suffix += "-" + dirtyHash
	}
	return suffix
}

// GetVersionInfo computes one line's version at HEAD from git alone (D9).
//
// The order is the decision order:
//
//  1. A shallow clone is refused. Its history and tags are a fraction of the
//     repository, so every answer below would be a plausible number computed
//     from an arbitrary cut of the past.
//  2. HEAD carrying a release tag of this line (linetag.Version) IS the
//     version: Base and Full are the tag's, with no suffix, because that
//     commit is the release. Two of the line's release tags on one commit is
//     ambiguous and refused. A tag of the line that is not a release, such as
//     a pre-release, does not tag HEAD.
//  3. Otherwise the version is the last release of the line
//     (linetag.LastRelease) advanced by the conventional commits that touch
//     it since that release, floored at a patch. A line with no release
//     starts at 0.0.0 with no advance: there is nothing to advance from.
//  4. An untagged commit carries the ordered suffix, which is what makes two
//     builds of one base version comparable and unique.
func GetVersionInfo(repoRoot string, line LineSpec) (*VersionInfo, error) {
	head, err := ReadHead(repoRoot, nil)
	if err != nil {
		return nil, err
	}
	return head.LineVersion(line)
}

// Head is what every version line reads from the repository in the same way:
// the tree state and the tags at HEAD, from a clone known to be complete.
type Head struct {
	repoRoot string
	tree     VersionInfo
	atHead   []string
}

// ReadHead reads the Head of repoRoot. It refuses a shallow clone (step 1 of
// GetVersionInfo). tree is the tree state the caller already captured, before
// anything in the run changed the tree; ReadHead then reads none. A nil tree is
// read here, with TreeState.
func ReadHead(repoRoot string, tree *VersionInfo) (*Head, error) {
	shallow, err := gitread.IsShallow(repoRoot)
	if err != nil {
		return nil, err
	}
	if shallow {
		return nil, fmt.Errorf("version and publish need a full clone with tags; run git fetch --unshallow --tags")
	}
	if tree == nil {
		if tree, err = TreeState(repoRoot); err != nil {
			return nil, err
		}
	}
	atHead, err := TagsAtHead(repoRoot)
	if err != nil {
		return nil, err
	}
	return &Head{repoRoot: repoRoot, tree: *tree, atHead: atHead}, nil
}

// LineVersion computes one line's version from the Head, steps 2 to 4 of
// GetVersionInfo. The tree fields of the result (SHA, Branch, Suffix, IsDirty)
// are the Head's.
func (h *Head) LineVersion(line LineSpec) (*VersionInfo, error) {
	info := h.tree
	info.Line = line.ScopePath
	info.Base, info.Full, info.Tagged, info.Tag = "", "", false, ""

	if strings.Count(line.TagPattern, wsproto.LineTagPlaceholder) != 1 {
		return nil, fmt.Errorf("line %q has tag pattern %q, which carries no %s placeholder",
			line.ScopePath, line.TagPattern, wsproto.LineTagPlaceholder)
	}

	var lineTags []string
	for _, tag := range h.atHead {
		if _, ok := linetag.Version(tag, line.TagPattern); ok {
			lineTags = append(lineTags, tag)
		}
	}
	if len(lineTags) > 1 {
		return nil, fmt.Errorf("HEAD carries %d tags of line %q (%s); name one line with --scope",
			len(lineTags), line.ScopePath, strings.Join(lineTags, ", "))
	}
	if len(lineTags) == 1 {
		version, _ := linetag.Version(lineTags[0], line.TagPattern)
		info.Tagged = true
		info.Tag = lineTags[0]
		info.Base = version
		info.Full = version
		return &info, nil
	}

	last, found, err := linetag.LastRelease(h.repoRoot, line.TagPattern)
	if err != nil {
		return nil, err
	}
	if !found {
		info.Base = "0.0.0"
		info.Full = info.Base + "-" + info.Suffix
		return &info, nil
	}
	commits, err := gitread.CommitsSince(h.repoRoot, last.Commit, line.Pathspecs)
	if err != nil {
		return nil, err
	}
	major, _, _ := parseSemverCore(last.Version)
	bump, err := BumpFor(commits, major == 0, LineStableTest(h.repoRoot, line))
	if err != nil {
		return nil, err
	}
	info.Base = NextVersion(last.Version, bump, true)
	info.Full = info.Base + "-" + info.Suffix
	return &info, nil
}

// commitTime returns the committer time of revision. The committer time, not
// the author time, is what changes when a commit is rebased or amended, so it
// is the time that orders what actually reached a branch. The revision follows
// --end-of-options so a caller-supplied value can never be read as a flag.
//
// Zero is a time: a commit dated at the Unix epoch records it. The hosted
// pull request runner dates its squash commit there so that one base and one
// tree always give one commit, and refusing it left that run with no version.
func commitTime(repoRoot, revision string) (time.Time, bool) {
	output, err := gitread.Run(repoRoot, "show", "-s", "--format=%ct", "--end-of-options", revision)
	if err != nil {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil || seconds < 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

func shortSHA(repoRoot string) string {
	output, err := gitread.Run(repoRoot, "rev-parse", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// HeadTree returns the full ID of the tree HEAD records: the content of the
// commit, which a squash-merge or a rebase keeps when it rewrites the commit.
func HeadTree(repoRoot string) (string, error) {
	output, err := gitread.Run(repoRoot, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(output)), nil
}

// WorktreeClean reports whether the checkout has no staged, unstaged, or
// untracked changes. Immutable revision-bound documents must reject a dirty
// tree: HEAD alone cannot describe the source bytes that planning observed.
// It takes no optional lock, so it never contends for the index with a task
// that writes to the same repository while it runs.
func WorktreeClean(repoRoot string) (bool, error) {
	output, err := gitread.Run(repoRoot, "--no-optional-locks", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, fmt.Errorf("inspect worktree status: %w", err)
	}
	return strings.TrimSpace(output) == "", nil
}

// DiffCommitFiles returns the tracked file paths changed directly between two
// immutable commits. Unlike DiffWorkingTree it deliberately excludes staged and
// untracked worktree changes, making its answer reproducible from base and
// head alone. Rename detection is disabled so Git configuration cannot change
// which source and destination paths are reported.
func DiffCommitFiles(repoRoot, base, head string) ([]string, error) {
	base, err := gitread.ResolveCommit(repoRoot, base)
	if err != nil {
		return nil, err
	}
	head, err = gitread.ResolveCommit(repoRoot, head)
	if err != nil {
		return nil, err
	}
	output, err := gitread.Run(repoRoot, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", base, head)
	if err != nil {
		return nil, fmt.Errorf("diff commits %s..%s: %w", base, head, err)
	}
	return dedupStrings(gitread.SplitNUL(output)), nil
}

// CommitReachableFromHead reports whether sha resolves to a commit that is an
// ancestor of HEAD. Descendant commits may exist locally after fetches, but they
// are not safe impacted baselines for the current checkout.
func CommitReachableFromHead(repoRoot, sha string) bool {
	sha = strings.TrimSpace(sha)
	if !gitread.ValidObjectID(sha) || !gitread.RefExists(repoRoot, sha) {
		return false
	}
	_, err := gitread.Run(repoRoot, "merge-base", "--is-ancestor", sha, "HEAD")
	return err == nil
}

// RevList returns the commits reachable from revision, revision first, as the
// repository's commit objects record them. Replace refs, a graft file and the
// commit-graph cache are ignored, so none of them can add or hide a parent. It
// returns at most maxCount commits: a caller that must know whether the
// history is longer asks for one more than it accepts. revision must be a full
// lowercase hex commit id. A shallow clone answers with the commits it has;
// IsShallow tells the caller that the list is incomplete.
func RevList(repoRoot, revision string, maxCount int) ([]string, error) {
	if !validSourceRevision(revision) {
		return nil, fmt.Errorf("list the ancestry: %q is not a full lowercase hex commit id", revision)
	}
	if maxCount < 1 {
		return nil, fmt.Errorf("list the ancestry: the commit limit %d is not positive", maxCount)
	}
	env := append(os.Environ(), "GIT_GRAFT_FILE="+filepath.Join(os.DevNull, "grafts"))
	stdout, stderr, err := gitread.RunCaptureEnv(repoRoot, env,
		"--no-replace-objects", "-c", "core.commitGraph=false",
		"rev-list", "--max-count="+strconv.Itoa(maxCount), revision, "--")
	if err != nil {
		return nil, fmt.Errorf("git rev-list (in %s): %w: %s", repoRoot, err, strings.TrimSpace(stderr))
	}
	commits := splitLines(stdout)
	for _, commit := range commits {
		if !validSourceRevision(commit) {
			return nil, fmt.Errorf("git rev-list (in %s): %q is not a full lowercase hex commit id", repoRoot, commit)
		}
	}
	return commits, nil
}

// RemoteURL returns the configured URL for a git remote, or an empty string
// when the remote is missing or git cannot read it.
func RemoteURL(repoRoot, remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	output, err := gitread.Run(repoRoot, "config", "--get", "remote."+remote+".url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// dirtyHash returns a short hash of uncommitted changes, or "" if clean.
func dirtyHash(repoRoot string) string {
	status, err := gitread.Run(repoRoot, "status", "--porcelain")
	if err != nil || strings.TrimSpace(status) == "" {
		return ""
	}
	diff, _ := gitread.Run(repoRoot, "diff", "HEAD", "--no-color")
	h := sha256.Sum256([]byte(diff))
	return fmt.Sprintf("%x", h[:4])[:7]
}

// BaselineSource names the resolution tier that produced a baseline ref, so a
// caller can explain a fallback choice instead of leaving an operator to infer
// it from the diff outcome.
type BaselineSource string

const (
	// BaselineSourceExplicit is a ref the caller (the --baseline flag)
	// provided; it is returned unchanged so typos or intentionally unusual refs
	// surface from the later git diff.
	BaselineSourceExplicit BaselineSource = "explicit"
	// BaselineSourceConfig is the workspace config baseline, which
	// ResolveBaselineDetailed receives as an explicit ref; the workspace caller
	// that read it from the config names it.
	BaselineSourceConfig BaselineSource = "config"
	// BaselineSourceEpic is a configured long-lived integration branch that sits
	// nearer to HEAD than the trunk.
	BaselineSourceEpic BaselineSource = "epic"
	// BaselineSourceTrunk is the remote default branch (origin/HEAD, or
	// origin/main when the symref is unset).
	BaselineSourceTrunk BaselineSource = "trunk"
	// BaselineSourceLocalTrunk is a local main/master ref, reached only when no
	// remote trunk ref exists; it can be stale relative to the remote.
	BaselineSourceLocalTrunk BaselineSource = "local-trunk"
	// BaselineSourceUpstream is the branch's upstream tracking ref, kept as the
	// LAST resort: on any pushed branch the upstream contains HEAD, so
	// preferring it made --impacted select nothing exactly where a review diff
	// exists.
	BaselineSourceUpstream BaselineSource = "upstream"
)

// ResolvedBaseline is a baseline ref plus the resolution tier that chose it.
type ResolvedBaseline struct {
	Ref    string
	Source BaselineSource
}

// ResolveBaselineDetailed chooses the ref --impacted diffs against. Explicit
// refs are returned unchanged so typos or intentionally unusual refs surface
// from the later git diff.
//
// The default measures a branch against the ref its work will be reviewed
// against — the nearest configured epic branch, else the trunk — and never
// against the branch itself. The upstream tracking ref is deliberately the
// last resort rather than the first: on any pushed branch the upstream
// already contains HEAD, so preferring it selects nothing exactly where a
// review diff exists.
//
// Order: explicit → nearest of configured epic branches vs trunk → trunk
// (origin/HEAD, origin/main) → local main/master → upstream tracking ref
// (skipped when it is this branch's own remote counterpart) → error.
func ResolveBaselineDetailed(repoRoot, explicit string, epicBranches []string) (ResolvedBaseline, error) {
	if ref := strings.TrimSpace(explicit); ref != "" {
		return ResolvedBaseline{Ref: ref, Source: BaselineSourceExplicit}, nil
	}

	currentBranch := gitread.CurrentBranchName(repoRoot)
	trunk := gitread.TrunkRef(repoRoot)

	if epic, ok := nearestEpicBaseline(repoRoot, epicBranches, trunk, currentBranch); ok {
		return ResolvedBaseline{Ref: epic, Source: BaselineSourceEpic}, nil
	}
	if trunk != "" {
		source := BaselineSourceTrunk
		if !strings.Contains(trunk, "/") {
			source = BaselineSourceLocalTrunk
		}
		return ResolvedBaseline{Ref: trunk, Source: source}, nil
	}
	if ref := gitread.UpstreamRef(repoRoot); ref != "" && !gitread.UpstreamIsSelfCounterpart(repoRoot, ref, currentBranch) {
		return ResolvedBaseline{Ref: ref, Source: BaselineSourceUpstream}, nil
	}
	return ResolvedBaseline{}, fmt.Errorf("resolve git baseline: no explicit baseline, origin/HEAD, origin/main, local main/master, or usable upstream tracking ref was found (a branch is never measured against itself) — pass --baseline or set the workspace config baseline")
}

// ResolveTrunk chooses the repository trunk ref for automatic bare job
// selection. It intentionally ignores the current branch's upstream because a
// feature branch may track a same-named remote branch rather than trunk.
func ResolveTrunk(repoRoot string) (string, error) {
	if ref := gitread.TrunkRef(repoRoot); ref != "" {
		return ref, nil
	}
	return "", fmt.Errorf("resolve git trunk: no origin/HEAD, origin/main, local main, or local master ref was found")
}

// nearestEpicBaseline picks the candidate whose merge-base with HEAD is the
// most recent by ancestry, among the configured epic branches and the trunk.
// It returns an epic ref only when it is strictly nearer than the trunk (a tie
// keeps the trunk), and never the current branch itself: an epic branch
// checked out directly is measured against the trunk, like any other branch.
func nearestEpicBaseline(repoRoot string, epicBranches []string, trunk, currentBranch string) (string, bool) {
	candidates := epicCandidateRefs(repoRoot, epicBranches, currentBranch)
	if len(candidates) == 0 {
		return "", false
	}
	if trunk != "" {
		// Trunk first: on a tie the earlier candidate wins, so an epic that adds
		// nothing over the trunk never displaces it.
		candidates = append([]string{trunk}, candidates...)
	}
	best, bestBase := "", ""
	for _, candidate := range candidates {
		output, err := gitread.Run(repoRoot, "merge-base", candidate, "HEAD")
		if err != nil {
			continue // unrelated history or unresolvable candidate: not a baseline
		}
		base := strings.TrimSpace(output)
		if best == "" {
			best, bestBase = candidate, base
			continue
		}
		if base != bestBase && gitread.IsAncestorOf(repoRoot, bestBase, base) {
			best, bestBase = candidate, base
		}
	}
	if best == "" || best == trunk {
		return "", false
	}
	return best, true
}

// OnEpicBranch reports whether the checkout works on a configured epic: its
// branch is one of the epic branches, or it forks from one (the tier
// ResolveBaselineDetailed names BaselineSourceEpic). It answers the same way
// whatever baseline a caller passed explicitly, so a gate given a base SHA
// still sees the epic its branch belongs to.
func OnEpicBranch(repoRoot string, epicBranches []string) bool {
	if len(epicBranches) == 0 {
		return false
	}
	branch := gitread.CurrentBranchName(repoRoot)
	if branch != "" {
		for _, ref := range epicCandidateRefs(repoRoot, epicBranches, "") {
			if strings.TrimPrefix(ref, "origin/") == branch {
				return true
			}
		}
	}
	_, ok := nearestEpicBaseline(repoRoot, epicBranches, gitread.TrunkRef(repoRoot), branch)
	return ok
}

// epicCandidateRefs expands the configured epic branch names or globs into
// existing refs, preferring the remote-tracking form over a local branch of
// the same name and excluding the current branch. Results are ordered by
// branch name so resolution is deterministic.
func epicCandidateRefs(repoRoot string, patterns []string, currentBranch string) []string {
	refArgs := make([]string, 0, len(patterns)*2)
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || strings.HasPrefix(pattern, "-") || strings.ContainsAny(pattern, " \t\n") {
			continue
		}
		refArgs = append(refArgs, "refs/remotes/origin/"+pattern, "refs/heads/"+pattern)
	}
	if len(refArgs) == 0 {
		return nil
	}
	output, err := gitread.Run(repoRoot, append([]string{"for-each-ref", "--format=%(refname:short)"}, refArgs...)...)
	if err != nil {
		return nil
	}
	byBranch := make(map[string]string)
	for _, ref := range splitLines(output) {
		branch := strings.TrimPrefix(ref, "origin/")
		if branch == "" || branch == "HEAD" || branch == currentBranch {
			continue
		}
		if existing, ok := byBranch[branch]; !ok || !strings.HasPrefix(existing, "origin/") {
			byBranch[branch] = ref
		}
	}
	branches := make([]string, 0, len(byBranch))
	for branch := range byBranch {
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	refs := make([]string, 0, len(branches))
	for _, branch := range branches {
		refs = append(refs, byBranch[branch])
	}
	return refs
}

// WorkingTreeDiff is one baseline diff of the working tree, with the evidence a
// reader needs to reproduce it: the commit it was measured against, and which
// of its paths no commit records yet.
type WorkingTreeDiff struct {
	// Base is the commit the diff was taken against — the merge base of the
	// baseline and HEAD — as a full object id. When git could not compute a
	// merge base, it is the commit the baseline ref resolved to, and it is the
	// ref itself only when that too failed.
	Base string
	// Files are every changed path, deduplicated, in the order git listed them:
	// changed since Base, then staged, then untracked.
	Files []string
	// Uncommitted are the Files that differ from HEAD in the index or the
	// working tree, or are untracked: the part of the diff that exists only on
	// this machine. Same order as Files; nil when the tree is clean.
	Uncommitted []string
}

// DiffWorkingTree returns the files changed between HEAD and the given baseline
// branch, with the evidence for them. Rename detection is disabled for the
// committed and staged diffs so the path set follows the same policy as
// DiffCommitFiles regardless of Git config. The evidence lets a selection
// computed on one machine be compared with one computed on another: the
// same Base and the same Files are the same diff, and a path listed in
// Uncommitted is one a commit alone would not reproduce — a file the run's own
// bootstrap rewrote, for example.
func DiffWorkingTree(repoRoot, baseline string) (WorkingTreeDiff, error) {
	baseline = strings.TrimSpace(baseline)
	if baseline == "" {
		return WorkingTreeDiff{}, fmt.Errorf("git diff baseline: empty baseline")
	}

	diffBase, err := diffBase(repoRoot, baseline)
	if err != nil {
		return WorkingTreeDiff{}, err
	}

	output, err := gitread.Run(repoRoot, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", diffBase)
	if err != nil {
		return WorkingTreeDiff{}, fmt.Errorf("git diff baseline %q: %w", baseline, err)
	}

	files := gitread.SplitNUL(output)

	// Also include untracked and staged files
	staged, _ := gitread.Run(repoRoot, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", "--cached")
	files = append(files, gitread.SplitNUL(staged)...)

	untracked, _ := gitread.Run(repoRoot, "ls-files", "-z", "--others", "--exclude-standard")
	files = append(files, gitread.SplitNUL(untracked)...)
	files = dedupStrings(files)

	// The index and the working tree against HEAD, staged changes included:
	// together with the untracked files, exactly what a clean checkout of HEAD
	// would not have.
	dirty, _ := gitread.Run(repoRoot, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", "HEAD")
	local := make(map[string]bool)
	for _, path := range append(gitread.SplitNUL(dirty), gitread.SplitNUL(untracked)...) {
		local[path] = true
	}
	var uncommitted []string
	for _, path := range files {
		if local[path] {
			uncommitted = append(uncommitted, path)
		}
	}

	// merge-base prints a full object id; the fallbacks return a ref.
	base := diffBase
	if full := len(base) == 40 || len(base) == 64; !full || !gitread.ValidObjectID(base) {
		if sha, err := gitread.ResolveCommit(repoRoot, base); err == nil {
			base = sha
		}
	}
	return WorkingTreeDiff{Base: base, Files: files, Uncommitted: uncommitted}, nil
}

// UncommittedPaths returns the paths no commit records: those that differ
// from HEAD in the index or the working tree, and the untracked files git does
// not ignore. It is the local part of a DiffWorkingTree, read on its own so a
// caller can record it before anything in the process writes to the tree.
func UncommittedPaths(repoRoot string) ([]string, error) {
	dirty, err := gitread.Run(repoRoot, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git diff HEAD: %w", err)
	}
	untracked, err := gitread.Run(repoRoot, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("git ls-files --others: %w", err)
	}
	return dedupStrings(append(gitread.SplitNUL(dirty), gitread.SplitNUL(untracked)...)), nil
}

func diffBase(repoRoot, baseline string) (string, error) {
	mergeBase, mergeErr := gitread.Run(repoRoot, "merge-base", baseline, "HEAD")
	if mergeErr == nil {
		return strings.TrimSpace(mergeBase), nil
	}

	tried := []string{baseline}
	if remoteRef, ok := remoteFallbackRef(repoRoot, baseline); ok {
		if mergeBase, err := gitread.Run(repoRoot, "merge-base", remoteRef, "HEAD"); err == nil {
			return strings.TrimSpace(mergeBase), nil
		}
		return remoteRef, nil
	}
	if gitread.RefExists(repoRoot, baseline) {
		return baseline, nil
	}
	return "", fmt.Errorf("git baseline %q could not be resolved (tried %s): %w", baseline, strings.Join(tried, ", "), mergeErr)
}

func remoteFallbackRef(repoRoot, baseline string) (string, bool) {
	branch := strings.TrimSpace(baseline)
	if branch == "" || branch == "HEAD" || strings.HasPrefix(branch, "origin/") || strings.HasPrefix(branch, "refs/remotes/") {
		return "", false
	}
	branch = strings.TrimPrefix(branch, "refs/heads/")
	candidate := "origin/" + branch
	if gitread.RefExists(repoRoot, candidate) {
		return candidate, true
	}
	return "", false
}

// IgnoredDirs returns the set of directories under repoRoot that git ignores
// entirely (e.g. generated output or node_modules), keyed by slash-separated
// path relative to repoRoot with no trailing slash. Directories that contain
// tracked files are not collapsed by git and therefore not returned.
//
// Returns nil (and no error) when git is unavailable or repoRoot is not a git
// repository, so callers can treat "no info" the same as "nothing ignored".
func IgnoredDirs(repoRoot string) map[string]bool {
	// --directory collapses fully-ignored directories to a single entry with a
	// trailing slash; partially-tracked directories are left expanded.
	output, err := gitread.Run(repoRoot, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil
	}
	dirs := make(map[string]bool)
	for _, line := range splitLines(output) {
		if !strings.HasSuffix(line, "/") {
			continue // only collapsed directories, not individual files
		}
		dirs[strings.TrimSuffix(line, "/")] = true
	}
	return dirs
}

// CommonDir returns the absolute path to the repository's shared git common
// directory (the main checkout's .git). Every linked worktree of a repo
// resolves to the same value, so it is a stable per-repo identity suitable for
// keying a machine-global, per-repo build store shared across all worktrees.
// Returns "" when git is unavailable or repoRoot is not inside a work tree.
func CommonDir(repoRoot string) string {
	output, err := gitread.Run(repoRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(output)
	if p == "" {
		return ""
	}
	// git prints a path relative to the invocation cwd for the primary checkout
	// (".git") and an absolute path for linked worktrees; normalize both.
	if !filepath.IsAbs(p) {
		p = filepath.Join(repoRoot, p)
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		abs = filepath.Clean(p)
	}
	// Follow directory links, a junction on Windows included, so a worktree
	// reached through a linked path (e.g. macOS /var → /private/var) yields the
	// SAME common dir as the main checkout, which git may report via the
	// canonical path. Without this, sibling worktrees could key different
	// stores. Best-effort: keep abs if it fails.
	if resolved, err := dirlink.Resolve(abs); err == nil {
		abs = resolved
	}
	return abs
}

func splitLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func dedupStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
