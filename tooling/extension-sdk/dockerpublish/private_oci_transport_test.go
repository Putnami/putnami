package dockerpublish

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/registrycred"
)

func TestPublishImageProjectRoutesPrivateBrokerWithoutChangingLogicalEvidence(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "local-image-publication-boundary", "private-loopback-preserves-logical-evidence")
	broker := newPrivateOCITestBroker(t, "")
	t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")
	ctx, manifest := newManagedImagePublishFixture(t)

	old := registrycred.ResolveToken
	var credentialHost string
	registrycred.ResolveToken = func(host string) (string, string) {
		credentialHost = host
		return privateOCITestToken, ""
	}
	defer func() { registrycred.ResolveToken = old }()

	status, data, err := publishImmutableImageProject(ctx, jsonl.New(), false, "", manifest)
	if err != nil || status != "OK" {
		t.Fatalf("publishImmutableImageProject() = (%q, %+v, %v), want OK", status, data, err)
	}
	localHost := strings.TrimPrefix(broker.server.URL, "http://")
	if credentialHost != localHost {
		t.Fatalf("credential host = %q, want private broker host %q", credentialHost, localHost)
	}
	wantRepository := managedOCIRegistry + "/team/delivery-images-ci-runner"
	wantImmutable := wantRepository + "@" + manifest.Digest
	if data["image"] != wantImmutable {
		t.Fatalf("published image = %v, want logical immutable ref %q", data["image"], wantImmutable)
	}
	published, err := pkgmeta.ReadPublishedImageManifest(ctx.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if published.ImmutableRef != wantImmutable || published.TargetRegistry != managedOCIRegistry {
		t.Fatalf("published evidence = %+v, want managed logical coordinates", published)
	}

	calls := broker.recordedCalls()
	var sawUpload, sawManifestPut, sawDigestHead bool
	for _, call := range calls {
		if !strings.HasPrefix(call.Path, "/oci/v2/") {
			t.Fatalf("broker received path %q outside /oci/v2", call.Path)
		}
		if call.Path != "/oci/v2/" && call.Authorization != "Bearer "+privateOCITestToken {
			t.Fatalf("broker request %s %s authorization = %q", call.Method, call.Path, call.Authorization)
		}
		sawUpload = sawUpload || call.Method == http.MethodPost && strings.Contains(call.Path, "/blobs/uploads/")
		sawManifestPut = sawManifestPut || call.Method == http.MethodPut && strings.Contains(call.Path, "/manifests/")
		sawDigestHead = sawDigestHead || call.Method == http.MethodHead && strings.HasSuffix(call.Path, "/manifests/"+manifest.Digest)
	}
	if !sawUpload || !sawManifestPut || !sawDigestHead {
		t.Fatalf("broker calls = %+v, want upload, manifest PUT, and immutable HEAD", calls)
	}
}

func TestPublishImageProjectRefusesHostilePrivateBrokerLocation(t *testing.T) {
	broker := newPrivateOCITestBroker(t, "https://attacker.invalid/v2/team/delivery-images-ci-runner/blobs/uploads/escape")
	t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")
	ctx, manifest := newManagedImagePublishFixture(t)

	old := registrycred.ResolveToken
	registrycred.ResolveToken = func(string) (string, string) { return privateOCITestToken, "" }
	defer func() { registrycred.ResolveToken = old }()

	status, _, err := publishImmutableImageProject(ctx, jsonl.New(), false, "", manifest)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "private OCI broker Location refused") {
		t.Fatalf("publishImmutableImageProject() = (%q, %v), want hostile Location refusal", status, err)
	}
	if strings.Contains(err.Error(), privateOCITestToken) {
		t.Fatal("private capability appeared in the publication error")
	}
	if _, readErr := pkgmeta.ReadPublishedImageManifest(ctx.OutputPath); readErr == nil {
		t.Fatal("failed private publication wrote verified evidence")
	}
}

