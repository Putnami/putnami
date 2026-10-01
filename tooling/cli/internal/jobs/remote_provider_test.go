package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/cache/objectcachetest"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/store"
)

const fakeProviderBlobContent = "provider artifact"

// fakeProviderVersion mimics a provider's SHA-stamped publish versions
// (0.0.0-<sha>-<sha>). It is deliberately lexically minimal: SHA stamps admit
// no order, so resolution must accept any of them — a semver MinVersion floor
// that a cleanup removed once rejected newer builds as "older".
const fakeProviderVersion = "0.0.0-00000000-0000000"

// fakeProviderExtensionName is deliberately NOT a first-party name. A
// cleanup deleted the product name from the cache-provider gate: any
// extension declaring the protocol's reserved provider command serves the
// remote cache, so the suite's provider must be one core has never heard of.
const fakeProviderExtensionName = "@acme/cache"

func TestMain(m *testing.M) {
	// A fixture task (fixtureTask) is this binary running the steps it was
	// handed, in place of a shell script.
	if script := os.Getenv(fixtureScriptEnv); script != "" {
		os.Exit(runFixtureScript(script, os.Args[1:]))
	}
	// An extension runtime (fixtureproc.Binary) is this binary answering the
	// CLI's runtime handshake with the document its environment carries.
	if info := os.Getenv(fakeRuntimeInfoEnv); info != "" && len(os.Args) == 3 &&
		os.Args[1] == "__putnami" && os.Args[2] == "runtime-info" {
		fmt.Println(info)
		os.Exit(0)
	}
	if os.Getenv("PUTNAMI_JOBS_FAKE_PROVIDER") == "1" {
		serveJobsFakeProvider(os.Stdin, os.Stdout, os.Getenv)
		return
	}
	// The same re-exec pattern for the other side of the provider: a JOB that
	// consumes the object cache through the socket core exported to it.
	if os.Getenv(objectCacheClientEnv) == "1" {
		runObjectCacheClientJob(os.Getenv(objectCacheJobResultEnv))
		return
	}
	// The ambient-tool version probe re-execs this binary as a stand-in for the
	// toolchain it identifies. It runs here, before m.Run parses flags, because
	// the probe passes `--version` — which the testing flag set would reject —
	// and because a child that must never return cannot be a test the harness
	// waits on.
	if mode, ok := toolProbeHelperMode(os.Args[0]); ok {
		runToolProbeHelper(mode)
		return
	}
	code := m.Run()
	fixtureproc.Remove()
	os.Exit(code)
}

