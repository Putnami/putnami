package deliverycli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// decodeResponses parses every output line with the protocol's own response
// validator — the same one core's session harness uses — so the test doubles as
// a conformance check: a malformed or non-conformant response fails here.
func decodeResponses(t *testing.T, r *bytes.Buffer) []*cache.ProviderResponse {
	t.Helper()
	var out []*cache.ProviderResponse
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxProviderRequestBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		resp, diags := cache.ParseAndValidateProviderResponse(line)
		if resp == nil {
			t.Fatalf("non-conformant provider response %q: %v", line, diags)
		}
		out = append(out, resp)
	}
	return out
}

// mustInit drives one initialize op against a session and returns the validated
// result.
func mustInit(t *testing.T, s *providerSession, p *cache.InitializeParams) *cache.InitializeResult {
	t.Helper()
	req := &cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolMinVersion, ID: 1, Op: cache.OpInitialize}
	resp := s.initialize(req, p)
	if !resp.OK {
		t.Fatalf("initialize not OK: %+v", resp.Error)
	}
	res, diags := cache.ParseAndValidateInitializeResult(resp.Payload)
	if res == nil {
		t.Fatalf("invalid initialize result: %v", diags)
	}
	return res
}

// enableCacheWritesForTest isolates write-path proofs from the pinned runner's
// deliberately inherited read-only cache environment. These tests exercise the
// provider's write semantics, so their authority must be explicit rather than
// depend on whether the surrounding CI runner can publish cache entries.
func enableCacheWritesForTest(t *testing.T) {
	t.Helper()
	t.Setenv(remotecache.ReadOnlyEnv, "false")
}

func TestRunCacheProviderSession_RoundTrip(t *testing.T) {
	t.Setenv("PUTNAMI_CACHE_URL", "") // deterministic: no cache configured → not ready
	key := strings.Repeat("a", cache.KeyLength)
	reqs := []string{
		`{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"blobExchangeDir":"/tmp/bx","mode":"full"}}`,
		`{"protocolVersion":1,"id":2,"op":"prefetch","payload":{"keys":["` + key + `"]}}`,
		`{"protocolVersion":1,"id":3,"op":"restore","payload":{"key":"` + key + `"}}`,
		`{"protocolVersion":1,"id":4,"op":"marker-lookup","payload":{"workspace":"w","branch":"main","commands":["build"],"selection":"all"}}`,
		`{"protocolVersion":1,"id":5,"op":"summary","payload":{}}`,
		`{"protocolVersion":1,"id":6,"op":"shutdown"}`,
	}
	var out, logw bytes.Buffer
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	if err := runCacheProviderSession(in, &out, &logw, t.TempDir()); err != nil {
		t.Fatalf("session: %v", err)
	}

	resps := decodeResponses(t, &out)
	if len(resps) != len(reqs) {
		t.Fatalf("got %d responses, want %d", len(resps), len(reqs))
	}
	for i, r := range resps {
		if r.ID != int64(i+1) {
			t.Errorf("response %d: ID = %d, want %d", i, r.ID, i+1)
		}
		if !r.OK {
			t.Errorf("response id %d: not OK: %+v", r.ID, r.Error)
		}
		if r.ProtocolVersion != cache.ProviderProtocolMinVersion {
			t.Errorf("response id %d: protocolVersion = %d, want %d", r.ID, r.ProtocolVersion, cache.ProviderProtocolMinVersion)
		}
	}

	init, diags := cache.ParseAndValidateInitializeResult(resps[0].Payload)
	if init == nil {
		t.Fatalf("initialize result invalid: %v", diags)
	}
	if init.ProviderName != providerName {
		t.Errorf("init providerName = %q, want %q", init.ProviderName, providerName)
	}
	if init.ProviderVersion == "" {
		t.Error("init ProviderVersion is empty; the protocol requires a non-empty version for the gate")
	}
	if init.Ready {
		t.Error("init Ready = true with no cache config, want false")
	}

	rest, diags := cache.ParseAndValidateRestoreResult(resps[2].Payload)
	if rest == nil {
		t.Fatalf("restore result invalid: %v", diags)
	}
	if rest.Status != cache.RestoreMiss {
		t.Errorf("restore status = %q, want %q (no client → miss)", rest.Status, cache.RestoreMiss)
	}
}

func TestProviderSessionRestore_V2ServesAuthenticatedProvenance(t *testing.T) {
	blob := []byte("trusted compiled output")
	digest := cache.DigestOf(blob)
	key := strings.Repeat("e", cache.KeyLength)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.NegotiatePath:
			if got := r.Header.Get(remotecache.EntryProvenanceHeader); got != "v1" {
				t.Errorf("provenance opt-in = %q, want v1", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protocolVersion": cache.ProtocolVersion,
				"results": []map[string]any{{
					"key":              key,
					"hit":              true,
					"result":           map[string]any{"status": "success", "sizeBytes": len(blob)},
					"manifest":         map[string]any{"files": []map[string]any{{"path": "out.js", "digest": digest, "size": len(blob)}}},
					"downloads":        []map[string]any{{"digest": digest, "url": srv.URL + "/blob", "method": cache.TransferGet, "sizeBytes": len(blob)}},
					"producer":         cache.ProducerCI,
					"producerIdentity": "ci-runner@example.test",
					"channel":          cache.ChannelTrusted,
				}},
			})
		case "/blob":
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	init := mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
		Mode:            cache.ModeFull,
		Capabilities:    []string{cache.CapabilityProviderProtocolV2},
	})
	if init.ProtocolVersion != cache.ProviderProtocolVersion {
		t.Fatalf("negotiated protocol = %d, want %d", init.ProtocolVersion, cache.ProviderProtocolVersion)
	}

	resp := s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpRestore}, &cache.RestoreParams{Key: key})
	result, diags := cache.ParseAndValidateRestoreResult(resp.Payload)
	if result == nil {
		t.Fatalf("invalid restore result: %v", diags)
	}
	if result.Producer != cache.ProducerCI || result.ProducerIdentity != "ci-runner@example.test" || result.Channel != cache.ChannelTrusted {
		t.Errorf("provenance = (%q, %q, %q), want trusted CI metadata", result.Producer, result.ProducerIdentity, result.Channel)
	}
}

