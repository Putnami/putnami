package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// The CAS read path: the local store's side of remote-cache lazy
// materialization ("Build without the Bytes").
//
// OpenBlob exposes local CAS bytes by digest so the remote-cache write path can
// upload them. Materialize is the inverse: it installs a remote hit (a result +
// manifest of per-file digests) as a first-class local entry, pulling any blob
// it does not already hold through a caller-supplied fetcher. Bytes already in
// CAS are reused (dedup), so a hit moves only the blobs the local store is
// missing. The on-disk result is identical to a locally produced entry, so a
// later local lookup hits without touching the network.

// BlobFetcher yields the bytes of a CAS blob the local store is missing while
// materializing a remote hit. The digest is "sha256:<hex>"; the caller closes
// the reader. The remote-cache client's presigned-download opener satisfies it.
type BlobFetcher func(digest string) (io.ReadCloser, error)

// OpenBlob returns a reader for the CAS blob addressed by digest
// ("sha256:<hex>"), satisfying the remote-cache write path's blob source. The
// caller closes the reader.
func (s *LocalStore) OpenBlob(digest string) (io.ReadCloser, error) {
	if !cache.ValidDigest(digest) {
		return nil, fmt.Errorf("open blob: invalid digest %q", digest)
	}
	f, err := os.Open(s.casBlobPath(digest))
	if err != nil {
		return nil, fmt.Errorf("open blob %s: %w", digest, err)
	}
	return f, nil
}

// Materialize installs a remote cache hit as a local entry under hash. The
// entry's Manifest lists each output file with its CAS digest; fetch supplies
// the bytes for any digest not already present locally. Each fetched blob is
// verified against its digest, published into the CAS, and hardline into the
// entry's files/ tree; meta/result/manifest are staged and the whole entry is
// committed with a single atomic rename, mirroring Put. fetch may be nil when
// every referenced blob is already local (e.g. shared with another entry).
func (s *LocalStore) Materialize(hash string, entry *Entry, fetch BlobFetcher) error {
	if entry == nil || entry.Result == nil {
		return fmt.Errorf("materialize %s: entry with result required", hash)
	}
	if entry.Manifest == nil {
		return fmt.Errorf("materialize %s: manifest required", hash)
	}

	// Shared lock for the whole CAS fetch+publish (mirrors Put): coexists with
	// sibling publishers, excludes GC.
	releaseLock := s.lockShared()
	defer releaseLock()

	tmpDir, err := s.createTmpDir()
	if err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) // no-op once renamed; cleanup on error

	if entry.Metadata == nil {
		entry.Metadata = &EntryMetadata{Hash: hash, CreatedAt: time.Now()}
	}
	entry.Metadata.Hash = hash

	resultJSON, err := json.MarshalIndent(entry.Result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "result.json"), resultJSON, 0o644); err != nil {
		return fmt.Errorf("write result.json: %w", err)
	}

	manifestJSON, err := json.MarshalIndent(entry.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, manifestFilename), manifestJSON, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	dstFiles := filepath.Join(tmpDir, "files")
	total, err := s.materializeFiles(entry.Manifest, dstFiles, fetch)
	if err != nil {
		return fmt.Errorf("materialize files: %w", err)
	}

	outputFiles := make([]string, 0, len(entry.Manifest.Files))
	for _, f := range entry.Manifest.Files {
		outputFiles = append(outputFiles, f.Path)
	}
	entry.Metadata.Size = total
	entry.Metadata.OutputFiles = outputFiles

	metaJSON, err := json.MarshalIndent(entry.Metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "meta.json"), metaJSON, 0o644); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}

	// In-process lock for the atomic publish (the cross-process shared lock is
	// already held for the whole call).
	s.mu.Lock()
	defer s.mu.Unlock()

	blobDir := s.blobDir(hash)
	if err := os.MkdirAll(filepath.Dir(blobDir), 0o755); err != nil {
		return fmt.Errorf("create blob parent: %w", err)
	}

	// First-writer-wins (see publishEntry): never clobber a sibling's entry.
	if err := s.publishEntry(tmpDir, blobDir); err != nil {
		return err
	}

	if len(entry.Manifest.Files) > 0 {
		entry.FilesDir = filepath.Join(blobDir, "files")
	}

	return nil
}

