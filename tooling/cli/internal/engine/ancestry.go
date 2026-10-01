package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/git"
)

// MaxAncestryCommits bounds the ancestry snapshot. A history above it is not
// read into memory: the snapshot records ErrAncestryLimit instead, and it
// contains no commit.
const MaxAncestryCommits = 1_000_000

// ErrAncestryLimit reports a bound commit that reaches more than
// MaxAncestryCommits commits.
var ErrAncestryLimit = errors.New("the bound commit's ancestry exceeds the commit limit")

// AncestrySnapshot is the set of commits the run's bound commit reaches, read
// once, before the run's first hook. A run that may publish reads it there
// because hooks and tasks run repository code, and repository code can write
// refs, replace refs, a graft file or a commit-graph that change what git
// answers afterwards. The snapshot is read from the commit objects alone and
// is held in memory, so nothing written after it changes its answers.
//
// The bound commit is PUTNAMI_SOURCE_REVISION when a runner sets it, else
// HEAD: the commit the release-set provenance records as its source revision.
// A caller that checks a commit against the snapshot first compares
// SourceRevision with the revision its plan names, and refuses a difference.
//
// Reading it never fails the run. A failure, an unreadable repository or a
// history above MaxAncestryCommits is kept in Err, and the snapshot then
// contains no commit, so every check against it fails closed. A nil snapshot,
// the answer for a run that cannot publish, behaves the same way.
type AncestrySnapshot struct {
	sourceRevision string
	commits        map[string]struct{}
	shallow        bool
	err            error
}

// SourceRevision is the full lowercase commit id the snapshot is bound to, or
// "" when it could not be read.
func (s *AncestrySnapshot) SourceRevision() string {
	if s == nil {
		return ""
	}
	return s.sourceRevision
}

// Commits is the number of commits the snapshot holds, the bound commit
// included.
func (s *AncestrySnapshot) Commits() int {
	if s == nil {
		return 0
	}
	return len(s.commits)
}

// Shallow reports whether the snapshot was read from a shallow clone. A
// shallow clone lacks part of the history, so a commit it does not contain may
// still be an ancestor of the bound commit.
func (s *AncestrySnapshot) Shallow() bool {
	return s != nil && s.shallow
}

// Contains reports whether rev, a full lowercase hex commit id, is the bound
// commit or one of its ancestors. It is false for a nil snapshot and for one
// that recorded an error.
func (s *AncestrySnapshot) Contains(rev string) bool {
	if s == nil || s.err != nil {
		return false
	}
	_, ok := s.commits[rev]
	return ok
}

// Err is why the snapshot holds no commit, or nil when it was read whole.
func (s *AncestrySnapshot) Err() error {
	if s == nil {
		return nil
	}
	return s.err
}

// captureAncestrySnapshot reads the ancestry of the commit the run is bound
// to, at most limit commits of it.
func captureAncestrySnapshot(root string, limit int) *AncestrySnapshot {
	snapshot := &AncestrySnapshot{}
	revision, set, err := git.BoundSourceRevision(root)
	if err == nil && !set {
		revision, err = git.HeadSHA(root)
	}
	if err != nil {
		snapshot.err = fmt.Errorf("read the ancestry: read the bound commit: %w", err)
		return snapshot
	}
	snapshot.sourceRevision = strings.ToLower(strings.TrimSpace(revision))
	if snapshot.shallow, err = git.IsShallow(root); err != nil {
		snapshot.err = fmt.Errorf("read the ancestry of %s: %w", snapshot.sourceRevision, err)
		return snapshot
	}
	commits, err := git.RevList(root, snapshot.sourceRevision, limit+1)
	if err != nil {
		snapshot.err = fmt.Errorf("read the ancestry of %s: %w", snapshot.sourceRevision, err)
		return snapshot
	}
	if len(commits) > limit {
		snapshot.err = fmt.Errorf("%w: %s reaches more than %d commits", ErrAncestryLimit, snapshot.sourceRevision, limit)
		return snapshot
	}
	snapshot.commits = make(map[string]struct{}, len(commits))
	for _, commit := range commits {
		snapshot.commits[commit] = struct{}{}
	}
	return snapshot
}

// readsAncestry reports whether the run may publish, and so reads the
// ancestry snapshot: it names a publish or deploy command, or it executes a
// bound request that carries invocation.publication. A watch iteration reads
// none: repository code has already run in its session, so a snapshot read
// there would not precede it.
func readsAncestry(req *Request) bool {
	if req.watchIteration {
		return false
	}
	if req.Portable != nil && req.Portable.Request.Invocation.Publication != nil {
		return true
	}
	return slices.ContainsFunc(req.Commands, func(command string) bool {
		return slices.Contains(runner.PublicationCommands, command)
	})
}