func TestProviderSessionUpload_DoesNotForwardAssertedProvenance(t *testing.T) {
	key := strings.Repeat("f", cache.KeyLength)
	var commit map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.StorePath:
			_ = json.NewEncoder(w).Encode(cache.StoreResponse{ProtocolVersion: cache.ProtocolVersion, Key: key})
		case cache.CommitPath:
			if err := json.NewDecoder(r.Body).Decode(&commit); err != nil {
				t.Fatalf("decode commit: %v", err)
			}
			_ = json.NewEncoder(w).Encode(cache.CommitResponse{ProtocolVersion: cache.ProtocolVersion, Key: key, Committed: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "developer-token")
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir(), Capabilities: []string{cache.CapabilityProviderProtocolV2}})
	resp := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload}, &cache.UploadParams{
		Key:      key,
		Result:   &cache.ActionResult{Status: "success"},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{{Path: "out", Digest: cache.DigestOf([]byte("output")), Size: int64(len("output"))}}},
		Producer: cache.ProducerCI, ProducerIdentity: "forged-ci", Channel: cache.ChannelTrusted,
	})
	if !resp.OK {
		t.Fatalf("upload response = %+v", resp)
	}
	_ = s.summary(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpSummary})
	if _, ok := commit["producer"]; ok {
		t.Errorf("commit forwarded untrusted producer: %+v", commit)
	}
	if _, ok := commit["producerIdentity"]; ok {
		t.Errorf("commit forwarded untrusted producer identity: %+v", commit)
	}
	if _, ok := commit["channel"]; ok {
		t.Errorf("commit forwarded untrusted channel: %+v", commit)
	}
}

