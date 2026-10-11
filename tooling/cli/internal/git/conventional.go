package git

import (
	"fmt"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
)

// Bump is how far a version advances. The zero value is BumpNone, and the
// constants are ordered so the strongest bump of a set of commits is their
// maximum.
type Bump int

// The ordered advance levels. BumpNone is "these commits say nothing about the
// version"; it is not the same as a patch, because a pre-release floors at a
// patch while an explicit tag does not.
const (
	BumpNone Bump = iota
	BumpPatch
	BumpMinor
	BumpMajor
)

// String renders a bump as the word a message or a test uses.
func (b Bump) String() string {
	switch b {
	case BumpPatch:
		return "patch"
	case BumpMinor:
		return "minor"
	case BumpMajor:
		return "major"
	default:
		return "none"
	}
}

// StableTest reports whether a commit, by the files it changes inside its
// line, touches a project the support catalog lists as stable. Only such a
// commit may advance a line past a patch: a preview or experimental project
// promises no compatibility, so neither its features nor its breaking changes
// move the line's minor or major. A nil StableTest reads every commit as stable,
// which is the reading of a workspace with no support catalog.
type StableTest func(files []string) bool

// CommitStableTest is StableTest asked of a commit: reading its files is a git
// call, so it can fail.
type CommitStableTest func(commit Commit) (bool, error)

// LineStableTest asks line.Stable of each commit, reading the commit's files
// inside the line's pathspecs. It is nil when line.Stable is, so a workspace
// with no catalog lists no file at all.
func LineStableTest(repoRoot string, line LineSpec) CommitStableTest {
	if line.Stable == nil {
		return nil
	}
	return func(commit Commit) (bool, error) {
		files, err := CommitFiles(repoRoot, commit.SHA, line.Pathspecs)
		if err != nil {
			return false, err
		}
		return line.Stable(files), nil
	}
}

// BumpFor reduces a set of commits to the strongest advance they justify.
// preOne selects the pre-1.0 reading, where the major number is not yet a
// compatibility promise: a breaking change is a minor and a feature is a patch,
// so a minor always means "you may have to migrate" and never only "something
// is new". A commit that stable reports as touching no stable project advances
// a patch at most.
//
// stable is asked only where its answer can raise the result: a commit whose
// level is above a patch and above the strongest one found so far. Before 1.0
// that is a breaking commit alone, so a range of features reads no file. The
// walk stops at the strongest level the reading allows.
//
// Types other than feat, fix and perf advance nothing on their own: a
// documentation commit does not make a release. A pre-release still floors at a
// patch, but that floor belongs to NextVersion, not here — an explicit
// `version tag` on a docs-only range must be able to see BumpNone and say so.
func BumpFor(commits []Commit, preOne bool, stable CommitStableTest) (Bump, error) {
	ceiling := BumpMajor
	if preOne {
		ceiling = BumpMinor
	}
	strongest := BumpNone
	for _, commit := range commits {
		parsed, ok := protocolcli.ParseConventionalCommit(commit.Subject, commit.Body)
		if !ok {
			continue
		}
		level := BumpNone
		switch {
		case parsed.Breaking && preOne:
			level = BumpMinor
		case parsed.Breaking:
			level = BumpMajor
		case parsed.Type == "feat" && preOne:
			level = BumpPatch
		case parsed.Type == "feat":
			level = BumpMinor
		case parsed.Type == "fix", parsed.Type == "perf":
			level = BumpPatch
		}
		if level <= strongest {
			continue
		}
		if level > BumpPatch && stable != nil {
			touchesStable, err := stable(commit)
			if err != nil {
				return BumpNone, err
			}
			if !touchesStable {
				level = BumpPatch
			}
		}
		if level > strongest {
			strongest = level
		}
		if strongest == ceiling {
			break
		}
	}
	return strongest, nil
}

// NextVersion advances last by bump and returns the result as "major.minor.patch".
//
// prerelease is the untagged case: the answer is the version an unreleased
// commit is stamped with, so it must be strictly above the last tag or two
// commits would publish under one number — hence the patch floor. The tagged
// and explicit case applies the bump exactly, with BumpNone meaning "no commit
// justified an advance, so take the smallest one": a release is still a release.
//
// A last version that is not a semver core is read as 0.0.0, which is also the
// answer for an untagged line.
func NextVersion(last string, bump Bump, prerelease bool) string {
	major, minor, patch := parseSemverCore(last)
	if prerelease && bump < BumpPatch {
		bump = BumpPatch
	}
	switch bump {
	case BumpMajor:
		return fmt.Sprintf("%d.0.0", major+1)
	case BumpMinor:
		return fmt.Sprintf("%d.%d.0", major, minor+1)
	case BumpPatch:
		return fmt.Sprintf("%d.%d.%d", major, minor, patch+1)
	default:
		return fmt.Sprintf("%d.%d.%d", major, minor, patch+1)
	}
}

// parseSemverCore reads the major.minor.patch of a version, tolerating a
// leading "v" and ignoring any pre-release or build metadata. An unreadable
// value is 0.0.0, the same answer as an untagged line: both mean "there is no
// released version to advance from".
func parseSemverCore(version string) (major, minor, patch int) {
	value := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if cut := strings.IndexAny(value, "-+"); cut >= 0 {
		value = value[:cut]
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return 0, 0, 0
	}
	numbers := make([]int, 3)
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return 0, 0, 0
		}
		numbers[i] = n
	}
	return numbers[0], numbers[1], numbers[2]
}

// BreakingCommits returns the commits that declare a breaking change, in the
// order given. It reads each message the way BumpFor does, so a commit listed
// here is one that moves a line past a feature release.
func BreakingCommits(commits []Commit) []Commit {
	var breaking []Commit
	for _, commit := range commits {
		if parsed, ok := protocolcli.ParseConventionalCommit(commit.Subject, commit.Body); ok && parsed.Breaking {
			breaking = append(breaking, commit)
		}
	}
	return breaking
}
