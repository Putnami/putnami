package oci

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"go.putnami.dev/sdk/extension/internal/regularfile"
)

// layoutImage returns the image whose manifest digest is digest in the OCI
// layout at dir: the image index.json lists with that digest and an image
// manifest media type. It reads index.json and every blob with
// regularfile.Open, so no read follows a symbolic link at the file or waits
// on a FIFO.
func layoutImage(dir string, digest v1.Hash) (v1.Image, error) {
	raw, err := readLayoutFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse index.json: %w", err)
	}
	for _, descriptor := range index.Manifests {
		if descriptor.Digest != digest {
			continue
		}
		if descriptor.MediaType != types.OCIManifestSchema1 && descriptor.MediaType != types.DockerManifestSchema2 {
			return nil, fmt.Errorf("index.json lists %s with media type %q, not an image manifest", digest, descriptor.MediaType)
		}
		return partial.CompressedToImage(&layoutManifest{dir: dir, descriptor: descriptor})
	}
	return nil, fmt.Errorf("index.json does not list %s", digest)
}

// readLayoutFile reads the regular file at name whole.
func readLayoutFile(name string) ([]byte, error) {
	file, err := regularfile.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

// layoutBlobPath is the path of the blob digest names in the layout at dir.
func layoutBlobPath(dir string, digest v1.Hash) string {
	return filepath.Join(dir, "blobs", digest.Algorithm, digest.Hex)
}

// layoutManifest is the image manifest a layout index lists.
type layoutManifest struct {
	dir        string
	descriptor v1.Descriptor

	once     sync.Once
	manifest []byte
	err      error
}

var _ partial.CompressedImageCore = (*layoutManifest)(nil)

func (m *layoutManifest) MediaType() (types.MediaType, error) {
	return m.descriptor.MediaType, nil
}

// RawManifest returns the manifest blob, read once.
func (m *layoutManifest) RawManifest() ([]byte, error) {
	m.once.Do(func() {
		m.manifest, m.err = readLayoutFile(layoutBlobPath(m.dir, m.descriptor.Digest))
	})
	return m.manifest, m.err
}

func (m *layoutManifest) RawConfigFile() ([]byte, error) {
	manifest, err := partial.Manifest(m)
	if err != nil {
		return nil, err
	}
	return readLayoutFile(layoutBlobPath(m.dir, manifest.Config.Digest))
}

// LayerByDigest returns the config or the layer the manifest names with
// digest.
func (m *layoutManifest) LayerByDigest(digest v1.Hash) (partial.CompressedLayer, error) {
	manifest, err := partial.Manifest(m)
	if err != nil {
		return nil, err
	}
	if digest == manifest.Config.Digest {
		return &layoutBlob{dir: m.dir, descriptor: manifest.Config}, nil
	}
	for _, layer := range manifest.Layers {
		if digest == layer.Digest {
			return &layoutBlob{dir: m.dir, descriptor: layer}, nil
		}
	}
	return nil, fmt.Errorf("the manifest names no blob %s", digest)
}

// layoutBlob is a blob a layout manifest names, described by the manifest.
type layoutBlob struct {
	dir        string
	descriptor v1.Descriptor
}

var _ partial.CompressedLayer = (*layoutBlob)(nil)

func (b *layoutBlob) Digest() (v1.Hash, error) { return b.descriptor.Digest, nil }

func (b *layoutBlob) Compressed() (io.ReadCloser, error) {
	return regularfile.Open(layoutBlobPath(b.dir, b.descriptor.Digest))
}

func (b *layoutBlob) Size() (int64, error) { return b.descriptor.Size, nil }

func (b *layoutBlob) MediaType() (types.MediaType, error) { return b.descriptor.MediaType, nil }

// Descriptor returns the manifest's descriptor of the blob, so a push carries
// its annotations and URLs.
func (b *layoutBlob) Descriptor() (*v1.Descriptor, error) {
	descriptor := b.descriptor
	return &descriptor, nil
}