// writeLinkedManifest writes a putnami.workspace.json binding root to a Cloud
// workspace, the same committed link a fresh worktree inherits (and the only
// piece of Cloud state that survives into a git-ignored-.putnami worktree).
func writeLinkedManifest(t *testing.T, root string) {
	t.Helper()
	manifest := `{"name":"putnami-cloud","options":{"@putnami/cloud":{"workspace":{"workspace_id":"11111111-test"}}}}`
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInitialize_LinkedWorkspaceSynthesizesCacheDefault pins the fresh-worktree
// fix: a workspace linked via the committed manifest but with no
// .putnami/cache.json still serves remote cache, because initialize synthesizes
// the managed default (URL + token source) instead of depending on the
// install-hook write, which can silently no-op on a cold checkout.
func TestInitialize_LinkedWorkspaceSynthesizesCacheDefault(t *testing.T) {
	t.Setenv(remotecache.URLEnv, "")  // no CI env override
	t.Setenv(remotecache.ModeEnv, "") // deterministic mode
	root := t.TempDir()
	writeLinkedManifest(t, root)
	if _, err := os.Stat(CachePath(root)); !os.IsNotExist(err) {
		t.Fatalf("precondition: expected no cache.json, stat err = %v", err)
	}

	s := newProviderSession(root, io.Discard)
	defer s.close()
	res := mustInit(t, s, &cache.InitializeParams{BlobExchangeDir: t.TempDir(), Mode: cache.Mode("full")})

	if !res.Ready {
		t.Error("linked workspace, no cache.json: init Ready = false, want true (synthesized managed default)")
	}
	if s.client == nil {
		t.Error("linked workspace: expected a remote-cache client to be built")
	}
}

// TestApplyLinkedCacheDefault_EnvTokenWins pins the complete remote-agent
// checkout contract: the committed link supplies the managed cache URL and
// fallback token recipe, while the user-provisioned environment secret wins at
// resolution time without executing that interactive-login recipe.
func TestApplyLinkedCacheDefault_EnvTokenWins(t *testing.T) {
	t.Setenv(remotecache.TokenEnv, "test-cache-token")
	root := t.TempDir()
	writeLinkedManifest(t, root)

	s := newProviderSession(root, io.Discard)
	defer s.close()
	cfg := &remotecache.Config{}
	s.applyLinkedCacheDefault(cfg)

	if cfg.URL != DefaultCacheURL {
		t.Fatalf("synthesized URL = %q, want %q", cfg.URL, DefaultCacheURL)
	}
	if got, want := strings.Join(cfg.Token.Command, " "), strings.Join(cacheTokenCommand(), " "); got != want {
		t.Fatalf("synthesized token command = %q, want %q", got, want)
	}
	got, err := cfg.ResolveToken(context.Background())
	if err != nil || got != "test-cache-token" {
		t.Fatalf("ResolveToken = %q, %v; want environment token", got, err)
	}
}

// TestInitialize_UnlinkedWorkspaceStaysLocalOnly guards the gate: an unlinked
// checkout (no committed workspace link, no cache.json) must not synthesize a
// cache config — remote caching stays off.
func TestInitialize_UnlinkedWorkspaceStaysLocalOnly(t *testing.T) {
	t.Setenv(remotecache.URLEnv, "")
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	res := mustInit(t, s, &cache.InitializeParams{BlobExchangeDir: t.TempDir(), Mode: cache.Mode("full")})
	if res.Ready {
		t.Error("unlinked workspace: init Ready = true, want false (no link → local-only)")
	}
	if s.client != nil {
		t.Error("unlinked workspace: expected no remote-cache client")
	}
}

// TestInitialize_DisabledCacheWinsOverSynthesis guards that an explicit
// `cloud cache disable` (enabled:false with the URL kept) is not overridden by
// the linked-workspace synthesis: the file's non-empty URL keeps synthesis out,
// and Active() reports the config disabled.
func TestInitialize_DisabledCacheWinsOverSynthesis(t *testing.T) {
	t.Setenv(remotecache.URLEnv, "")
	root := t.TempDir()
	writeLinkedManifest(t, root)
	if err := os.MkdirAll(filepath.Dir(CachePath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	disabled := `{"enabled":false,"url":"https://cache.putnami.cloud","mode":"full"}`
	if err := os.WriteFile(CachePath(root), []byte(disabled), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newProviderSession(root, io.Discard)
	defer s.close()
	res := mustInit(t, s, &cache.InitializeParams{BlobExchangeDir: t.TempDir(), Mode: cache.Mode("full")})
	if res.Ready {
		t.Error("disabled cache config: init Ready = true, want false (explicit disable must win)")
	}
}

func TestRunCacheProviderSession_ShutdownStopsLoop(t *testing.T) {
	reqs := []string{
		`{"protocolVersion":1,"id":1,"op":"summary","payload":{}}`,
		`{"protocolVersion":1,"id":2,"op":"shutdown"}`,
		`{"protocolVersion":1,"id":3,"op":"summary","payload":{}}`, // must not be served
	}
	var out, logw bytes.Buffer
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	if err := runCacheProviderSession(in, &out, &logw, t.TempDir()); err != nil {
		t.Fatalf("session: %v", err)
	}
	resps := decodeResponses(t, &out)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2 (loop stops after shutdown)", len(resps))
	}
	if resps[1].ID != 2 {
		t.Errorf("last response ID = %d, want 2 (the shutdown ack)", resps[1].ID)
	}
}

func TestRunCacheProviderSession_MalformedRequestSkipped(t *testing.T) {
	reqs := []string{
		`this is not json`,
		`{"protocolVersion":1,"id":7,"op":"summary","payload":{}}`,
	}
	var out, logw bytes.Buffer
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	if err := runCacheProviderSession(in, &out, &logw, t.TempDir()); err != nil {
		t.Fatalf("session: %v", err)
	}
	resps := decodeResponses(t, &out)
	if len(resps) != 1 || resps[0].ID != 7 {
		t.Fatalf("got %d responses %+v, want 1 (id 7) — malformed line skipped, valid one served", len(resps), resps)
	}
	if !strings.Contains(logw.String(), "malformed") {
		t.Errorf("expected a malformed-request log on stderr, got %q", logw.String())
	}
}

func TestRunCacheProviderSession_EOFWithoutShutdown(t *testing.T) {
	var out, logw bytes.Buffer
	in := strings.NewReader(`{"protocolVersion":1,"id":1,"op":"summary","payload":{}}` + "\n")
	// A closed stdin without a shutdown op is the crash/normal-exit path: clean nil.
	if err := runCacheProviderSession(in, &out, &logw, t.TempDir()); err != nil {
		t.Fatalf("session: %v", err)
	}
	if resps := decodeResponses(t, &out); len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
}

func TestProviderSessionInitialize_Readiness(t *testing.T) {
	t.Setenv("PUTNAMI_CACHE_URL", "") // ignore any ambient cache config

	noConfig := newProviderSession(t.TempDir(), io.Discard)
	defer noConfig.close()
	if mustInit(t, noConfig, &cache.InitializeParams{ProtocolVersion: 1, BlobExchangeDir: t.TempDir(), Mode: cache.ModeFull}).Ready {
		t.Error("no cache config → not ready")
	}

	dir := t.TempDir()
	if err := WriteCacheConfig(dir, &CacheConfig{Enabled: true, URL: "https://cache.example", Mode: "full"}); err != nil {
		t.Fatal(err)
	}
	configured := newProviderSession(dir, io.Discard)
	defer configured.close()
	if !mustInit(t, configured, &cache.InitializeParams{ProtocolVersion: 1, BlobExchangeDir: t.TempDir(), Mode: cache.ModeFull}).Ready {
		t.Error("enabled config with a URL → ready")
	}
}

// TestProviderSessionRestore_Hit exercises the data plane end-to-end: a restore
// negotiates a hit against a fake cache server, downloads the hit's blob, and
// lands it content-correct in the blob-exchange directory for core to ingest.
func TestProviderSessionRestore_Hit(t *testing.T) {
	blob := []byte("compiled output bytes")
	digest := cache.DigestOf(blob)
	key := strings.Repeat("a", cache.KeyLength)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.NegotiatePath:
			_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Results: []cache.KeyResult{{
					Key:    key,
					Hit:    true,
					Result: &cache.ActionResult{Status: "success", SizeBytes: int64(len(blob))},
					Manifest: &cache.Manifest{Files: []cache.FileEntry{
						{Path: "out.js", Digest: digest, Mode: 0o644, Size: int64(len(blob))},
					}},
					Downloads: []cache.BlobTransfer{{
						Digest: digest, URL: srv.URL + "/blob", Method: cache.TransferGet, SizeBytes: int64(len(blob)),
					}},
				}},
			})
		case "/blob":
			_, _ = w.Write(blob)
		default:
			// download-batch and anything else miss → the fetcher falls back to GET.
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")

	exchangeDir := t.TempDir()
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	if !mustInit(t, s, &cache.InitializeParams{ProtocolVersion: 1, BlobExchangeDir: exchangeDir, Mode: cache.ModeFull}).Ready {
		t.Fatal("expected ready with PUTNAMI_CACHE_URL set")
	}

	req := &cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpRestore}
	resp := s.restore(req, &cache.RestoreParams{Key: key})
	if !resp.OK {
		t.Fatalf("restore not OK: %+v", resp.Error)
	}
	res, diags := cache.ParseAndValidateRestoreResult(resp.Payload)
	if res == nil {
		t.Fatalf("invalid restore result: %v", diags)
	}
	if res.Status != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", res.Status)
	}

	path, ok := cache.BlobExchangePath(exchangeDir, digest)
	if !ok {
		t.Fatal("bad digest")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("blob not materialized into the exchange dir: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Errorf("materialized blob = %q, want %q", got, blob)
	}
}

