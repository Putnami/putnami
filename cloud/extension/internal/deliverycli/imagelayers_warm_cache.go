package deliverycli

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type imageLayersWarmLockEntry struct {
	Version      string            `json:"version"`
	Integrities  map[string]string `json:"integrities"`
	ManifestHash string            `json:"manifestHash,omitempty"`
}

// Seed only lock-selected artifacts from the same store used by the launcher.
// The official installer still rechecks the manifest binding and normalizes the
// destination. No credential files, unrelated entries or host source paths are
// copied into the materialized image store.
func imageLayersSeedWarmStore(workspaceRoot, workspaceDir, destination string, artifacts []imageLayersWarmArtifact) error {
	var lock struct {
		Version    int                                 `json:"version"`
		Extensions map[string]imageLayersWarmLockEntry `json:"extensions"`
	}
	if err := imageBuildReadJSON(filepath.Join(workspaceRoot, "putnami.lock.json"), &lock); err != nil {
		return err
	}
	selected := make(map[string]imageLayersWarmLockEntry, len(artifacts))
	for _, artifact := range artifacts {
		entry := lock.Extensions[artifact.name]
		if entry.Version != artifact.version || entry.Integrities[imageLayersWarmPlatform] != artifact.sha256 {
			return fmt.Errorf("warm extension lock changed for %s", artifact.name)
		}
		selected[artifact.name] = entry
	}
	data, err := json.Marshal(map[string]any{"version": lock.Version, "extensions": selected})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "putnami.lock.json"), data, 0o600); err != nil {
		return err
	}
	root := os.Getenv("PUTNAMI_ARTIFACT_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		root = filepath.Join(home, ".putnami", "artifacts")
	}
	for _, artifact := range artifacts {
		entry := selected[artifact.name]
		if !imageLayersSHA256Pattern.MatchString(entry.ManifestHash) {
			continue // An unbound cache entry cannot replace a verified download.
		}
		relative := filepath.Join(imageLayersWarmStoreDir, artifact.sha256[:imageLayersWarmShardLen], artifact.sha256)
		source := filepath.Join(root, relative)
		manifest, err := os.ReadFile(filepath.Join(source, "putnami.extension.json")) //nolint:gosec // G304: exact validated lock digest beneath the configured artifact store
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		digest := sha256.Sum256(manifest)
		if hex.EncodeToString(digest[:]) != entry.ManifestHash {
			continue // Let the installer fetch and verify a clean archive.
		}
		entries, err := imageLayersWarmEntries(source, "artifact")
		if err != nil {
			return err
		}
		for _, item := range entries {
			target := filepath.Join(destination, relative, filepath.FromSlash(strings.TrimPrefix(item.name, "artifact/")))
			switch item.typeflag {
			case tar.TypeDir:
				err = os.MkdirAll(target, 0o755)
			case tar.TypeReg:
				var input *os.File
				input, err = os.Open(item.blob) //nolint:gosec // G304: bounded validated store walk
				if err == nil {
					err = imageLayersCopyEntry(input, target, item.size)
					_ = input.Close()
				}
				if err == nil {
					mode := os.FileMode(0o644)
					if item.mode&0o111 != 0 {
						mode = 0o755
					}
					err = os.Chmod(target, mode)
				}
			case tar.TypeSymlink:
				err = os.Symlink(item.linkname, target)
			}
			if err != nil {
				return fmt.Errorf("seed warm extension %s: %w", artifact.name, err)
			}
		}
	}
	return nil
}