func TestPrivateOCITransportCompatibilityAndFailClosedValidation(t *testing.T) {
	target := imagePublishTarget{Host: managedOCIRegistry, Managed: true}
	t.Run("ordinary remote HTTPS remains unchanged", func(t *testing.T) {
		t.Setenv(privateOCIRegistryURLEnv, "https://oci.putnami.dev")
		transport, credentialHost, err := privateOCITransportFor(target)
		if err != nil || transport != nil || credentialHost != managedOCIRegistry {
			t.Fatalf("privateOCITransportFor() = (%T, %q, %v), want unchanged direct path", transport, credentialHost, err)
		}
	})

	for _, raw := range []string{
		"http://localhost:8080/oci",
		"http://127.0.0.1/oci",
		"http://127.0.0.1:8080/",
		"http://user@127.0.0.1:8080/oci",
		"http://127.0.0.1:8080/oci?scope=wide",
		"https://127.0.0.1:8080/oci",
		"http://192.0.2.10:8080/oci",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(privateOCIRegistryURLEnv, raw)
			if _, _, err := privateOCITransportFor(target); err == nil {
				t.Fatalf("privateOCITransportFor() accepted %q", raw)
			}
		})
	}

	t.Run("private broker cannot route a generic target", func(t *testing.T) {
		t.Setenv(privateOCIRegistryURLEnv, "http://127.0.0.1:8080/oci")
		if _, _, err := privateOCITransportFor(imagePublishTarget{Host: "ghcr.io"}); err == nil {
			t.Fatal("privateOCITransportFor() accepted a non-managed target")
		}
	})
}

func TestPrivateOCITransportRefusesRedirects(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Location", "/oci/v2/redirected")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	t.Setenv(privateOCIRegistryURLEnv, server.URL+"/oci")
	transport, _, err := privateOCITransportFor(imagePublishTarget{Host: managedOCIRegistry, Managed: true})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, "https://"+managedOCIRegistry+"/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("RoundTrip() error = %v, want redirect refusal", err)
	}
	if calls != 1 {
		t.Fatalf("redirect caused %d broker calls, want exactly one", calls)
	}
}

const privateOCITestToken = "private-oci-capability-token" //nolint:gosec // test-only credential

type privateOCITestCall struct {
	Method        string
	Path          string
	Authorization string
}

type privateOCITestBroker struct {
	server          *httptest.Server
	inner           http.Handler
	hostileLocation string
	// manifestPolicy mirrors the production broker's manifest rule when set:
	// a manifest request whose selector it rejects is denied with 403.
	manifestPolicy func(selector string) bool
	mu             sync.Mutex
	calls          []privateOCITestCall
}

func newPrivateOCITestBroker(t *testing.T, hostileLocation string) *privateOCITestBroker {
	t.Helper()
	broker := &privateOCITestBroker{inner: registry.New(), hostileLocation: hostileLocation}
	broker.server = httptest.NewServer(broker)
	t.Cleanup(broker.server.Close)
	return broker
}

func (b *privateOCITestBroker) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	b.mu.Lock()
	b.calls = append(b.calls, privateOCITestCall{
		Method: request.Method, Path: request.URL.Path, Authorization: request.Header.Get("Authorization"),
	})
	b.mu.Unlock()
	if !strings.HasPrefix(request.URL.Path, "/oci/v2/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if request.Header.Get("Authorization") != "Bearer "+privateOCITestToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if b.manifestPolicy != nil {
		if _, selector, found := strings.Cut(request.URL.Path, "/manifests/"); found && !b.manifestPolicy(selector) {
			http.Error(w, "manifest selector denied", http.StatusForbidden)
			return
		}
	}

	upstreamRequest := request.Clone(request.Context())
	upstreamURL := *request.URL
	upstreamURL.Path = strings.TrimPrefix(request.URL.Path, "/oci")
	upstreamRequest.URL = &upstreamURL
	upstreamRequest.RequestURI = upstreamURL.RequestURI()
	recorder := httptest.NewRecorder()
	b.inner.ServeHTTP(recorder, upstreamRequest)
	result := recorder.Result()
	defer result.Body.Close()
	for name, values := range result.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	if location := result.Header.Get("Location"); location != "" {
		if b.hostileLocation != "" && request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/blobs/uploads/") {
			w.Header().Set("Location", b.hostileLocation)
		} else {
			parsed, err := url.Parse(location)
			if err != nil {
				http.Error(w, "bad registry Location", http.StatusInternalServerError)
				return
			}
			resolved := (&url.URL{Scheme: "http", Host: request.Host}).ResolveReference(parsed)
			w.Header().Set("Location", privateOCIBrokerLocation(b.server.URL, resolved))
		}
	}
	w.WriteHeader(result.StatusCode)
	_, _ = io.Copy(w, result.Body)
}

