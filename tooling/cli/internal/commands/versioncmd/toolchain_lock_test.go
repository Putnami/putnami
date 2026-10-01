package versioncmd

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

func TestRefreshLockMetadataPinsDeclaredToolchainsAndCLIProtocol(t *testing.T) {
	const (
		goSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		bunSHA     = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		cliVersion = "1.2.3"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/go":
			_, _ = w.Write([]byte(`[{"version":"go1.26.2","files":[` +
				`{"filename":"go1.26.2.linux-amd64.tar.gz","os":"linux","arch":"amd64","kind":"archive","sha256":"` + goSHA + `"}] }]`))
		case "/bun":
			_, _ = w.Write([]byte(`{"assets":[{"name":"bun-linux-x64.zip","digest":"sha256:` + bunSHA + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "go.work"),
		"go 1.26.1 // minimum\ntoolchain go1.26.2 // CI\n")
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{"packageManager":"bun@1.3.14"}`)
	lf := &lockfile.LockFile{
		Version:    lockfile.FormatVersionV2,
		Extensions: map[string]lockfile.LockEntry{},
		Templates:  map[string]lockfile.LockEntry{},
	}
	lf.SetCLI(lockfile.LockEntry{Version: cliVersion})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}

	if err := RefreshLockMetadata(context.Background(), ws, "v"+cliVersion); err != nil {
		t.Fatal(err)
	}
	got, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != lockfile.FormatVersionV3 {
		t.Fatalf("lock version = %d, want v3", got.Version)
	}
	cli, _ := got.GetCLI()
	if cli.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Errorf("cli.protocolVersion = %d, want %d", cli.ProtocolVersion, protocolcli.ResultProtocolVersion)
	}
	wants := map[string]struct {
		version  string
		platform string
		digest   string
	}{
		"go":  {"1.26.2", "linux/amd64", goSHA},
		"bun": {"1.3.14", "linux/amd64", bunSHA},
	}
	for name, want := range wants {
		entry, ok := got.GetToolchain(name)
		if !ok || entry.Version != want.version || entry.Integrities[want.platform] != want.digest || entry.Source == "" {
			t.Errorf("toolchains.%s = %+v, want version=%s %s=%s", name, entry, want.version, want.platform, want.digest)
		}
	}
	spectest.Proves(t, "cli/toolchain-lock", "verified-release-metadata", "go-and-bun-pins-carry-version-source-and-digests")
}

func TestRefreshLockMetadataWithResultSkipsByteIdenticalLockWrite(t *testing.T) {
	const bunSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unchanged exact Bun pin must not use the network")
	})})

	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{"packageManager":"bun@1.4.0"}`)
	lf := lockfile.NewLockFile()
	lf.SetToolchain("bun", lockfile.LockEntry{
		Version:     "1.4.0",
		Integrities: map[string]string{"linux/amd64": bunSHA},
		Source:      "https://github.com/oven-sh/bun/releases/download/bun-v1.4.0/",
	})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, lockfile.LockFilename)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshLockMetadataWithResult(context.Background(), ws, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("byte-identical lock refresh reported a change")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("byte-identical lock refresh replaced the lock inode")
	}
}

