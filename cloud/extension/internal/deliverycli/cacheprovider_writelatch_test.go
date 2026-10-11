package deliverycli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// writeRefusingCacheServer is a cache server that serves reads and refuses
// every task-cache write with 403, the answer a credential without write
// authority gets. It counts the write requests that reached it. extra names
// capabilities it advertises beside the batched write path.
func writeRefusingCacheServer(t *testing.T, extra ...string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var writes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.NegotiatePath:
			_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
		case cache.CapabilitiesPath:
			_ = json.NewEncoder(w).Encode(cache.CapabilitiesResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Capabilities:    append([]string{cache.CapabilityFindMissing, cache.CapabilityCommitBatch}, extra...),
			})
		case cache.StorePath, cache.CommitPath, cache.FindMissingPath, cache.CommitBatchPath,
			cache.UploadBatchPath, cache.RunMarkerPublishPath:
			writes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(cache.ErrorResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Code:            "forbidden",
				Message:         "this credential reads the cache; only CI writes",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

func latchTestUpload(key string) *cache.UploadParams {
	return &cache.UploadParams{
		Key:      key,
		Result:   &cache.ActionResult{Status: "success"},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{{Path: "out", Digest: cache.DigestOf([]byte("o")), Size: 1}}},
	}
}

// A developer credential reads the shared cache and writes nothing: the first
// refused write closes the write path, the run sends no further write, and the
// summary says so plainly instead of warning that the cache is cold.
func TestDeveloperWriteRefusalLatchesReadOnlyAndSaysSoPlainly(t *testing.T) {
	enableCacheWritesForTest(t)
	srv, writes := writeRefusingCacheServer(t)
	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "developer-token")
	t.Setenv(SessionReporterIngestURLEnv, "") // a laptop: no hosted launcher

	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir()})

	first := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload},
		latchTestUpload(strings.Repeat("a", cache.KeyLength)))
	if result, _ := cache.ParseAndValidateUploadResult(first.Payload); result == nil || !result.Accepted {
		t.Fatalf("first upload = %+v; the write path is open until the server refuses a write", first)
	}
	summary := s.summary(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpSummary})
	if !summary.OK {
		t.Fatalf("summary = %+v", summary)
	}
	if !s.writesLatched() {
		t.Fatal("a 403 on a write did not latch the write path")
	}

	// Latched: the next upload and marker write are answered locally.
	second := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 4, Op: cache.OpUpload},
		latchTestUpload(strings.Repeat("b", cache.KeyLength)))
	if result, _ := cache.ParseAndValidateUploadResult(second.Payload); result == nil || result.Accepted {
		t.Fatalf("upload after the latch = %+v, want Accepted=false", second)
	}
	marker := s.markerWrite(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 5, Op: cache.OpMarkerWrite},
		&cache.MarkerWriteParams{Workspace: "w", Branch: "main", Commands: []string{"build"}, SHA: strings.Repeat("c", 40)})
	if result, _ := cache.ParseAndValidateMarkerWriteResult(marker.Payload); result == nil || result.Published {
		t.Fatalf("marker write after the latch = %+v, want Published=false", marker)
	}
	s.bg.Wait()
	if got := writes.Load(); got != 1 {
		t.Fatalf("write requests = %d, want exactly the one refused write", got)
	}

	log := logw.String()
	if !strings.Contains(log, "this credential reads the shared cache and writes nothing to it: only hosted CI runs write") {
		t.Fatalf("the summary must say plainly that this credential only reads, got %q", log)
	}
	if strings.Contains(log, "WARNING") || strings.Contains(log, "next run starts cold") {
		t.Fatalf("an expected developer refusal must not raise the cold-cache warning, got %q", log)
	}
	if strings.Contains(log, "developer-token") {
		t.Fatalf("the provider log leaked the cache bearer: %q", log)
	}
}

// A server that packs small blobs takes them in upload-batch, which carries
// their bytes. The session's first write is still a find-missing, which carries
// none, so a developer credential is refused there: one refused request, the
// write path closed, and the plain read-only line instead of the cold-cache
// warning. TestAReadOnlyRunSendsNoBlobBytes counts the bytes.
func TestDeveloperRefusalOnAPackingServerLatchesReadOnly(t *testing.T) {
	enableCacheWritesForTest(t)
	srv, writes := writeRefusingCacheServer(t, cache.CapabilityUploadBatch)
	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "developer-token")
	t.Setenv(SessionReporterIngestURLEnv, "") // a laptop: no hosted launcher

	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()
	exchangeDir := t.TempDir()
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: exchangeDir})

	// The blob is staged, so nothing but the order of the writes keeps its
	// bytes off the wire.
	mustUpload(t, s, strings.Repeat("a", cache.KeyLength), stageBlob(t, exchangeDir, "o"))
	if stats := mustSummary(t, s); stats.UploadedCount != 0 {
		t.Fatalf("summary counted %d entries after a refused window", stats.UploadedCount)
	}
	if !s.writesLatched() {
		t.Fatal("a 403 on the first write did not latch the write path")
	}
	second := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 4, Op: cache.OpUpload},
		latchTestUpload(strings.Repeat("b", cache.KeyLength)))
	if result, _ := cache.ParseAndValidateUploadResult(second.Payload); result == nil || result.Accepted {
		t.Fatalf("upload after the latch = %+v, want Accepted=false", second)
	}
	s.bg.Wait()
	if got := writes.Load(); got != 1 {
		t.Fatalf("write requests = %d, want exactly the one refused find-missing", got)
	}

	log := logw.String()
	if !strings.Contains(log, "op=find-missing") {
		t.Fatalf("the refused exchange must be named, got %q", log)
	}
	if !strings.Contains(log, "this credential reads the shared cache and writes nothing to it: only hosted CI runs write") {
		t.Fatalf("the summary must say plainly that this credential only reads, got %q", log)
	}
	if strings.Contains(log, "WARNING") || strings.Contains(log, "next run starts cold") {
		t.Fatalf("an expected developer refusal must not raise the cold-cache warning, got %q", log)
	}
}