func privateOCIBrokerLocation(base string, resolved *url.URL) string {
	location := base + "/oci" + resolved.Path
	if resolved.RawQuery != "" {
		location += "?" + resolved.RawQuery
	}
	return location
}

func (b *privateOCITestBroker) recordedCalls() []privateOCITestCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]privateOCITestCall(nil), b.calls...)
}

// A workload image (a Go or TypeScript project publishing `--docker`, not an
// `image` project) under a native publication run must take the same loopback
// route as a first-class image: every registry request goes to the broker, the
// credential seam is asked about the broker host (the cloud answers that host
// with the run's capability), the content is written by digest because the
// broker admits no content tag, and the evidence keeps the logical coordinates.
// This is the shape of a CI run where the workload publisher
// pushed straight to oci.putnami.dev and asked its token endpoint with no session.
func TestPublishWorkloadImageRoutesPrivateBrokerByDigest(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "oci-publish-by-digest", "private-loopback-routes-workload-images")
	fixture := newDockerPublishFixture(t)
	fixture.ctx.Params["registries"] = json.RawMessage(`{"oci":{"publish":"` + managedOCIRegistry + `/putnami"}}`)
	broker := newPrivateOCITestBroker(t, "")
	broker.manifestPolicy = func(selector string) bool {
		return selector == fixture.digest || selector == fixture.version
	}
	t.Setenv(privateOCIRegistryURLEnv, broker.server.URL+"/oci")

	var credentialHosts []string
	registrycred.ResolveToken = func(host string) (string, string) {
		credentialHosts = append(credentialHosts, host)
		return privateOCITestToken, ""
	}

	var status string
	var data map[string]any
	var err error
	events := captureEvents(t, func() {
		status, data, err = Publish(fixture.ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("Publish() = (%q, %+v, %v), want OK", status, data, err)
	}

	localHost := strings.TrimPrefix(broker.server.URL, "http://")
	if len(credentialHosts) != 1 || credentialHosts[0] != localHost {
		t.Fatalf("credential seam asked for %v, want exactly the private broker host %q", credentialHosts, localHost)
	}
	wantImage := managedOCIRegistry + "/putnami/" + fixture.repository + ":" + fixture.version
	if data["image"] != wantImage || data["image_digest"] != fixture.digest {
		t.Fatalf("published image = %v (%v), want logical %q at digest %q", data["image"], data["image_digest"], wantImage, fixture.digest)
	}
	members := eventsOfKind(events, extproto.PublishedMemberEventKind)
	if len(members) != 1 || eventString(t, members[0], "coordinate") != "putnami/"+fixture.repository ||
		eventString(t, members[0], "artifactDigest") != fixture.digest {
		t.Fatalf("published-member events = %+v, want one member at the managed coordinate with the digest", members)
	}

	var sawVersionPut, sawDigestWrite bool
	for _, call := range broker.recordedCalls() {
		if !strings.HasPrefix(call.Path, "/oci/v2/") {
			t.Fatalf("broker received path %q outside /oci/v2", call.Path)
		}
		if call.Path != "/oci/v2/" && call.Authorization != "Bearer "+privateOCITestToken {
			t.Fatalf("broker request %s %s authorization = %q, want the run capability", call.Method, call.Path, call.Authorization)
		}
		if strings.Contains(call.Path, "/manifests/"+fixture.contentTag) {
			t.Fatalf("broker received a content-tag request %s %s; the broker admits no content tag", call.Method, call.Path)
		}
		sawVersionPut = sawVersionPut || call.Method == http.MethodPut && strings.HasSuffix(call.Path, "/manifests/"+fixture.version)
		sawDigestWrite = sawDigestWrite || call.Method == http.MethodPut && strings.HasSuffix(call.Path, "/manifests/"+fixture.digest)
	}
	if !sawDigestWrite || !sawVersionPut {
		t.Fatalf("broker calls = %+v, want a manifest write by digest and the version tag", broker.recordedCalls())
	}
}

// Without a broker the workload path is unchanged: the seam is asked about the
// registry host itself and the content tag is still written. That is the
// operator laptop, pushing to oci.putnami.dev with the user's registry token.
func TestPublishWorkloadImageWithoutBrokerAsksTheRegistryHost(t *testing.T) {
	fixture := newDockerPublishFixture(t)
	t.Setenv(privateOCIRegistryURLEnv, "")

	var credentialHosts []string
	registrycred.ResolveToken = func(host string) (string, string) {
		credentialHosts = append(credentialHosts, host)
		return "", ""
	}
	status, _, err := Publish(fixture.ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("Publish() = (%q, %v), want OK", status, err)
	}
	if len(credentialHosts) != 1 || credentialHosts[0] != fixture.host {
		t.Fatalf("credential seam asked for %v, want the registry host %q", credentialHosts, fixture.host)
	}
	tags := registryTags(t, fixture.host, fixture.repository)
	if len(tags) != 2 {
		t.Fatalf("registry tags = %v, want the content tag and the version", tags)
	}
}

func TestResolveDockerRouteFailsClosed(t *testing.T) {
	layout := &pkgmeta.DockerManifest{Layout: "oci", Digest: "sha256:" + strings.Repeat("a", 64)}
	daemon := &pkgmeta.DockerManifest{}

	t.Run("empty registry consults no route", func(t *testing.T) {
		t.Setenv(privateOCIRegistryURLEnv, "http://127.0.0.1:8080/oci")
		route, err := resolveDockerRoute("", daemon)
		if err != nil || route.private() || route.credentialHost != "" {
			t.Fatalf("resolveDockerRoute(\"\") = (%+v, %v), want the zero route", route, err)
		}
	})
	t.Run("managed registry under a broker routes through it", func(t *testing.T) {
		t.Setenv(privateOCIRegistryURLEnv, "http://127.0.0.1:8080/oci")
		route, err := resolveDockerRoute(managedOCIRegistry+"/putnami", layout)
		if err != nil || !route.private() || route.credentialHost != "127.0.0.1:8080" {
			t.Fatalf("resolveDockerRoute(managed) = (%+v, %v), want the broker route", route, err)
		}
	})
	t.Run("daemon candidate cannot bypass the broker", func(t *testing.T) {
		t.Setenv(privateOCIRegistryURLEnv, "http://127.0.0.1:8080/oci")
		if _, err := resolveDockerRoute(managedOCIRegistry+"/putnami", daemon); err == nil || !strings.Contains(err.Error(), "OCI layout candidate") {
			t.Fatalf("resolveDockerRoute(daemon) error = %v, want a layout refusal", err)
		}
	})
	t.Run("generic registry under a broker is refused", func(t *testing.T) {
		t.Setenv(privateOCIRegistryURLEnv, "http://127.0.0.1:8080/oci")
		if _, err := resolveDockerRoute("registry.example.com/team", layout); err == nil || !strings.Contains(err.Error(), "managed OCI target") {
			t.Fatalf("resolveDockerRoute(generic) error = %v, want the managed-target refusal", err)
		}
	})
	t.Run("no broker keeps the direct path", func(t *testing.T) {
		t.Setenv(privateOCIRegistryURLEnv, "")
		route, err := resolveDockerRoute(managedOCIRegistry+"/putnami", daemon)
		if err != nil || route.private() || route.credentialHost != managedOCIRegistry {
			t.Fatalf("resolveDockerRoute(direct) = (%+v, %v), want the registry host", route, err)
		}
	})
}

func newManagedImagePublishFixture(t *testing.T) (*pctx.Context, *pkgmeta.DockerManifest) {
	t.Helper()
	image, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	contentHash := strings.Repeat("e", 64)
	manifest := &pkgmeta.DockerManifest{
		Image: "delivery-images-ci-runner", Tags: []string{"delivery-images-ci-runner:c-" + contentHash},
		Version: "c-" + contentHash, ContentHash: contentHash, Digest: digest.String(), Layout: "oci", Platform: "linux/amd64",
	}
	root := t.TempDir()
	projectPath := "delivery/images/ci-runner"
	dockerDir := pkgmeta.PackageOutputDir(root, projectPath, "docker")
	if _, err := oci.WriteLayout(filepath.Join(dockerDir, manifest.Layout), image); err != nil {
		t.Fatal(err)
	}
	return &pctx.Context{
		WorkspaceRoot: root,
		Workspace:     pctx.Workspace{Name: "team"},
		Project:       pctx.Project{Name: projectPath, Path: projectPath, Type: "image"},
		OutputPath:    filepath.Join(root, ".putnami", "out", projectPath, "publish"),
	}, manifest
}
