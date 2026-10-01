// Package runnersource captures source bytes for portable execution. It does
// not execute a plan or establish authority to read another workspace's blobs.
package runnersource

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	runner "go.putnami.dev/protocol/runner"
)

// Store is a caller-owned private blob namespace. Its lifetime and access are
// owned by that caller; a digest is an integrity check, never an access token.
type Store struct {
	root *os.Root
}

// OpenStore opens a private source store. It never uses the shared task cache.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil || broadPermissions(info.Mode()) {
		_ = root.Close()
		return nil, fmt.Errorf("source store must be a private directory: %s", dir)
	}
	return &Store{root: root}, nil
}

// Close releases the store directory handle without deleting its contents.
func (s *Store) Close() error { return s.root.Close() }

// put publishes a complete immutable object without replacing an existing one.
// A concurrent publisher may win the link; its bytes are then validated too.
func (s *Store) put(reader io.Reader, size int64) (string, error) {
	name := ".capture-" + rand.Text()
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer func() { _ = s.root.Remove(name) }()
	digest, copyErr := copyBlob(file, reader, size)
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	if err := s.root.Link(name, strings.TrimPrefix(digest, "sha256:")); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := s.copyTo(io.Discard, runner.SourceEntry{Digest: digest, Size: size}); err != nil {
		return "", err
	}
	return digest, nil
}

func copyBlob(dst io.Writer, src io.Reader, size int64) (string, error) {
	if size < 0 || size > runner.MaxSourceFileBytes {
		return "", fmt.Errorf("source blob size %d is outside protocol limits", size)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(src, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("source changed or blob is corrupt: expected %d bytes, read %d", size, n)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *Store) copyTo(dst io.Writer, entry runner.SourceEntry) error {
	// Validate before opening so even internal callers cannot supply a path in
	// place of a digest. Symlink objects are forbidden, including safe links.
	name, ok := strings.CutPrefix(entry.Digest, "sha256:")
	if !ok || len(name) != sha256.Size*2 || name != strings.ToLower(name) {
		return fmt.Errorf("invalid source blob digest")
	}
	if _, err := hex.DecodeString(name); err != nil {
		return fmt.Errorf("invalid source blob digest")
	}
	info, err := s.root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source blob %s is not a regular file", entry.Digest)
	}
	file, err := s.root.OpenFile(name, sourceReadFlags, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("source blob changed while opening")
	}
	digest, err := copyBlob(dst, file, entry.Size)
	if err != nil {
		return err
	}
	if digest != entry.Digest {
		return fmt.Errorf("source blob %s failed digest verification", entry.Digest)
	}
	return nil
}

// WriteBlob copies one verified blob out of the store. It exists for transfer
// of exactly the blobs a receiver reported missing; a digest the store does
// not hold is an error, never a silently empty file.
func (s *Store) WriteBlob(dst io.Writer, entry runner.SourceEntry) error {
	return s.copyTo(dst, entry)
}

// Ingest publishes one blob handed over by a transfer and returns the digest
// its bytes actually have. The caller compares it against the digest the
// manifest declared; a mismatch is a corrupt transfer, never accepted source.
func (s *Store) Ingest(reader io.Reader, size int64) (string, error) {
	return s.put(reader, size)
}

// Has reports whether the store already holds a verified blob for the entry.
func (s *Store) Has(entry runner.SourceEntry) bool {
	return s.copyTo(io.Discard, entry) == nil
}
