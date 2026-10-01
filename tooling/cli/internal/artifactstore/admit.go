package artifactstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StageFunc populates a freshly-created, empty staging directory with the
// artifact's contents. It MUST verify that what it writes corresponds to the
// digest being admitted (e.g. the download archive hashes to it, and any
// manifest binding holds) and return a non-nil error otherwise: Admit publishes
// the staged tree ONLY when StageFunc returns nil, so a failed verification
// never lands bytes under a digest path that every repo on the machine would
// then trust. The blast radius of a poisoned entry is machine-wide, so this
// verify-before-publish contract is load-bearing, not advisory.
//
// LIFETIME: stageDir is valid only for the duration of the call. Admit either
// renames it into the store or deletes it the moment StageFunc returns, so
// StageFunc MUST join every subprocess it starts against that tree (including
// verification execs of a staged binary) before returning — a process still
// exec'ing under stageDir would have the file yanked out from under it, which
// surfaces as a signal death, not an error.
type StageFunc func(stageDir string) error

// Admit places a verified artifact directory in the store under digest, with
// first-writer-wins semantics so concurrent admitters of the same content
// converge on one entry. It returns the absolute path of the admitted (or
// already-present) directory under <root>/sha256/<dd>/<digest>.
func (s *Store) Admit(digest string, stage StageFunc) (string, error) {
	return s.AdmitContext(context.Background(), digest, stage)
}

// AdmitContext is Admit with context-bounded store and digest ownership locks.
// The context does not interrupt a StageFunc after it starts; callers must keep
// their own staging work bounded. It does guarantee that lock contention cannot
// hold a read-preparation path past its deadline.
func (s *Store) AdmitContext(ctx context.Context, digest string, stage StageFunc) (string, error) {
	if !isHexDigest(digest) {
		return "", fmt.Errorf("artifactstore: invalid digest %q", digest)
	}
	return s.admit(ctx, digest, s.Path(digest), stage)
}

// admit publishes a verified staged tree into dest (an absolute entry dir under
// this store) with first-writer-wins, returning dest. It is the shared core of
// Admit (the sha256/ tree) and AdmitCLI (the cli/ subtree).
//
// Flow: take the store lock SHARED (coexist with sibling admitters, exclude GC
// for the whole admit so a sweep cannot reap the staging dir mid-populate);
// short-circuit if dest is already present; otherwise stage into a fresh tmp/
// dir, run stage (verify+populate), and atomically rename the staged tree into
// dest. The staged tree is verified BEFORE it is renamed into the canonical
// path — never populated directly into it.
func (s *Store) admit(ctx context.Context, digest, dest string, stage StageFunc) (string, error) {
	release, err := s.lockSharedContext(ctx)
	if err != nil {
		return "", fmt.Errorf("acquire shared artifact-store lock: %w", err)
	}
	defer release()

	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		stampUsed(dest, time.Now())
		return dest, nil // first-writer-wins: a sibling already admitted this
	}

	// Atomic publication prevents corruption, but without an ownership lock
	// every same-digest contender can still run the expensive StageFunc. Claim
	// this digest only, then re-check: exactly one process computes it while
	// unrelated digest admits remain concurrent.
	releaseDigest, err := s.lockDigestContext(ctx, digest)
	if err != nil {
		return "", fmt.Errorf("claim artifact digest %s: %w", digest, err)
	}
	defer releaseDigest()
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		stampUsed(dest, time.Now())
		return dest, nil
	}

	tmpRoot := filepath.Join(s.root, tmpDirName)
	if err := os.MkdirAll(tmpRoot, dirPerm); err != nil {
		return "", fmt.Errorf("create artifact staging root: %w", err)
	}
	stageDir, err := os.MkdirTemp(tmpRoot, "staging-")
	if err != nil {
		return "", fmt.Errorf("create artifact staging dir: %w", err)
	}
	// no-op once renamed; cleans up on loss/error. Safe against an exec from the
	// staged tree because this fires only after stage returned, and StageFunc's
	// documented lifetime contract is that it joins its subprocesses first. It
	// also only ever removes THIS admit's private MkdirTemp dir, so it can never
	// reach a sibling's staging tree.
	defer os.RemoveAll(stageDir)

	if err := stage(stageDir); err != nil {
		return "", err // verification/population failed: nothing is published
	}

	if err := s.publish(stageDir, dest); err != nil {
		return "", err
	}
	stampUsed(dest, time.Now())
	return dest, nil
}

// publish atomically moves the staged dir to dest with first-writer-wins: dest
// is never removed before the rename, so a sibling that admitted the same
// (content-addressed, byte-identical) tree first is left intact. Staging and
// dest live under one root (same volume), so a single intra-device rename
// suffices; a rename failure is re-checked against dest to absorb the TOCTOU
// window where a sibling won the race.
func (s *Store) publish(stageDir, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil // sibling won between the Admit Stat and here
	}
	if err := os.MkdirAll(filepath.Dir(dest), dirPerm); err != nil {
		return fmt.Errorf("create artifact shard: %w", err)
	}
	if err := os.Rename(stageDir, dest); err != nil {
		if _, statErr := os.Stat(dest); statErr == nil {
			return nil // a sibling published it in the race window; keep theirs
		}
		return fmt.Errorf("publish artifact: %w", err)
	}
	return nil
}
