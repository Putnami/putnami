// Package imagetest serves the image fixtures of the stack extensions' tests:
// an in-memory OCI registry with a base image to package onto.
//
// A test binary that uses it calls dockertest.IsolateConfig in its TestMain.
package imagetest

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"go.putnami.dev/sdk/extension/dockertest"
)

// Registry serves an in-memory OCI registry for the test and pushes a
// base image index to it with a linux/amd64 and a linux/arm64 image, each
// naming its platform in its config, as a distroless base does. It answers
// the registry host and the base's digest reference.
//
// It fails a test whose binary reads the developer's Docker configuration:
// the default keychain would run the credential helper that configuration
// names, even for this registry, and a helper that blocks hangs the test
// until the job timeout.
func Registry(t testing.TB) (string, string) {
	t.Helper()
	if !dockertest.ConfigIsolated() {
		t.Fatal("imagetest.Registry: this test binary reads the developer's Docker configuration: call dockertest.IsolateConfig (go.putnami.dev/sdk/extension/dockertest) in TestMain, and leave DOCKER_CONFIG as it sets it")
	}
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	var addenda []mutate.IndexAddendum
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
		if image, err = mutate.ConfigFile(image, config); err != nil {
			t.Fatal(err)
		}
		addenda = append(addenda, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}}})
	}
	index := mutate.AppendManifests(empty.Index, addenda...)
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
