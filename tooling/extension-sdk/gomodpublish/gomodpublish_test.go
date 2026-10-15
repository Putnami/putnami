package gomodpublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	gomod "go.putnami.dev/protocol/gomod"
)

const (
	testToken   = "target-token"
	testModule  = "go.putnami.dev/mod"
	testVersion = "v1.2.3"
)

// otherDigest is a well-formed digest of no uploaded bytes.
var otherDigest = "sha256:" + strings.Repeat("a", 64)

type recordedReport struct {
	lines []string
}

func (r *recordedReport) Diagnostic(severity, message, _ string, _ int) {
	r.lines = append(r.lines, severity+": "+message)
}

func (r *recordedReport) Log(level, message string) {
	r.lines = append(r.lines, level+": "+message)
}

func (r *recordedReport) String() string { return strings.Join(r.lines, "\n") }

func testClient(t *testing.T) *http.Client {
	t.Helper()
	client, err := NewHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// fakeRegistry implements the gomod-write blob upload and version PUT, and
// serves each published version's zip and go.mod at the standard Go proxy
// paths. A version is immutable: a second PUT answers 409.
type fakeRegistry struct {
	mu       sync.Mutex
	server   *httptest.Server
	blobs    map[string][]byte
	zips     map[string][]byte
	mods     map[string][]byte
	uploads  int
	versions int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	registry := &fakeRegistry{blobs: map[string][]byte{}, zips: map[string][]byte{}, mods: map[string][]byte{}}
	registry.server = httptest.NewServer(http.HandlerFunc(registry.serve))
	t.Cleanup(registry.server.Close)
	return registry
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	versionPath := gomod.VersionPath(testModule, testVersion)
	switch {
	case req.Method == http.MethodPost && req.URL.Path == gomod.BlobUploadPath(testModule):
		data, _ := io.ReadAll(req.Body)
		r.uploads++
		digest := Digest(data)
		r.blobs[digest] = data
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"digest":%q}`, digest)
	case req.Method == http.MethodPut && req.URL.Path == versionPath:
		var body gomod.PublishVersionRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, exists := r.zips[versionPath]; exists {
			w.WriteHeader(http.StatusConflict)
			return
		}
		r.versions++
		r.zips[versionPath] = r.blobs[body.ZipDigest]
		r.mods[versionPath] = []byte(body.GoMod)
		w.WriteHeader(http.StatusCreated)
	case req.Method == http.MethodGet && req.URL.Path == versionPath+".zip":
		_, _ = w.Write(r.zips[versionPath])
	case req.Method == http.MethodGet && req.URL.Path == versionPath+".mod":
		_, _ = w.Write(r.mods[versionPath])
	default:
		http.NotFound(w, req)
	}
}

func testModuleVersion(zip string) Module {
	return Module{Path: testModule, Version: testVersion, Zip: []byte(zip), Mod: []byte("module go.putnami.dev/mod\n\ngo 1.25\n")}
}

func TestGoModuleUploadReusesAnExistingVersionAtTheSameDigest(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "an-upload-reuses-a-version-at-the-same-digest")
	registry := newFakeRegistry(t)
	client := testClient(t)
	ctx := context.Background()

	digest, reused, err := Publish(ctx, client, nil, registry.server.URL, testToken, testModuleVersion("zip bytes"))
	if err != nil || reused || digest != Digest([]byte("zip bytes")) {
		t.Fatalf("first publish digest=%q reused=%t err=%v, want a fresh version at the zip digest", digest, reused, err)
	}

	report := &recordedReport{}
	digest, reused, err = Publish(ctx, client, report, registry.server.URL, testToken, testModuleVersion("zip bytes"))
	if err != nil || !reused || digest != Digest([]byte("zip bytes")) {
		t.Fatalf("second publish digest=%q reused=%t err=%v, want reuse at the same digest", digest, reused, err)
	}
	if registry.versions != 1 {
		t.Fatalf("registry recorded %d versions, want 1", registry.versions)
	}
	if !strings.Contains(report.String(), "already exists; verifying immutable registry bytes") {
		t.Fatalf("reuse report = %q", report)
	}

	_, reused, err = Publish(ctx, client, nil, registry.server.URL, testToken, testModuleVersion("other zip bytes"))
	if err == nil || reused || !strings.Contains(err.Error(), "does not match uploaded digest") {
		t.Fatalf("publish of other bytes at the same version reused=%t err=%v, want a digest mismatch", reused, err)
	}

	other := testModuleVersion("zip bytes")
	other.Mod = []byte("module go.putnami.dev/mod\n\ngo 1.26\n")
	_, reused, err = Publish(ctx, client, nil, registry.server.URL, testToken, other)
	if err == nil || reused || !strings.Contains(err.Error(), "does not match submitted bytes") {
		t.Fatalf("publish of another go.mod at the same version reused=%t err=%v, want a go.mod mismatch", reused, err)
	}
}

func TestPublishVersionRefusesAConflictUnlessAllowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	mod := []byte("module go.putnami.dev/mod\n")
	if _, err := PublishVersion(context.Background(), server.Client(), nil, server.URL, testToken, testModule, testVersion, mod, otherDigest, false); err == nil || !strings.Contains(err.Error(), "publish returned 409") {
		t.Fatalf("conflict without allowExisting err=%v", err)
	}
	reused, err := PublishVersion(context.Background(), server.Client(), nil, server.URL, testToken, testModule, testVersion, mod, otherDigest, true)
	if err != nil || !reused {
		t.Fatalf("conflict with allowExisting reused=%t err=%v", reused, err)
	}
}

func TestUploadZipRefusesRegistryDigestMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"digest":"sha256:%s"}`, strings.Repeat("f", 64))
	}))
	defer server.Close()
	if _, err := UploadZip(context.Background(), server.Client(), nil, server.URL, testToken, testModule, []byte("uploaded")); err == nil || !strings.Contains(err.Error(), "does not match uploaded zip digest") {
		t.Fatalf("upload mismatch error = %v", err)
	}
}

