package imagetest

import (
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestRegistryServesATwoPlatformBase(t *testing.T) {
	host, base := Registry(t)
	if !strings.HasPrefix(base, host+"/base@sha256:") {
		t.Fatalf("base = %s", base)
	}
	reference, err := name.ParseReference(base)
	if err != nil {
		t.Fatal(err)
	}
	index, err := remote.Index(reference)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	var platforms []string
	for _, m := range manifest.Manifests {
		platforms = append(platforms, m.Platform.OS+"/"+m.Platform.Architecture)
	}
	if strings.Join(platforms, ",") != "linux/amd64,linux/arm64" {
		t.Errorf("platforms = %v", platforms)
	}
}
