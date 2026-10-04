package artifactstore

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// implementationFile is the per-entry sidecar that records a value derived
	// from the entry's payload: the implementation digest a cache key carries
	// for an installed extension. The payload of an entry never changes, so
	// the value derived from it never does either, and a later run reads the
	// record instead of hashing every payload byte again.
	implementationFile = "implementationdigest"
	// maxImplementationRecord bounds the bytes read from a record. A record is
	// one short line; a larger file is not a record this store wrote.
	maxImplementationRecord = 4096
)

// Entry returns the archive digest of the published entry that dir names, and
// false when dir is not one: a path outside the store, the store's staging or
// CLI trees, or a directory below an entry's root. dir and the store root are
// compared after resolving symbolic links, so a worktree link to an entry
// names that entry.
func (s *Store) Entry(dir string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	root, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Join(root, shaDirName), resolved)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || !isHexDigest(parts[1]) || parts[0] != parts[1][:2] {
		return "", false
	}
	return parts[1], true
}

// Implementation returns the implementation record of the published entry for
// digest.
//
// A recorded value that accept admits is returned without reading the
// payload. Otherwise derive computes the value from the entry directory and
// the store records it beside the payload with an atomic rename. A record that
// cannot be read, or that accept refuses, such as one of another schema, is
// derived again and replaced.
//
// Reading, deriving and recording run under the shared store lock, so GC
// cannot remove or replace the entry while its value is derived or recorded.
// When ctx ends before the lock is held, derive runs without it and nothing is
// recorded. A failed record write is ignored: the next run derives again.
func (s *Store) Implementation(
	ctx context.Context,
	digest string,
	accept func(string) bool,
	derive func(dir string) (string, error),
) (string, error) {
	if !isHexDigest(digest) {
		return "", os.ErrNotExist
	}
	dir := s.Path(digest)
	var value string
	var err error
	lockErr := s.WithSharedContext(ctx, func() {
		if recorded, ok := readImplementation(dir); ok && accept(recorded) {
			value = recorded
			return
		}
		value, err = derive(dir)
		if err == nil {
			writeImplementation(dir, value)
		}
	})
	if lockErr != nil {
		return derive(dir)
	}
	return value, err
}

// readImplementation returns the record in dir, and false when there is none
// or it cannot be read whole within maxImplementationRecord bytes.
func readImplementation(dir string) (string, bool) {
	file, err := os.Open(filepath.Join(dir, implementationFile))
	if err != nil {
		return "", false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxImplementationRecord+1))
	if err != nil || len(data) > maxImplementationRecord {
		return "", false
	}
	return string(data), true
}

// writeImplementation atomically records value in dir. It writes nothing when
// dir is gone, so it never recreates an entry GC removed.
func writeImplementation(dir, value string) {
	tmp, err := os.CreateTemp(dir, implementationFile+"-") // IsBookkeeping covers this name
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.WriteString(value)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, filepath.Join(dir, implementationFile)); err != nil {
		os.Remove(tmpName)
	}
}