// recordWaits makes retryPolicy record each wait instead of sleeping, with no
// jitter, until t ends.
func recordWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	saved := retryPolicy
	t.Cleanup(func() { retryPolicy = saved })
	waits := &[]time.Duration{}
	retryPolicy.Jitter = func(time.Duration) time.Duration { return 0 }
	retryPolicy.Wait = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
	return waits
}

// faultyRegistry answers the first count blob uploads, or every one when count
// is negative, with status and a body that echoes the bearer, and hands every
// other request to registry. It returns the bytes of every blob upload.
func faultyRegistry(t *testing.T, registry *fakeRegistry, count, status int, header ...string) (*httptest.Server, *[][]byte) {
	t.Helper()
	var mu sync.Mutex
	sent := &[][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != gomod.BlobUploadPath(testModule) {
			registry.serve(w, r)
			return
		}
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		*sent = append(*sent, data)
		fail := count < 0 || len(*sent) <= count
		mu.Unlock()
		if !fail {
			r.Body = io.NopCloser(bytes.NewReader(data))
			registry.serve(w, r)
			return
		}
		for index := 0; index+1 < len(header); index += 2 {
			w.Header().Set(header[index], header[index+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "Bearer "+testToken)
	}))
	t.Cleanup(server.Close)
	return server, sent
}

func TestUploadZipRetriesATransientFailure(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	waits := recordWaits(t)
	registry := newFakeRegistry(t)
	server, sent := faultyRegistry(t, registry, 1, http.StatusBadGateway)
	digest, err := UploadZip(context.Background(), testClient(t), nil, server.URL, testToken, testModule, []byte("zip bytes"))
	if err != nil || digest != Digest([]byte("zip bytes")) {
		t.Fatalf("UploadZip = %q, %v; want the zip digest from the second attempt", digest, err)
	}
	if len(*sent) != 2 || !bytes.Equal((*sent)[0], (*sent)[1]) || registry.uploads != 1 || fmt.Sprint(*waits) != "[1s]" {
		t.Fatalf("%d uploads (%d stored) waited %v, want the zip twice and one wait of 1s", len(*sent), registry.uploads, *waits)
	}

	waits = recordWaits(t)
	server, sent = faultyRegistry(t, registry, 1, http.StatusServiceUnavailable, "Retry-After", "5")
	if _, err := UploadZip(context.Background(), testClient(t), nil, server.URL, testToken, testModule, []byte("zip bytes")); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 2 || fmt.Sprint(*waits) != "[5s]" {
		t.Fatalf("%d uploads waited %v, want 2 uploads that waited the Retry-After 5s", len(*sent), *waits)
	}
}

func TestUploadZipFailsAfterTheAttemptBoundOrAtOnce(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	for status, tc := range map[int]struct {
		attempts int
		want     string
	}{
		http.StatusBadGateway:   {4, "blob upload returned 502 after 4 attempts"},
		http.StatusBadRequest:   {1, "blob upload returned 400"},
		http.StatusUnauthorized: {1, "blob upload returned 401"},
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			waits := recordWaits(t)
			server, sent := faultyRegistry(t, newFakeRegistry(t), -1, status)
			report := &recordedReport{}
			_, err := UploadZip(context.Background(), testClient(t), report, server.URL, testToken, testModule, []byte("zip bytes"))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("UploadZip = %v, want %q", err, tc.want)
			}
			if len(*sent) != tc.attempts || len(*waits) != tc.attempts-1 {
				t.Fatalf("%d uploads waited %v, want %d uploads", len(*sent), *waits, tc.attempts)
			}
			if strings.Contains(report.String(), testToken) {
				t.Fatalf("report exposed the response body: %s", report)
			}
		})
	}
}

