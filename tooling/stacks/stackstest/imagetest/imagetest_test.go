package imagetest

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"go.putnami.dev/sdk/extension/dockertest"
)

func TestMain(m *testing.M) {
	restore := dockertest.IsolateConfig()
	code := m.Run()
	restore()
	os.Exit(code)
}

// refusingT records that the fixture failed the test. Fatal ends the
// goroutine, as testing.T does, so the fixture runs in one of its own.
type refusingT struct {
	testing.TB
	refused bool
}

func (r *refusingT) Helper() {}

func (r *refusingT) Fatal(...any) {
	r.refused = true
	runtime.Goexit()
}

func TestRegistryRefusesTheDeveloperDockerConfiguration(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	recorder := &refusingT{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Registry(recorder)
	}()
	<-done
	if !recorder.refused {
		t.Fatal("the registry serves a binary that reads the developer's Docker configuration")
	}
}

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