// PrefetchBlobs installs the manifest's missing bytes into CAS without
// publishing a result or files tree under blobs/. It deliberately uses the
// same bounded, digest-verifying materialization path as a full restore; the
// temporary hardlink tree is discarded once every blob is present.
func (s *LocalStore) PrefetchBlobs(manifest *cache.Manifest, fetch BlobFetcher) error {
	if manifest == nil {
		return fmt.Errorf("prefetch blobs: manifest required")
	}

	releaseLock := s.lockShared()
	defer releaseLock()

	tmpDir, err := s.createTmpDir()
	if err != nil {
		return fmt.Errorf("prefetch blobs: create tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if _, err := s.materializeFiles(manifest, filepath.Join(tmpDir, "files"), fetch); err != nil {
		return fmt.Errorf("prefetch blobs: %w", err)
	}
	return nil
}

// materializeConcurrency bounds the in-flight blob fetches for a single entry. A
// manifest is often hundreds of tiny files (a TypeScript build's .d.ts fan-out is
// the worst case: ~300 sub-2 KB files), so restore latency is dominated by
// per-blob round trips, not bytes — fetching them serially turns the makespan into
// a sum of latencies. Fanning out collapses that into a parallel max; the store's
// only shared mutation (the CAS publish) is already serialized inside
// installBlobLocked, so the fan-out needs no extra locking.
const materializeConcurrency = 16

// materializeFiles installs each manifest file under dstFiles as a hardlink into
// the CAS, fetching and verifying any blob the store does not already hold. Files
// are independent — each missing blob is staged off-lock and published under the
// store lock — so they are materialized concurrently, bounded by
// materializeConcurrency. It returns the total logical size of the manifest's
// files. The first error stops scheduling further fetches and is returned once all
// in-flight work drains.
func (s *LocalStore) materializeFiles(manifest *cache.Manifest, dstFiles string, fetch BlobFetcher) (int64, error) {
	// Validate every digest and path up front so a corrupt manifest fails
	// before any network I/O, preserving the serial path's fail-fast behavior.
	// A path must also be local on this host (filepath.IsLocal). On Windows
	// that refuses a colon, which names an alternate data stream, and a
	// reserved device name such as NUL; on Unix every path the protocol admits
	// is local.
	for _, f := range manifest.Files {
		if !cache.ValidDigest(f.Digest) {
			return 0, fmt.Errorf("manifest file %q has invalid digest %q", f.Path, f.Digest)
		}
		if !filepath.IsLocal(filepath.FromSlash(f.Path)) {
			return 0, fmt.Errorf("manifest file %q is not a local path on this host", f.Path)
		}
	}

	var (
		total atomic.Int64
		wg    sync.WaitGroup
		errMu sync.Mutex
		first error
	)
	firstErr := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return first
	}
	setErr := func(err error) {
		errMu.Lock()
		if first == nil {
			first = err
		}
		errMu.Unlock()
	}

	work := make(chan cache.FileEntry)
	workers := materializeConcurrency
	if len(manifest.Files) < workers {
		workers = len(manifest.Files)
	}
	for range workers {
		wg.Go(func() {
			for f := range work {
				if firstErr() != nil {
					continue
				}
				if err := s.materializeFile(f, dstFiles, fetch); err != nil {
					setErr(err)
					continue
				}
				total.Add(f.Size)
			}
		})
	}

	for _, f := range manifest.Files {
		if firstErr() != nil {
			break
		}
		work <- f
	}
	close(work)
	wg.Wait()
	if err := firstErr(); err != nil {
		return 0, err
	}
	return total.Load(), nil
}

// materializeFile installs one manifest file: it stages the blob's verified bytes
// into a temp file off-lock when the CAS is missing them (so a network fetch never
// blocks the store), then publishes and hardlinks it under the store lock. The
// digest is assumed already validated by materializeFiles. Safe to call from many
// goroutines: stageVerifiedBlob writes uniquely-named temp files and
// installBlobLocked serializes the CAS publish.
func (s *LocalStore) materializeFile(f cache.FileEntry, dstFiles string, fetch BlobFetcher) error {
	var staged string
	if !s.hasBlob(f.Digest) {
		if fetch == nil {
			return fmt.Errorf("blob %s for %q not local and no fetcher provided", f.Digest, f.Path)
		}
		rc, err := fetch(f.Digest)
		if err != nil {
			return fmt.Errorf("fetch blob %s: %w", f.Digest, err)
		}
		staged, err = s.stageVerifiedBlob(f.Digest, rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}

	target := filepath.Join(dstFiles, filepath.FromSlash(f.Path))
	return s.installBlobLocked(f.Digest, staged, target, os.FileMode(f.Mode))
}

// stageVerifiedBlob streams r into a temp file in the CAS directory, verifying
// the bytes content-address to digest, and returns the temp file path for the
// caller to publish under the store lock. The temp file is removed on any
// verification or IO failure.
func (s *LocalStore) stageVerifiedBlob(digest string, r io.Reader) (string, error) {
	casDir := filepath.Join(s.root, "cas")
	if err := os.MkdirAll(casDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(casDir, "fetch-")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()

	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, h), r)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("read blob %s: %w", digest, copyErr)
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return "", closeErr
	}

	got := cache.DigestAlgorithm + ":" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		os.Remove(tmpName)
		return "", fmt.Errorf("blob content-addresses to %s, want %s", got, digest)
	}
	return tmpName, nil
}

// installBlobLocked publishes a freshly staged blob into the CAS (if not
// already present) and hardlinks it into the entry's files/ tree, holding the
// store lock across both steps. staged is "" when the blob was already in
// CAS; a non-empty staged that loses the publish race is discarded.
func (s *LocalStore) installBlobLocked(digest, staged, target string, mode os.FileMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.hasBlob(digest) {
		if staged == "" {
			return fmt.Errorf("blob %s missing and not staged", digest)
		}
		dst := s.casBlobPath(digest)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			os.Remove(staged)
			return err
		}
		var size int64
		if info, err := os.Stat(staged); err == nil {
			size = info.Size()
		}
		published, err := s.publishBlob(staged, digest)
		if err != nil {
			os.Remove(staged)
			return fmt.Errorf("publish blob %s: %w", digest, err)
		}
		if published {
			s.added.Add(size)
		} else {
			os.Remove(staged) // another writer published the same digest
		}
	} else if staged != "" {
		os.Remove(staged) // raced with another writer; the CAS copy wins
	}

	return s.linkBlob(digest, target, mode)
}
