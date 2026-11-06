package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/sdk/extension/dirlink"
)

// Format-1 fixture writers. A 0.3.0 CLI wrote format-1 entries: an output
// directory captured whole under the raw cache key, as meta.json, result.json,
// manifest.json and a files/ tree of hardlinks into the CAS. This CLI writes
// none and still reads them (LocalStore.Get, CacheManager.Lookup, GC), so the
// reader tests build their fixtures with these writers.

// putFormatOneEntry publishes entry under hash the way a 0.3.0 CLI did:
// entry.FilesDir, when set, is ingested into the CAS and linked into files/,
// and the staged blob is committed with publishEntry under the shared lock.

func putFormatOneEntry(s *LocalStore, hash string, entry *Entry) error {
	s.ensureGeneration() // count this build before taking the store lock

	// Shared lock for the whole ingest+publish: coexists with sibling
	// publishers, excludes GC.
	releaseLock := s.lockShared()
	defer releaseLock()

	// Create staging directory (lock-free — each tmp dir is unique)
	tmpDir, err := s.createTmpDir()
	if err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) // cleanup on error

	// Write meta.json
	if entry.Metadata == nil {
		entry.Metadata = &EntryMetadata{Hash: hash, CreatedAt: time.Now()}
	}
	entry.Metadata.Hash = hash

	metaJSON, err := json.MarshalIndent(entry.Metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "meta.json"), metaJSON, 0o644); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}

	// Write result.json
	if entry.Result != nil {
		resultJSON, err := json.MarshalIndent(entry.Result, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "result.json"), resultJSON, 0o644); err != nil {
			return fmt.Errorf("write result.json: %w", err)
		}
	}

	// Ingest output files into the CAS and stage a files/ tree of hardlinks
	// plus a manifest of per-file digests. Throwaway intermediates (e.g. lcov
	// coverage temp shards) are filtered out so they never enter the cache.
	if entry.FilesDir != "" {
		destFiles := filepath.Join(tmpDir, "files")
		manifest, totalSize, err := ingestFormatOneFiles(s, entry.FilesDir, destFiles)
		if err != nil {
			return fmt.Errorf("ingest files: %w", err)
		}

		// Write the manifest alongside the result.
		manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal manifest: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, manifestFilename), manifestJSON, 0o644); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}

		outputFiles := make([]string, 0, len(manifest.Files))
		for _, f := range manifest.Files {
			outputFiles = append(outputFiles, f.Path)
		}
		entry.Metadata.Size = totalSize
		entry.Metadata.OutputFiles = outputFiles

		// Re-write meta.json with updated size/files (best-effort — metadata is advisory)
		if updated, err := json.MarshalIndent(entry.Metadata, "", "  "); err == nil {
			os.WriteFile(filepath.Join(tmpDir, "meta.json"), updated, 0o644)
		}
	}

	// In-process lock for the atomic publish (the cross-process shared lock is
	// already held for the whole call).
	s.mu.Lock()
	defer s.mu.Unlock()

	// Atomic move: tmp → blobs/{hash[0:2]}/{hash}/
	blobDir := s.blobDir(hash)
	blobParent := filepath.Dir(blobDir)
	if err := os.MkdirAll(blobParent, 0o755); err != nil {
		return fmt.Errorf("create blob parent: %w", err)
	}

	if err := s.publishEntry(tmpDir, blobDir); err != nil {
		return err
	}
	// Stamp recency + this build's generation so a freshly-built entry has an
	// idle-reclaim baseline even if it is never hit again.
	writeLastUsed(blobDir, time.Now(), s.gen)
	return nil
}

// saveFormatOneEntry stores result and meta under hash, capturing outputDir
// whole when it holds at least one entry.
func saveFormatOneEntry(cm *CacheManager, hash string, result *EntryResult, meta *EntryMetadata, outputDir string) error {
	entry := &Entry{
		Result:   result,
		Metadata: meta,
	}

	// If outputDir exists and has files, include them
	if outputDir != "" {
		if info, err := os.Stat(outputDir); err == nil && info.IsDir() {
			// Check if directory is non-empty
			entries, _ := os.ReadDir(outputDir)
			if len(entries) > 0 {
				entry.FilesDir = outputDir
			}
		}
	}

	return putFormatOneEntry(cm.store, hash, entry)
}

// saveFormatOneDirs stores one entry whose files/ tree holds each directory of
// dirs under its id, the multi-rooted shape RestoreSubdir reads. An absent or
// empty directory is skipped.
func saveFormatOneDirs(cm *CacheManager, hash string, result *EntryResult, meta *EntryMetadata, dirs map[string]string) error {
	staging, err := cm.store.createTmpDir()
	if err != nil {
		return fmt.Errorf("stage output dirs: %w", err)
	}
	defer os.RemoveAll(staging)

	// Deterministic order keeps the staging step stable across runs.
	ids := make([]string, 0, len(dirs))
	for id := range dirs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	staged := false
	for _, id := range ids {
		src := dirs[id]
		info, statErr := os.Stat(src)
		if statErr != nil || !info.IsDir() {
			continue
		}
		entries, _ := os.ReadDir(src)
		if len(entries) == 0 {
			continue
		}
		if err := copyDir(src, filepath.Join(staging, id)); err != nil {
			return fmt.Errorf("stage %q: %w", id, err)
		}
		staged = true
	}

	entry := &Entry{Result: result, Metadata: meta}
	if staged {
		entry.FilesDir = staging
	}
	return putFormatOneEntry(cm.store, hash, entry)
}

// ingestFormatOneFiles ingests each cacheable file under srcDir into the CAS,
// links it into dstFiles, and returns the manifest sorted by path and the total
// logical size. A srcDir that is a directory link is resolved first.
func ingestFormatOneFiles(s *LocalStore, srcDir, dstFiles string) (*cache.Manifest, int64, error) {
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
		// A symlink's own mode is 0777 while digestFile reads its target's
		// bytes; capture the target's mode instead.
		if info.Mode()&os.ModeSymlink != 0 {
			if info, err = os.Stat(path); err != nil {
				return err
			}
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

// materializeFormatOneEntry publishes a remote hit as a format-1 entry under
// hash: each manifest blob is fetched unless the CAS holds it, verified,
// linked into files/, and the blob is committed with publishEntry.
func materializeFormatOneEntry(s *LocalStore, hash string, entry *Entry, fetch BlobFetcher) error {
	if entry == nil || entry.Result == nil {
		return fmt.Errorf("materialize %s: entry with result required", hash)
	}
	if entry.Manifest == nil {
		return fmt.Errorf("materialize %s: manifest required", hash)
	}

	// Shared lock for the whole CAS fetch+publish (as putFormatOneEntry): coexists with
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