// serveJobsFakeProvider runs the fake provider RPC loop over the given pipes.
// The re-exec'd subprocess passes real stdio and os.Getenv; the in-process
// tests (see useInProcessFakeProvider) pass io.Pipe ends and a lookup over the
// LaunchSpec env, so the providerLaunchSpec→provider env chain stays asserted
// without paying ~1s of race-runtime startup per spawned subprocess.
func serveJobsFakeProvider(stdin io.Reader, stdout io.Writer, getenv func(string) string) {
	markerSHA := getenv("FAKE_MARKER_SHA")
	markerOut := getenv("FAKE_MARKER_OUT")
	markerLookupOut := getenv("FAKE_MARKER_LOOKUP_OUT")
	uploadOut := getenv("FAKE_UPLOAD_OUT")
	envOut := getenv("FAKE_ENV_OUT")
	restoreEmpty := getenv("FAKE_RESTORE_EMPTY") == "1"
	restoreClientOutput := getenv("FAKE_RESTORE_CLIENT_OUTPUT")
	restoreStatus := getenv("FAKE_RESTORE_STATUS")
	restoreChannel := cache.Channel(getenv("FAKE_RESTORE_CHANNEL"))
	providerProtocolVersion := cache.ProviderProtocolVersion
	if getenv("FAKE_PROVIDER_LEGACY") == "1" {
		providerProtocolVersion = cache.ProviderProtocolMinVersion
	}
	objectCache := getenv("FAKE_OBJECT_CACHE") == "1"
	objectChannel := cache.Channel(getenv("FAKE_OBJECT_CHANNEL"))
	capabilitiesOut := getenv("FAKE_CAPABILITIES_OUT")
	// FAKE_OPS_OUT receives one line per request: the op, and for authenticate
	// the credential it carried. FAKE_RUN_CREDENTIAL_ECHO echoes the
	// run-credential capability; FAKE_AUTHENTICATE_FAIL refuses authenticate.
	opsOut := getenv("FAKE_OPS_OUT")
	runCredentialEcho := getenv("FAKE_RUN_CREDENTIAL_ECHO") == "1"
	authenticateFail := getenv("FAKE_AUTHENTICATE_FAIL") == "1"
	var objectServer *objectcachetest.Server
	defer func() {
		if objectServer != nil {
			_ = objectServer.Close()
		}
	}()
	restoreCountOut := getenv("FAKE_RESTORE_COUNT_OUT")
	restoreReady := getenv("FAKE_RESTORE_READY")
	restoreRelease := getenv("FAKE_RESTORE_RELEASE")
	if restoreStatus == "" {
		restoreStatus = "success"
	}
	exchangeDir := ""
	var restoredCount, uploadedCount int
	var restoredBytes, uploadedBytes int64
	in := bufio.NewScanner(stdin)
	out := stdout

	write := func(resp *cache.ProviderResponse) {
		b, _ := json.Marshal(resp)
		_, _ = out.Write(append(b, '\n'))
	}
	ok := func(protocolVersion int, id int64, payload any) {
		raw, _ := cache.MarshalPayload(payload)
		write(&cache.ProviderResponse{ProtocolVersion: protocolVersion, ID: id, OK: true, Payload: raw})
	}

	for in.Scan() {
		req, diags := cache.ParseAndValidateProviderRequest(in.Bytes())
		if req == nil {
			write(&cache.ProviderResponse{
				ProtocolVersion: cache.ProviderProtocolVersion,
				ID:              0,
				OK:              false,
				Error:           &cache.ProviderError{Code: "bad-request", Message: diagString(diags)},
			})
			continue
		}
		if opsOut != "" {
			entry := string(req.Op)
			if p, _ := cache.ParseAndValidateAuthenticateParams(req.Payload); req.Op == cache.OpAuthenticate && p != nil {
				entry += " " + p.Credential
			}
			if f, err := os.OpenFile(opsOut, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				_, _ = fmt.Fprintln(f, entry)
				_ = f.Close()
			}
		}
		switch req.Op {
		case cache.OpAuthenticate:
			if authenticateFail {
				write(&cache.ProviderResponse{
					ProtocolVersion: req.ProtocolVersion,
					ID:              req.ID,
					OK:              false,
					Error:           &cache.ProviderError{Code: "unauthorized", Message: "run credential refused"},
				})
				continue
			}
			ok(req.ProtocolVersion, req.ID, &cache.AuthenticateResult{})
		case cache.OpInitialize:
			p, _ := cache.ParseAndValidateInitializeParams(req.Payload)
			if p != nil {
				exchangeDir = p.BlobExchangeDir
			}
			if envOut != "" {
				_ = os.WriteFile(envOut, []byte(strings.Join([]string{
					getenv("PUTNAMI_CACHE_URL"),
					getenv("PUTNAMI_CACHE_TOKEN"),
					getenv("PUTNAMI_CACHE_MODE"),
				}, "|")), 0o644)
			}
			negotiatedVersion := req.ProtocolVersion
			if p != nil && providerProtocolVersion >= cache.ProviderProtocolProvenanceVersion &&
				containsString(p.Capabilities, cache.CapabilityProviderProtocolV2) {
				negotiatedVersion = providerProtocolVersion
			}
			if capabilitiesOut != "" && p != nil {
				_ = os.WriteFile(capabilitiesOut, []byte(strings.Join(p.Capabilities, ",")), 0o644)
			}
			// The object cache is served only when core asked for it, and the
			// socket is created under the blob-exchange directory core named.
			var capabilities []string
			socketPath := ""
			if objectCache && p != nil && containsString(p.Capabilities, cache.CapabilityObjectCache) {
				serverOpts := []objectcachetest.Option{}
				if objectChannel != "" {
					serverOpts = append(serverOpts, objectcachetest.WithChannel(objectChannel))
				}
				if started, err := objectcachetest.Start(exchangeDir, serverOpts...); err == nil {
					objectServer = started
					capabilities = append(capabilities, cache.CapabilityObjectCache)
					socketPath = started.Path()
				}
			}
			if runCredentialEcho && p != nil && containsString(p.Capabilities, cache.CapabilityRunCredential) {
				capabilities = append(capabilities, cache.CapabilityRunCredential)
			}
			ok(req.ProtocolVersion, req.ID, &cache.InitializeResult{
				ProtocolVersion:   negotiatedVersion,
				ProviderName:      fakeProviderExtensionName,
				ProviderVersion:   fakeProviderVersion,
				Capabilities:      capabilities,
				ObjectCacheSocket: socketPath,
				Ready:             true,
			})
		case cache.OpPrefetch:
			started := 0
			if p, _ := cache.ParseAndValidatePrefetchParams(req.Payload); p != nil {
				started = len(p.Keys)
			}
			ok(req.ProtocolVersion, req.ID, &cache.PrefetchResult{Started: started})
		case cache.OpRestore:
			if restoreCountOut != "" {
				if f, err := os.OpenFile(restoreCountOut, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
					_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
					_ = f.Close()
				}
			}
			if restoreReady != "" {
				_ = os.WriteFile(restoreReady, []byte("restore-started"), 0o644)
			}
			if restoreRelease != "" {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(restoreRelease); err == nil || time.Now().After(deadline) {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			if restoreEmpty {
				// A status-only entry: a hit whose manifest has zero files, as the
				// upload path shares for lint/test/describe results (see
				// prepareUpload). No blob is staged; the exchange carries only the
				// key→result mapping. FAKE_RESTORE_CLIENT_OUTPUT makes the result
				// still name a generated clients directory, the case that must stay
				// rejected: materializeCachedOutput would delete it.
				actionResult := &cache.ActionResult{Status: restoreStatus, DurationMs: 1500}
				if restoreClientOutput != "" {
					actionResult.Data = map[string]any{"clientOutput": restoreClientOutput}
				}
				restoredCount++
				res := &cache.RestoreResult{
					Status:   cache.RestoreHit,
					Result:   actionResult,
					Manifest: &cache.Manifest{Files: []cache.FileEntry{}},
				}
				applyFakeRestoreProvenance(res, restoreChannel, req.ProtocolVersion)
				ok(req.ProtocolVersion, req.ID, res)
				continue
			}
			digest := cache.DigestOf([]byte(fakeProviderBlobContent))
			if path, ok := cache.BlobExchangePath(exchangeDir, digest); ok {
				// Stage the blob atomically (temp + rename), like a real provider:
				// concurrent restores share this content-addressed path, so a
				// non-atomic write would let a peer's Materialize read a truncated file.
				writeExchangeBlobAtomically(path, []byte(fakeProviderBlobContent))
			}
			restoredCount++
			restoredBytes += int64(len(fakeProviderBlobContent))
			res := &cache.RestoreResult{
				Status: cache.RestoreHit,
				Result: &cache.ActionResult{
					Status:     "success",
					DurationMs: 1500,
					SizeBytes:  int64(len(fakeProviderBlobContent)),
				},
				Manifest: &cache.Manifest{Files: []cache.FileEntry{{
					Path:   "dist/out.js",
					Digest: digest,
					Mode:   0o644,
					Size:   int64(len(fakeProviderBlobContent)),
				}}},
			}
			applyFakeRestoreProvenance(res, restoreChannel, req.ProtocolVersion)
			ok(req.ProtocolVersion, req.ID, res)
		case cache.OpUpload:
			accepted := false
			key := ""
			fileCount := 0
			if p, _ := cache.ParseAndValidateUploadParams(req.Payload); p != nil && p.Manifest != nil {
				accepted = true
				key = p.Key
				fileCount = len(p.Manifest.Files)
				for _, f := range p.Manifest.Files {
					path, ok := cache.BlobExchangePath(exchangeDir, f.Digest)
					if !ok {
						accepted = false
						continue
					}
					if _, err := os.Stat(path); err != nil {
						accepted = false
					}
				}
			}
			if uploadOut != "" {
				_ = os.WriteFile(uploadOut, []byte(fmt.Sprintf("%t:%s:%d", accepted, key, fileCount)), 0o644)
			}
			if accepted {
				uploadedCount++
				if p, _ := cache.ParseAndValidateUploadParams(req.Payload); p != nil && p.Manifest != nil {
					for _, f := range p.Manifest.Files {
						uploadedBytes += f.Size
					}
				}
			}
			ok(req.ProtocolVersion, req.ID, &cache.UploadResult{Accepted: accepted})
		case cache.OpMarkerLookup:
			if markerLookupOut != "" {
				_ = os.WriteFile(markerLookupOut, []byte(markerSHA), 0o644)
			}
			ok(req.ProtocolVersion, req.ID, &cache.MarkerLookupResult{
				Found:  markerSHA != "",
				Marker: &cache.RunMarker{SHA: markerSHA},
			})
		case cache.OpMarkerWrite:
			sha := ""
			if p, _ := cache.ParseAndValidateMarkerWriteParams(req.Payload); p != nil {
				sha = p.SHA
			}
			if markerOut != "" {
				_ = os.WriteFile(markerOut, []byte(sha), 0o644)
			}
			ok(req.ProtocolVersion, req.ID, &cache.MarkerWriteResult{Published: true, Marker: &cache.RunMarker{SHA: sha}})
		case cache.OpSummary:
			ok(req.ProtocolVersion, req.ID, &cache.SummaryResult{
				RestoredCount: restoredCount,
				RestoredBytes: restoredBytes,
				UploadedCount: uploadedCount,
				UploadedBytes: uploadedBytes,
			})
		case cache.OpShutdown:
			ok(req.ProtocolVersion, req.ID, nil)
			return
		default:
			write(&cache.ProviderResponse{
				ProtocolVersion: req.ProtocolVersion,
				ID:              req.ID,
				OK:              false,
				Error:           &cache.ProviderError{Code: "unsupported", Message: string(req.Op)},
			})
		}
	}
}

func applyFakeRestoreProvenance(res *cache.RestoreResult, channel cache.Channel, protocolVersion int) {
	if res == nil || channel == "" || !cache.ProviderProtocolHasProvenance(protocolVersion) {
		return
	}
	res.Channel = channel
	res.ProducerIdentity = "fake-provider-identity"
	if channel == cache.ChannelTrusted {
		res.Producer = cache.ProducerCI
	} else {
		res.Producer = cache.ProducerDeveloper
	}
}

// useInProcessFakeProvider reroutes provider spawning to run the fake provider
// in-process over pipes for the duration of the test. The fake reads its
// configuration and the PUTNAMI_CACHE_* passthrough from the LaunchSpec env
// that providerLaunchSpec built, so the env-forwarding contract stays covered;
// only exec.Command's own env application (stdlib behavior) goes unexercised.
// TestProviderRemoteCache_RestoreAndUpload keeps the real subprocess as the
// end-to-end spawn integration test.
func useInProcessFakeProvider(t *testing.T) {
	t.Helper()
	orig := spawnProviderSession
	spawnProviderSession = func(_ context.Context, spec cacheprovider.LaunchSpec, opts ...cacheprovider.Option) (*cacheprovider.Session, error) {
		env := make(map[string]string, len(spec.Env))
		for _, kv := range spec.Env {
			if k, v, found := strings.Cut(kv, "="); found {
				env[k] = v // last entry wins, matching process env semantics
			}
		}
		reqR, reqW := io.Pipe()
		respR, respW := io.Pipe()
		go func() {
			serveJobsFakeProvider(reqR, respW, func(k string) string { return env[k] })
			_ = respW.Close() // the session's readLoop sees EOF, as on provider exit
			_ = reqR.Close()
		}()
		return cacheprovider.Connect(reqW, respR, opts...), nil
	}
	t.Cleanup(func() { spawnProviderSession = orig })
}

func diagString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// writeExchangeBlobAtomically stages content at a content-addressed exchange
// path the way a real provider does: write a sibling temp file, then rename it
// into place. The rename is atomic, so a concurrent reader sees either the old
// state or the fully-written blob — never a partial one. Re-staging the same
// digest (every restore here uses the same blob) is harmless.
func writeExchangeBlobAtomically(path string, content []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "blob-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}

func fakeProviderExtension(t *testing.T, env map[string]string) *extension.ExtensionDescription {
	t.Helper()
	extRoot := t.TempDir()
	jobEnv := map[string]string{"PUTNAMI_JOBS_FAKE_PROVIDER": "1"}
	for k, v := range env {
		jobEnv[k] = v
	}
	return &extension.ExtensionDescription{
		Name:     fakeProviderExtensionName,
		Version:  fakeProviderVersion,
		Path:     extRoot,
		Commands: map[string]string{cache.ProviderCommandName: "provider"},
		Jobs: map[string]*extension.JobDefinition{
			cache.ProviderCommandName: {
				ExtensionName: fakeProviderExtensionName,
				Name:          cache.ProviderCommandName,
				Command:       os.Args[0],
				Cwd:           "{workspaceRoot}",
				Env:           jobEnv,
			},
		},
	}
}

func enableProviderRemoteCache(t *testing.T) {
	t.Helper()
	t.Setenv(cacheURLEnv, "https://cache.example")
}

func TestLoadRemoteCache_UnconfiguredIsSilent(t *testing.T) {
	// No cache.json and no PUTNAMI_CACHE_* override: local-only is the expected
	// default, not a degradation, so there must be no cache and no notice.
	// Pin the override to empty: the suite must assert the unconfigured default
	// even when the invoking environment (CI, the scripts/bench-cache.sh benchmark)
	// exports a real PUTNAMI_CACHE_URL.
	t.Setenv(cacheURLEnv, "")
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), nil, nil, store.CacheTrustAny)
	if remote != nil {
		t.Fatalf("unconfigured workspace must build locally (nil cache), got %+v", remote)
	}
	if notice != "" {
		t.Fatalf("unconfigured remote cache must be silent, got notice %q", notice)
	}
}

func TestLoadRemoteCache_UnconfiguredWithProviderIsSilent(t *testing.T) {
	// Installing a provider extension alone must not activate remote caching. A
	// provider command is usable only after cache.json or PUTNAMI_CACHE_URL opts in.
	t.Setenv(cacheURLEnv, "")
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), []*extension.ExtensionDescription{
		fakeProviderExtension(t, nil),
	}, nil, store.CacheTrustAny)
	if remote != nil {
		t.Fatalf("unconfigured workspace must build locally (nil cache), got %+v", remote)
	}
	if notice != "" {
		t.Fatalf("unconfigured remote cache must be silent, got notice %q", notice)
	}
}

