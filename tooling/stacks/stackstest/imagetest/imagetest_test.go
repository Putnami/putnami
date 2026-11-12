package imagetest

import (
	"fmt"
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

// fatalRecorder records the failure of the helper under test. Fatal ends the
// goroutine, as testing.T does, so the helper runs in one of its own.
type fatalRecorder struct {
	testing.TB
	message string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatal(args ...any) {
	r.message = strings.TrimSpace(fmt.Sprintln(args...))
	runtime.Goexit()
}

func TestRegistryRefusesTheDeveloperDockerConfiguration(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Registry(recorder)
	}()
	<-done
	if !strings.Contains(recorder.message, "call dockertest.IsolateConfig") {
		t.Fatalf("message = %q", recorder.message)
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