// countingCacheServer is a fake cache server that counts negotiate calls and
// serves one key as a hit with a single blob. It lets the prefetch-index tests
// assert how many negotiate round trips a restore actually cost.
func countingCacheServer(t *testing.T, hitKey string, blob []byte) (*httptest.Server, *int32) {
	t.Helper()
	digest := cache.DigestOf(blob)
	var negotiates int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.NegotiatePath:
			atomic.AddInt32(&negotiates, 1)
			var nreq cache.NegotiateRequest
			_ = json.NewDecoder(r.Body).Decode(&nreq)
			resp := cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion}
			for _, k := range nreq.Keys {
				if k.Key != hitKey {
					resp.Results = append(resp.Results, cache.KeyResult{Key: k.Key, Hit: false})
					continue
				}
				resp.Results = append(resp.Results, cache.KeyResult{
					Key:    k.Key,
					Hit:    true,
					Result: &cache.ActionResult{Status: "success", SizeBytes: int64(len(blob))},
					Manifest: &cache.Manifest{Files: []cache.FileEntry{
						{Path: "out.js", Digest: digest, Mode: 0o644, Size: int64(len(blob))},
					}},
					Downloads: []cache.BlobTransfer{{
						Digest: digest, URL: srv.URL + "/blob", Method: cache.TransferGet, SizeBytes: int64(len(blob)),
					}},
				})
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/blob":
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &negotiates
}

// TestProviderSessionRestore_ServedFromPrefetchIndex asserts that restores are
// answered from the prefetch's batched negotiate — hits and misses alike — with
// no per-key negotiate round trip. Before the index, every restore re-negotiated
// its single key synchronously in the read loop, so a warm rebuild's wall time
// was ~one network round trip per cached task, serialized.
func TestProviderSessionRestore_ServedFromPrefetchIndex(t *testing.T) {
	blob := []byte("compiled output bytes")
	hitKey := strings.Repeat("a", cache.KeyLength)
	missKey := strings.Repeat("b", cache.KeyLength)
	srv, negotiates := countingCacheServer(t, hitKey, blob)

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")

	exchangeDir := t.TempDir()
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	if !mustInit(t, s, &cache.InitializeParams{ProtocolVersion: 1, BlobExchangeDir: exchangeDir, Mode: cache.ModeFull}).Ready {
		t.Fatal("expected ready with PUTNAMI_CACHE_URL set")
	}

	pre := s.prefetch(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpPrefetch},
		&cache.PrefetchParams{Keys: []string{hitKey, missKey}})
	if !pre.OK {
		t.Fatalf("prefetch not OK: %+v", pre.Error)
	}

	// The hit is served from the index (restore waits for the in-flight batch).
	resp := s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpRestore},
		&cache.RestoreParams{Key: hitKey})
	res, diags := cache.ParseAndValidateRestoreResult(resp.Payload)
	if res == nil {
		t.Fatalf("invalid restore result: %v", diags)
	}
	if res.Status != cache.RestoreHit {
		t.Fatalf("restore status = %q, want hit", res.Status)
	}

	// The batched miss is answered from the index too — no re-ask for a key
	// this run already knows is absent.
	resp = s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 4, Op: cache.OpRestore},
		&cache.RestoreParams{Key: missKey})
	res, diags = cache.ParseAndValidateRestoreResult(resp.Payload)
	if res == nil {
		t.Fatalf("invalid restore result: %v", diags)
	}
	if res.Status != cache.RestoreMiss {
		t.Fatalf("restore status = %q, want miss", res.Status)
	}

	s.bg.Wait() // drain the prefetch warmer before counting
	if n := atomic.LoadInt32(negotiates); n != 1 {
		t.Fatalf("negotiate round trips = %d, want exactly 1 (the prefetch batch)", n)
	}
}

// TestProviderSessionRestore_UncoveredKeyFallsBack asserts a restore for a key
// outside the prefetch batch still negotiates individually — the exception
// path when core restores something it never handed to prefetch.
func TestProviderSessionRestore_UncoveredKeyFallsBack(t *testing.T) {
	blob := []byte("compiled output bytes")
	batched := strings.Repeat("a", cache.KeyLength)
	uncovered := strings.Repeat("c", cache.KeyLength)
	srv, negotiates := countingCacheServer(t, batched, blob)

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")

	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	if !mustInit(t, s, &cache.InitializeParams{ProtocolVersion: 1, BlobExchangeDir: t.TempDir(), Mode: cache.ModeFull}).Ready {
		t.Fatal("expected ready")
	}

	pre := s.prefetch(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpPrefetch},
		&cache.PrefetchParams{Keys: []string{batched}})
	if !pre.OK {
		t.Fatalf("prefetch not OK: %+v", pre.Error)
	}

	resp := s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpRestore},
		&cache.RestoreParams{Key: uncovered})
	res, diags := cache.ParseAndValidateRestoreResult(resp.Payload)
	if res == nil {
		t.Fatalf("invalid restore result: %v", diags)
	}
	if res.Status != cache.RestoreMiss {
		t.Fatalf("restore status = %q, want miss", res.Status)
	}

	s.bg.Wait()
	if n := atomic.LoadInt32(negotiates); n != 2 {
		t.Fatalf("negotiate round trips = %d, want 2 (batch + uncovered-key fallback)", n)
	}
}