func TestLoadRemoteCache_ConfiguredWithoutProviderEmitsNotice(t *testing.T) {
	enableProviderRemoteCache(t)
	// Caching is configured but no installed extension declares the provider
	// command, so it cannot be served. The degradation policy requires a
	// one-line notice rather than a silent fall back to local-only.
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), nil, nil, store.CacheTrustAny)
	if remote != nil {
		t.Fatalf("a configured cache with no provider must build locally, got %+v", remote)
	}
	if notice == "" {
		t.Fatal("a configured-but-unusable remote cache must emit a one-line notice")
	}
	// The actionable identity is the RESERVED COMMAND, not a vendor: any
	// extension declaring it can serve the cache.
	if !strings.Contains(notice, cache.ProviderCommandName) {
		t.Fatalf("notice should name the reserved provider command so the hint is actionable, got %q", notice)
	}
}

// TestLoadRemoteCache_SkippedExtensionCitesRootCause proves an UNLOADABLE
// extension is still distinguishable from an uninstalled one now that the gate
// no longer knows which product ships the provider: the notice carries the skip
// records discovery collected, so a contract-too-new manifest is a fixable
// fault instead of a silent local-only run.
func TestLoadRemoteCache_SkippedExtensionCitesRootCause(t *testing.T) {
	enableProviderRemoteCache(t)
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), nil, []extension.SkippedExtension{
		{Ref: "@acme/cache", Name: "@acme/cache", Reason: errors.New("requires a newer putnami (contract 4 > 3)")},
	}, store.CacheTrustAny)
	if remote != nil {
		t.Fatalf("a configured cache with no loadable provider must build locally, got %+v", remote)
	}
	for _, want := range []string{"@acme/cache", "requires a newer putnami"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice %q should cite the skip cause %q", notice, want)
		}
	}
}

