package oci

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// compressionBufferSize matches go-containerregistry's gzip stream buffer.
// It bounds memory without making the buffer part of the layer identity.
const compressionBufferSize = 2 << 16

// fileLayer is a reopenable compressed layer whose descriptor was computed
// while the blob was produced. It avoids the extra gzip and tar rereads that
// tarball.LayerFromFile requires for an uncompressed staged tar.
type fileLayer struct {
	path      string
	digest    v1.Hash
	diffID    v1.Hash
	size      int64
	mediaType types.MediaType
}

func (l *fileLayer) Digest() (v1.Hash, error) { return l.digest, nil }

func (l *fileLayer) DiffID() (v1.Hash, error) { return l.diffID, nil }

func (l *fileLayer) Size() (int64, error) { return l.size, nil }

func (l *fileLayer) MediaType() (types.MediaType, error) { return l.mediaType, nil }

func (l *fileLayer) Compressed() (io.ReadCloser, error) { return os.Open(l.path) }

func (l *fileLayer) Uncompressed() (io.ReadCloser, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &layerUncompressedReader{Reader: zr, gzip: zr, file: f}, nil
}

type layerUncompressedReader struct {
	io.Reader
	gzip *gzip.Reader
	file *os.File
}

func (r *layerUncompressedReader) Close() error {
	return errors.Join(r.gzip.Close(), r.file.Close())
}

type layerSizeWriter struct{ size int64 }

func (w *layerSizeWriter) Write(p []byte) (int, error) {
	w.size += int64(len(p))
	return len(p), nil
}

// buildFileLayer creates a deterministic compressed blob in one pass over the
// source files. The uncompressed tar hash (diffID), compressed blob hash, and
// compressed size are computed as the bytes flow to disk.
func buildFileLayer(layer Layer, blobPath string, mediaType types.MediaType) (v1.Layer, error) {
	return buildPlannedFileLayer(layer, nil, blobPath, mediaType)
}

func buildPlannedFileLayer(layer Layer, plan *LayerPlan, blobPath string, mediaType types.MediaType) (v1.Layer, error) {
	f, err := os.OpenFile(blobPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	cleanUp := func() {
		_ = f.Close()
		_ = os.Remove(blobPath)
	}

	diffHasher := sha256.New()
	compressedHasher := sha256.New()
	compressedSize := &layerSizeWriter{}
	buffered := bufio.NewWriterSize(
		io.MultiWriter(f, compressedHasher, compressedSize),
		compressionBufferSize,
	)
	zw, err := gzip.NewWriterLevel(buffered, layerCompressionLevel)
	if err != nil {
		cleanUp()
		return nil, err
	}
	if err := writePlannedLayerTar(io.MultiWriter(zw, diffHasher), layer, plan); err != nil {
		_ = zw.Close()
		cleanUp()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		cleanUp()
		return nil, err
	}
	if err := buffered.Flush(); err != nil {
		cleanUp()
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(blobPath)
		return nil, err
	}

	return &fileLayer{
		path: blobPath,
		digest: v1.Hash{
			Algorithm: "sha256",
			Hex:       hex.EncodeToString(compressedHasher.Sum(nil)),
		},
		diffID: v1.Hash{
			Algorithm: "sha256",
			Hex:       hex.EncodeToString(diffHasher.Sum(nil)),
		},
		size:      compressedSize.size,
		mediaType: mediaType,
	}, nil
}
