package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/sdk/extension/robustio"
)

// Content-addressed store (CAS) layout, alongside the Action Cache blobs:
//
//	.putnami/store/
//	  cas/{digest[0:2]}/{digest}   raw file bytes, addressed by content digest
//
// Each Action Cache entry's files/ directory is a tree of HARDLINKS into CAS,
// so identical bytes across entries are stored once and the existing
// blobs/{hash}/files contract (out/ symlinks, RestoreFiles) is preserved.

// casBlobPath returns the on-disk path for a CAS blob given its digest
// ("sha256:<hex>"). The algorithm prefix is stripped for the filesystem path.
func (s *LocalStore) casBlobPath(digest string) string {
	hexsum := digest
	if i := len(cache.DigestAlgorithm) + 1; len(digest) > i {
		hexsum = digest[i:] // strip "sha256:"
	}
	prefix := hexsum
	if len(hexsum) >= 2 {
		prefix = hexsum[:2]
	}
	return filepath.Join(s.root, "cas", prefix, hexsum)
}

// hasBlob reports whether a CAS blob exists for the digest.
func (s *LocalStore) hasBlob(digest string) bool {
	_, err := os.Stat(s.casBlobPath(digest))
	return err == nil
}

// digestFile streams a file to compute its CAS digest without loading it whole
// into memory. The format matches cache.DigestOf ("sha256:<hex>").
func digestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return cache.DigestAlgorithm + ":" + hex.EncodeToString(h.Sum(nil)), n, nil
}

// ensureBlobLocked ensures the bytes of srcPath are present in CAS under digest.
// s.mu must be held by the caller.
func (s *LocalStore) ensureBlobLocked(srcPath, digest string) error {
	if s.hasBlob(digest) {
		return nil
	}

	dst := s.casBlobPath(digest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), "blob-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	src, err := os.Open(srcPath)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	written, copyErr := io.Copy(tmp, src)
	_ = src.Close()
	closeErr := tmp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}

	// Another writer in this process is serialized by s.mu, but this keeps the
	// write idempotent if an external process populated the same store path.
	if s.hasBlob(digest) {
		return nil
	}
	published, err := s.publishBlob(tmpName, digest)
	if err != nil {
		return err
	}
	if published {
		s.added.Add(written)
	}
	return nil
}

// renameBlob moves a staged blob to its CAS path. It is a variable only so a
// test can fail it the way Windows fails a rename over a blob that another
// process holds open.
var renameBlob = robustio.Rename

// publishBlob renames the staged file to the CAS path of digest and reports
// whether this call put the blob there. A rename that fails while the blob
// exists is a lost race, not an error: the digest names the bytes, so the blob
// another writer published is the one this call would have published. The
// caller removes the staged file when it is not published.
func (s *LocalStore) publishBlob(staged, digest string) (bool, error) {
	if err := renameBlob(staged, s.casBlobPath(digest)); err != nil {
		if s.hasBlob(digest) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// linkBlob materializes a CAS blob at target as a hardlink, falling back to a
// copy across devices. The file mode is applied to the materialized file.
func (s *LocalStore) linkBlob(digest, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	src := s.casBlobPath(digest)
	os.Remove(target) // clear any stale entry so Link/rename succeeds
	if err := os.Link(src, target); err != nil {
		// Cross-device or unsupported: fall back to a plain copy.
		if cpErr := copyFile(src, target); cpErr != nil {
			return fmt.Errorf("materialize blob: link: %w; copy: %w", err, cpErr)
		}
	}
	return os.Chmod(target, mode)
}

// ingestFiles walks srcDir, ingests each non-excluded file into CAS, hardlinks
// it into dstFiles, and returns the manifest plus total logical size. The
// manifest is sorted by path for deterministic output.
func (s *LocalStore) ingestFiles(srcDir, dstFiles string) (*cache.Manifest, int64, error) {
	manifest := &cache.Manifest{}
	var total int64

	// Defense in depth: if srcDir is a symlink or junction to a directory (e.g. a stale
	// out/ link into the CAS that a job runner failed to reset), filepath.Walk
	// would Lstat the root as a non-dir and try to digest it as a file
	// ("read <dir>: is a directory"). Resolve it so we walk the real directory.
	if fi, err := os.Lstat(srcDir); err == nil && dirlink.IsLink(srcDir, fi) {
		if resolved, err := dirlink.Resolve(srcDir); err == nil {
			srcDir = resolved
		}
	}

	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if uncacheableArtifact(rel) {
			return nil
		}

		digest, size, err := digestFile(path)
		if err != nil {
			return err
		}
		s.mu.Lock()
		err = s.ensureBlobLocked(path, digest)
		if err == nil {
			err = s.linkBlob(digest, filepath.Join(dstFiles, rel), info.Mode())
		}
		s.mu.Unlock()
		if err != nil {
			return err
		}

		manifest.Files = append(manifest.Files, cache.FileEntry{
			Path:   filepath.ToSlash(rel),
			Digest: digest,
			Mode:   uint32(info.Mode().Perm()),
			Size:   size,
		})
		total += size
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	cache.NormalizeManifestFiles(manifest)
	return manifest, total, nil
}