// TestRunCacheProviderSession_ConcurrentRestores drives restores through the
// real read loop against a serving session and asserts every one is answered
// (out-of-order responses are the protocol — core matches by ID) and that the
// shutdown ack still lands last, after in-flight restores drain.
func TestRunCacheProviderSession_ConcurrentRestores(t *testing.T) {
	blob := []byte("compiled output bytes")
	hitKey := strings.Repeat("a", cache.KeyLength)
	srv, negotiates := countingCacheServer(t, hitKey, blob)

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")

	exchangeDir := t.TempDir()
	reqs := []string{
		`{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"blobExchangeDir":` + strconv.Quote(exchangeDir) + `,"mode":"full"}}`,
		`{"protocolVersion":1,"id":2,"op":"prefetch","payload":{"keys":["` + hitKey + `"]}}`,
	}
	const restores = 24
	for i := 0; i < restores; i++ {
		reqs = append(reqs, `{"protocolVersion":1,"id":`+strconv.Itoa(3+i)+`,"op":"restore","payload":{"key":"`+hitKey+`"}}`)
	}
	shutdownID := int64(3 + restores)
	reqs = append(reqs, `{"protocolVersion":1,"id":`+strconv.FormatInt(shutdownID, 10)+`,"op":"shutdown"}`)

	var out, logw bytes.Buffer
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	if err := runCacheProviderSession(in, &out, &logw, t.TempDir()); err != nil {
		t.Fatalf("session: %v (log: %s)", err, logw.String())
	}

	resps := decodeResponses(t, &out)
	if len(resps) != len(reqs) {
		t.Fatalf("got %d responses, want %d (log: %s)", len(resps), len(reqs), logw.String())
	}
	hits := 0
	for _, r := range resps {
		if !r.OK {
			t.Fatalf("response id %d not OK: %+v", r.ID, r.Error)
		}
		if r.ID >= 3 && r.ID < shutdownID {
			res, diags := cache.ParseAndValidateRestoreResult(r.Payload)
			if res == nil {
				t.Fatalf("invalid restore result: %v", diags)
			}
			if res.Status == cache.RestoreHit {
				hits++
			}
		}
	}
	if hits != restores {
		t.Fatalf("restore hits = %d, want %d", hits, restores)
	}
	if last := resps[len(resps)-1]; last.ID != shutdownID {
		t.Fatalf("last response ID = %d, want the shutdown ack %d (must drain in-flight restores first)", last.ID, shutdownID)
	}
	if n := atomic.LoadInt32(negotiates); n != 1 {
		t.Fatalf("negotiate round trips = %d, want exactly 1 for %d restores", n, restores)
	}
}

func TestBlobExchangeSource(t *testing.T) {
	dir := t.TempDir()
	blob := []byte("hello")
	digest := cache.DigestOf(blob)
	path, ok := cache.BlobExchangePath(dir, digest)
	if !ok {
		t.Fatal("bad digest")
	}
	if _, err := writeExchangeBlob(func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(blob)), nil
	}, digest, path); err != nil {
		t.Fatalf("writeExchangeBlob: %v", err)
	}

	rc, err := blobExchangeSource{dir: dir}.OpenBlob(digest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, blob) {
		t.Errorf("OpenBlob read = %q, want %q", got, blob)
	}

	if _, err := (blobExchangeSource{dir: dir}).OpenBlob("not-a-digest"); err == nil {
		t.Error("expected an error opening an invalid digest")
	}
}

// TestCacheConfigWireCompatible guards the writer (cloud's CacheConfig, produced
// by `cloud setup`) against the reader (remotecache.Config, loaded by the
// cache-provider): the two are separate views of .putnami/cache.json and must not
// drift.
func TestCacheConfigWireCompatible(t *testing.T) {
	dir := t.TempDir()
	written := &CacheConfig{
		Enabled: true,
		URL:     "https://cache.example",
		Mode:    "toplevel",
		Token:   &clicore.TokenSource{Command: []string{"putnami", "cloud", "token", "--for", "cache"}},
	}
	if err := WriteCacheConfig(dir, written); err != nil {
		t.Fatal(err)
	}

	got, err := remotecache.LoadConfig(CachePath(dir))
	if err != nil {
		t.Fatalf("remotecache.LoadConfig: %v", err)
	}
	if !got.Active() {
		t.Error("writer enabled:true + url → reader Active() should be true")
	}
	if got.URL != written.URL {
		t.Errorf("url = %q, want %q", got.URL, written.URL)
	}
	if string(got.Mode) != written.Mode {
		t.Errorf("mode = %q, want %q", got.Mode, written.Mode)
	}
	if strings.Join(got.Token.Command, " ") != strings.Join(written.Token.Command, " ") {
		t.Errorf("token command = %v, want %v", got.Token.Command, written.Token.Command)
	}

	// A disabled config the writer produces must read back as inactive.
	if err := WriteCacheConfig(dir, &CacheConfig{Enabled: false, URL: written.URL, Mode: written.Mode}); err != nil {
		t.Fatal(err)
	}
	got2, err := remotecache.LoadConfig(CachePath(dir))
	if err != nil {
		t.Fatalf("remotecache.LoadConfig (disabled): %v", err)
	}
	if got2.Active() {
		t.Error("writer enabled:false → reader Active() should be false")
	}
}

// TestProviderSessionRestore_KnownDigestSkipped verifies the core-owned presence
// decision: a digest core already has (carried in InitializeParams.KnownDigests)
// is not re-downloaded into the exchange directory — core ingests it from its own
// CAS — yet the key still restores as a hit.
func TestProviderSessionRestore_KnownDigestSkipped(t *testing.T) {
	blob := []byte("already in core's CAS")
	digest := cache.DigestOf(blob)
	key := strings.Repeat("b", cache.KeyLength)

	var blobRequested bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.NegotiatePath:
			_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Results: []cache.KeyResult{{
					Key:    key,
					Hit:    true,
					Result: &cache.ActionResult{Status: "success", SizeBytes: int64(len(blob))},
					Manifest: &cache.Manifest{Files: []cache.FileEntry{
						{Path: "out.js", Digest: digest, Mode: 0o644, Size: int64(len(blob))},
					}},
					Downloads: []cache.BlobTransfer{{
						Digest: digest, URL: srv.URL + "/blob", Method: cache.TransferGet, SizeBytes: int64(len(blob)),
					}},
				}},
			})
		case "/blob":
			blobRequested = true
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "test-token")

	exchangeDir := t.TempDir()
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	if !mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: 1, BlobExchangeDir: exchangeDir, Mode: cache.ModeFull, KnownDigests: []string{digest},
	}).Ready {
		t.Fatal("expected ready")
	}

	resp := s.restore(
		&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 1, Op: cache.OpRestore},
		&cache.RestoreParams{Key: key},
	)
	res, diags := cache.ParseAndValidateRestoreResult(resp.Payload)
	if res == nil {
		t.Fatalf("invalid restore result: %v", diags)
	}
	if res.Status != cache.RestoreHit {
		t.Fatalf("status = %q, want hit", res.Status)
	}
	if blobRequested {
		t.Error("a known blob was downloaded; KnownDigests must skip it")
	}
	path, _ := cache.BlobExchangePath(exchangeDir, digest)
	if _, err := os.Stat(path); err == nil {
		t.Error("a known blob was staged into the exchange dir; core ingests it from its own CAS")
	}
}

