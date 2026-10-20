package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/cache/objectcachetest"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
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
			negotiatedVersion := req.ProtocolVersion
			if p != nil && providerProtocolVersion >= cache.ProviderProtocolProvenanceVersion &&
				slices.Contains(p.Capabilities, cache.CapabilityProviderProtocolV2) {
				negotiatedVersion = providerProtocolVersion
			}
			if capabilitiesOut != "" && p != nil {
				_ = os.WriteFile(capabilitiesOut, []byte(strings.Join(p.Capabilities, ",")), 0o644)
			}
			// The object cache is served only when core asked for it, and the
			// socket is created under the blob-exchange directory core named.
			var capabilities []string
			socketPath := ""
			if objectCache && p != nil && slices.Contains(p.Capabilities, cache.CapabilityObjectCache) {
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
			if runCredentialEcho && p != nil && slices.Contains(p.Capabilities, cache.CapabilityRunCredential) {
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
// configuration from the LaunchSpec env that providerLaunchSpec built, so the
// env-forwarding contract stays covered;
// only exec.Command's own env application (stdlib behavior) goes unexercised.
// TestProviderRemoteCache_RestoreAndUpload keeps the real subprocess as the
// end-to-end spawn integration test.
func useInProcessFakeProvider(t *testing.T) {
	t.Helper()
	orig := remotecache.SpawnProviderSession
	remotecache.SpawnProviderSession = func(_ context.Context, spec cacheprovider.LaunchSpec, opts ...cacheprovider.Option) (*cacheprovider.Session, error) {
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
	t.Cleanup(func() { remotecache.SpawnProviderSession = orig })
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

// fakeProviderLaunch stands in for the runner's provider launch
// (scheduler.PrepareProviderLaunch): it expands the provider command's templates,
// working directory and environment the same way, and prepares no runtime.
// The tests of the launch itself pass the runner's launch instead.
func fakeProviderLaunch(_ context.Context, workspaceRoot string, ext *extension.ExtensionDescription, command string) (cacheprovider.LaunchSpec, error) {
	job := ext.Jobs[command]
	if job == nil || strings.TrimSpace(job.Command) == "" {
		return cacheprovider.LaunchSpec{}, fmt.Errorf("provider command %q of %s has no executable", command, ext.Name)
	}
	vars := extension.BuildTemplateVars(workspaceRoot, workspaceRoot, ext.Path, filepath.Join(workspaceRoot, ".putnami", "out"))
	vars[extensionproto.TemplateVarExtensionRuntime] = ext.RuntimeExecutable
	launch := cacheprovider.LaunchSpec{
		Command: extension.ExpandTemplateVars(job.Command, vars),
		Args:    make([]string, 0, len(job.Args)),
		Dir:     workspaceRoot,
		Env:     os.Environ(),
	}
	for _, arg := range job.Args {
		launch.Args = append(launch.Args, extension.ExpandTemplateVars(arg, vars))
	}
	if job.Cwd != "" {
		launch.Dir = extension.ExpandTemplateVars(job.Cwd, vars)
	}
	for _, key := range slices.Sorted(maps.Keys(job.Env)) {
		launch.Env = append(launch.Env, key+"="+extension.ExpandTemplateVars(job.Env[key], vars))
	}
	return launch, nil
}

// TestProviderRemoteCache_FilesLessRequiredInputRestore pins the required-input
// guard's asymmetry: a describe-style dependency — it writes the optional
// clients resource and produced no files — is a legitimate status-only hit
// and must be restored, while a gen-writing dependency with an empty
// manifest must still be rebuilt locally: downstream jobs need its .gen
// tree. Without the carve-out every client-less describe was a permanent
// remote miss (rejected on every build), even though its key was stable and
// its result deterministic.