func TestRefreshToolchainLockDropsNodeDeclarationAndStalePin(t *testing.T) {
	metadataRequests := 0
	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		metadataRequests++
		return nil, errors.New("unexpected toolchain metadata request")
	})})

	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{"engines":{"node":">=22 <23"}}`)
	lf := lockfile.NewLockFile()
	lf.SetToolchain("node", lockfile.LockEntry{
		Version:     "22.8.0",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)},
		Source:      "https://nodejs.org/dist/v22.8.0/",
	})

	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	if _, ok := lf.GetToolchain("node"); ok {
		t.Fatal("engines.node or its stale lock entry survived the toolchain refresh")
	}
	if len(lf.Toolchains) != 0 {
		t.Fatalf("toolchains = %+v, want none", lf.Toolchains)
	}
	if metadataRequests != 0 {
		t.Fatalf("engines.node caused %d metadata requests, want none", metadataRequests)
	}
	if lf.Version != lockfile.FormatVersionV4 {
		t.Fatalf("toolchain refresh downgraded lock v4 to v%d", lf.Version)
	}
	spectest.Proves(t, "cli/toolchain-lock", "supported-runtimes-only", "only-go-and-bun-declarations-are-locked")
}

// A pin no go.work or package.json declaration derives stays in the lock while
// a declared extension's runtime resolves it, verbatim and without a metadata
// request; a pin nothing resolves is dropped; a declaration still wins over the
// committed pin.
func TestRefreshToolchainLockKeepsPinsADeclaredExtensionResolves(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "extension-resolved-pins-survive", "refresh-keeps-only-pins-a-declared-extension-resolves")
	metadataRequests := 0
	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		metadataRequests++
		return nil, errors.New("a kept pin must not use the network")
	})})

	ws := t.TempDir()
	extRoot := filepath.Join(ws, "compiled")
	if err := os.MkdirAll(extRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteToolchainTestFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"name":"toolchains","includes":["compiled"]}`)
	mustWriteToolchainTestFile(t, filepath.Join(extRoot, "putnami.json"), `{"name":"@example/compiled"}`)
	mustWriteToolchainTestFile(t, filepath.Join(extRoot, "putnami.extension.json"), `{
  "name": "@example/compiled",
  "version": "1.0.0",
  "cliContract": 4,
  "runtime": {
    "executable": "compiled/example",
    "toolchains": {
      "compiler": {"lock": "go", "candidates": [{"from": "path", "path": "go"}], "probe": {"args": ["env", "GOVERSION"], "expect": "go{version}"}},
      "runner": {"lock": "bun", "optional": true, "candidates": [{"from": "path", "path": "bun"}], "probe": {"args": ["--version"], "expect": "{version}"}}
    },
    "runToolchains": ["runner"],
    "prepare": {"command": "{extensionRoot}/bin/prepare", "inputs": ["cmd/**"], "toolchains": ["compiler"]}
  },
  "commands": {"hello": {"description": "Say hello", "run": [{"id": "hello", "task": "hello"}]}},
  "tasks": {"hello": {"kind": "command", "command": "{extensionRuntime}", "cache": false}}
}`)
	pins := map[string]lockfile.LockEntry{
		"go":   {Version: "1.26.1", Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)}, Source: "https://go.dev/dl/"},
		"bun":  {Version: "1.4.0", Integrities: map[string]string{"darwin/arm64": strings.Repeat("b", 64)}, Source: "https://example.test/bun/"},
		"node": {Version: "22.8.0", Integrities: map[string]string{"linux/amd64": strings.Repeat("c", 64)}, Source: "https://nodejs.org/dist/v22.8.0/"},
	}
	lf := lockfile.NewLockFile()
	for name, entry := range pins {
		lf.SetToolchain(name, entry)
	}

	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go", "bun"} {
		got, ok := lf.GetToolchain(name)
		want := pins[name]
		if !ok || got.Version != want.Version || got.Source != want.Source || !maps.Equal(got.Integrities, want.Integrities) {
			t.Errorf("toolchains.%s = %+v (present %v), want the committed pin %+v kept verbatim", name, got, ok, want)
		}
	}
	if got, ok := lf.GetToolchain("node"); ok {
		t.Errorf("toolchains.node = %+v survived; no declaration or declared extension resolves it", got)
	}
	if metadataRequests != 0 {
		t.Fatalf("keeping extension-resolved pins made %d metadata requests, want none", metadataRequests)
	}

	// A declaration derives its pin whatever an extension resolves.
	const goSHA = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/go" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"version":"go1.26.2","files":[` +
			`{"filename":"go1.26.2.linux-amd64.tar.gz","os":"linux","arch":"amd64","kind":"archive","sha256":"` + goSHA + `"}]}]`))
	}))
	defer server.Close()
	withToolchainTestClient(t, nil)
	withToolchainTestEndpoints(t, server.URL)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "go.work"), "go 1.26.2\n")
	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	if got, _ := lf.GetToolchain("go"); got.Version != "1.26.2" || got.Integrities["linux/amd64"] != goSHA {
		t.Errorf("toolchains.go = %+v, want the go.work declaration re-resolved to 1.26.2", got)
	}
}