// TestLoadRemoteCache_AmbiguousProviderFails pins the one resolution failure
// that survived: two extensions declaring the reserved command. Picking one
// silently would make the active provider a function of discovery order.
func TestLoadRemoteCache_AmbiguousProviderFails(t *testing.T) {
	enableProviderRemoteCache(t)
	first := fakeProviderExtension(t, nil)
	second := fakeProviderExtension(t, nil)
	second.Name = "@other/cache"
	_, _, err := loadProviderRemoteCache(context.Background(), t.TempDir(),
		[]*extension.ExtensionDescription{first, second}, nil, store.CacheTrustAny)
	if !errors.Is(err, extension.ErrProviderAmbiguous) {
		t.Fatalf("error = %v, want ErrProviderAmbiguous", err)
	}
}

func TestLoadRemoteCache_NoneIgnoresConfiguredRemote(t *testing.T) {
	enableProviderRemoteCache(t)
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), nil, nil, store.CacheTrustNone)
	if remote != nil || notice != "" {
		t.Fatalf("cache trust none = remote %#v, notice %q; want remote ignored silently", remote, notice)
	}
}

func TestLoadRemoteCache_ShaStampedProviderVersionIsAccepted(t *testing.T) {
	enableProviderRemoteCache(t)
	// The fake provider's version is a lexically-minimal SHA stamp
	// (fakeProviderVersion). Every published cloud build carries such a stamp,
	// and stamps are unordered, so the gate must accept it without a notice: a
	// semver version floor here once rejected builds NEWER than the floor as
	// "older", permanently disabling the remote cache.
	remote, notice := LoadRemoteCache(context.Background(), t.TempDir(), []*extension.ExtensionDescription{
		fakeProviderExtension(t, nil),
	}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("a SHA-stamped provider version must be accepted, got %+v", remote)
	}
	defer remote.Close()
	if notice != "" {
		t.Fatalf("a compatible provider must load without a notice, got %q", notice)
	}
}

