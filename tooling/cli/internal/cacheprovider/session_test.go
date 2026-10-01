package cacheprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/cache/objectcachetest"
	registry "go.putnami.dev/protocol/registry"
)

// TestMain doubles as the fake cache provider: when the harness re-execs this
// test binary with PUTNAMI_TEST_FAKE_PROVIDER=1, it runs the provider RPC loop
// instead of the tests. Only the timeout and crash tests pay for the real
// subprocess (process death and hung-pipe fallback need one); the happy-path
// tests run the same loop in-process over pipes, because under -race each
// subprocess spawn costs ~1s of race-runtime startup.
func TestMain(m *testing.M) {
	if os.Getenv("PUTNAMI_TEST_FAKE_PROVIDER") == "1" {
		protocolVersion, _ := strconv.Atoi(os.Getenv("FAKE_PROTOCOL_VERSION"))
		os.Exit(serveFakeProvider(os.Stdin, os.Stdout, fakeProviderConfig{
			hangOp:          os.Getenv("FAKE_HANG_OP"),
			crashOp:         os.Getenv("FAKE_CRASH_OP"),
			failOp:          os.Getenv("FAKE_FAIL_OP"),
			badInitVersion:  os.Getenv("FAKE_BAD_INIT_VERSION") == "1",
			protocolVersion: protocolVersion,
		}))
	}
	os.Exit(m.Run())
}

const fakeBlobContent = "hello"

// fakeProviderConfig steers fake-provider misbehavior for the fallback tests.
type fakeProviderConfig struct {
	hangOp          string // read but never answer this op (drives the per-op timeout)
	crashOp         string // exit mid-op (drives crash fallback)
	failOp          string // answer this op with ok=false (drives the OpError path)
	badInitVersion  bool   // answer initialize with an unsupported protocol version
	protocolVersion int    // 0 accepts v1/v2; non-zero emulates a strict legacy provider
	seenVersions    chan<- int
	// objectCache selects how the fake answers the object-cache negotiation (see
	// the objectCache* constants). The zero value serves no object cache at all,
	// like a provider that ignores the capability.
	objectCache string
	// initCapabilities receives the capabilities core advertised, so a test can
	// assert what the bootstrap asked for.
	initCapabilities chan<- []string
	// requests receives every request line in the order the fake read it.
	requests chan<- string
	// runCredentialEcho echoes cache.CapabilityRunCredential when core lists it;
	// refuseAuthenticate then answers authenticate with ok=false and a message
	// that quotes the credential.
	runCredentialEcho  bool
	refuseAuthenticate bool
}

// The object-cache negotiation shapes a provider can present. Only the first is
// usable; the others are the ways a provider can be wrong, and core must refuse
// each of them without failing the session.
const (
	objectCacheServe     = "serve"      // real socket + echoed capability
	objectCacheNoEcho    = "no-echo"    // real socket, capability NOT echoed
	objectCacheRelative  = "relative"   // echoed capability, relative path
	objectCacheMissing   = "missing"    // echoed capability, path that does not exist
	objectCacheNotSocket = "not-socket" // echoed capability, path is a regular file
)