// Discovering the extensions that resolve a pin is part of the refresh: a
// workspace that cannot load fails it, and the committed pins stay untouched.
func TestRefreshToolchainLockFailsClosedWhenTheWorkspaceCannotLoad(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteToolchainTestFile(t, filepath.Join(ws, "app", "putnami.json"), `{"name":"app","tasks":{"lint":{"timeoutMs":0}}}`)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"name":"broken","includes":["app"]}`)
	pinned := lockfile.LockEntry{Version: "1.26.1", Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)}, Source: "https://go.dev/dl/"}
	lf := lockfile.NewLockFile()
	lf.SetToolchain("go", pinned)

	err := refreshToolchainLock(context.Background(), ws, lf)
	if err == nil || !strings.Contains(err.Error(), "load workspace for extension toolchains") {
		t.Fatalf("refresh over an unloadable workspace = %v, want a workspace load failure", err)
	}
	if got, ok := lf.GetToolchain("go"); !ok || got.Version != pinned.Version {
		t.Fatalf("toolchains.go = %+v (present %v), want the committed pin untouched", got, ok)
	}
}

func TestRefreshToolchainLockReusesPinnedBunEntryWithoutNetwork(t *testing.T) {
	const bunSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	bunCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bun" {
			bunCalls++
			// Mirrors the credential-free CI reality: unauthenticated
			// api.github.com requests rate-limit to 403.
			http.Error(w, "rate limited", http.StatusForbidden)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{"packageManager":"bun@1.4.0"}`)
	lf := lockfile.NewLockFile()
	lf.SetToolchain("bun", lockfile.LockEntry{
		Version:     "1.4.0",
		Integrities: map[string]string{"linux/amd64": bunSHA},
		Source:      "https://github.com/oven-sh/bun/releases/download/bun-v1.4.0/",
	})
	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	if bunCalls != 0 {
		t.Fatalf("Bun release API called %d times; a satisfying exact lock must stay pinned", bunCalls)
	}
	entry, _ := lf.GetToolchain("bun")
	if entry.Version != "1.4.0" || entry.Integrities["linux/amd64"] != bunSHA || entry.Source == "" {
		t.Errorf("bun entry = %+v, want the pinned 1.4.0 entry reused verbatim", entry)
	}
}

func TestRefreshToolchainLockReResolvesBunWhenPinDiffers(t *testing.T) {
	const bunSHA = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bun" {
			_, _ = w.Write([]byte(`{"assets":[{"name":"bun-linux-x64.zip","digest":"sha256:` + bunSHA + `"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{"packageManager":"bun@1.4.0"}`)
	lf := lockfile.NewLockFile()
	lf.SetToolchain("bun", lockfile.LockEntry{
		Version:     "1.3.14",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("1", 64)},
		Source:      "https://github.com/oven-sh/bun/releases/download/bun-v1.3.14/",
	})
	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	entry, _ := lf.GetToolchain("bun")
	if entry.Version != "1.4.0" || entry.Integrities["linux/amd64"] != bunSHA {
		t.Errorf("bun entry = %+v, want a freshly resolved 1.4.0 entry", entry)
	}
}

