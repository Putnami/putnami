package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	extensionproto "go.putnami.dev/protocol/extension"
)

const (
	layerCacheFormat = 2
)

type layerCacheRecord struct {
	Format int    `json:"format"`
	Key    string `json:"key"`
	Digest string `json:"digest"`
	DiffID string `json:"diffID"`
	Size   int64  `json:"size"`
}

// LayerCacheRootFromEnv returns the core-owned OCI cache root handed to image
// packaging jobs. An empty value disables layer caching, which keeps the SDK
// usable outside the Putnami orchestrator without inventing a local path.
func LayerCacheRootFromEnv() string {
	return strings.TrimSpace(os.Getenv(extensionproto.SharedOCILayerCacheRootEnv))
}

// fileLayerCacheKey is the compatibility entrypoint for callers without an
// image plan. Planned packaging passes the already-computed LayerPlan directly.
func fileLayerCacheKey(layer Layer) (string, error) {
	plan, err := NewImagePlan(Spec{Layers: []Layer{layer}}, PlanOptions{})
	if err != nil {
		return "", err
	}
	key, err := plan.Layers[0].Key()
	return string(key), err
}

func digestSourceFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	if size != info.Size() {
		return "", 0, fmt.Errorf("source size changed while fingerprinting: read %d bytes, expected %d", size, info.Size())
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// loadOrBuildFileLayer returns a verified cached layer when available. A
// missing, malformed, truncated, or digest-mismatched entry is an ordinary
// cache miss and is replaced atomically after a successful build.
func loadOrBuildFileLayer(layer Layer, blobPath, cacheRoot string, mediaType types.MediaType) (v1.Layer, bool, error) {
	if cacheRoot == "" {
		built, err := buildFileLayer(layer, blobPath, mediaType)
		return built, false, err
	}
	plan, err := NewImagePlan(Spec{Layers: []Layer{layer}}, PlanOptions{})
	if err != nil {
		return nil, false, err
	}
	return loadOrBuildPlannedFileLayer(layer, &plan.Layers[0], blobPath, cacheRoot, mediaType)
}

// loadOrBuildPlannedFileLayer reuses the fingerprint acquired by ImagePlan.
// A hit never opens source files. A miss verifies the same fingerprint while
// bytes flow into the tar, avoiding a separate before/after hashing pass.
func loadOrBuildPlannedFileLayer(layer Layer, plan *LayerPlan, blobPath, cacheRoot string, mediaType types.MediaType) (v1.Layer, bool, error) {
	keyValue, err := plan.Key()
	if err != nil {
		return nil, false, err
	}
	key := string(keyValue)
	if cacheRoot == "" {
		built, err := buildPlannedFileLayer(layer, plan, blobPath, mediaType)
		return built, false, err
	}
	entryDir := layerCacheEntryDir(cacheRoot, key)
	cached, cacheErr := loadCachedFileLayer(entryDir, key, mediaType)
	if cacheErr == nil {
		// Never expose a GC-managed path to assembly. Materializing the verified
		// blob into this build's work directory lets clean/GC safely evict the
		// cache immediately after the hit without breaking the returned image.
		if err := materializeLayerBlob(cached.path, blobPath); err == nil {
			cached.path = blobPath
			_ = os.Chtimes(entryDir, time.Now(), time.Now())
			return cached, true, nil
		}
	} else if !errors.Is(cacheErr, os.ErrNotExist) {
		_ = os.RemoveAll(entryDir)
	}

	built, err := buildPlannedFileLayer(layer, plan, blobPath, mediaType)
	if err != nil {
		return nil, false, err
	}
	if staged, ok := built.(*fileLayer); ok {
		// A cache write failure never makes a valid image build fail. The complete
		// staged layer remains available to this assembly; the next run retries.
		_ = storeCachedFileLayer(cacheRoot, key, staged)
	}
	return built, false, nil
}

func layerCacheEntryDir(root, key string) string {
	return filepath.Join(root, fmt.Sprintf("v%d", layerCacheFormat), key[:2], key)
}

func loadCachedFileLayer(entryDir, key string, mediaType types.MediaType) (*fileLayer, error) {
	recordBytes, err := os.ReadFile(filepath.Join(entryDir, "record.json"))
	if err != nil {
		return nil, err
	}
	var record layerCacheRecord
	if err := json.Unmarshal(recordBytes, &record); err != nil {
		return nil, err
	}
	if record.Format != layerCacheFormat || record.Key != key || record.Size < 0 {
		return nil, fmt.Errorf("invalid layer cache record")
	}
	wantDigest, err := v1.NewHash(record.Digest)
	if err != nil {
		return nil, err
	}
	wantDiffID, err := v1.NewHash(record.DiffID)
	if err != nil {
		return nil, err
	}
	blobPath := filepath.Join(entryDir, "layer.tar.gz")
	f, err := os.Open(blobPath)
	if err != nil {
		return nil, err
	}
	gotDigest, gotSize, hashErr := v1.SHA256(f)
	closeErr := f.Close()
	if hashErr != nil {
		return nil, hashErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if gotDigest != wantDigest || gotSize != record.Size {
		return nil, fmt.Errorf("cached layer blob does not match its record")
	}

	verified := &fileLayer{
		path:      blobPath,
		digest:    wantDigest,
		diffID:    wantDiffID,
		size:      record.Size,
		mediaType: mediaType,
	}
	uncompressed, err := verified.Uncompressed()
	if err != nil {
		return nil, err
	}
	gotDiffID, _, hashErr := v1.SHA256(uncompressed)
	closeErr = uncompressed.Close()
	if hashErr != nil {
		return nil, hashErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if gotDiffID != wantDiffID {
		return nil, fmt.Errorf("cached layer diffID does not match its record")
	}
	return verified, nil
}

func storeCachedFileLayer(cacheRoot, key string, layer *fileLayer) error {
	entryDir := layerCacheEntryDir(cacheRoot, key)
	parent := filepath.Dir(entryDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(parent, ".layer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := copyLayerBlob(layer.path, filepath.Join(tmpDir, "layer.tar.gz")); err != nil {
		return err
	}
	record := layerCacheRecord{
		Format: layerCacheFormat,
		Key:    key,
		Digest: layer.digest.String(),
		DiffID: layer.diffID.String(),
		Size:   layer.size,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "record.json"), encoded, 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(entryDir); err == nil {
		return nil
	}
	if err := os.Rename(tmpDir, entryDir); err != nil {
		if _, statErr := os.Stat(entryDir); statErr == nil {
			return nil // a concurrent writer won with the same content key
		}
		return err
	}
	return nil
}

func copyLayerBlob(source, target string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func materializeLayerBlob(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".layer-materialize-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	src, err := os.Open(source)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	_, copyErr := io.Copy(tmp, src)
	srcCloseErr := src.Close()
	closeErr := tmp.Close()
	if copyErr != nil {
		return copyErr
	}
	if srcCloseErr != nil {
		return srcCloseErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmpPath, target)
}