// serveFakeProvider runs the provider RPC loop over the given pipes and returns
// the exit code: the re-exec'd subprocess passes real stdio and exits with it,
// the in-process tests pass io.Pipe ends and ignore it.
func serveFakeProvider(stdin io.Reader, stdout io.Writer, cfg fakeProviderConfig) int {
	hangOp := cfg.hangOp
	crashOp := cfg.crashOp
	failOp := cfg.failOp
	providerProtocolVersion := cfg.protocolVersion
	if providerProtocolVersion == 0 {
		providerProtocolVersion = cache.ProviderProtocolVersion
	}
	var exchangeDir string
	var objectServer *objectcachetest.Server
	defer func() {
		if objectServer != nil {
			_ = objectServer.Close()
		}
	}()

	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 0, 64*1024), maxResponseBytes)
	out := stdout

	writeResp := func(resp *cache.ProviderResponse) {
		b, _ := json.Marshal(resp)
		_, _ = out.Write(append(b, '\n'))
	}
	ok := func(protocolVersion int, id int64, payload any) {
		raw, _ := cache.MarshalPayload(payload)
		writeResp(&cache.ProviderResponse{ProtocolVersion: protocolVersion, ID: id, OK: true, Payload: raw})
	}
	fail := func(protocolVersion int, id int64, code, msg string) {
		writeResp(&cache.ProviderResponse{
			ProtocolVersion: protocolVersion, ID: id, OK: false,
			Error: &cache.ProviderError{Code: code, Message: msg},
		})
	}

	for in.Scan() {
		line := in.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		req, diags := cache.ParseAndValidateProviderRequest(line)
		if req == nil {
			fmt.Fprintf(os.Stderr, "fake provider: bad request: %v\n", diags)
			continue
		}
		// A deployed v1 provider rejects a v2 envelope before it can return an
		// InitializeResult. Model that strict behavior by deliberately providing
		// no response for a mismatched request.
		if cfg.protocolVersion != 0 && req.ProtocolVersion != cfg.protocolVersion {
			continue
		}
		if cfg.seenVersions != nil {
			select {
			case cfg.seenVersions <- req.ProtocolVersion:
			default:
			}
		}
		if cfg.requests != nil {
			select {
			case cfg.requests <- string(line):
			default:
			}
		}
		switch {
		case string(req.Op) == crashOp:
			return 1
		case string(req.Op) == hangOp:
			continue // never answer
		case string(req.Op) == failOp:
			fail(req.ProtocolVersion, req.ID, "boom", "configured failure")
			continue
		}

		switch req.Op {
		case cache.OpInitialize:
			p, _ := cache.ParseAndValidateInitializeParams(req.Payload)
			if cfg.protocolVersion != 0 && (p == nil || p.ProtocolVersion != cfg.protocolVersion) {
				continue
			}
			if p != nil {
				exchangeDir = p.BlobExchangeDir
			}
			if cfg.initCapabilities != nil && p != nil {
				select {
				case cfg.initCapabilities <- append([]string(nil), p.Capabilities...):
				default:
				}
			}
			ver := req.ProtocolVersion
			if p != nil && providerProtocolVersion > req.ProtocolVersion && hasCapability(p.Capabilities, cache.CapabilityProviderProtocolV2) {
				ver = providerProtocolVersion
			}
			if cfg.badInitVersion {
				ver = 999
			}
			capabilities := []string{cache.CapabilityFindMissing}
			socketPath := ""
			if cfg.objectCache != "" && p != nil && hasCapability(p.Capabilities, cache.CapabilityObjectCache) {
				capabilities, socketPath = objectCacheAnswer(cfg.objectCache, capabilities, exchangeDir, &objectServer)
			}
			if cfg.runCredentialEcho && p != nil && hasCapability(p.Capabilities, cache.CapabilityRunCredential) {
				capabilities = append(capabilities, cache.CapabilityRunCredential)
			}
			ok(req.ProtocolVersion, req.ID, &cache.InitializeResult{
				ProtocolVersion:   ver,
				ProviderName:      "fake",
				ProviderVersion:   "9.9.9",
				Capabilities:      capabilities,
				ObjectCacheSocket: socketPath,
				Ready:             true,
			})
		case cache.OpAuthenticate:
			p, _ := cache.ParseAndValidateAuthenticateParams(req.Payload)
			switch {
			case p == nil:
				fail(req.ProtocolVersion, req.ID, "invalid-credential", "malformed authenticate")
			case cfg.refuseAuthenticate:
				fail(req.ProtocolVersion, req.ID, "unauthorized", "run credential "+p.Credential+" is not valid")
			default:
				ok(req.ProtocolVersion, req.ID, &cache.AuthenticateResult{})
			}
		case cache.OpPrefetch:
			n := 0
			if p, _ := cache.ParseAndValidatePrefetchParams(req.Payload); p != nil {
				n = len(p.Keys)
			}
			ok(req.ProtocolVersion, req.ID, &cache.PrefetchResult{Started: n})
		case cache.OpRestore:
			// Hybrid blob path: write the hit's blob into the exchange dir (core
			// would ingest it into its CAS) and return only the manifest over the pipe.
			digest := cache.DigestOf([]byte(fakeBlobContent))
			if path, okp := cache.BlobExchangePath(exchangeDir, digest); okp {
				_ = os.MkdirAll(filepath.Dir(path), 0o755)
				_ = os.WriteFile(path, []byte(fakeBlobContent), 0o644)
			}
			ok(req.ProtocolVersion, req.ID, &cache.RestoreResult{
				Status:   cache.RestoreHit,
				Result:   &cache.ActionResult{Status: "success"},
				Manifest: &cache.Manifest{Files: []cache.FileEntry{{Path: "dist/out.js", Digest: digest, Mode: 0o644, Size: int64(len(fakeBlobContent))}}},
			})
		case cache.OpUpload:
			accepted := true
			if p, _ := cache.ParseAndValidateUploadParams(req.Payload); p != nil && p.Manifest != nil {
				for _, f := range p.Manifest.Files {
					if path, okp := cache.BlobExchangePath(exchangeDir, f.Digest); okp {
						if _, err := os.Stat(path); err != nil {
							accepted = false
						}
					}
				}
			}
			ok(req.ProtocolVersion, req.ID, &cache.UploadResult{Accepted: accepted})
		case cache.OpMarkerLookup:
			ok(req.ProtocolVersion, req.ID, &cache.MarkerLookupResult{Found: true, Marker: &cache.RunMarker{SHA: "cafef00d"}})
		case cache.OpMarkerWrite:
			sha := "abc"
			if p, _ := cache.ParseAndValidateMarkerWriteParams(req.Payload); p != nil {
				sha = p.SHA
			}
			ok(req.ProtocolVersion, req.ID, &cache.MarkerWriteResult{Published: true, Marker: &cache.RunMarker{SHA: sha}})
		case cache.OpSummary:
			ok(req.ProtocolVersion, req.ID, &cache.SummaryResult{RestoredCount: 1, UploadedCount: 1})
		case cache.OpShutdown:
			ok(req.ProtocolVersion, req.ID, nil)
			return 0
		default:
			fail(req.ProtocolVersion, req.ID, "unsupported", "unknown op")
		}
	}
	return 0
}

