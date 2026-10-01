package store

import (
	"fmt"
	"os"
	"path/filepath"

	cache "go.putnami.dev/protocol/cache"
)

// ExportManifestBlobs exports every CAS blob referenced by manifest into the
// provider/core exchange directory. It is the inverse of Materialize's fetcher:
// core keeps ownership of the live CAS and hands the provider hardlinks/copies
// in the protocol-defined exchange layout.
func (cm *CacheManager) ExportManifestBlobs(manifest *cache.Manifest, exchangeDir string) error {
	return cm.store.ExportManifestBlobs(manifest, exchangeDir)
}

func (s *LocalStore) ExportManifestBlobs(manifest *cache.Manifest, exchangeDir string) error {
	if manifest == nil {
		return fmt.Errorf("manifest required")
	}
	if exchangeDir == "" {
		return fmt.Errorf("exchange directory required")
	}

	releaseLock := s.lockShared()
	defer releaseLock()

	for _, f := range manifest.Files {
		if !cache.ValidDigest(f.Digest) {
			return fmt.Errorf("manifest file %q has invalid digest %q", f.Path, f.Digest)
		}
		dst, ok := cache.BlobExchangePath(exchangeDir, f.Digest)
		if !ok {
			return fmt.Errorf("invalid exchange digest %q", f.Digest)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		src := s.casBlobPath(f.Digest)
		if err := os.Link(src, dst); err != nil {
			if cpErr := copyFile(src, dst); cpErr != nil {
				return fmt.Errorf("export blob %s: link: %w; copy: %w", f.Digest, err, cpErr)
			}
		}
	}
	return nil
}