// --- named regression proofs ---------------------------------------------------

// The pinned runner deliberately carries cache.read without cache.write: pull
// request code is untrusted, so granting it write authority would let it seed
// cache entries consumed by trusted runs. Read-only mode must preserve the
// useful read path while declining every write before any network exchange.
func TestProviderReadOnlyModeServesReadsWithoutAttemptingWrites(t *testing.T) {
	key := strings.Repeat("a", cache.KeyLength)
	var readRequests atomic.Int64
	var writeRequests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.NegotiatePath:
			readRequests.Add(1)
			_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
		case cache.RunMarkerLookupPath:
			readRequests.Add(1)
			http.NotFound(w, r)
		default:
			writeRequests.Add(1)
			http.Error(w, "a read-only provider must never reach this path", http.StatusForbidden)
		}
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "read-only-token")
	t.Setenv(remotecache.ReadOnlyEnv, "true")
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	if !mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
	}).Ready {
		t.Fatal("read-only remote cache must stay ready for reads")
	}

	restore := s.restore(
		&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpRestore},
		&cache.RestoreParams{Key: key},
	)
	restoreResult, diags := cache.ParseAndValidateRestoreResult(restore.Payload)
	if restoreResult == nil || restoreResult.Status != cache.RestoreMiss {
		t.Fatalf("read-only restore = %+v, diagnostics=%v; want a normal remote miss", restoreResult, diags)
	}

	lookup := s.markerLookup(
		&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpMarkerLookup},
		&cache.MarkerLookupParams{Workspace: "workspace", Branch: "main", Commands: []string{"build"}},
	)
	lookupResult, diags := cache.ParseAndValidateMarkerLookupResult(lookup.Payload)
	if lookupResult == nil || lookupResult.Found {
		t.Fatalf("read-only marker lookup = %+v, diagnostics=%v; want a normal remote miss", lookupResult, diags)
	}

	upload := s.upload(
		&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 4, Op: cache.OpUpload},
		&cache.UploadParams{Key: key, Result: &cache.ActionResult{Status: "success"}, Manifest: &cache.Manifest{}},
	)
	uploadResult, diags := cache.ParseAndValidateUploadResult(upload.Payload)
	if uploadResult == nil || uploadResult.Accepted {
		t.Fatalf("read-only upload = %+v, diagnostics=%v; want Accepted=false", uploadResult, diags)
	}

	markerWrite := s.markerWrite(
		&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 5, Op: cache.OpMarkerWrite},
		&cache.MarkerWriteParams{Workspace: "workspace", Branch: "main", Commands: []string{"build"}, SHA: strings.Repeat("b", 40)},
	)
	markerWriteResult, diags := cache.ParseAndValidateMarkerWriteResult(markerWrite.Payload)
	if markerWriteResult == nil || markerWriteResult.Published {
		t.Fatalf("read-only marker write = %+v, diagnostics=%v; want Published=false", markerWriteResult, diags)
	}

	if got := readRequests.Load(); got != 2 {
		t.Fatalf("remote read requests = %d, want negotiate + marker lookup", got)
	}
	if got := writeRequests.Load(); got != 0 {
		t.Fatalf("remote write requests = %d, want zero", got)
	}
}

// TestCacheUploadFailureNeverChangesTheRunVerdict is proof (b), the cache half.
//
// The remote cache is an OPTIMIZATION, so every one of its failure modes has to
// be invisible to the gate's verdict. An upload runs on a background goroutine
// precisely so a slow or broken cache server cannot hold a build hostage — which
// also means an upload error has no path back to the job that produced the
// artifact, and must not acquire one. The proof: with a store server that fails
// every request, the upload is still ACCEPTED, the summary still returns, no
// upload is counted, and the session keeps serving the next operation.
func TestCacheUploadFailureNeverChangesTheRunVerdict(t *testing.T) {
	enableCacheWritesForTest(t)
	key := strings.Repeat("b", cache.KeyLength)
	var storeAttempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cache.StorePath:
			storeAttempts.Add(1)
			http.Error(w, "cache store is down", http.StatusInternalServerError)
		case cache.NegotiatePath:
			// The gate's next lookup still gets an answer: a broken upload path
			// degrades to a miss, never to a failed job.
			_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
	})

	resp := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload},
		&cache.UploadParams{
			Key:      key,
			Result:   &cache.ActionResult{Status: "success"},
			Manifest: &cache.Manifest{Files: []cache.FileEntry{{Path: "out", Digest: cache.DigestOf([]byte("o")), Size: 1}}},
		})
	if !resp.OK {
		t.Fatalf("upload response = %+v; a failing cache must never fail the op", resp)
	}
	uploadResult, diags := cache.ParseAndValidateUploadResult(resp.Payload)
	if uploadResult == nil {
		t.Fatalf("invalid upload result: %v", diags)
	}
	if !uploadResult.Accepted {
		t.Fatal("upload was refused; the entry is handed off asynchronously and acceptance is not a durability claim")
	}

	// summary drains the background upload. It must return normally even though
	// the store failed, and must not claim bytes it never durably stored.
	summary := s.summary(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpSummary})
	if !summary.OK {
		t.Fatalf("summary response = %+v after a failed upload", summary)
	}
	stats, diags := cache.ParseAndValidateSummaryResult(summary.Payload)
	if stats == nil {
		t.Fatalf("invalid summary result: %v", diags)
	}
	if stats.UploadedCount != 0 || stats.UploadedBytes != 0 {
		t.Fatalf("summary counted %d uploads / %d bytes after a failing store; a failed upload must never be reported as durable",
			stats.UploadedCount, stats.UploadedBytes)
	}
	if storeAttempts.Load() == 0 {
		t.Fatal("precondition: the upload never reached the store server")
	}
	if !strings.Contains(logw.String(), "upload ") {
		t.Fatalf("a failed upload must be diagnosed on the provider log, got %q", logw.String())
	}

	// The session is still serving: the next operation answers normally.
	restore := s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 4, Op: cache.OpRestore},
		&cache.RestoreParams{Key: key})
	if !restore.OK {
		t.Fatalf("restore after a failed upload = %+v, want a normal (miss) answer", restore)
	}
}