// objectCacheAnswer builds the initialize answer for one object-cache shape: the
// capability list the provider echoes and the socket path it advertises. Only
// objectCacheServe produces a usable pair; the rest are the misconfigurations
// core must refuse.
func objectCacheAnswer(mode string, capabilities []string, exchangeDir string, server **objectcachetest.Server) ([]string, string) {
	switch mode {
	case objectCacheNoEcho:
		started, err := objectcachetest.Start(exchangeDir)
		if err != nil {
			return capabilities, ""
		}
		*server = started
		return capabilities, started.Path()
	case objectCacheRelative:
		return append(capabilities, cache.CapabilityObjectCache), filepath.Join("relative", "objects.sock")
	case objectCacheMissing:
		return append(capabilities, cache.CapabilityObjectCache), filepath.Join(exchangeDir, "absent.sock")
	case objectCacheNotSocket:
		path := filepath.Join(exchangeDir, "regular-file.sock")
		_ = os.MkdirAll(exchangeDir, 0o755)
		_ = os.WriteFile(path, []byte("not a socket"), 0o644)
		return append(capabilities, cache.CapabilityObjectCache), path
	default:
		started, err := objectcachetest.Start(exchangeDir)
		if err != nil {
			return capabilities, ""
		}
		*server = started
		return append(capabilities, cache.CapabilityObjectCache), started.Path()
	}
}

func hasCapability(capabilities []string, capability string) bool {
	for _, got := range capabilities {
		if got == capability {
			return true
		}
	}
	return false
}

