package oci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The layout reader returns the image the index lists, with its manifest,
// config and layers, and refuses an index it cannot read or that lists the
// digest as something other than an image manifest.
func TestLayoutImageReadsTheListedImage(t *testing.T) {
	dir, digest := writeRandomLayout(t)
	want, err := v1.NewHash(digest)
	if err != nil {
		t.Fatal(err)
	}
	img, err := layoutImage(dir, want)
	if err != nil {
		t.Fatalf("layoutImage = %v", err)
	}
	if got, err := img.Digest(); err != nil || got != want {
		t.Fatalf("image digest = %s, %v; want %s", got, err, want)
	}
	if _, err := img.ConfigFile(); err != nil {
		t.Fatalf("config: %v", err)
	}
	layers, err := img.Layers()
	if err != nil || len(layers) != 2 {
		t.Fatalf("layers = %d, %v; want 2", len(layers), err)
	}
	for _, layer := range layers {
		reader, err := layer.Compressed()
		if err != nil {
			t.Fatalf("layer: %v", err)
		}
		_ = reader.Close()
		descriptor, err := partial.Descriptor(layer)
		if err != nil || descriptor.Size == 0 {
			t.Fatalf("layer descriptor = %+v, %v", descriptor, err)
		}
	}
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	core := &layoutManifest{dir: dir, descriptor: v1.Descriptor{Digest: want, MediaType: types.OCIManifestSchema1}}
	if _, err := core.LayerByDigest(manifest.Config.Digest); err != nil {
		t.Fatalf("config blob: %v", err)
	}
	if _, err := core.LayerByDigest(v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("a blob the manifest does not name was found")
	}

	for name, index := range map[string]string{
		"an index that is not JSON":     "not json",
		"a digest listed as an index":   `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"` + digest + `","size":1}]}`,
		"an index that omits the image": `{"schemaVersion":2,"manifests":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(index), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := layoutImage(dir, want); err == nil {
				t.Fatal("layoutImage accepted the index")
			}
		})
	}
	if _, err := layoutImage(filepath.Join(dir, "missing"), want); err == nil {
		t.Fatal("layoutImage read a missing layout")
	}
}
