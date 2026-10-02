package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	// root is the workspace the snapshot was read from.
	root           string
	sourceRevision string
	// commits maps each commit to its position in the snapshot, the bound
	// commit at 0.
	commits map[string]int
	shallow bool
	err     error
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
	_, ok := s.Position(rev)
	return ok
}

// Position is rev's index in the snapshot, in the order git lists the bound
// commit's history: the bound commit is 0. It is false whenever Contains is.
func (s *AncestrySnapshot) Position(rev string) (int, bool) {
	if s == nil || s.err != nil {
		return 0, false
	}
	position, ok := s.commits[rev]
	return position, ok
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
	snapshot := &AncestrySnapshot{root: filepath.Clean(root)}
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
	snapshot.commits = make(map[string]int, len(commits))
	for position, commit := range commits {
		if _, listed := snapshot.commits[commit]; !listed {
			snapshot.commits[commit] = position
		}
	}
	return snapshot
}

// ancestryContextKey carries the snapshot CaptureAncestry read.
type ancestryContextKey struct{}

// CaptureAncestry reads the ancestry snapshot of the workspace at root when
// the invocation may publish, and returns ctx carrying it. commands are the
// invocation's commands and portable its bound request, nil without one.
//
// An adapter calls it at process start, before the first-use bootstrap or an
// install runs repository code. Engine.Run then reuses the snapshot for the
// same workspace instead of reading one after them. An invocation that cannot
// publish reads nothing and returns ctx unchanged.
func CaptureAncestry(ctx context.Context, root string, commands []string, portable *runner.ExecutionRequest) context.Context {
	if root == "" || !mayPublish(commands, portable) {
		return ctx
	}
	return context.WithValue(ctx, ancestryContextKey{}, captureAncestrySnapshot(root, MaxAncestryCommits))
}

// capturedAncestry is the snapshot CaptureAncestry read for the workspace at
// root, or nil.
func capturedAncestry(ctx context.Context, root string) *AncestrySnapshot {
	if ctx == nil {
		return nil
	}
	snapshot, _ := ctx.Value(ancestryContextKey{}).(*AncestrySnapshot)
	if snapshot == nil || snapshot.root != filepath.Clean(root) {
		return nil
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
	var portable *runner.ExecutionRequest
	if req.Portable != nil {
		portable = &req.Portable.Request
	}
	return mayPublish(req.Commands, portable)
}

// mayPublish reports whether an invocation may publish: a bound request that
// carries invocation.publication, or a publish or deploy command.
func mayPublish(commands []string, portable *runner.ExecutionRequest) bool {
	if portable != nil && portable.Invocation.Publication != nil {
		return true
	}
	return slices.ContainsFunc(commands, func(command string) bool {
		return slices.Contains(runner.PublicationCommands, command)
	})
}