func TestReadToolchainDeclarationsRejectsMalformedPackageJSON(t *testing.T) {
	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{`)
	_, err := readToolchainDeclarations(ws)
	if err == nil || !strings.Contains(err.Error(), "parse package.json toolchains") {
		t.Fatalf("error = %v", err)
	}
}

// A metadata endpoint that accepts the connection, answers with headers, and
// then never produces body bytes must not be able to wedge `putnami install`:
// the install context is cancelable but deadline-free, so the bound has to come
// from the client itself.
func TestToolchainMetadataFetchIsBoundedWhenServerStalls(t *testing.T) {
	t.Setenv(extension.HTTPTimeoutEnv, "300ms")

	// abandon releases the handler unconditionally at cleanup: without it an
	// unbounded client (the regression) would leave the handler and
	// server.Close() blocked on each other until the test binary is killed,
	// turning a clear failure into a hang.
	abandon := make(chan struct{})
	clientGaveUp := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select { // stall: headers are sent, body bytes never are
		case <-r.Context().Done():
			close(clientGaveUp)
		case <-abandon:
		}
	}))
	t.Cleanup(func() {
		close(abandon)
		server.Close()
	})

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := get(context.Background(), server.URL+"/stalled")
		done <- result{err: err, elapsed: time.Since(start)}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("get returned no error after %s; the stalled fetch must fail", got.elapsed)
		}
		var timeoutErr interface{ Timeout() bool }
		isTimeout := errors.Is(got.err, context.DeadlineExceeded) ||
			(errors.As(got.err, &timeoutErr) && timeoutErr.Timeout())
		if !isTimeout {
			t.Fatalf("error = %v, want a timeout", got.err)
		}
		if !strings.Contains(got.err.Error(), server.URL) {
			t.Errorf("error %q does not name the endpoint", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("get never returned: toolchain metadata fetch is unbounded")
	}

	// A hang detector, not a latency budget: the handler observes the drop
	// through r.Context, which needs the server's goroutines scheduled, and a
	// short bound let host load fail the test for an answer that was on its
	// way.
	select {
	case <-clientGaveUp:
	case <-time.After(60 * time.Second):
		t.Fatal("the client never dropped the stalled connection: the handler's request context did not end within 60s of the fetch timing out")
	}
}

// The client timeout bounds a stalled peer, but it must not swallow
// cancellation: Ctrl-C during a fetch has to abort long before the timeout.
func TestToolchainMetadataFetchStaysCancelable(t *testing.T) {
	t.Setenv(extension.HTTPTimeoutEnv, "60s") // far longer than this test may run

	abandon := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-abandon:
		}
	}))
	t.Cleanup(func() {
		close(abandon)
		server.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := get(ctx, server.URL+"/stalled")
		done <- err
	}()
	time.AfterFunc(50*time.Millisecond, cancel)
	defer cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("get ignored cancellation")
	}
}

// The default (production) client must carry the registry bound, never the
// process-wide zero-timeout http.DefaultClient.
func TestToolchainClientIsBoundedAndConfigurable(t *testing.T) {
	t.Setenv(extension.HTTPTimeoutEnv, "")
	client := toolchainClient()
	if client == http.DefaultClient {
		t.Fatal("toolchain fetches use http.DefaultClient, which has no timeout")
	}
	if client.Timeout != extension.DefaultHTTPTimeout {
		t.Errorf("default timeout = %s, want %s", client.Timeout, extension.DefaultHTTPTimeout)
	}

	t.Setenv(extension.HTTPTimeoutEnv, "5s")
	if got := toolchainClient().Timeout; got != 5*time.Second {
		t.Errorf("timeout with %s=5s is %s, want 5s", extension.HTTPTimeoutEnv, got)
	}

	injected := &http.Client{Timeout: time.Second}
	withToolchainTestClient(t, injected)
	if toolchainClient() != injected {
		t.Error("the test seam no longer overrides the client")
	}
}

// An over-cap document is rejected as oversized rather than truncated into a
// document that only fails later as a parse/checksum error.
func TestToolchainMetadataFetchRejectsOversizedResponse(t *testing.T) {
	withToolchainMaxMetadataBytes(t, 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), 65))
	}))
	defer server.Close()

	_, err := get(context.Background(), server.URL+"/oversized")
	if err == nil {
		t.Fatal("oversized response accepted")
	}
	if !strings.Contains(err.Error(), "exceeds the 64 byte metadata limit") {
		t.Fatalf("error = %v, want an explicit oversized-response error", err)
	}

	// A document exactly at the cap is still served in full.
	atCap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), 64))
	}))
	defer atCap.Close()
	body, err := get(context.Background(), atCap.URL+"/at-cap")
	if err != nil || len(body) != 64 {
		t.Fatalf("get at the cap = %d bytes, %v", len(body), err)
	}
}

func withToolchainTestClient(t *testing.T, client *http.Client) {
	t.Helper()
	old := toolchainHTTPClient
	toolchainHTTPClient = client
	t.Cleanup(func() { toolchainHTTPClient = old })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withToolchainMaxMetadataBytes(t *testing.T, limit int64) {
	t.Helper()
	old := toolchainMaxMetadataBytes
	toolchainMaxMetadataBytes = limit
	t.Cleanup(func() { toolchainMaxMetadataBytes = old })
}

func withToolchainTestEndpoints(t *testing.T, base string) {
	t.Helper()
	oldGoDownloads, oldGoSource := goDownloadsURL, goSourceURL
	oldBunAPI, oldBunRelease := bunReleaseAPIURL, bunReleaseURL
	goDownloadsURL = base + "/go"
	goSourceURL = base + "/go-downloads/"
	bunReleaseAPIURL = func(string) string { return base + "/bun" }
	bunReleaseURL = func(version string) string { return base + "/bun/v" + version + "/" }
	t.Cleanup(func() {
		goDownloadsURL, goSourceURL = oldGoDownloads, oldGoSource
		bunReleaseAPIURL, bunReleaseURL = oldBunAPI, oldBunRelease
	})
}

func mustWriteToolchainTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A source workspace declares `cli.source: "workspace"` and no version. The
// metadata refresh must leave that entry exactly as committed: stamping a
// protocolVersion would claim an output protocol for bytes that do not exist,
// and rewriting the entry would erase the sentinel — either way the next
// `putnami install` would dirty a committed lock.
func TestRefreshLockMetadataLeavesTheSourceWorkspaceSentinelIntact(t *testing.T) {
	const bunSHA = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("an unchanged exact Bun pin must not use the network")
	})})

	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{"packageManager":"bun@1.3.14"}`)
	lf := &lockfile.LockFile{
		Version:    lockfile.FormatVersionV3,
		Extensions: map[string]lockfile.LockEntry{},
		Templates:  map[string]lockfile.LockEntry{},
		Toolchains: map[string]lockfile.LockEntry{
			"bun": {Version: "1.3.14", Integrities: map[string]string{"linux/amd64": bunSHA}, Source: "https://example.test/bun"},
		},
	}
	lf.SetCLI(lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}

	// The running version is deliberately non-empty: a naive `sameCLIVersion`
	// check on an empty pinned version must not be the only thing preventing the
	// stamp.
	changed, err := RefreshLockMetadataWithResult(context.Background(), ws, "9.9.9")
	if err != nil {
		t.Fatalf("RefreshLockMetadataWithResult: %v", err)
	}
	if changed {
		t.Error("refreshing a source workspace's already-exact lock must write nothing")
	}
	after, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refresh rewrote a committed sentinel lock:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	got, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	cli, ok := got.GetCLI()
	if !ok || !cli.IsWorkspaceSource() {
		t.Fatalf("cli entry = %+v, %v; want the source-workspace sentinel", cli, ok)
	}
	if cli.ProtocolVersion != 0 {
		t.Errorf("cli.protocolVersion = %d, want 0: a source workspace has no published artifact", cli.ProtocolVersion)
	}
	if cli.Version != "" {
		t.Errorf("cli.version = %q, want empty", cli.Version)
	}
}