// A hosted run whose writes are refused is a misconfiguration, not the expected
// developer refusal: it still latches, and it keeps the prominent warning.
// The run's own cache token from authenticate is what marks it hosted.
func TestHostedWriteRefusalKeepsTheColdCacheWarning(t *testing.T) {
	enableCacheWritesForTest(t)
	srv, writes := writeRefusingCacheServer(t)
	delivery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"protocolVersion": 1, "token": testRunCacheToken, "expiresAt": "2026-10-10T06:00:00Z"})
	}))
	t.Cleanup(delivery.Close)
	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", testConfiguredToken)
	t.Setenv(SessionReporterIngestURLEnv, delivery.URL+"/v1/delivery/records")
	stubCacheProviderGuards(t, nil)

	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
		Capabilities:    []string{cache.CapabilityRunCredential},
	})
	auth := s.authenticate(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolMinVersion, ID: 2, Op: cache.OpAuthenticate},
		&cache.AuthenticateParams{Credential: testRunCredential})
	if !auth.OK {
		t.Fatalf("authenticate = %+v", auth.Error)
	}

	s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpUpload},
		latchTestUpload(strings.Repeat("a", cache.KeyLength)))
	s.summary(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 4, Op: cache.OpSummary})
	if !s.writesLatched() {
		t.Fatal("a 403 on a hosted run's write did not latch the write path")
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("write requests = %d, want exactly the one refused write", got)
	}

	log := logw.String()
	if !strings.Contains(log, "source="+string(remotecache.TokenClassRun)) {
		t.Fatalf("the refusal must name the run's cache token as its source, got %q", log)
	}
	if !strings.Contains(log, "WARNING: every remote-cache write was refused") {
		t.Fatalf("a hosted run whose writes are refused must keep the prominent warning, got %q", log)
	}
	if strings.Contains(log, "this credential reads the shared cache") {
		t.Fatalf("a hosted refusal was reported as the expected developer one: %q", log)
	}
}

// Only a 403 on a write latches. A 401 can be a transient auth failure, and a
// refused read says nothing about write authority.
func TestOnlyAForbiddenWriteLatchesTheWritePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		op     string
		status int
		latch  bool
	}{
		{name: "403 on store", op: "store", status: http.StatusForbidden, latch: true},
		{name: "403 on commit-batch", op: "commit-batch", status: http.StatusForbidden, latch: true},
		{name: "403 on a marker publish", op: "run-marker publish", status: http.StatusForbidden, latch: true},
		{name: "401 on find-missing", op: "find-missing", status: http.StatusUnauthorized, latch: false},
		{name: "403 on negotiate", op: "negotiate", status: http.StatusForbidden, latch: false},
		{name: "403 on an object store", op: "objects store", status: http.StatusForbidden, latch: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logw bytes.Buffer
			s := newProviderSession(t.TempDir(), &logw)
			defer s.close()
			s.observeAuthRefusal(remotecache.AuthFailure{Op: test.op, Status: test.status, Source: remotecache.TokenClassCommand})
			if got := s.writesLatched(); got != test.latch {
				t.Fatalf("latched = %t, want %t", got, test.latch)
			}
			if test.latch != strings.Contains(logw.String(), "this run sends no further cache writes") {
				t.Fatalf("the latch must be said exactly when it closes the path, got %q", logw.String())
			}
		})
	}
}

// The developer reading of a refusal needs every hosted sign to be absent: a
// hosted launcher's ingest base, the run's own cache token, or a
// service-account identity each keep the warning.
func TestExpectedDeveloperRefusalNeedsNoHostedSign(t *testing.T) {
	for _, test := range []struct {
		name      string
		hosted    bool
		source    remotecache.TokenClass
		developer bool
	}{
		{name: "cloud token recipe on a laptop", source: remotecache.TokenClassCommand, developer: true},
		{name: "injected machine token on a laptop", source: remotecache.TokenClassEnv, developer: true},
		{name: "injected token under a hosted launcher", hosted: true, source: remotecache.TokenClassEnv},
		{name: "cloud token recipe under a hosted launcher", hosted: true, source: remotecache.TokenClassCommand},
		{name: "the run's cache token", source: remotecache.TokenClassRun},
		{name: "a service-account identity", source: remotecache.TokenClassMetadata},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &providerSession{hosted: test.hosted}
			if got := s.expectedDeveloperRefusal(remotecache.AuthFailure{Op: "store", Status: http.StatusForbidden, Source: test.source}); got != test.developer {
				t.Fatalf("developer = %t, want %t", got, test.developer)
			}
		})
	}
}

// hostedLaunch reads the ingest base a hosted launcher exports.
func TestHostedLaunchReadsTheIngestBase(t *testing.T) {
	t.Setenv(SessionReporterIngestURLEnv, "")
	if hostedLaunch() {
		t.Fatal("no ingest base: hostedLaunch = true")
	}
	t.Setenv(SessionReporterIngestURLEnv, "https://delivery.example/v1/delivery/records")
	if !hostedLaunch() {
		t.Fatal("ingest base exported: hostedLaunch = false")
	}
}
