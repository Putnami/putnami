package stackstest

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	ociMutate "github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// WriteFile writes content at rel under root, a slash-separated path, creating
// the directories it needs.
func WriteFile(t testing.TB, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ImageRegistry serves an in-memory OCI registry for the test and pushes a
// base image index to it with a linux/amd64 and a linux/arm64 image, each
// naming its platform in its config, as a distroless base does. It answers
// the registry host and the base's digest reference.
func ImageRegistry(t testing.TB) (string, string) {
	t.Helper()
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	var addenda []ociMutate.IndexAddendum
	for _, arch := range []string{"amd64", "arm64"} {
		image, err := random.Image(256, 1)
		if err != nil {
			t.Fatal(err)
		}
		config, err := image.ConfigFile()
		if err != nil {
			t.Fatal(err)
		}
		config = config.DeepCopy()
		config.OS, config.Architecture = "linux", arch
		if image, err = ociMutate.ConfigFile(image, config); err != nil {
			t.Fatal(err)
		}
		addenda = append(addenda, ociMutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}}})
	}
	index := ociMutate.AppendManifests(empty.Index, addenda...)
	reference, err := name.ParseReference(host + "/base:latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(reference, index); err != nil {
		t.Fatal(err)
	}
	digest, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return host, host + "/base@" + digest.String()
}