// The implicit first-use install pins a declared toolchain the committed lock
// has no pin for, and leaves every committed entry as it is: a pin that
// differs from its declaration, a pin nothing declares, and the CLI entry.
// With nothing missing it reads no metadata and writes nothing, and without a
// lock it creates none.
func TestFillMissingToolchainPinsPinsOnlyWhatTheLockLacks(t *testing.T) {
	const (
		goSHA      = "abababababababababababababababababababababababababababababababab"
		cliVersion = "1.2.3"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/go" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"version":"go1.26.2","files":[` +
			`{"filename":"go1.26.2.linux-amd64.tar.gz","os":"linux","arch":"amd64","kind":"archive","sha256":"` + goSHA + `"}] }]`))
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "go.work"), "go 1.26.1\ntoolchain go1.26.2\n")
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{"packageManager":"bun@1.3.14"}`)
	lf := &lockfile.LockFile{
		Version:    lockfile.FormatVersionV3,
		Extensions: map[string]lockfile.LockEntry{},
		Templates:  map[string]lockfile.LockEntry{},
	}
	lf.SetCLI(lockfile.LockEntry{Version: cliVersion})
	committedBun := lockfile.LockEntry{Version: "1.3.0", Integrities: map[string]string{"linux/amd64": goSHA}, Source: "https://example.test/bun/"}
	lf.SetToolchain("bun", committedBun)
	undeclared := lockfile.LockEntry{Version: "9.9.9", Integrities: map[string]string{"linux/amd64": goSHA}, Source: "https://example.test/other/"}
	lf.SetToolchain("other", undeclared)
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}

	changed, err := FillMissingToolchainPins(context.Background(), ws)
	if err != nil || !changed {
		t.Fatalf("FillMissingToolchainPins = %v, %v; want the missing go pin written", changed, err)
	}
	got, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != lockfile.FormatVersionV3 {
		t.Errorf("lock version = %d, want the committed v3", got.Version)
	}
	if entry, ok := got.GetToolchain("go"); !ok || entry.Version != "1.26.2" || entry.Integrities["linux/amd64"] != goSHA || entry.Source == "" {
		t.Errorf("toolchains.go = %+v, want the declared 1.26.2 with its vendor digest", entry)
	}
	for name, want := range map[string]lockfile.LockEntry{"bun": committedBun, "other": undeclared} {
		if entry, _ := got.GetToolchain(name); !maps.Equal(entry.Integrities, want.Integrities) || entry.Version != want.Version || entry.Source != want.Source {
			t.Errorf("toolchains.%s = %+v, want the committed %+v", name, entry, want)
		}
	}
	if cli, _ := got.GetCLI(); cli.ProtocolVersion != 0 {
		t.Errorf("cli.protocolVersion = %d, want the committed entry without a stamp", cli.ProtocolVersion)
	}

	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("a lock with no missing pin must not use the network")
	})})
	before, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := FillMissingToolchainPins(context.Background(), ws); err != nil || changed {
		t.Fatalf("second FillMissingToolchainPins = %v, %v; want no change", changed, err)
	}
	after, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a lock with no missing pin was rewritten")
	}

	lockless := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(lockless, "go.work"), "go 1.26.1\n")
	if changed, err := FillMissingToolchainPins(context.Background(), lockless); err != nil || changed {
		t.Fatalf("FillMissingToolchainPins without a lock = %v, %v; want no change", changed, err)
	}
	if _, err := os.Stat(filepath.Join(lockless, lockfile.LockFilename)); !os.IsNotExist(err) {
		t.Fatalf("a workspace without a lock got one: stat err %v", err)
	}
	spectest.Proves(t, "cli/toolchain-lock", "implicit-install-fills-missing-pins",
		"a-missing-pin-is-filled-and-every-other-entry-is-kept")
}

// An explicit install pins the declared release before the workspace
// installers install it: a pin that differs from its declaration is replaced,
// a missing one is written, and every other entry is kept. With every pin at
// its declaration it reads no metadata and writes nothing, and without a lock
// it creates none.
func TestPinDeclaredToolchainsRepinsADeclarationTheLockDoesNotMatch(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "install-pins-before-installers", "a-declaration-the-lock-does-not-match-is-repinned")
	const (
		goSHA  = "abababababababababababababababababababababababababababababababab"
		bunSHA = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/go":
			_, _ = w.Write([]byte(`[{"version":"go1.26.2","files":[` +
				`{"filename":"go1.26.2.linux-amd64.tar.gz","os":"linux","arch":"amd64","kind":"archive","sha256":"` + goSHA + `"}] }]`))
		case "/bun":
			_, _ = w.Write([]byte(`{"assets":[{"name":"bun-linux-x64.zip","digest":"sha256:` + bunSHA + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "go.work"), "go 1.26.1\ntoolchain go1.26.2\n")
	mustWriteToolchainTestFile(t, filepath.Join(ws, "package.json"), `{"packageManager":"bun@1.3.14"}`)
	lf := &lockfile.LockFile{
		Version:    lockfile.FormatVersionV3,
		Extensions: map[string]lockfile.LockEntry{},
		Templates:  map[string]lockfile.LockEntry{},
	}
	lf.SetCLI(lockfile.LockEntry{Version: "1.2.3"})
	lf.SetToolchain("go", lockfile.LockEntry{Version: "1.25.7", Integrities: map[string]string{"linux/amd64": bunSHA}, Source: "https://example.test/go/"})
	undeclared := lockfile.LockEntry{Version: "9.9.9", Integrities: map[string]string{"linux/amd64": goSHA}, Source: "https://example.test/other/"}
	lf.SetToolchain("other", undeclared)
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}

	changed, err := PinDeclaredToolchains(context.Background(), ws)
	if err != nil || !changed {
		t.Fatalf("PinDeclaredToolchains = %v, %v; want the stale go pin and the missing bun pin written", changed, err)
	}
	got, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	if entry, _ := got.GetToolchain("go"); entry.Version != "1.26.2" || entry.Integrities["linux/amd64"] != goSHA {
		t.Errorf("toolchains.go = %+v, want the declared 1.26.2 with its vendor digest", entry)
	}
	if entry, _ := got.GetToolchain("bun"); entry.Version != "1.3.14" || entry.Integrities["linux/amd64"] != bunSHA {
		t.Errorf("toolchains.bun = %+v, want the declared 1.3.14 with its vendor digest", entry)
	}
	if entry, _ := got.GetToolchain("other"); entry.Version != undeclared.Version || entry.Source != undeclared.Source {
		t.Errorf("toolchains.other = %+v, want the committed %+v", entry, undeclared)
	}
	if cli, _ := got.GetCLI(); cli.Version != "1.2.3" || cli.ProtocolVersion != 0 {
		t.Errorf("cli entry = %+v, want the committed entry", cli)
	}

	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("a lock that pins every declaration must not use the network")
	})})
	before, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := PinDeclaredToolchains(context.Background(), ws); err != nil || changed {
		t.Fatalf("second PinDeclaredToolchains = %v, %v; want no change", changed, err)
	}
	after, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a lock that pins every declaration was rewritten")
	}

	lockless := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(lockless, "go.work"), "go 1.26.1\n")
	if changed, err := PinDeclaredToolchains(context.Background(), lockless); err != nil || changed {
		t.Fatalf("PinDeclaredToolchains without a lock = %v, %v; want no change", changed, err)
	}
	if _, err := os.Stat(filepath.Join(lockless, lockfile.LockFilename)); !os.IsNotExist(err) {
		t.Fatalf("a workspace without a lock got one: stat err %v", err)
	}
}
