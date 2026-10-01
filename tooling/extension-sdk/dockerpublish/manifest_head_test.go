package dockerpublish

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
)

// recordingRegistry is the in-process distribution registry behind a handler
// that records every request, so a test can count what a publication asks.
type recordingRegistry struct {
	inner http.Handler
	mu    sync.Mutex
	calls []privateOCITestCall
}

func newRecordingRegistry() *recordingRegistry {
	return &recordingRegistry{inner: registry.New()}
}

func (r *recordingRegistry) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	r.calls = append(r.calls, privateOCITestCall{
		Method: request.Method, Path: request.URL.Path, Authorization: request.Header.Get("Authorization"),
	})
	r.mu.Unlock()
	r.inner.ServeHTTP(w, request)
}

func (r *recordingRegistry) recordedCalls() []privateOCITestCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]privateOCITestCall(nil), r.calls...)
}

// manifestHeads returns the paths of the manifest HEAD requests among calls.
func manifestHeads(calls []privateOCITestCall) []string {
	var heads []string
	for _, call := range calls {
		if call.Method == http.MethodHead && strings.Contains(call.Path, "/manifests/") {
			heads = append(heads, call.Path)
		}
	}
	return heads
}

// blobWrites counts the requests that start or carry a blob upload.
func blobWrites(calls []privateOCITestCall) int {
	count := 0
	for _, call := range calls {
		if call.Method != http.MethodHead && call.Method != http.MethodGet && strings.Contains(call.Path, "/blobs/") {
			count++
		}
	}
	return count
}

// publicationRoute is one way an image reaches a registry: the publish call,
// the requests the registry has received so far, and the packaged digest.
type publicationRoute struct {
	publish func() (string, map[string]any, error)
	calls   func() []privateOCITestCall
	digest  string
}

// A manifest HEAD that misses is the most expensive request a registry serves,
// and every new image used to send three: the cache lookup, the one
// remote.Write sends before its PUT, and a digest lookup after it. On every
// route, a new image now costs exactly the lookup, and a reused one costs the
// lookup and no blob upload.
func TestPublishSendsOneManifestHeadPerImage(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "oci-publish-by-digest", "one-manifest-existence-check-per-image")
	routes := []struct {
		name  string
		setup func(t *testing.T) publicationRoute
	}{
		{"workload image, direct registry", func(t *testing.T) publicationRoute {
			fixture := newDockerPublishFixture(t)
			return publicationRoute{
				publish: func() (string, map[string]any, error) { return Publish(fixture.ctx, jsonl.New(), nil) },
				calls:   fixture.registry.recordedCalls,
				digest:  fixture.digest,
			}
		}},
		{"workload image, private broker", func(t *testing.T) publicationRoute {
			fixture := newDockerPublishFixture(t)
			fixture.ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"` + managedOCIRegistry + `/putnami"}}`)
			broker := newPrivateOCITestBroker(t, "")
			broker.manifestPolicy = func(selector string) bool {
				return selector == fixture.digest || selector == fixture.version
			}
			t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")
			registrycred.ResolveToken = func(string) (string, string) { return privateOCITestToken, "" }
			return publicationRoute{
				publish: func() (string, map[string]any, error) { return Publish(fixture.ctx, jsonl.New(), nil) },
				calls:   broker.recordedCalls,
				digest:  fixture.digest,
			}
		}},
		{"image project, direct registry", func(t *testing.T) publicationRoute {
			recording := newRecordingRegistry()
			server := newTestServer(t, recording)
			ctx, manifest := newManagedImagePublishFixture(t)
			stubResolveToken(t, "")
			host := strings.TrimPrefix(server, "http://")
			return publicationRoute{
				publish: func() (string, map[string]any, error) {
					return publishImmutableImageProject(ctx, jsonl.New(), false, host, manifest)
				},
				calls:  recording.recordedCalls,
				digest: manifest.Digest,
			}
		}},
		{"image project, private broker", func(t *testing.T) publicationRoute {
			broker := newPrivateOCITestBroker(t, "")
			t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")
			ctx, manifest := newManagedImagePublishFixture(t)
			stubResolveToken(t, privateOCITestToken)
			return publicationRoute{
				publish: func() (string, map[string]any, error) {
					return publishImmutableImageProject(ctx, jsonl.New(), false, "", manifest)
				},
				calls:  broker.recordedCalls,
				digest: manifest.Digest,
			}
		}},
	}
	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			route := tc.setup(t)

			status, data, err := route.publish()
			if err != nil || status != "OK" {
				t.Fatalf("first Publish() = (%q, %+v, %v), want OK", status, data, err)
			}
			miss := route.calls()
			assertOneDigestHead(t, "miss", miss, route.digest)
			if blobWrites(miss) == 0 {
				t.Fatalf("miss uploaded no blob; calls = %+v", miss)
			}
			if data["image_digest"] != route.digest {
				t.Fatalf("miss image_digest = %v, want the pushed digest %s", data["image_digest"], route.digest)
			}

			status, data, err = route.publish()
			if err != nil || status != "OK" {
				t.Fatalf("second Publish() = (%q, %+v, %v), want OK", status, data, err)
			}
			hit := route.calls()[len(miss):]
			assertOneDigestHead(t, "hit", hit, route.digest)
			if writes := blobWrites(hit); writes != 0 {
				t.Fatalf("hit sent %d blob writes, want none; calls = %+v", writes, hit)
			}
			if data["contentStatus"] == "pushed" || data["image_digest"] != route.digest {
				t.Fatalf("hit result = %+v, want reused content at digest %s", data, route.digest)
			}
		})
	}
}

func assertOneDigestHead(t *testing.T, phase string, calls []privateOCITestCall, digest string) {
	t.Helper()
	heads := manifestHeads(calls)
	if len(heads) != 1 || !strings.HasSuffix(heads[0], "/manifests/"+digest) {
		t.Fatalf("%s sent manifest HEADs %v, want exactly one, by digest %s", phase, heads, digest)
	}
}

// The digest publish reports without asking the registry again is the one it
// has proven: a digest-addressed hit, or a push of exactly the packaged bytes.
// Anything else falls back to the registry lookup.
func TestProvenContentDigest(t *testing.T) {
	packaged := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	cases := []struct {
		name          string
		registry      string
		contentDigest string
		contentExists bool
		pushedDigest  string
		want          string
	}{
		{name: "digest-addressed hit", registry: "r.example", contentDigest: packaged, contentExists: true, want: packaged},
		{name: "push of the packaged bytes", registry: "r.example", contentDigest: packaged, pushedDigest: packaged, want: packaged},
		{name: "push of other bytes asks", registry: "r.example", contentDigest: packaged, pushedDigest: other},
		{name: "daemon push asks", registry: "r.example", contentDigest: packaged},
		{name: "no packaged digest asks", registry: "r.example", contentExists: true, pushedDigest: packaged},
		{name: "mutable reference asks", registry: "r.example", contentDigest: "latest", contentExists: true},
		{name: "no registry reports none", contentDigest: packaged, contentExists: true},
	}
	for _, tc := range cases {
		if got := provenContentDigest(tc.registry, tc.contentDigest, tc.contentExists, tc.pushedDigest); got != tc.want {
			t.Errorf("%s: provenContentDigest() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func newTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func stubResolveToken(t *testing.T, token string) {
	t.Helper()
	old := registrycred.ResolveToken
	registrycred.ResolveToken = func(string) (string, string) { return token, "" }
	t.Cleanup(func() { registrycred.ResolveToken = old })
}
