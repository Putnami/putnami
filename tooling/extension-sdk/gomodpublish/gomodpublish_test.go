package gomodpublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	spectest.Proves(t, "gomod/module-write-contract", "the-version-is-bound-to-the-stored-zip", "the-extension-sdk-publishes-and-reuses-the-same-bytes")
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
	spectest.Proves(t, "gomod/module-write-contract", "the-version-is-bound-to-the-stored-zip", "the-extension-sdk-reuses-a-conflict-only-when-allowed")
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
	spectest.Proves(t, "gomod/module-write-contract", "the-version-is-bound-to-the-stored-zip", "the-extension-sdk-refuses-another-upload-digest")
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
// is negative, with status and a body that echoes the bearer, or closes the
// connection without an answer when status is 0, and hands every other
// request to registry. It returns the bytes of every blob upload.
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
		if status == 0 {
			dropConnection(w)
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

// dropConnection closes the connection without a response.
func dropConnection(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("the test server cannot hijack a connection")
	}
	conn, _, err := hijacker.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// silentListener accepts connections and never writes to them, until t ends.
// It returns its address.
func silentListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var accepted []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range accepted {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()
	return listener.Addr().String()
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

	waits = recordWaits(t)
	server, sent = faultyRegistry(t, registry, 1, 0)
	if _, err := UploadZip(context.Background(), testClient(t), nil, server.URL, testToken, testModule, []byte("zip bytes")); err != nil {
		t.Fatalf("UploadZip = %v, want the second attempt after a dropped connection stored", err)
	}
	if len(*sent) != 2 || fmt.Sprint(*waits) != "[1s]" {
		t.Fatalf("%d uploads waited %v, want 2 uploads after a dropped connection", len(*sent), *waits)
	}
}

func TestUploadZipRetriesATLSHandshakeTimeout(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	waits := recordWaits(t)
	client := &http.Client{Transport: &http.Transport{TLSHandshakeTimeout: 50 * time.Millisecond}}
	_, err := UploadZip(context.Background(), client, nil, "https://"+silentListener(t), testToken, testModule, []byte("zip bytes"))
	if err == nil || !strings.HasPrefix(err.Error(), "blob upload failed after 4 attempts: ") || !strings.Contains(err.Error(), "TLS handshake timeout") {
		t.Fatalf("UploadZip = %v, want a TLS handshake timeout after 4 attempts", err)
	}
	if fmt.Sprint(*waits) != "[1s 2s 4s]" {
		t.Fatalf("waits = %v, want [1s 2s 4s]", *waits)
	}
}

func TestUploadZipDoesNotRetryAClientTimeout(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-blob-upload-retries-a-transient-failure")
	waits := recordWaits(t)
	release := make(chan struct{})
	var sent atomic.Int32
	zip := []byte("zip bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if sent.Add(1) == 1 {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"digest":%q}`, Digest(zip))
			return
		}
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	// The first upload is answered, so the timed upload reuses its connection.
	client := testClient(t)
	if _, err := UploadZip(context.Background(), client, nil, server.URL, testToken, testModule, zip); err != nil {
		t.Fatal(err)
	}
	timed := *client
	timed.Timeout = 100 * time.Millisecond
	if _, err := UploadZip(context.Background(), &timed, nil, server.URL, testToken, testModule, zip); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("UploadZip = %v, want the client timeout", err)
	}
	if sent.Load() != 2 || len(*waits) != 0 {
		t.Fatalf("%d uploads waited %v, want one timed upload and no wait", sent.Load(), *waits)
	}
}

