package git

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// SourceRevisionEnv names the commit a publication is BOUND to when it is
	// not the checkout's HEAD. A runner that tests and publishes a synthetic
	// merge or squash commit of the commit it was asked to check sets it to
	// that commit, so the version suffix and the release-set source revision
	// name what the check and the publish authority are keyed on rather than
	// the synthetic commit. It is a full lowercase hex commit id, 40 or 64
	// characters; empty or unset means HEAD. The tree that is built and hashed
	// is always the checkout's.
	SourceRevisionEnv = "PUTNAMI_SOURCE_REVISION"
	// SourceCommitTimeEnv is the committer time of SourceRevisionEnv's commit
	// in unix seconds. It is read only when that commit is not in the
	// repository, where git cannot answer; when git can, git's time wins and
	// this variable is ignored.
	SourceCommitTimeEnv = "PUTNAMI_SOURCE_COMMIT_TIME"
)

// SourceRevisionOverride reads SourceRevisionEnv. set reports whether the
// variable carries a value after trimming; err reports a value that is not a
// full lowercase hex commit id, in which case revision is "" and set is true.
// This is the ONE reader of the variable: the version suffix, the release-set
// provenance and `version tag` all go through it, so they cannot disagree
// about which commit a run is bound to.
func SourceRevisionOverride() (revision string, set bool, err error) {
	revision = strings.TrimSpace(os.Getenv(SourceRevisionEnv))
	if revision == "" {
		return "", false, nil
	}
	if !validSourceRevision(revision) {
		return "", true, fmt.Errorf("%s=%q is not a full lowercase hex commit id (40 or 64 characters)", SourceRevisionEnv, revision)
	}
	return revision, true, nil
}

// validSourceRevision accepts exactly a lowercase hex object id of full
// length: 40 characters for SHA-1, 64 for SHA-256. Uppercase is refused rather
// than folded, so one commit has one spelling wherever the value is compared.
func validSourceRevision(revision string) bool {
	if len(revision) != 40 && len(revision) != 64 {
		return false
	}
	for _, r := range revision {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

// BoundSourceRevision is SourceRevisionOverride plus the proof that a version
// can be stamped from it. When the variable is set, it returns the override
// only if the commit's short SHA and committer time can be established: from
// git when the repository has the commit, else from SourceCommitTimeEnv. When
// they cannot, err says why, and a caller must fail rather than publish or
// stamp without a version. Unset, it returns ("", false, nil) and runs no git
// command, so a caller outside a repository keeps its behavior.
//
// The release-set provenance calls it before recording the override as the
// members' source revision, so the release set cannot name a commit whose
// version TreeState could not build, even when no snapshot was captured in the
// same process. The run's own snapshot needs no second check: TreeState fails
// on the same condition, and the engine fails the run on that error.
func BoundSourceRevision(repoRoot string) (revision string, set bool, err error) {
	revision, set, err = SourceRevisionOverride()
	if err != nil || !set {
		return revision, set, err
	}
	if _, _, err := boundCommit(repoRoot); err != nil {
		return "", true, err
	}
	return revision, true, nil
}

// sourceCommitTime reads SourceCommitTimeEnv for a bound commit the
// repository does not have. The variable is required there: falling back to
// HEAD's time would stamp the synthetic commit's date on a publication bound
// to another commit, and a fixed date would sort below everything on the
// channel.
func sourceCommitTime() (time.Time, error) {
	raw := strings.TrimSpace(os.Getenv(SourceCommitTimeEnv))
	if raw == "" {
		return time.Time{}, fmt.Errorf("%s is required when %s names a commit the repository does not have", SourceCommitTimeEnv, SourceRevisionEnv)
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, fmt.Errorf("%s=%q is not a unix time in seconds", SourceCommitTimeEnv, raw)
	}
	return time.Unix(seconds, 0).UTC(), nil
}

// sourceRevisionShortLength is how many leading characters of
// SourceRevisionEnv the version suffix carries. It is fixed rather than git's
// abbreviation: git lengthens its abbreviation as a repository grows, and a
// runner without the commit has no abbreviation at all, so one bound commit
// would otherwise publish under a different version on each runner. Twelve is
// the length Go gives the revision of a pseudo-version, which the suffix of a
// line at 0.0.0 already resembles, and its 48 bits make two commits sharing a
// prefix negligible without counting on the commit time beside it.
const sourceRevisionShortLength = 12

// boundCommit answers the short SHA and committer time the version suffix is
// built from: HEAD's, unless SourceRevisionEnv names the commit the run is
// bound to. The bound commit's short SHA is always its first
// sourceRevisionShortLength characters, with no git call. Its time is git's
// when the repository has the commit, else SourceCommitTimeEnv, which is then
// required. Unset, both come from git exactly as before.
func boundCommit(repoRoot string) (string, time.Time, error) {
	override, set, err := SourceRevisionOverride()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read the source revision: %w", err)
	}
	if !set {
		sha := shortSHA(repoRoot)
		if sha == "" {
			return "", time.Time{}, fmt.Errorf("read HEAD: the repository has no commit")
		}
		committedAt, ok := commitTime(repoRoot, "HEAD")
		if !ok {
			return "", time.Time{}, fmt.Errorf("read the commit time of HEAD")
		}
		return sha, committedAt, nil
	}
	sha := override[:sourceRevisionShortLength]
	// One call answers both questions: whether the repository has the commit,
	// and its committer time. ^{commit} refuses a tree or blob id.
	if committedAt, ok := commitTime(repoRoot, override+"^{commit}"); ok {
		return sha, committedAt, nil
	}
	committedAt, err := sourceCommitTime()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read the source revision: %w", err)
	}
	return sha, committedAt, nil
}