func TestUploadZipStopsWhenTheContextEnds(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	recordWaits(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retryPolicy.Wait = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	server, sent := faultyRegistry(t, newFakeRegistry(t), -1, http.StatusServiceUnavailable)
	if _, err := UploadZip(ctx, testClient(t), nil, server.URL, testToken, testModule, []byte("zip bytes")); !errors.Is(err, context.Canceled) {
		t.Fatalf("UploadZip = %v, want context.Canceled", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("%d uploads were sent, want 1", len(*sent))
	}
}

func TestRegistryResponsesAreBoundedAndNeverReported(t *testing.T) {
	t.Run("blob error body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "sensitive-blob-detail")
		}))
		defer server.Close()
		report := &recordedReport{}
		_, err := UploadZip(context.Background(), server.Client(), report, server.URL, testToken, testModule, []byte("zip"))
		if err == nil || !strings.Contains(err.Error(), "blob upload returned 401") {
			t.Fatalf("blob upload error = %v", err)
		}
		if strings.Contains(report.String(), "sensitive-blob-detail") {
			t.Fatalf("report exposed the response body: %s", report)
		}
	})
	t.Run("blob success body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(bytes.Repeat([]byte("x"), MaxResponseBytes+1))
		}))
		defer server.Close()
		if _, err := UploadZip(context.Background(), server.Client(), nil, server.URL, testToken, testModule, []byte("zip")); err == nil || !strings.Contains(err.Error(), "response exceeds") {
			t.Fatalf("oversized blob response error = %v", err)
		}
	})
	t.Run("version error body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "sensitive-version-detail")
		}))
		defer server.Close()
		report := &recordedReport{}
		_, err := PublishVersion(context.Background(), server.Client(), report, server.URL, testToken, testModule, testVersion, []byte("module go.putnami.dev/mod\n"), otherDigest, true)
		if err == nil || !strings.Contains(err.Error(), "publish returned 403") {
			t.Fatalf("publish error = %v", err)
		}
		if strings.Contains(report.String(), "sensitive-version-detail") {
			t.Fatalf("report exposed the response body: %s", report)
		}
	})
}

func TestVerifyRefusesOtherServedBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("module go.putnami.dev/other\n"))
	}))
	defer server.Close()
	if err := VerifyZip(context.Background(), server.Client(), server.URL, "", testModule, testVersion, Digest([]byte("uploaded bytes"))); err == nil || !strings.Contains(err.Error(), "does not match uploaded digest") {
		t.Fatalf("zip mismatch error = %v", err)
	}
	if err := VerifyMod(context.Background(), server.Client(), server.URL, "", testModule, testVersion, []byte("module go.putnami.dev/mod\n")); err == nil || !strings.Contains(err.Error(), "does not match submitted bytes") {
		t.Fatalf("go.mod mismatch error = %v", err)
	}
}

// A redirect is refused whatever client the caller passes, so the bearer
// reaches the registry it was given and no other host.
func TestVerifyRefusesRedirectWithAnyClient(t *testing.T) {
	var redirected int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected++
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirect.Close()
	for name, client := range map[string]*http.Client{"managed": testClient(t), "default": redirect.Client()} {
		err := VerifyZip(context.Background(), client, redirect.URL, testToken, testModule, testVersion, otherDigest)
		if err == nil || !strings.Contains(err.Error(), "returned 302") {
			t.Fatalf("%s client redirect verification err=%v", name, err)
		}
	}
	if redirected != 0 {
		t.Fatalf("verification followed %d redirects", redirected)
	}
	if err := VerifyZip(context.Background(), nil, redirect.URL, testToken, testModule, testVersion, otherDigest); err == nil {
		t.Fatal("verification without a client succeeded")
	}
}

func TestValidateRegistryURLAllowsHTTPOnlyForLoopback(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:8080/base/",
		"http://registry.localhost:8080/base/",
		"http://127.0.0.1:8080/base/",
		"http://[::1]:8080/base/",
		"https://registry.example/base/",
	} {
		if _, err := ValidateRegistryURL(raw); err != nil {
			t.Errorf("ValidateRegistryURL(%q): %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://registry.example/base/",
		"https://user:secret@registry.example",
		"https://registry.example/?token=secret",
		"ftp://registry.example",
		"registry.example",
	} {
		_, err := ValidateRegistryURL(raw)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("ValidateRegistryURL(%q) err=%v, want a refusal without the credential", raw, err)
		}
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