func TestLoadRemoteCacheProvider_RunMarkers(t *testing.T) {
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	wsRoot := t.TempDir()
	markerOut := filepath.Join(wsRoot, "marker.txt")
	ext := fakeProviderExtension(t, map[string]string{
		"FAKE_MARKER_SHA": "cafef00d",
		"FAKE_MARKER_OUT": markerOut,
	})

	remote, _ := LoadRemoteCache(context.Background(), wsRoot, []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected provider-backed remote cache, got %+v", remote)
	}
	defer remote.Close()

	sha, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, "")
	if !ok || sha != "cafef00d" {
		t.Fatalf("LookupRunMarker = %q, %v; want cafef00d, true", sha, ok)
	}

	// The scheduler calls Stop() at the end of the run; the CLI publishes the
	// success marker afterwards (recordSuccessfulBuild). Stop must not tear down
	// the provider session, or the publish below silently writes nothing.
	remote.Stop()

	remote.PublishRunMarker(context.Background(), "ws-id", "main", []string{"build"}, "", "deadbeef", "")
	got, err := os.ReadFile(markerOut)
	if err != nil {
		t.Fatalf("read marker write: %v", err)
	}
	if string(got) != "deadbeef" {
		t.Fatalf("published marker = %q, want deadbeef", got)
	}
}