// connectFake runs the fake provider in-process over pipes: identical protocol
// behavior to the spawned fake, minus the subprocess startup cost. opts adds
// session options.
func connectFake(t *testing.T, cfg fakeProviderConfig, opts ...Option) *Session {
	t.Helper()
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	go func() {
		serveFakeProvider(reqR, respW, cfg)
		_ = respW.Close() // the session's readLoop sees EOF, as on provider exit
		_ = reqR.Close()
	}()
	sess := Connect(reqW, respR, append([]Option{WithOpTimeout(5 * time.Second), WithShutdownTimeout(2 * time.Second)}, opts...)...)
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func spawnFake(t *testing.T, extraEnv ...string) *Session {
	t.Helper()
	env := append(os.Environ(), "PUTNAMI_TEST_FAKE_PROVIDER=1")
	env = append(env, extraEnv...)
	sess, err := Spawn(context.Background(), LaunchSpec{
		Command: os.Args[0],
		Env:     env,
	}, WithStderr(os.Stderr), WithOpTimeout(5*time.Second), WithShutdownTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("spawn fake provider: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func mustInit(t *testing.T, s *Session, exchangeDir string) *cache.InitializeResult {
	t.Helper()
	res, err := s.Initialize(context.Background(), &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolVersion,
		BlobExchangeDir: exchangeDir,
		Mode:            cache.ModeFull,
		Workspace:       "ws-id",
		Branch:          "main",
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return res
}

// TestSession_RoundTrip drives every op against the fake provider, including the
// hybrid CAS path (restore writes a blob, upload reads it back).
func TestSession_RoundTrip(t *testing.T) {
	exchangeDir := t.TempDir()
	s := connectFake(t, fakeProviderConfig{})

	init := mustInit(t, s, exchangeDir)
	if !init.Ready || init.ProviderVersion != "9.9.9" || init.ProtocolVersion != cache.ProviderProtocolVersion {
		t.Fatalf("unexpected initialize result: %+v", init)
	}

	key := strings.Repeat("a", cache.KeyLength)

	pf, err := s.Prefetch(context.Background(), &cache.PrefetchParams{Keys: []string{key}})
	if err != nil || pf.Started != 1 {
		t.Fatalf("prefetch: %+v err=%v", pf, err)
	}

	rr, err := s.Restore(context.Background(), &cache.RestoreParams{Key: key})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rr.Status != cache.RestoreHit || rr.Manifest == nil || len(rr.Manifest.Files) != 1 {
		t.Fatalf("unexpected restore result: %+v", rr)
	}
	// Verify the hybrid blob path: the provider wrote the blob into the exchange
	// dir (where core would ingest it into its CAS).
	digest := rr.Manifest.Files[0].Digest
	blobPath, ok := cache.BlobExchangePath(exchangeDir, digest)
	if !ok {
		t.Fatalf("bad digest from provider: %q", digest)
	}
	if got, err := os.ReadFile(blobPath); err != nil || string(got) != fakeBlobContent {
		t.Fatalf("blob not staged into the exchange dir: got %q err=%v", got, err)
	}

	// Upload references the same blob, now present in the CAS.
	up, err := s.Upload(context.Background(), &cache.UploadParams{
		Key:      key,
		Result:   &cache.ActionResult{Status: "success"},
		Manifest: rr.Manifest,
	})
	if err != nil || !up.Accepted {
		t.Fatalf("upload: %+v err=%v", up, err)
	}

	ml, err := s.LookupMarker(context.Background(), &cache.MarkerLookupParams{
		Workspace: "ws-id", Branch: "main", Commands: []string{"build"}, Selection: cache.RunMarkerSelectionAll,
	})
	if err != nil || !ml.Found || ml.Marker == nil {
		t.Fatalf("marker lookup: %+v err=%v", ml, err)
	}

	mw, err := s.WriteMarker(context.Background(), &cache.MarkerWriteParams{
		Workspace: "ws-id", Branch: "main", Commands: []string{"build"},
		Selection: cache.RunMarkerSelectionAll, SHA: "deadbeef",
	})
	if err != nil || !mw.Published || mw.Marker.SHA != "deadbeef" {
		t.Fatalf("marker write: %+v err=%v", mw, err)
	}

	sum, err := s.Summary(context.Background(), nil)
	if err != nil || sum.RestoredCount != 1 || sum.UploadedCount != 1 {
		t.Fatalf("summary: %+v err=%v", sum, err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestSession_InitialVersionNegotiation proves the first initialize request
// uses the v1 bootstrap and that a v2 provider then upgrades later requests.
// The fake selects v2 only after it sees CapabilityProviderProtocolV2 in the
// v1 initialize payload, so this covers both directions of the negotiation.
func TestSession_InitialVersionNegotiation(t *testing.T) {
	versions := make(chan int, 3)
	s := connectFake(t, fakeProviderConfig{seenVersions: versions})

	init := mustInit(t, s, t.TempDir())
	if init.ProtocolVersion != cache.ProviderProtocolVersion {
		t.Fatalf("initialize negotiated v%d, want v%d", init.ProtocolVersion, cache.ProviderProtocolVersion)
	}
	if _, err := s.Summary(context.Background(), nil); err != nil {
		t.Fatalf("summary after v2 negotiation: %v", err)
	}

	if got := <-versions; got != cache.ProviderProtocolMinVersion {
		t.Fatalf("initialize envelope version = %d, want v%d bootstrap", got, cache.ProviderProtocolMinVersion)
	}
	if got := <-versions; got != cache.ProviderProtocolVersion {
		t.Fatalf("post-initialize envelope version = %d, want negotiated v%d", got, cache.ProviderProtocolVersion)
	}
}

// TestSession_LegacyV1Provider exercises the real stdio subprocess path against
// a provider that strictly rejects every envelope and initialize body except
// v1. It therefore fails if the session ever starts (or later continues) at v2.
func TestSession_LegacyV1Provider(t *testing.T) {
	s := spawnFake(t, "FAKE_PROTOCOL_VERSION="+strconv.Itoa(cache.ProviderProtocolMinVersion))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	init, err := s.Initialize(ctx, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolVersion,
		BlobExchangeDir: t.TempDir(),
		Mode:            cache.ModeFull,
		Workspace:       "ws-id",
		Branch:          "main",
	})
	if err != nil {
		t.Fatalf("initialize strict v1 provider: %v", err)
	}
	if init.ProtocolVersion != cache.ProviderProtocolMinVersion {
		t.Fatalf("initialize negotiated v%d, want legacy v%d", init.ProtocolVersion, cache.ProviderProtocolMinVersion)
	}
	if _, err := s.Summary(ctx, nil); err != nil {
		t.Fatalf("summary through strict v1 provider: %v", err)
	}
}

// TestSession_Timeout verifies a per-op timeout surfaces as an error (the
// caller's signal to build locally) while leaving the session usable.
func TestSession_Timeout(t *testing.T) {
	s := spawnFake(t, "FAKE_HANG_OP=restore")
	mustInit(t, s, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.Restore(ctx, &cache.RestoreParams{Key: strings.Repeat("a", cache.KeyLength)})
	if err == nil {
		t.Fatal("expected restore to time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline-exceeded, got %v", err)
	}

	// The session survives a single op timeout: another op still works.
	if _, err := s.Summary(context.Background(), nil); err != nil {
		t.Fatalf("summary after timeout should succeed: %v", err)
	}
}

// TestSession_Crash verifies a provider crash fails the in-flight op and every
// subsequent op, so the build falls back locally instead of hanging.
func TestSession_Crash(t *testing.T) {
	s := spawnFake(t, "FAKE_CRASH_OP=restore")
	mustInit(t, s, t.TempDir())

	_, err := s.Restore(context.Background(), &cache.RestoreParams{Key: strings.Repeat("a", cache.KeyLength)})
	if err == nil {
		t.Fatal("expected restore to fail after provider crash")
	}

	// Subsequent ops fail fast on the dead session.
	if _, err := s.Summary(context.Background(), nil); err == nil {
		t.Fatal("expected ops on a dead session to fail")
	}

	// Close is safe on a dead session.
	if err := s.Close(); err != nil {
		t.Fatalf("close after crash: %v", err)
	}
}

// TestSession_OpError verifies a non-OK response becomes an OpError, distinct
// from a transport failure, and does not kill the session.
func TestSession_OpError(t *testing.T) {
	s := connectFake(t, fakeProviderConfig{failOp: "upload"})
	mustInit(t, s, t.TempDir())

	_, err := s.Upload(context.Background(), &cache.UploadParams{
		Key:      strings.Repeat("a", cache.KeyLength),
		Result:   &cache.ActionResult{Status: "success"},
		Manifest: &cache.Manifest{Files: nil},
	})
	var opErr *OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("expected *OpError, got %v", err)
	}
	if opErr.Code != "boom" || opErr.Op != cache.OpUpload {
		t.Fatalf("unexpected op error: %+v", opErr)
	}

	// A handled op failure leaves the session alive.
	if _, err := s.Summary(context.Background(), nil); err != nil {
		t.Fatalf("summary after op error should succeed: %v", err)
	}
}

// TestSession_InitProtocolMismatch verifies the runtime half of the version
// gate: an initialize result advertising an unsupported provider protocol
// version is rejected.
func TestSession_InitProtocolMismatch(t *testing.T) {
	s := connectFake(t, fakeProviderConfig{badInitVersion: true})
	_, err := s.Initialize(context.Background(), &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolVersion,
		BlobExchangeDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected initialize to reject an unsupported provider protocol version")
	}
}

// --- object cache negotiation ---

// TestInitialize_AdvertisesObjectCacheCapability pins the bootstrap half of the
// negotiation: core asks for the object cache in the SAME forward-tolerant
// capabilities list it uses for the v2 opt-in, so a provider that has never
// heard of it parses the bootstrap unchanged.
func TestInitialize_AdvertisesObjectCacheCapability(t *testing.T) {
	advertised := make(chan []string, 1)
	s := connectFake(t, fakeProviderConfig{initCapabilities: advertised})
	mustInit(t, s, t.TempDir())

	select {
	case got := <-advertised:
		if !hasCapability(got, cache.CapabilityObjectCache) {
			t.Fatalf("advertised capabilities = %v, want %q", got, cache.CapabilityObjectCache)
		}
		if !hasCapability(got, cache.CapabilityProviderProtocolV2) {
			t.Fatalf("advertised capabilities = %v, want the v2 opt-in kept", got)
		}
	default:
		t.Fatal("the provider never saw an initialize")
	}
}

// TestInitialize_NegotiatesTheObjectCacheSocket is the happy path: a provider
// that echoes the capability and listens on a real socket makes the path
// available to core, which will export it to every job.
func TestInitialize_NegotiatesTheObjectCacheSocket(t *testing.T) {
	s := connectFake(t, fakeProviderConfig{objectCache: objectCacheServe})
	init := mustInit(t, s, t.TempDir())

	if init.ObjectCacheSocket == "" {
		t.Fatal("the fake advertised no socket")
	}
	if got := s.ObjectCacheSocket(); got != init.ObjectCacheSocket {
		t.Fatalf("ObjectCacheSocket() = %q, want the negotiated %q", got, init.ObjectCacheSocket)
	}
	info, err := os.Stat(s.ObjectCacheSocket())
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("negotiated path is not a live socket: %v (%v)", err, info)
	}
}

// TestInitialize_RefusesAnUnusableObjectCacheSocket walks every way a provider
// can offer a socket core must not use. Each one leaves the object cache OFF
// while the session itself stays healthy — the feature fails closed, the run
// does not fail at all.
func TestInitialize_RefusesAnUnusableObjectCacheSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		why  string
	}{
		{"capability not echoed", objectCacheNoEcho, "a path without the echoed capability is not an answer to what core asked"},
		{"relative path", objectCacheRelative, "a job runs in its project directory, so a relative socket path is meaningless"},
		{"path does not exist", objectCacheMissing, "every job would dial a socket that can never connect"},
		{"path is not a socket", objectCacheNotSocket, "a regular file is not a provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := connectFake(t, fakeProviderConfig{objectCache: tc.mode})
			mustInit(t, s, t.TempDir())
			if got := s.ObjectCacheSocket(); got != "" {
				t.Fatalf("ObjectCacheSocket() = %q, want empty: %s", got, tc.why)
			}
			// The session is still usable: refusing the socket must not break the
			// ops the provider does serve.
			if _, err := s.Prefetch(context.Background(), &cache.PrefetchParams{Keys: nil}); err != nil {
				t.Fatalf("prefetch after a refused socket: %v", err)
			}
		})
	}
}

// TestInitialize_LegacyProviderServesNoObjectCache pins the compatibility
// promise: a provider that ignores the capability answers exactly as before and
// core simply has no object cache to export.
func TestInitialize_LegacyProviderServesNoObjectCache(t *testing.T) {
	s := connectFake(t, fakeProviderConfig{})
	init := mustInit(t, s, t.TempDir())
	if init.ObjectCacheSocket != "" {
		t.Fatalf("a provider that ignored the capability answered with a socket: %q", init.ObjectCacheSocket)
	}
	if got := s.ObjectCacheSocket(); got != "" {
		t.Fatalf("ObjectCacheSocket() = %q, want empty", got)
	}
}

// TestObjectCacheSocket_NilSessionIsSafe keeps the accessor usable on the
// local-only path, where callers hold no session.
func TestObjectCacheSocket_NilSessionIsSafe(t *testing.T) {
	var s *Session
	if got := s.ObjectCacheSocket(); got != "" {
		t.Fatalf("nil session socket = %q, want empty", got)
	}
}

// drainRequests returns the request lines the fake has read so far.
func drainRequests(requests <-chan string) []string {
	var lines []string
	for {
		select {
		case line := <-requests:
			lines = append(lines, line)
		default:
			return lines
		}
	}
}

// initializeLine is the exact bootstrap line core sends for mustInit's params
// with capabilities, the JSON array body.
func initializeLine(t *testing.T, exchangeDir, capabilities string) string {
	t.Helper()
	dir, err := json.Marshal(exchangeDir)
	if err != nil {
		t.Fatal(err)
	}
	return `{"protocolVersion":1,"id":1,"op":"initialize","payload":{"protocolVersion":1,"blobExchangeDir":` + string(dir) +
		`,"mode":"full","workspace":"ws-id","branch":"main","capabilities":[` + capabilities + `]}}`
}

// TestInitialize_WithoutARunCredentialIsUnchanged pins the local path byte for
// byte: without a run credential, or with an empty one, the bootstrap line is
// the one core sent before run credentials existed, and nothing follows it
// even when the provider would take a credential.
func TestInitialize_WithoutARunCredentialIsUnchanged(t *testing.T) {
	for name, opts := range map[string][]Option{"no option": nil, "empty credential": {WithRunCredential("")}} {
		t.Run(name, func(t *testing.T) {
			requests := make(chan string, 16)
			s := connectFake(t, fakeProviderConfig{requests: requests, runCredentialEcho: true}, opts...)
			exchangeDir := t.TempDir()
			mustInit(t, s, exchangeDir)
			if _, err := s.Prefetch(context.Background(), &cache.PrefetchParams{}); err != nil {
				t.Fatalf("prefetch: %v", err)
			}
			lines := drainRequests(requests)
			if len(lines) != 2 || !strings.Contains(lines[1], `"op":"prefetch"`) {
				t.Fatalf("requests = %q, want initialize then prefetch", lines)
			}
			if want := initializeLine(t, exchangeDir, `"provider-protocol-v2","object-cache"`); lines[0] != want {
				t.Fatalf("initialize line\n%s\nwant\n%s", lines[0], want)
			}
		})
	}
}

// TestInitialize_AuthenticatesOnceAfterTheEcho pins the order: with a run
// credential core lists run-credential, and a provider that echoes it
// receives exactly one authenticate carrying the credential, right after
// initialize and before any other op.
func TestInitialize_AuthenticatesOnceAfterTheEcho(t *testing.T) {
	const secret = "prc_fixture-run-credential"
	requests := make(chan string, 16)
	s := connectFake(t, fakeProviderConfig{requests: requests, runCredentialEcho: true}, WithRunCredential(secret))
	exchangeDir := t.TempDir()
	init := mustInit(t, s, exchangeDir)
	if !hasCapability(init.Capabilities, cache.CapabilityRunCredential) {
		t.Fatalf("the fake did not echo the capability: %v", init.Capabilities)
	}
	if _, err := s.Prefetch(context.Background(), &cache.PrefetchParams{}); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	lines := drainRequests(requests)
	if len(lines) != 3 {
		t.Fatalf("requests = %q, want initialize, authenticate, prefetch", lines)
	}
	if want := initializeLine(t, exchangeDir, `"provider-protocol-v2","object-cache","run-credential"`); lines[0] != want {
		t.Fatalf("initialize line\n%s\nwant\n%s", lines[0], want)
	}
	if want := `{"protocolVersion":2,"id":2,"op":"authenticate","payload":{"credential":"` + secret + `"}}`; lines[1] != want {
		t.Fatalf("authenticate line\n%s\nwant\n%s", lines[1], want)
	}
	if !strings.Contains(lines[2], `"op":"prefetch"`) {
		t.Fatalf("third request = %s, want prefetch", lines[2])
	}
}

// TestInitialize_NoEchoSendsNoAuthenticate keeps the credential with core when
// the provider does not take it: no authenticate, no line carrying the value,
// and the session serves as before.
func TestInitialize_NoEchoSendsNoAuthenticate(t *testing.T) {
	const secret = "prc_fixture-run-credential"
	requests := make(chan string, 16)
	s := connectFake(t, fakeProviderConfig{requests: requests}, WithRunCredential(secret))
	init := mustInit(t, s, t.TempDir())
	if hasCapability(init.Capabilities, cache.CapabilityRunCredential) {
		t.Fatalf("the fake echoed the capability: %v", init.Capabilities)
	}
	if _, err := s.Prefetch(context.Background(), &cache.PrefetchParams{}); err != nil {
		t.Fatalf("prefetch after no echo: %v", err)
	}
	lines := drainRequests(requests)
	if len(lines) != 2 {
		t.Fatalf("requests = %q, want initialize then prefetch", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, `"op":"authenticate"`) || strings.Contains(line, secret) {
			t.Fatalf("the credential left core without an echo: %s", line)
		}
	}
}

// TestInitialize_RefusedAuthenticateFailsInitialize makes a refused
// authenticate the same outcome as a failed initialize, and keeps a message
// that quotes the credential from carrying it into the error.
func TestInitialize_RefusedAuthenticateFailsInitialize(t *testing.T) {
	const secret = "prc_refused-run-credential"
	s := connectFake(t, fakeProviderConfig{runCredentialEcho: true, refuseAuthenticate: true}, WithRunCredential(secret))
	_, err := s.Initialize(context.Background(), &cache.InitializeParams{BlobExchangeDir: t.TempDir()})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != cache.OpAuthenticate {
		t.Fatalf("initialize = %v, want the authenticate OpError", err)
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("the error carries the credential: %v", err)
	}
}

// TestInitialize_MalformedRunCredentialSendsNothing fails Initialize before
// any line is written, without quoting the value. A prefetch after it is the
// first line the provider reads: the pipe keeps order, so an initialize sent
// before it would have been read first.
func TestInitialize_MalformedRunCredentialSendsNothing(t *testing.T) {
	for _, credential := range []string{"prc secret", "prc_\xff", strings.Repeat("a", cache.MaxRunCredentialBytes+1)} {
		requests := make(chan string, 16)
		s := connectFake(t, fakeProviderConfig{requests: requests, runCredentialEcho: true}, WithRunCredential(credential))
		_, err := s.Initialize(context.Background(), &cache.InitializeParams{BlobExchangeDir: t.TempDir()})
		if err == nil || strings.Contains(err.Error(), credential) {
			t.Fatalf("a malformed credential of %d bytes: %v", len(credential), err)
		}
		if _, err := s.Prefetch(context.Background(), &cache.PrefetchParams{}); err != nil {
			t.Fatalf("prefetch: %v", err)
		}
		if lines := drainRequests(requests); len(lines) != 1 || !strings.Contains(lines[0], `"op":"prefetch"`) {
			t.Fatalf("a malformed credential sent %q before the prefetch", lines)
		}
	}
}

// TestRunCredentialRuleMatchesTheRegistry keeps the two protocols' copies of
// the run-credential rule in step: the engine hands one value to both.
func TestRunCredentialRuleMatchesTheRegistry(t *testing.T) {
	if cache.MaxRunCredentialBytes != registry.MaxRunCredentialBytes {
		t.Fatalf("bounds differ: cache %d, registry %d", cache.MaxRunCredentialBytes, registry.MaxRunCredentialBytes)
	}
	values := []string{
		"", "prc_x", "prc x", "prc\tx", "prc_x\n", "prc\u0085x", "prc\u00a0x", "prc\u2028x", "prc\u3000x", "prc\u200bx",
		"prc_\u00e9", "prc_\xff", "\x00", strings.Repeat("a", cache.MaxRunCredentialBytes), strings.Repeat("a", cache.MaxRunCredentialBytes+1),
		strings.Repeat("\u00e9", cache.MaxRunCredentialBytes/2), strings.Repeat("\u00e9", cache.MaxRunCredentialBytes/2+1),
	}
	for _, value := range values {
		if got, want := cache.ValidRunCredential(value), registry.ValidRunCredential(value); got != want {
			t.Errorf("%.24q: cache %v, registry %v", value, got, want)
		}
	}
}
