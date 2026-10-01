package oci

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"go.putnami.dev/protocol/features/spectest"
)

// countingRegistry serves an in-memory registry and counts its requests.
type countingRegistry struct {
	server   *httptest.Server
	host     string
	requests atomic.Int64
}

func newCountingRegistry(t *testing.T) *countingRegistry {
	t.Helper()
	registryHandler := registry.New()
	counting := &countingRegistry{}
	counting.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counting.requests.Add(1)
		registryHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(counting.server.Close)
	counting.host = strings.TrimPrefix(counting.server.URL, "http://")
	return counting
}

func writeRandomLayout(t *testing.T) (string, string) {
	t.Helper()
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "layout")
	digest, err := WriteLayout(dir, img)
	if err != nil {
		t.Fatalf("WriteLayout: %v", err)
	}
	return dir, digest
}

func TestPushLayoutReturnsTheDescriptorDigestOrRefuses(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "an-upload-reuses-a-version-at-the-same-digest")
	ctx := context.Background()

	t.Run("pushes, tags and reuses", func(t *testing.T) {
		reg := newCountingRegistry(t)
		dir, digest := writeRandomLayout(t)
		target := LayoutTarget{Repository: reg.host + "/team/app", Digest: digest, Tags: []string{"1.4.0", "1.4.0", "latest"}}

		pushed, err := PushLayout(ctx, dir, target, "")
		if err != nil || pushed.Digest != digest || pushed.Reused {
			t.Fatalf("first push = %+v, %v; want a fresh push of %s", pushed, err, digest)
		}
		for _, tag := range []string{"1.4.0", "latest"} {
			got, err := crane.Digest(reg.host + "/team/app:" + tag)
			if err != nil || got != digest {
				t.Fatalf("tag %s resolves to %s, %v; want %s", tag, got, err, digest)
			}
		}

		pushed, err = PushLayout(ctx, dir, target, "")
		if err != nil || pushed.Digest != digest || !pushed.Reused {
			t.Fatalf("second push = %+v, %v; want a reuse of %s", pushed, err, digest)
		}
	})

	refusals := map[string]func(t *testing.T, dir, digest, host string) (string, LayoutTarget){
		"a digest the layout does not index": func(_ *testing.T, dir, _, host string) (string, LayoutTarget) {
			return dir, LayoutTarget{Repository: host + "/team/app", Digest: "sha256:" + strings.Repeat("a", 64)}
		},
		"a malformed digest": func(_ *testing.T, dir, digest, host string) (string, LayoutTarget) {
			return dir, LayoutTarget{Repository: host + "/team/app", Digest: strings.ToUpper(digest)}
		},
		"a tampered manifest": func(t *testing.T, dir, digest, host string) (string, LayoutTarget) {
			hash, err := v1.NewHash(digest)
			if err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(dir, "blobs", hash.Algorithm, hash.Hex)
			data, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, append(data, ' '), 0o644); err != nil {
				t.Fatal(err)
			}
			return dir, LayoutTarget{Repository: host + "/team/app", Digest: digest}
		},
		"a repository without a registry host": func(_ *testing.T, dir, digest, _ string) (string, LayoutTarget) {
			return dir, LayoutTarget{Repository: "team/app", Digest: digest}
		},
		"a repository with a tag": func(_ *testing.T, dir, digest, host string) (string, LayoutTarget) {
			return dir, LayoutTarget{Repository: host + "/team/app:latest", Digest: digest}
		},
		"an invalid tag": func(_ *testing.T, dir, digest, host string) (string, LayoutTarget) {
			return dir, LayoutTarget{Repository: host + "/team/app", Digest: digest, Tags: []string{"-latest"}}
		},
		"a missing layout": func(_ *testing.T, dir, digest, host string) (string, LayoutTarget) {
			return filepath.Join(dir, "missing"), LayoutTarget{Repository: host + "/team/app", Digest: digest}
		},
	}
	if runtime.GOOS != "windows" {
		refusals["a symbolic link inside the layout"] = func(t *testing.T, dir, digest, host string) (string, LayoutTarget) {
			if err := os.Symlink(filepath.Join(dir, "index.json"), filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			return dir, LayoutTarget{Repository: host + "/team/app", Digest: digest}
		}
		refusals["a symbolic link as the layout"] = func(t *testing.T, dir, digest, host string) (string, LayoutTarget) {
			link := filepath.Join(t.TempDir(), "layout")
			if err := os.Symlink(dir, link); err != nil {
				t.Fatal(err)
			}
			return link, LayoutTarget{Repository: host + "/team/app", Digest: digest}
		}
	}
	for name, setup := range refusals {
		t.Run("refuses "+name, func(t *testing.T) {
			reg := newCountingRegistry(t)
			dir, digest := writeRandomLayout(t)
			layoutDir, target := setup(t, dir, digest, reg.host)
			pushed, err := PushLayout(ctx, layoutDir, target, "")
			if err == nil || pushed != (PushedLayout{}) {
				t.Fatalf("PushLayout = %+v, %v; want a refusal", pushed, err)
			}
			if got := reg.requests.Load(); got != 0 {
				t.Fatalf("a refused push sent %d registry requests", got)
			}
		})
	}
}

func TestPushLayoutKeepsAContextError(t *testing.T) {
	reg := newCountingRegistry(t)
	dir, digest := writeRandomLayout(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PushLayout(ctx, dir, LayoutTarget{Repository: reg.host + "/team/app", Digest: digest}, "secret-bearer")
	if err == nil || !strings.Contains(err.Error(), "context canceled") || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled push err=%v, want context.Canceled", err)
	}
}