func TestLoadRemoteCacheProvider_CITrustRejectsUnprovenRunMarker(t *testing.T) {
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	wsRoot := t.TempDir()
	lookupOut := filepath.Join(wsRoot, "marker-lookup.txt")
	ext := fakeProviderExtension(t, map[string]string{
		"FAKE_MARKER_SHA":        "cafef00d",
		"FAKE_MARKER_LOOKUP_OUT": lookupOut,
	})

	remote, _ := LoadRemoteCache(context.Background(), wsRoot, []*extension.ExtensionDescription{ext}, nil, store.CacheTrustCI)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected provider-backed remote cache, got %+v", remote)
	}
	defer remote.Close()

	if sha, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); ok || sha != "" {
		t.Fatalf("LookupRunMarker = %q, %v; want unproven marker rejected", sha, ok)
	}
	if _, err := os.Stat(lookupOut); !os.IsNotExist(err) {
		t.Fatalf("CI trust contacted provider for an unproven marker: %v", err)
	}
}

// readProviderOps returns the ops the fake recorded in path, then removes it.
func readProviderOps(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// A run credential reaches the cache provider in one authenticate, right
// after initialize and before any other op, and never through the provider's
// launch environment: that environment is the one a run without a credential
// gets. Without a credential nothing is authenticated.
func TestLoadRemoteCacheProvider_RunCredentialAuthenticatesOverTheRPC(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "providers-receive-it-over-rpc", "cache-provider-authenticates-after-the-capability")
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	inProcess := spawnProviderSession
	var launchEnv []string
	spawnProviderSession = func(ctx context.Context, spec cacheprovider.LaunchSpec, opts ...cacheprovider.Option) (*cacheprovider.Session, error) {
		launchEnv = append([]string(nil), spec.Env...)
		return inProcess(ctx, spec, opts...)
	}
	t.Cleanup(func() { spawnProviderSession = inProcess })

	const secret = "prc_jobs-run-credential"
	wsRoot := t.TempDir()
	opsOut := filepath.Join(wsRoot, "ops.txt")
	ext := fakeProviderExtension(t, map[string]string{"FAKE_RUN_CREDENTIAL_ECHO": "1", "FAKE_OPS_OUT": opsOut})
	run := func(options ...RemoteCacheOption) ([]string, []string) {
		t.Helper()
		launchEnv = nil
		remote, notice := LoadRemoteCache(context.Background(), wsRoot, []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny, options...)
		if remote == nil || notice != "" {
			t.Fatalf("expected a provider-backed remote cache, got %+v, notice %q", remote, notice)
		}
		remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, "")
		remote.Close()
		return readProviderOps(t, opsOut), launchEnv
	}

	plainOps, plainEnv := run()
	runOps, runEnv := run(WithCacheRunCredential(secret))
	if len(plainOps) < 2 || plainOps[0] != "initialize" || plainOps[1] != "marker-lookup" || strings.Contains(strings.Join(plainOps, "\n"), "authenticate") {
		t.Fatalf("ops without a run credential = %q, want initialize then marker-lookup and no authenticate", plainOps)
	}
	if len(runOps) != len(plainOps)+1 || runOps[0] != "initialize" || runOps[1] != "authenticate "+secret ||
		strings.Join(runOps[2:], "\n") != strings.Join(plainOps[1:], "\n") {
		t.Fatalf("ops with a run credential = %q, want %q with one authenticate right after initialize", runOps, plainOps)
	}
	if len(runEnv) == 0 || strings.Join(runEnv, "\n") != strings.Join(plainEnv, "\n") {
		t.Fatal("the run credential changed the provider's launch environment")
	}
	for _, entry := range runEnv {
		if strings.Contains(entry, secret) {
			t.Fatal("the run credential reached the provider's launch environment")
		}
	}
}