// A version PUT whose answer is lost after the registry stored the version is
// sent again when a stored version is allowed, and its 409 is a reuse.
func TestPublishVersionRetriesOnlyWhenAStoredVersionIsAllowed(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-publish-retries-a-transient-failure")
	mod := []byte("module go.putnami.dev/mod\n")
	for _, allowExisting := range []bool{true, false} {
		t.Run(fmt.Sprint(allowExisting), func(t *testing.T) {
			waits := recordWaits(t)
			registry := newFakeRegistry(t)
			var puts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || puts.Add(1) > 1 {
					registry.serve(w, r)
					return
				}
				registry.serve(httptest.NewRecorder(), r)
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(server.Close)
			reused, err := PublishVersion(context.Background(), testClient(t), nil, server.URL, testToken, testModule, testVersion, mod, otherDigest, allowExisting)
			if allowExisting {
				if err != nil || !reused || puts.Load() != 2 || fmt.Sprint(*waits) != "[1s]" {
					t.Fatalf("PublishVersion = %t, %v after %d PUTs and waits %v, want a reuse after 2 PUTs", reused, err, puts.Load(), *waits)
				}
				return
			}
			if err == nil || err.Error() != "publish returned 502" || puts.Load() != 1 || len(*waits) != 0 {
				t.Fatalf("PublishVersion = %v after %d PUTs and waits %v, want 502 after one PUT", err, puts.Load(), *waits)
			}
		})
	}
}

func TestVerifyRetriesATransientDownloadFailure(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "a-publish-retries-a-transient-failure")
	zip := []byte("zip bytes")
	mod := []byte("module go.putnami.dev/mod\n")
	for name, verify := range map[string]func(ctx context.Context, client *http.Client, registryURL string) error{
		"zip": func(ctx context.Context, client *http.Client, registryURL string) error {
			return VerifyZip(ctx, client, registryURL, testToken, testModule, testVersion, Digest(zip))
		},
		"mod": func(ctx context.Context, client *http.Client, registryURL string) error {
			return VerifyMod(ctx, client, registryURL, testToken, testModule, testVersion, mod)
		},
	} {
		t.Run(name, func(t *testing.T) {
			waits := recordWaits(t)
			var gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if gets.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if strings.HasSuffix(r.URL.Path, ".zip") {
					_, _ = w.Write(zip)
					return
				}
				_, _ = w.Write(mod)
			}))
			t.Cleanup(server.Close)
			if err := verify(context.Background(), testClient(t), server.URL); err != nil {
				t.Fatalf("verify = %v, want the second download verified", err)
			}
			if gets.Load() != 2 || fmt.Sprint(*waits) != "[1s]" {
				t.Fatalf("%d downloads waited %v, want 2 downloads and one wait of 1s", gets.Load(), *waits)
			}

			waits = recordWaits(t)
			failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(failing.Close)
			if err := verify(context.Background(), testClient(t), failing.URL); err == nil || !strings.Contains(err.Error(), "download returned 502 after 4 attempts") {
				t.Fatalf("verify = %v, want 502 after 4 attempts", err)
			}
			if len(*waits) != 3 {
				t.Fatalf("waits = %v, want 3", *waits)
			}

			waits = recordWaits(t)
			var cut atomic.Int32
			truncating := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served := mod
				if strings.HasSuffix(r.URL.Path, ".zip") {
					served = zip
				}
				if cut.Add(1) > 1 {
					_, _ = w.Write(served)
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(served)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(served[:len(served)/2])
				w.(http.Flusher).Flush()
				dropConnection(w)
			}))
			t.Cleanup(truncating.Close)
			if err := verify(context.Background(), testClient(t), truncating.URL); err != nil {
				t.Fatalf("verify = %v, want the second download verified after a body cut short", err)
			}
			if cut.Load() != 2 || fmt.Sprint(*waits) != "[1s]" {
				t.Fatalf("%d downloads waited %v, want 2 downloads and one wait of 1s", cut.Load(), *waits)
			}

			waits = recordWaits(t)
			var mismatched atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mismatched.Add(1)
				_, _ = w.Write([]byte("other bytes"))
			}))
			t.Cleanup(other.Close)
			if err := verify(context.Background(), testClient(t), other.URL); err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("verify = %v, want a mismatch", err)
			}
			if mismatched.Load() != 1 || len(*waits) != 0 {
				t.Fatalf("%d downloads waited %v, want one download for a mismatch", mismatched.Load(), *waits)
			}
		})
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
