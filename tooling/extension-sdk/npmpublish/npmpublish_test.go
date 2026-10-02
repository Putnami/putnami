package npmpublish

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const testToken = "target-token"

func testPayload() Payload {
	return Payload{
		Name:        "@test/pkg",
		Versions:    map[string]json.RawMessage{"1.2.3-r42": json.RawMessage(`{"name":"@test/pkg","version":"1.2.3-r42"}`)},
		Attachments: map[string]Attachment{"pkg-1.2.3-r42.tgz": {ContentType: "application/octet-stream", Data: "dGd6", Length: 3}},
		DistTags:    map[string]string{},
	}
}

func testClient(t *testing.T) *http.Client {
	t.Helper()
	client, err := NewHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// fakeRegistry stores what a PUT attaches and serves it back at the npm
// tarball path. Every request must carry the bearer.
type fakeRegistry struct {
	mu       sync.Mutex
	server   *httptest.Server
	tarballs map[string][]byte
	puts     int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	registry := &fakeRegistry{tarballs: map[string][]byte{}}
	registry.server = httptest.NewServer(http.HandlerFunc(registry.serve))
	t.Cleanup(registry.server.Close)
	return registry
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch req.Method {
	case http.MethodPut:
		var payload Payload
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.puts++
		for name, attachment := range payload.Attachments {
			data, err := base64.StdEncoding.DecodeString(attachment.Data)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.tarballs[payload.Name+"/-/"+name] = data
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		data, ok := r.tarballs[strings.TrimPrefix(req.URL.Path, "/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(data)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func testArtifact(tarball string) Artifact {
	return Artifact{
		Name:     "@test/pkg",
		Version:  "1.2.3-r42",
		Manifest: []byte(`{"name":"@test/pkg","version":"1.2.3-r42","publishConfig":{"provenance":true}}`),
		Tarball:  []byte(tarball),
	}
}

func TestNPMUploadIsIdempotentByDigest(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "an-upload-reuses-a-version-at-the-same-digest")
	registry := newFakeRegistry(t)
	client := testClient(t)
	ctx := context.Background()

	reused, err := Publish(ctx, client, registry.server.URL, testToken, testArtifact("tarball bytes"))
	if err != nil || reused {
		t.Fatalf("first publish reused=%t err=%v, want a fresh upload", reused, err)
	}
	if registry.puts != 1 {
		t.Fatalf("first publish sent %d PUTs, want 1", registry.puts)
	}

	reused, err = Publish(ctx, client, registry.server.URL, testToken, testArtifact("tarball bytes"))
	if err != nil || !reused {
		t.Fatalf("second publish reused=%t err=%v, want reuse of the same digest", reused, err)
	}
	if registry.puts != 1 {
		t.Fatalf("a re-publish at the same digest sent a PUT: %d PUTs", registry.puts)
	}

	reused, err = Publish(ctx, client, registry.server.URL, testToken, testArtifact("other bytes!!"))
	if err == nil || reused || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("publish of other bytes at the same version reused=%t err=%v, want a digest mismatch", reused, err)
	}
	if registry.puts != 1 {
		t.Fatalf("a digest mismatch sent a PUT: %d PUTs", registry.puts)
	}
}

func TestPublishVerifiesTheUploadedBytes(t *testing.T) {
	var puts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, err := Publish(context.Background(), testClient(t), server.URL, testToken, testArtifact("tarball bytes"))
	if err == nil || !strings.Contains(err.Error(), "404 Not Found") || puts != 1 {
		t.Fatalf("publish to a registry that serves nothing back err=%v puts=%d", err, puts)
	}
}

func TestPutUsesAuthenticatedTaglessPut(t *testing.T) {
	var method, escapedPath, authorization, contentType string
	var received Payload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		escapedPath = r.URL.EscapedPath()
		authorization = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode managed npm request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	payload := testPayload()
	if err := Put(context.Background(), testClient(t), server.URL, testToken, payload); err != nil {
		t.Fatalf("put: %v", err)
	}
	if method != http.MethodPut || escapedPath != "/@test%2Fpkg" {
		t.Fatalf("endpoint = %s %s, want PUT /@test%%2Fpkg", method, escapedPath)
	}
	if authorization != "Bearer "+testToken || contentType != "application/json" {
		t.Fatalf("headers authorization=%q content-type=%q", authorization, contentType)
	}
	if received.Name != payload.Name || len(received.DistTags) != 0 || len(received.Versions) != 1 || len(received.Attachments) != 1 {
		t.Fatalf("wire payload = %#v, want exact tagless payload", received)
	}
}

func TestPutIgnoresAmbientProxy(t *testing.T) {
	var proxyRequests int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyRequests++
		http.Error(w, "reflected "+testToken, http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	err := Put(context.Background(), testClient(t), "http://registry.example.invalid", testToken, testPayload())
	if err == nil {
		t.Fatal("put error = nil, want unreachable target")
	}
	if proxyRequests != 0 {
		t.Fatalf("ambient proxy observed %d managed requests", proxyRequests)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked token: %v", err)
	}
}

// The registry's error body names the cause, so the error carries a bounded,
// single-line excerpt of it with every bearer credential redacted.
func TestRegistryErrorsShowBoundedRedactedBody(t *testing.T) {
	cause := `{"code":"distribution.local_access.stale","error":"package ownership data on this registry is stale; retry shortly"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(cause + "\x1b[31m\r\n echoed " + r.Header.Get("Authorization") +
			" raw " + testToken + " and Bearer other-secret " + strings.Repeat("x", 1000)))
	}))
	defer server.Close()
	digest := Digest([]byte("x"))
	client := testClient(t)
	_, probeErr := Probe(context.Background(), client, server.URL, testToken, "@test/pkg", "1.2.3-r42", digest, 1)
	for name, err := range map[string]error{
		"put":   Put(context.Background(), client, server.URL, testToken, testPayload()),
		"probe": probeErr,
	} {
		if err == nil {
			t.Fatalf("%s error = nil, want the registry 503", name)
		}
		message := err.Error()
		if !strings.Contains(message, "503 Service Unavailable: "+cause) {
			t.Fatalf("%s error = %q, want the status and the registry's cause", name, message)
		}
		if strings.Contains(message, testToken) || strings.Contains(message, "other-secret") {
			t.Fatalf("%s error leaked a bearer credential: %q", name, message)
		}
		if !strings.Contains(message, "Bearer [redacted]") || strings.ContainsAny(message, "\x1b\r\n") {
			t.Fatalf("%s error = %q, want redacted bearers and no control characters", name, message)
		}
		_, excerpt, _ := strings.Cut(message, "Service Unavailable: ")
		if got := len([]rune(excerpt)); got > ErrorExcerptLimit+len("...") {
			t.Fatalf("%s excerpt has %d characters, want at most %d", name, got, ErrorExcerptLimit)
		}
	}
}

func TestProbeUsesAuthenticatedBoundedTarballRead(t *testing.T) {
	artifact := []byte("immutable npm tarball bytes")
	digest := Digest(artifact)
	var gotPath, gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write(artifact)
	}))
	defer server.Close()

	client := testClient(t)
	found, err := Probe(context.Background(), client, server.URL+"/npm", testToken, "@test/pkg", "1.2.3-r42", digest, int64(len(artifact)))
	if err != nil || !found {
		t.Fatalf("probe found=%t err=%v", found, err)
	}
	if gotPath != "/npm/@test/pkg/-/pkg-1.2.3-r42.tgz" || gotAuthorization != "Bearer "+testToken {
		t.Fatalf("tarball request path=%q authorization=%q", gotPath, gotAuthorization)
	}
	found, err = Probe(context.Background(), client, server.URL+"/npm", testToken, "@test/pkg", "1.2.3-r42", digest, int64(len(artifact)-1))
	if found || err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("overlong artifact found=%t err=%v", found, err)
	}
}

// A redirect is refused whatever client the caller passes, so the bearer
// reaches the registry it was given and no other host.
func TestProbeRefusesRedirectWithAnyClient(t *testing.T) {
	var redirected int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected++
		_, _ = w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	digest := Digest([]byte("x"))
	for name, client := range map[string]*http.Client{"managed": testClient(t), "default": redirect.Client()} {
		found, err := Probe(context.Background(), client, redirect.URL, testToken, "@test/pkg", "1.2.3-r42", digest, 1)
		if found || err == nil || !strings.Contains(err.Error(), "302") || strings.Contains(err.Error(), testToken) {
			t.Fatalf("%s client redirect probe found=%t err=%v", name, found, err)
		}
	}
	if redirected != 0 {
		t.Fatalf("verification followed %d redirects", redirected)
	}
	if _, err := Probe(context.Background(), nil, redirect.URL, testToken, "@test/pkg", "1.2.3-r42", digest, 1); err == nil {
		t.Fatal("probe without a client succeeded")
	}
}

func TestNewHTTPClientDisablesProxyAndRedirects(t *testing.T) {
	client := testClient(t)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v, want direct transport with Proxy=nil", client.Transport)
	}
	request, err := http.NewRequest(http.MethodGet, "https://other.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.CheckRedirect(request, nil); !errors.Is(got, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy = %v, want http.ErrUseLastResponse", got)
	}
}

func TestBuildPayloadStripsPublishConfig(t *testing.T) {
	payload, err := BuildPayload(testArtifact("tgz"))
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	var wireManifest map[string]json.RawMessage
	if err := json.Unmarshal(payload.Versions["1.2.3-r42"], &wireManifest); err != nil {
		t.Fatalf("decode wire manifest: %v", err)
	}
	if _, present := wireManifest["publishConfig"]; present {
		t.Fatalf("wire manifest retained publishConfig: %s", payload.Versions["1.2.3-r42"])
	}
	attachment, ok := payload.Attachments["pkg-1.2.3-r42.tgz"]
	if !ok || attachment.Length != 3 || attachment.Data != base64.StdEncoding.EncodeToString([]byte("tgz")) || len(payload.DistTags) != 0 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestPublishRefusesAManifestThatNamesAnotherVersion(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	for name, manifest := range map[string]string{
		"other name":    `{"name":"@test/other","version":"1.2.3-r42"}`,
		"other version": `{"name":"@test/pkg","version":"9.9.9"}`,
		"no version":    `{"name":"@test/pkg"}`,
		"not json":      `{`,
	} {
		artifact := testArtifact("tgz")
		artifact.Manifest = []byte(manifest)
		if _, err := Publish(context.Background(), testClient(t), server.URL, testToken, artifact); err == nil {
			t.Fatalf("%s: published a manifest that does not name the artifact", name)
		}
	}
	if requests != 0 {
		t.Fatalf("a refused artifact sent %d requests", requests)
	}
}

func TestTarballURL(t *testing.T) {
	for _, tc := range []struct{ registry, name, want string }{
		{"https://npm.example/", "@scope/pkg", "https://npm.example/@scope/pkg/-/pkg-1.0.0.tgz"},
		{"https://npm.example/base", "pkg", "https://npm.example/base/pkg/-/pkg-1.0.0.tgz"},
	} {
		got, err := TarballURL(tc.registry, tc.name, "1.0.0")
		if err != nil || got != tc.want {
			t.Fatalf("TarballURL(%q, %q) = %q, %v; want %q", tc.registry, tc.name, got, err, tc.want)
		}
	}
	for _, name := range []string{"", "@scope", "@/pkg", "@scope/pkg/extra", "a/b"} {
		if _, err := TarballURL("https://npm.example", name, "1.0.0"); err == nil {
			t.Fatalf("TarballURL accepted invalid coordinate %q", name)
		}
	}
	if got := fmt.Sprint(ErrorExcerpt(strings.NewReader("   "), testToken)); got != "" {
		t.Fatalf("empty body excerpt = %q", got)
	}
}