// A provider that does not echo the capability receives no authenticate and
// keeps serving the run.
func TestLoadRemoteCacheProvider_RunCredentialWithoutEchoSendsNothing(t *testing.T) {
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	const secret = "prc_jobs-run-credential"
	wsRoot := t.TempDir()
	opsOut := filepath.Join(wsRoot, "ops.txt")
	ext := fakeProviderExtension(t, map[string]string{"FAKE_MARKER_SHA": "cafef00d", "FAKE_OPS_OUT": opsOut})
	remote, _ := LoadRemoteCache(context.Background(), wsRoot, []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny, WithCacheRunCredential(secret))
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v", remote)
	}
	if sha, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); !ok || sha != "cafef00d" {
		t.Fatalf("LookupRunMarker = %q, %v; want the session to keep serving", sha, ok)
	}
	remote.Close()
	for _, op := range readProviderOps(t, opsOut) {
		if strings.HasPrefix(op, "authenticate") || strings.Contains(op, secret) {
			t.Fatalf("a provider that did not echo the capability received %q", op)
		}
	}
}

// A refused authenticate degrades like a failed initialize: the session is
// closed, its exchange directory removed, the run builds locally, and the
// provider is not started again.
func TestLoadRemoteCacheProvider_RefusedAuthenticateBuildsLocally(t *testing.T) {
	enableProviderRemoteCache(t)
	useInProcessFakeProvider(t)
	const secret = "prc_jobs-run-credential"
	wsRoot := t.TempDir()
	opsOut := filepath.Join(wsRoot, "ops.txt")
	ext := fakeProviderExtension(t, map[string]string{
		"FAKE_RUN_CREDENTIAL_ECHO": "1", "FAKE_AUTHENTICATE_FAIL": "1", "FAKE_MARKER_SHA": "cafef00d", "FAKE_OPS_OUT": opsOut,
	})
	remote, _ := LoadRemoteCache(context.Background(), wsRoot, []*extension.ExtensionDescription{ext}, nil, store.CacheTrustAny, WithCacheRunCredential(secret))
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v", remote)
	}
	for attempt := range 2 {
		if sha, ok := remote.LookupRunMarker(context.Background(), "ws-id", "main", []string{"build"}, ""); ok || sha != "" {
			t.Fatalf("attempt %d: LookupRunMarker = %q, %v; want a local build", attempt, sha, ok)
		}
	}
	p := remote.provider
	p.mu.Lock()
	initErr, session, exchangeDir := p.initErr, p.session, p.exchangeDir
	p.mu.Unlock()
	if initErr == nil || session != nil || exchangeDir != "" {
		t.Fatalf("after a refused authenticate: initErr=%v session=%v exchangeDir=%q; want the failed-initialize state", initErr, session != nil, exchangeDir)
	}
	remote.Close()
	if ops := readProviderOps(t, opsOut); strings.Join(ops, ",") != "initialize,authenticate "+secret+",shutdown" {
		t.Fatalf("ops = %q, want one initialize, one authenticate, then shutdown", ops)
	}
}