// TestCacheMarkerKeysAreWorkspaceScoped is proof (c), the cache half: a run
// marker is addressed by the workspace it belongs to, so one workspace's
// "this commit was green" fact can never satisfy another's lookup.
//
// The namespace itself is derived SERVER-SIDE from the verified bearer and is
// never asserted by the client — that is the outer fence, and the reason this
// test asserts on what the client PUTS ON THE WIRE: the workspace must ride
// every marker request, unmodified, or two tenants sharing a namespace-less key
// would share a green verdict.
func TestCacheMarkerKeysAreWorkspaceScoped(t *testing.T) {
	enableCacheWritesForTest(t)
	var lookups, publishes []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode %s body: %v", r.URL.Path, err)
			return
		}
		switch r.URL.Path {
		case cache.RunMarkerLookupPath:
			lookups = append(lookups, body)
			// Answer with a marker ONLY for ws-a, so a ws-b lookup that leaked
			// ws-a's key would visibly find one.
			if body["workspace"] == "ws-a" {
				_ = json.NewEncoder(w).Encode(cache.RunMarkerResponse{
					ProtocolVersion: cache.ProtocolVersion,
					Marker:          &cache.RunMarker{SHA: "sha-from-ws-a"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(cache.RunMarkerResponse{ProtocolVersion: cache.ProtocolVersion})
		case cache.RunMarkerPublishPath:
			publishes = append(publishes, body)
			_ = json.NewEncoder(w).Encode(cache.PublishRunMarkerResponse{
				ProtocolVersion: cache.ProtocolVersion, Published: true,
				Marker: &cache.RunMarker{SHA: "sha-b"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "ci-token")
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
	})

	// Same branch, same commands, same params: the workspace is the ONLY thing
	// separating these two lookups.
	lookup := func(workspace string) *cache.MarkerLookupResult {
		t.Helper()
		resp := s.markerLookup(
			&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpMarkerLookup},
			&cache.MarkerLookupParams{
				Workspace: workspace, Branch: "main",
				Commands: []string{"lint", "test", "build"}, ParamsHash: "params-1",
			})
		if !resp.OK {
			t.Fatalf("marker-lookup(%s) = %+v", workspace, resp)
		}
		out, diags := cache.ParseAndValidateMarkerLookupResult(resp.Payload)
		if out == nil {
			t.Fatalf("invalid marker-lookup result: %v", diags)
		}
		return out
	}

	if got := lookup("ws-a"); !got.Found {
		t.Fatal("precondition: ws-a's own marker was not found")
	}
	if got := lookup("ws-b"); got.Found {
		t.Fatalf("ws-b resolved ws-a's run marker (%+v); a green verdict crossed a workspace boundary", got.Marker)
	}

	resp := s.markerWrite(
		&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 4, Op: cache.OpMarkerWrite},
		&cache.MarkerWriteParams{
			Workspace: "ws-b", Branch: "main",
			Commands: []string{"lint", "test", "build"}, ParamsHash: "params-1",
			SHA: "sha-b", ObservedSHA: "sha-a",
		})
	if !resp.OK {
		t.Fatalf("marker-write = %+v", resp)
	}

	// Every request carried its own workspace, verbatim: the key the server
	// namespaces is (namespace, workspace, branch, commands, params), so a client
	// that dropped or rewrote the workspace would merge two tenants' markers.
	if len(lookups) != 2 {
		t.Fatalf("lookups = %d, want 2", len(lookups))
	}
	for i, want := range []string{"ws-a", "ws-b"} {
		if got := lookups[i]["workspace"]; got != want {
			t.Fatalf("lookup %d workspace = %v, want %q", i, got, want)
		}
	}
	if len(publishes) != 1 || publishes[0]["workspace"] != "ws-b" {
		t.Fatalf("publishes = %+v, want exactly one scoped to ws-b", publishes)
	}
}

// A run whose every cache write was refused persists nothing, so the next run
// starts cold and the loop repeats. Reported as uploadedCount=0
// alone that is byte-identical to a fully deduped warm run, so the provider must
// say the refusal out loud — while still never failing the build over it.
func TestCacheAuthRefusalsAreReportedProminentlyAtSummary(t *testing.T) {
	enableCacheWritesForTest(t)
	key := strings.Repeat("d", cache.KeyLength)
	const secret = "pkt_injected_machine_token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cache.NegotiatePath {
			_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
			return
		}
		// Every write is refused, the way an expired or revoked bearer is.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(cache.ErrorResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Code:            cache.CodeUnauthorized,
			Message:         "authentication with a workspace or organization scope is required",
		})
	}))
	defer srv.Close()

	t.Setenv("PUTNAMI_CACHE_URL", srv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", secret)
	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
	})

	upload := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpUpload},
		&cache.UploadParams{
			Key:      key,
			Result:   &cache.ActionResult{Status: "success"},
			Manifest: &cache.Manifest{Files: []cache.FileEntry{{Path: "out", Digest: cache.DigestOf([]byte("o")), Size: 1}}},
		})
	if !upload.OK {
		t.Fatalf("upload response = %+v; a refused cache must never fail the op", upload)
	}

	summary := s.summary(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 3, Op: cache.OpSummary})
	if !summary.OK {
		t.Fatalf("summary response = %+v after a refused write", summary)
	}
	stats, diags := cache.ParseAndValidateSummaryResult(summary.Payload)
	if stats == nil {
		t.Fatalf("invalid summary result: %v", diags)
	}
	if stats.UploadedCount != 0 {
		t.Fatalf("summary counted %d uploads after a refused store", stats.UploadedCount)
	}

	log := logw.String()
	if !strings.Contains(log, "auth refused:") || !strings.Contains(log, "op=store") {
		t.Fatalf("the refused exchange must be named on the provider log, got %q", log)
	}
	if !strings.Contains(log, "source="+string(remotecache.TokenClassEnv)) {
		t.Fatalf("the auth SOURCE class must be recorded so the CI record identifies the selected path, got %q", log)
	}
	if !strings.Contains(log, "refreshed=false") {
		t.Fatalf("a static injected token must be recorded as not refreshed, got %q", log)
	}
	if !strings.Contains(log, "next run starts cold") {
		t.Fatalf("an all-writes-refused run must not be able to look warm, got %q", log)
	}
	if strings.Contains(log, secret) {
		t.Fatalf("the provider log leaked the cache bearer: %q", log)
	}
}

