package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// plannedTarballLayer verifies the raw tarball fingerprint in the same reads
// go-containerregistry already performs for compression detection and OCI
// descriptors. It adds no standalone verification pass.
func plannedTarballLayer(path string, expected SourceFingerprint, mediaType types.MediaType) (v1.Layer, error) {
	if err := expected.validate(); err != nil {
		return nil, err
	}
	opener := func() (io.ReadCloser, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if info.Size() != expected.Size {
			_ = file.Close()
			return nil, fmt.Errorf("source %s changed since image plan", path)
		}
		return &fingerprintReadCloser{
			file: file, path: path, expected: expected,
			hasher: sha256.New(),
		}, nil
	}
	return tarball.LayerFromOpener(opener,
		tarball.WithCompressionLevel(layerCompressionLevel),
		tarball.WithMediaType(mediaType))
}

type fingerprintReadCloser struct {
	file     *os.File
	path     string
	expected SourceFingerprint
	hasher   hash.Hash
	size     int64
}

func (r *fingerprintReadCloser) Read(buffer []byte) (int, error) {
	read, err := r.file.Read(buffer)
	if read > 0 {
		_, _ = r.hasher.Write(buffer[:read])
		r.size += int64(read)
	}
	if err == io.EOF && (r.size != r.expected.Size || hex.EncodeToString(r.hasher.Sum(nil)) != r.expected.Digest) {
		return read, fmt.Errorf("source %s changed since image plan", r.path)
	}
	return read, err
}

func (r *fingerprintReadCloser) Close() error { return r.file.Close() }