func TestRemoteRestoreEstimatedCostUsesLeaseBreakEvenFloor(t *testing.T) {
	job := cacheableJob("build", "/proj", "proj", "proj")
	floor := cache.DefaultBreakEven.MinDurationMs

	job.ExpectedWallMs = floor - 1
	if store.WorthCoalescing(remoteRestoreEstimatedCost(job)) {
		t.Fatal("a known sub-floor remote restore should bypass leasing")
	}
	job.ExpectedWallMs = floor
	if !store.WorthCoalescing(remoteRestoreEstimatedCost(job)) {
		t.Fatal("a remote restore at the break-even floor should coalesce")
	}
	job.ExpectedWallMs = 0
	if !store.WorthCoalescing(remoteRestoreEstimatedCost(job)) {
		t.Fatal("an unknown cold-key cost should remain eligible for coalescing")
	}
}

// TestProviderRemoteCache_FilesLessRequiredInputRestore pins the required-input
// guard's asymmetry: a describe-style dependency — it writes the optional
// clients resource and produced no files — is a legitimate status-only hit
// and must be restored, while a gen-writing dependency with an empty
// manifest must still be rebuilt locally: downstream jobs need its .gen
// tree. Without the carve-out every client-less describe was a permanent
// remote miss (rejected on every build), even though its key was stable and
// its result deterministic.