// The counterpart: a healthy run must stay quiet. The warning only earns its
// prominence if it never fires on a run that stored normally.
func TestCacheSummaryStaysQuietWithoutRefusals(t *testing.T) {
	t.Setenv("PUTNAMI_CACHE_URL", "")
	t.Setenv("PUTNAMI_CACHE_TOKEN", "")
	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion,
		BlobExchangeDir: t.TempDir(),
	})

	if resp := s.summary(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpSummary}); !resp.OK {
		t.Fatalf("summary response = %+v", resp)
	}
	if strings.Contains(logw.String(), "refused") {
		t.Fatalf("a run with no refusals must print no refusal report, got %q", logw.String())
	}
}

// An OBJECT write refused must not make the run claim its task cache was
// refused. Object writes are best-effort and — until the default-branch cache
// write lease is armed — refused on EVERY run, so folding them into the task
// verdict would print "this run persisted NO cache entries" on a run whose every
// task entry was stored, and train operators to ignore the one warning that
// catches a genuinely cold cache.
func TestObjectAuthRefusalStaysOutOfTheTaskCacheVerdict(t *testing.T) {
	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()

	s.observeAuthRefusal(remotecache.AuthFailure{
		Op:      "objects store",
		Status:  http.StatusForbidden,
		Source:  remotecache.TokenClassEnv,
		Code:    "forbidden",
		Message: "token is not authorized for cache.object.write",
	})

	s.mu.Lock()
	refusals, writeRefusals, first := s.authRefusals, s.authWriteRefusals, s.firstAuthRefusal
	s.mu.Unlock()
	if refusals != 0 || writeRefusals != 0 {
		t.Errorf("task-cache counters = %d refusals / %d write refusals after an OBJECT refusal; object writes are not task entries",
			refusals, writeRefusals)
	}
	if first != "" {
		t.Errorf("the object refusal consumed the session's first-refusal slot (%q); that record belongs to the cache-401 investigation", first)
	}

	// A task run that stored nothing is exactly the state the cold-cache warning
	// exists for, so the absence of the warning here has to be caused by the
	// refusal being an OBJECT one, not by the run looking healthy.
	resp := s.summary(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpSummary})
	if !resp.OK {
		t.Fatalf("summary response = %+v", resp)
	}
	log := logw.String()
	if strings.Contains(log, "next run starts cold") || strings.Contains(log, "on the write path") {
		t.Errorf("an object-write refusal claimed the whole cache was refused: %q", log)
	}
	// It still speaks — once, under its own name, carrying classification only.
	if !strings.Contains(log, "object cache: auth refused: op=objects store status=403") {
		t.Errorf("the object refusal must still be recorded on its own line, got %q", log)
	}
	if !strings.Contains(log, "code=forbidden") {
		t.Errorf("the object refusal line lost the server's classification, got %q", log)
	}

	// The slot is still there for the exchange the investigation cares about.
	s.observeAuthRefusal(remotecache.AuthFailure{Op: "store", Status: http.StatusUnauthorized, Source: remotecache.TokenClassEnv})
	if !strings.Contains(logw.String(), "auth refused: op=store status=401") {
		t.Errorf("a later TASK refusal must still be logged in full, got %q", logw.String())
	}
}

func TestCacheTokenCommandFailureIsReportedOncePerSession(t *testing.T) {
	t.Setenv("PUTNAMI_CACHE_TOKEN", "")
	_, tokenErr := (remotecache.TokenSource{Command: []string{"sh", "-c", "printf '%s\\n' 'Invalid client credentials' >&2; exit 3"}}).Resolve(context.Background())
	if tokenErr == nil {
		t.Fatal("token command unexpectedly succeeded")
	}

	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()
	s.logCacheError("prefetch negotiate", tokenErr)
	s.logCacheError("marker-write", fmt.Errorf("publish marker: %w", tokenErr))

	log := logw.String()
	if got := strings.Count(log, "remote cache disabled for this session"); got != 1 {
		t.Fatalf("session warning count = %d, want 1; log=%q", got, log)
	}
	if !strings.Contains(log, "diagnostic: Invalid client credentials") {
		t.Fatalf("session warning hid the safe root cause: %q", log)
	}
}

// The provider must wire the class-aware resolver, not the class-blind one: an
// injected PUTNAMI_CACHE_TOKEN wins over the persisted recipe and is bytes the
// runner cannot re-derive, so it must reach the client labeled non-renewable.
// The renewable half (the runner's tokenless metadata path) is pinned in
// remotecache's own config tests, where the metadata seam is reachable.
func TestProviderWiresClassAwareBearerResolution(t *testing.T) {
	t.Setenv("PUTNAMI_CACHE_TOKEN", "pkt_injected")
	cfg := &remotecache.Config{
		URL:   "https://cache.putnami.cloud",
		Token: remotecache.TokenSource{Command: []string{"/nonexistent/cache-token-command"}},
	}
	injected, err := cfg.ResolveBearer(context.Background())
	if err != nil {
		t.Fatalf("ResolveBearer with an injected token: %v", err)
	}
	if injected.Token != "pkt_injected" {
		t.Fatalf("token = %q; the env override must win over the configured recipe", injected.Token)
	}
	if injected.Class != remotecache.TokenClassEnv || injected.Class.Renewable() {
		t.Fatalf("class = %q (renewable=%t); an injected machine token must not be re-mintable",
			injected.Class, injected.Class.Renewable())
	}
}
