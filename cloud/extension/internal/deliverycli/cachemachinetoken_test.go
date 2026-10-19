package deliverycli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	cache "go.putnami.dev/protocol/cache"
)

// A cloud agent session or a CI job holds one secret, PUTNAMI_CLOUD_TOKEN, a
// read-only workspace machine token, and no signed-in user. These tests pin
// when the cache provider sends that token to the cache server, and every case
// where it must not.

const testMachineToken = "pkt_workspace_read_5c1e"

// machineTokenSession isolates a test from the host: an empty Putnami home (no
// stored user session), no hosted launcher, no cache token override, no
// metadata selector, and cloudToken in PUTNAMI_CLOUD_TOKEN. It returns the
// Putnami home.
func machineTokenSession(t *testing.T, cloudToken string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PUTNAMI_HOME", home)
	t.Setenv(cloudTokenEnv, cloudToken)
	t.Setenv(remotecache.TokenEnv, "")
	t.Setenv(remotecache.TokenSourceEnv, "")
	t.Setenv(remotecache.URLEnv, "")
	t.Setenv(remotecache.ModeEnv, "")
	t.Setenv(remotecache.ReadOnlyEnv, "false")
	t.Setenv(SessionReporterIngestURLEnv, "")
	return home
}

// signIn stores a user session in the Putnami home, the way
// `putnami cloud login` does.
func signIn(t *testing.T, home string) {
	t.Helper()
	stored := &clicore.StoredToken{AccessToken: "user-session-access-token", TokenType: "Bearer"}
	if err := clicore.WriteAuth(stored, map[string]string{"PUTNAMI_HOME": home}); err != nil {
		t.Fatal(err)
	}
}

// canonicalRecipe returns the canonical `putnami cloud token --for cache`
// recipe with a stand-in `putnami` that prints recipe-token and leaves a
// marker file. ran reports whether the recipe was invoked.
func canonicalRecipe(t *testing.T) (command []string, ran func() bool) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := "#!/bin/sh\n: > '" + marker + "'\nprintf recipe-token\n"
	executable := filepath.Join(dir, "putnami")
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil { //nolint:gosec // G306: the stand-in must be executable
		t.Fatal(err)
	}
	return []string{executable, "cloud", "token", "--for", "cache"}, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}
}

// writeCacheConfig writes .putnami/cache.json under root with the given cache
// URL and token source, and returns the bytes written.
func writeCacheConfig(t *testing.T, root, url string, token map[string]any) []byte {
	t.Helper()
	body := map[string]any{"enabled": true, "url": url, "mode": "full"}
	if token != nil {
		body["token"] = token
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(CachePath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CachePath(root), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

// bearerRecordingCacheServer serves reads and records the bearer of every
// request. refuse, when set, answers every negotiate with that status and a
// message that echoes the bearer back.
func bearerRecordingCacheServer(t *testing.T, refuse int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bearers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get(cache.AuthorizationHeader), "Bearer ")
		mu.Lock()
		bearers = append(bearers, bearer)
		mu.Unlock()
		switch {
		case r.URL.Path != cache.NegotiatePath:
			http.NotFound(w, r)
		case refuse != 0:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(refuse)
			_ = json.NewEncoder(w).Encode(cache.ErrorResponse{
				ProtocolVersion: cache.ProtocolVersion,
				Code:            cache.CodeUnauthorized,
				Message:         "token " + bearer + " is revoked",
			})
		default:
			_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bearers...)
	}
}

// restoreOnce drives one restore of a key the server does not hold, which
// negotiates it once, and returns the restore status.
func restoreOnce(t *testing.T, s *providerSession) cache.RestoreStatus {
	t.Helper()
	resp := s.restore(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 2, Op: cache.OpRestore},
		&cache.RestoreParams{Key: strings.Repeat("e", cache.KeyLength)})
	result, diags := cache.ParseAndValidateRestoreResult(resp.Payload)
	if result == nil {
		t.Fatalf("invalid restore result: %v", diags)
	}
	s.bg.Wait()
	return result.Status
}

// mustReadyInit initializes a session that serves the remote cache.
func mustReadyInit(t *testing.T, s *providerSession) {
	t.Helper()
	if !mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir(), Mode: cache.ModeFull}).Ready {
		t.Fatal("initialize: Ready = false, want a serving remote cache")
	}
}

// With no signed-in user, the canonical recipe cannot mint a bearer. The
// provider reads the cache with PUTNAMI_CLOUD_TOKEN instead: the recipe is not
// invoked, and the token reaches neither the log nor the cache config file.
func TestCacheReadsWithTheCloudTokenWhenNoUserIsSignedIn(t *testing.T) {
	for _, test := range []struct {
		name   string
		recipe bool
		linked bool
	}{
		{name: "canonical recipe in the cache config", recipe: true},
		{name: "cache URL with no token source"},
		// The documented agent setup: a linked workspace with no
		// .putnami/cache.json, which the provider fills with the managed
		// cache URL and the canonical recipe.
		{name: "linked workspace with no cache config", linked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			machineTokenSession(t, testMachineToken)
			srv, bearers := bearerRecordingCacheServer(t, 0)
			root := t.TempDir()
			ran := func() bool { return false }
			var written []byte
			switch {
			case test.recipe:
				var command []string
				command, ran = canonicalRecipe(t)
				written = writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})
			case test.linked:
				writeLinkedManifest(t, root)
				// The linked default runs `putnami` from PATH: a stand-in
				// there records a call, so a regression never runs the
				// real CLI.
				var command []string
				command, ran = canonicalRecipe(t)
				t.Setenv("PATH", filepath.Dir(command[0])+string(os.PathListSeparator)+os.Getenv("PATH"))
			default:
				t.Setenv(remotecache.URLEnv, srv.URL)
			}

			var logw bytes.Buffer
			s := newProviderSession(root, &logw)
			defer s.close()
			if test.linked {
				s.managedCacheURL = srv.URL
			}
			mustReadyInit(t, s)
			if status := restoreOnce(t, s); status != cache.RestoreMiss {
				t.Fatalf("restore status = %q, want a miss answered by the server", status)
			}

			if got := bearers(); len(got) != 1 || got[0] != testMachineToken {
				t.Fatalf("the cache server saw %d request(s); want exactly one, carrying the machine token", len(got))
			}
			if ran() {
				t.Fatal("the canonical recipe ran; it needs a signed-in user and must be skipped")
			}
			if strings.Contains(logw.String(), testMachineToken) {
				t.Fatal("the provider log carries the machine token")
			}
			if test.recipe {
				after, err := os.ReadFile(CachePath(root))
				if err != nil || !bytes.Equal(after, written) {
					t.Fatalf("the cache config changed (read error %v); the machine token is never persisted", err)
				}
			} else if _, err := os.Stat(CachePath(root)); !os.IsNotExist(err) {
				t.Fatalf("a cache config was written (stat error %v); the machine token is never persisted", err)
			}
		})
	}
}

// PUTNAMI_CACHE_TOKEN stays the explicit override: when both variables are
// set, the cache token is the bearer and the cloud token stays off the wire.
func TestCacheTokenWinsOverTheCloudToken(t *testing.T) {
	machineTokenSession(t, testMachineToken)
	t.Setenv(remotecache.TokenEnv, "pkt_cache_override_91aa")
	srv, bearers := bearerRecordingCacheServer(t, http.StatusUnauthorized)
	root := t.TempDir()
	command, ran := canonicalRecipe(t)
	writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})

	var logw bytes.Buffer
	s := newProviderSession(root, &logw)
	defer s.close()
	mustReadyInit(t, s)
	restoreOnce(t, s)

	got := bearers()
	if len(got) != 1 || got[0] != "pkt_cache_override_91aa" {
		t.Fatalf("the cache server saw %d request(s); want exactly one, carrying the cache token override", len(got))
	}
	if ran() {
		t.Fatal("the recipe ran while PUTNAMI_CACHE_TOKEN was set")
	}
	if !strings.Contains(logw.String(), "source="+string(remotecache.TokenClassEnv)) {
		t.Fatalf("the refusal must name the cache token override as its source, got %q", logw.String())
	}
}

// A signed-in user keeps the command recipe. A PUTNAMI_CLOUD_TOKEN exported
// beside the session, for a deploy, neither replaces the recipe nor reaches
// the cache server.
func TestSignedInSessionKeepsTheCommandRecipe(t *testing.T) {
	home := machineTokenSession(t, testMachineToken)
	signIn(t, home)
	srv, bearers := bearerRecordingCacheServer(t, 0)
	root := t.TempDir()
	command, ran := canonicalRecipe(t)
	writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})

	s := newProviderSession(root, &bytes.Buffer{})
	defer s.close()
	mustReadyInit(t, s)
	restoreOnce(t, s)

	if got := bearers(); len(got) != 1 || got[0] != "recipe-token" {
		t.Fatalf("the cache server saw %d request(s); want exactly one, carrying the recipe's bearer", len(got))
	}
	if !ran() {
		t.Fatal("the recipe did not run for a signed-in user")
	}
}

// A hosted CI launch never sends PUTNAMI_CLOUD_TOKEN to the cache server: a
// hosted capability does not cross to another audience. The configured source
// serves the run as before.
func TestHostedLaunchNeverSendsTheCloudTokenToTheCache(t *testing.T) {
	machineTokenSession(t, testMachineToken)
	t.Setenv(SessionReporterIngestURLEnv, "https://delivery.example/v1/delivery/records")
	srv, bearers := bearerRecordingCacheServer(t, 0)
	root := t.TempDir()
	command, ran := canonicalRecipe(t)
	writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})

	s := newProviderSession(root, &bytes.Buffer{})
	defer s.close()
	mustReadyInit(t, s)
	if !s.hosted {
		t.Fatal("precondition: the session is not hosted")
	}
	restoreOnce(t, s)

	if got := bearers(); len(got) != 1 || got[0] != "recipe-token" {
		t.Fatalf("the cache server saw %d request(s); want exactly one, carrying the configured source's bearer", len(got))
	}
	if !ran() {
		t.Fatal("the configured source did not run on a hosted launch")
	}
}

// A custom command is a source the workspace chose. It keeps precedence, end
// to end: the machine token never reaches the cache server.
func TestCustomCommandKeepsPrecedenceOverTheCloudToken(t *testing.T) {
	machineTokenSession(t, testMachineToken)
	srv, bearers := bearerRecordingCacheServer(t, 0)
	root := t.TempDir()
	writeCacheConfig(t, root, srv.URL, map[string]any{"command": []string{"sh", "-c", "printf custom-command-token"}})

	s := newProviderSession(root, &bytes.Buffer{})
	defer s.close()
	mustReadyInit(t, s)
	restoreOnce(t, s)

	if got := bearers(); len(got) != 1 || got[0] != "custom-command-token" {
		t.Fatalf("the cache server saw %d request(s); want exactly one, carrying the custom command's bearer", len(got))
	}
}

// cacheMachineToken is the one decision. Each row removes one condition, and
// each must keep the machine token away from the cache server without a log
// line: these are valid sessions that use another credential.
func TestCacheMachineTokenNeedsEveryCondition(t *testing.T) {
	canonical := map[string]any{"command": cacheTokenCommand()}
	for _, test := range []struct {
		name     string
		token    map[string]any
		url      string
		disabled bool
		hosted   bool
		signedIn bool
		setenv   map[string]string
		want     bool
		// readOnly marks a session that does not send PUTNAMI_CLOUD_TOKEN but
		// still opens read-only: its PUTNAMI_CACHE_TOKEN is a machine token.
		readOnly bool
	}{
		{name: "canonical recipe, no session", token: canonical, url: "https://cache.putnami.cloud", want: true},
		{name: "no token source, no session", url: "https://cache.putnami.cloud", want: true},
		{name: "hosted launch", token: canonical, url: "https://cache.putnami.cloud", hosted: true},
		{name: "stored user session", token: canonical, url: "https://cache.putnami.cloud", signedIn: true},
		{name: "cache token override", token: canonical, url: "https://cache.putnami.cloud",
			setenv: map[string]string{remotecache.TokenEnv: "pkt_cache_override"}, readOnly: true},
		{name: "metadata selector", token: canonical, url: "https://cache.putnami.cloud",
			setenv: map[string]string{remotecache.TokenSourceEnv: "metadata"}},
		{name: "custom command", token: map[string]any{"command": []string{"my-token-helper"}}, url: "https://cache.putnami.cloud"},
		{name: "URL source", token: map[string]any{"url": "https://tokens.example/cache"}, url: "https://cache.putnami.cloud"},
		{name: "persisted metadata audience", token: map[string]any{"audience": "https://cache.example"}, url: "https://cache.putnami.cloud"},
		{name: "cache disabled", token: canonical, url: "https://cache.putnami.cloud", disabled: true},
		{name: "no cloud token", token: canonical, url: "https://cache.putnami.cloud",
			setenv: map[string]string{cloudTokenEnv: ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := machineTokenSession(t, testMachineToken)
			for key, value := range test.setenv {
				t.Setenv(key, value)
			}
			if test.signedIn {
				signIn(t, home)
			}
			root := t.TempDir()
			writeCacheConfig(t, root, test.url, test.token)
			cfg, err := remotecache.LoadConfig(CachePath(root))
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.ApplyEnv(); err != nil {
				t.Fatal(err)
			}
			if test.disabled {
				off := false
				cfg.Enabled = &off
			}

			token, problem := cacheMachineToken(cfg, test.hosted, nil)
			if problem != "" {
				t.Fatalf("problem = %q, want none: the session uses another credential", problem)
			}
			if got := token == testMachineToken; got != test.want {
				t.Fatalf("machine token used = %t, want %t", got, test.want)
			}
			if !test.want && token != "" {
				t.Fatal("a token was returned for a session that must not use one")
			}

			// The provider hands the config exactly that decision.
			var logw bytes.Buffer
			s := &providerSession{hosted: test.hosted, logw: &logw}
			s.applyMachineToken(cfg)
			if cfg.SendsMachineToken() != test.want {
				t.Fatalf("SendsMachineToken = %t, want %t", cfg.SendsMachineToken(), test.want)
			}
			readOnly := test.want || test.readOnly
			if s.readOnly != readOnly {
				t.Fatalf("readOnly = %t, want %t: a session that holds a machine token sends no write", s.readOnly, readOnly)
			}
			if !readOnly && logw.Len() != 0 {
				t.Fatalf("the provider logged %q; a session with another credential logs nothing here", logw.String())
			}
			if readOnly && strings.Count(logw.String(), "\n") != 1 {
				t.Fatalf("the provider logged %q; the read-only state is stated in exactly one line", logw.String())
			}
		})
	}
}

// The value must be exactly one bearer token, the check clicore.WorkspaceAuth
// applies to the same variable. A value that is not one is refused with one
// line that names the variable and never the value, and the configured source
// is left in place.
func TestCloudTokenMustBeExactlyOneBearerToken(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "two tokens", value: "pkt_first_4d1 pkt_second_9be"},
		{name: "trailing newline", value: "pkt_first_4d1\n"},
		{name: "leading space", value: " pkt_first_4d1"},
		{name: "header injection", value: "pkt_first_4d1\r\nX-Injected: 1"},
		{name: "control byte", value: "pkt_first_4d1\x00"},
		{name: "non-ASCII", value: "pkt_first_4d1é"},
	} {
		t.Run(test.name, func(t *testing.T) {
			machineTokenSession(t, "")
			env := map[string]string{"PUTNAMI_HOME": t.TempDir(), cloudTokenEnv: test.value}
			cfg := &remotecache.Config{URL: "https://cache.putnami.cloud", Token: remotecache.TokenSource{Command: cacheTokenCommand()}}

			token, problem := cacheMachineToken(cfg, false, env)
			if token != "" {
				t.Fatal("a value that is not one bearer token was accepted")
			}
			if problem != "PUTNAMI_CLOUD_TOKEN must contain exactly one bearer token; the remote cache does not use it" {
				t.Fatalf("problem = %q, want the one-bearer-token line", problem)
			}
			if strings.Contains(problem, "pkt_first_4d1") {
				t.Fatal("the problem line carries the value")
			}
		})
	}
}

// The same refusal through a provider session: the line is logged once, the
// value stays out of the log, and the cache server never sees it.
func TestProviderRefusesAMultiWordCloudToken(t *testing.T) {
	machineTokenSession(t, "pkt_first_4d1 pkt_second_9be")
	srv, bearers := bearerRecordingCacheServer(t, 0)
	root := t.TempDir()
	command, ran := canonicalRecipe(t)
	writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})

	var logw bytes.Buffer
	s := newProviderSession(root, &logw)
	defer s.close()
	mustReadyInit(t, s)
	restoreOnce(t, s)

	log := logw.String()
	if got := strings.Count(log, "PUTNAMI_CLOUD_TOKEN must contain exactly one bearer token"); got != 1 {
		t.Fatalf("the refusal was logged %d time(s), want once; log=%q", got, log)
	}
	if strings.Contains(log, "pkt_first_4d1") || strings.Contains(log, "pkt_second_9be") {
		t.Fatal("the provider log carries the refused value")
	}
	for _, bearer := range bearers() {
		if strings.Contains(bearer, "pkt_") {
			t.Fatal("the cache server saw the refused value")
		}
	}
	if !ran() {
		t.Fatal("the configured source was not used after the refusal")
	}
}

// Only a workspace machine token is derived. A hosted CI capability travels in
// the same variable as a signed token with another shape, so a value without
// the machine token prefix is left alone, without a log line: the session
// keeps its configured source.
func TestOnlyAWorkspaceMachineTokenIsDerived(t *testing.T) {
	for _, value := range []string{
		"eyJhbGciOiJFUzI1NiJ9.eyJzY29wZSI6ImNpLmNvbmZpZy1kcmlmdCJ9.c2lnbmF0dXJl",
		"opaque-capability-0f3c",
		"PKT_uppercase_prefix",
	} {
		machineTokenSession(t, value)
		cfg := &remotecache.Config{URL: "https://cache.putnami.cloud", Token: remotecache.TokenSource{Command: cacheTokenCommand()}}
		token, problem := cacheMachineToken(cfg, false, nil)
		if token != "" || problem != "" {
			t.Fatalf("a %d-byte value without the machine token prefix: used=%t problem=%q, want unused and silent",
				len(value), token != "", problem)
		}
	}
}

// A refused machine token is sent once: the session cannot mint another, so
// the recipe it replaced is not invoked to "refresh" it. The refusal names its
// source class, and a server that echoes the bearer cannot put it in the log.
func TestRefusedCloudTokenIsNotReMinted(t *testing.T) {
	machineTokenSession(t, testMachineToken)
	srv, bearers := bearerRecordingCacheServer(t, http.StatusUnauthorized)
	root := t.TempDir()
	command, ran := canonicalRecipe(t)
	writeCacheConfig(t, root, srv.URL, map[string]any{"command": command})

	var logw bytes.Buffer
	s := newProviderSession(root, &logw)
	defer s.close()
	mustReadyInit(t, s)
	if status := restoreOnce(t, s); status != cache.RestoreError {
		t.Fatalf("restore status = %q, want an error after the refusal", status)
	}

	if got := bearers(); len(got) != 1 || got[0] != testMachineToken {
		t.Fatalf("the cache server saw %d request(s); a refused machine token is sent exactly once", len(got))
	}
	if ran() {
		t.Fatal("the recipe ran after the refusal; a machine token is never re-minted")
	}
	log := logw.String()
	if !strings.Contains(log, "auth refused: op=negotiate status=401 source="+string(remotecache.TokenClassCloudEnv)+" refreshed=false") {
		t.Fatalf("the refusal must name the machine token as its source and say it was not refreshed, got %q", log)
	}
	if strings.Contains(log, testMachineToken) {
		t.Fatal("the provider log carries the machine token")
	}
}

// A member's machine token reads the shared cache and writes nothing: the
// cache server stores only a hosted CI run's results. The session knows that
// before the first write, so it opens read-only: no write request reaches the
// server, the run prints one info line saying so, and no refusal line.
func TestCloudTokenSessionIsReadOnlyBeforeTheFirstWrite(t *testing.T) {
	machineTokenSession(t, testMachineToken)
	srv, writes := writeRefusingCacheServer(t)
	t.Setenv(remotecache.URLEnv, srv.URL)

	var logw bytes.Buffer
	s := newProviderSession(t.TempDir(), &logw)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir()})

	for i, key := range []string{strings.Repeat("a", cache.KeyLength), strings.Repeat("b", cache.KeyLength)} {
		upload := s.upload(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: int64(2 + i), Op: cache.OpUpload},
			latchTestUpload(key))
		if result, _ := cache.ParseAndValidateUploadResult(upload.Payload); result == nil || result.Accepted {
			t.Fatalf("upload %d = %+v, want Accepted=false: a machine token never writes", i, upload)
		}
	}
	marker := s.markerWrite(&cache.ProviderRequest{ProtocolVersion: cache.ProviderProtocolVersion, ID: 5, Op: cache.OpMarkerWrite},
		&cache.MarkerWriteParams{Workspace: "w", Branch: "main", Commands: []string{"build"}, SHA: strings.Repeat("c", 40)})
	if result, _ := cache.ParseAndValidateMarkerWriteResult(marker.Payload); result == nil || result.Published {
		t.Fatalf("marker write = %+v, want Published=false", marker)
	}
	mustSummary(t, s)
	s.bg.Wait()
	if got := writes.Load(); got != 0 {
		t.Fatalf("write requests = %d, want none: the upload path never starts", got)
	}

	log := logw.String()
	if got := strings.Count(log, "the remote cache is read-only for this run"); got != 1 {
		t.Fatalf("read-only info lines = %d, want exactly one, got %q", got, log)
	}
	for _, banned := range []string{"auth refused", "refused a find-missing", "writes nothing to it: only hosted CI runs write, so", "WARNING"} {
		if strings.Contains(log, banned) {
			t.Fatalf("the log carries %q for a read-only token: %q", banned, log)
		}
	}
	if strings.Contains(log, testMachineToken) {
		t.Fatal("the provider log carries the machine token")
	}
}

// A machine token set through PUTNAMI_CACHE_TOKEN reads the shared cache and
// writes nothing, like one set through PUTNAMI_CLOUD_TOKEN: the session opens
// read-only and names the variable that holds it. Any other cache token, and a
// hosted run, keep the write path.
func TestCacheTokenOverrideMachineTokenSessionIsReadOnly(t *testing.T) {
	for _, test := range []struct {
		name     string
		token    string
		hosted   bool
		readOnly bool
	}{
		{name: "machine token", token: testMachineToken, readOnly: true},
		{name: "machine token with spaces around it", token: " " + testMachineToken + "\n", readOnly: true},
		{name: "another bearer", token: "developer-token"},
		{name: "hosted run", token: testMachineToken, hosted: true},
		{name: "no override"},
	} {
		t.Run(test.name, func(t *testing.T) {
			machineTokenSession(t, "")
			t.Setenv(remotecache.TokenEnv, test.token)
			srv, writes := writeRefusingCacheServer(t)
			t.Setenv(remotecache.URLEnv, srv.URL)

			var logw bytes.Buffer
			s := newProviderSession(t.TempDir(), &logw)
			defer s.close()
			s.hosted = test.hosted
			cfg, err := remotecache.LoadConfig(CachePath(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.ApplyEnv(); err != nil {
				t.Fatal(err)
			}
			s.applyMachineToken(cfg)

			if s.readOnly != test.readOnly {
				t.Fatalf("readOnly = %t, want %t", s.readOnly, test.readOnly)
			}
			line := "the remote cache is read-only for this run: " + remotecache.TokenEnv + " is a workspace machine token"
			if got := strings.Contains(logw.String(), line); got != test.readOnly {
				t.Fatalf("read-only line logged = %t, want %t, got %q", got, test.readOnly, logw.String())
			}
			if strings.Contains(logw.String(), strings.TrimSpace(test.token)) && test.token != "" {
				t.Fatal("the provider log carries the cache token")
			}
			if got := writes.Load(); got != 0 {
				t.Fatalf("write requests = %d, want none before any upload", got)
			}
		})
	}
}

// A refused write with a machine token is the expected developer refusal,
// unless a hosted launcher started the run.
func TestMachineTokenRefusalIsTheExpectedDeveloperRefusal(t *testing.T) {
	refusal := remotecache.AuthFailure{Op: "store", Status: http.StatusForbidden, Source: remotecache.TokenClassCloudEnv}
	if !(&providerSession{}).expectedDeveloperRefusal(refusal) {
		t.Fatal("a machine token refusal outside a hosted run is not read as the developer refusal")
	}
	if (&providerSession{hosted: true}).expectedDeveloperRefusal(refusal) {
		t.Fatal("a refusal under a hosted launcher is read as the developer refusal")
	}
}

// The cache status names the credential a build sends. In a session that
// reads the cache with PUTNAMI_CLOUD_TOKEN, the identity check is ok and names
// the variable; it does not ask for a sign-in that the session cannot do.
func TestCacheStatusNamesTheMachineTokenIdentity(t *testing.T) {
	machineTokenSession(t, "")
	root := t.TempDir()
	writeCacheConfig(t, root, "https://cache.test", map[string]any{"command": cacheTokenCommand()})
	home := t.TempDir()
	env := map[string]string{"PUTNAMI_HOME": home, cloudTokenEnv: testMachineToken}

	built := statusBuild(root)
	identity := cacheIdentity(built, env)
	if identity[machineTokenIdentityKey] != cloudTokenEnv {
		t.Fatalf("identity = %v, want the machine token variable", identity)
	}
	cfg, err := statusCacheConfig(built)
	if err != nil {
		t.Fatal(err)
	}
	node := CacheStatusNodeFrom(cfg, nil, identity, nil, nil)
	want := strings.Join([]string{
		"cache  ok  enabled",
		"",
		"  STATE  CHECK     DETAIL",
		"  ok     config    enabled in .putnami/cache.json, https://cache.test (mode full)",
		"  ok     identity  workspace machine token from PUTNAMI_CLOUD_TOKEN, no signed-in user",
	}, "\n")
	got := strings.Join(clicore.RenderStatusReport(node), "\n")
	if got != want {
		t.Fatalf("cache status = %q, want %q", got, want)
	}
	if strings.Contains(got, testMachineToken) {
		t.Fatal("the cache status carries the machine token")
	}

	// Each of these sessions sends another credential or none, so the status
	// keeps its earlier answer.
	if identity := cacheIdentity(statusBuild(root), map[string]string{"PUTNAMI_HOME": home}); identity != nil {
		t.Fatalf("no cloud token: identity = %v, want none", identity)
	}
	hosted := map[string]string{"PUTNAMI_HOME": home, cloudTokenEnv: testMachineToken, SessionReporterIngestURLEnv: "https://delivery.example/v1/delivery/records"}
	if identity := cacheIdentity(statusBuild(root), hosted); identity != nil {
		t.Fatalf("hosted launch: identity = %v, want none", identity)
	}
	custom := t.TempDir()
	writeCacheConfig(t, custom, "https://cache.test", map[string]any{"command": []string{"my-token-helper"}})
	if identity := cacheIdentity(statusBuild(custom), env); identity != nil {
		t.Fatalf("custom command: identity = %v, want none", identity)
	}
	t.Setenv(remotecache.TokenEnv, "pkt_cache_override")
	if identity := cacheIdentity(statusBuild(root), env); identity[machineTokenIdentityKey] != nil || identity[cacheTokenIdentityKey] != remotecache.TokenEnv {
		t.Fatalf("cache token override: identity = %v, want the cache token variable", identity)
	}
	t.Setenv(remotecache.TokenEnv, "")
	t.Setenv(remotecache.ReadOnlyEnv, "not-a-boolean")
	if identity := cacheIdentity(statusBuild(root), env); identity != nil {
		t.Fatalf("malformed cache override: identity = %v, want none", identity)
	}
}

// A linked workspace with no cache config file reads the cache through the
// managed default. The status takes the provider's decision there too.
func TestCacheStatusFollowsTheLinkedWorkspaceDefault(t *testing.T) {
	machineTokenSession(t, "")
	root := t.TempDir()
	writeLinkedManifest(t, root)
	env := map[string]string{"PUTNAMI_HOME": t.TempDir(), cloudTokenEnv: testMachineToken}
	if identity := cacheIdentity(statusBuild(root), env); identity[machineTokenIdentityKey] != cloudTokenEnv {
		t.Fatalf("linked workspace: identity = %v, want the machine token variable", identity)
	}
	if identity := cacheIdentity(statusBuild(t.TempDir()), env); identity != nil {
		t.Fatalf("unlinked workspace: identity = %v, want none", identity)
	}
}

// statusBuild is the cache config the cache status builds for root.
func statusBuild(root string) builtCacheConfig {
	return buildCacheConfig(root, DefaultCacheURL)
}

// runCacheStatus runs `putnami cloud cache status` and returns its output.
func runCacheStatus(t *testing.T, root string, ioctx clicore.IO) string {
	t.Helper()
	var lines []string
	ioctx.Stdout = func(line string) { lines = append(lines, line) }
	args := []string{"status"}
	if err := Cache(clicore.MergeParams(clicore.ParseFlags(args)), args, root, ioctx.Env, ioctx); err != nil {
		t.Fatalf("cache status: %v", err)
	}
	return strings.Join(lines, "\n")
}

// A fresh checkout of a linked workspace has no .putnami/cache.json, and its
// builds read the cache through the workspace link. The status says so
// instead of "not configured".
func TestCacheStatusOfALinkedWorkspaceWithoutACacheConfig(t *testing.T) {
	machineTokenSession(t, "")
	fake := ciStatusFake(CIStatusResponse{Workspace: "ws-acme", Healthy: true, Dimensions: []CIStatusDimension{},
		LastRunCache: &CIRunCache{Tasks: 4, Hits: 3, Misses: 1, ServedMS: 21, ExecutedMS: 41000}})
	ioctx, root := newCITestIO(t, fake)
	text := runCacheStatus(t, root, ioctx)
	for _, want := range []string{
		"cache  ok  enabled; last run reused 3 of 4 tasks",
		"ok     config    enabled from the workspace link, " + DefaultCacheURL + "; no " + CacheFileRelative,
		"token from `putnami cloud token --for cache`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("cache status = %q, want %q", text, want)
		}
	}
	if _, err := os.Stat(CachePath(root)); !os.IsNotExist(err) {
		t.Fatalf("the cache status wrote %s (stat error %v)", CacheFileRelative, err)
	}
}

// In a session with PUTNAMI_CLOUD_TOKEN and no signed-in user, the cache status
// names the machine token and does not ask the ci status route for the last
// run: that route denies workspace machine tokens.
func TestMachineTokenCacheStatusIsTruthful(t *testing.T) {
	machineTokenSession(t, "")
	fake := ciStatusFake(CIStatusResponse{Workspace: "ws-acme", Healthy: true, Dimensions: []CIStatusDimension{}})
	root := t.TempDir()
	writeReportLinkFile(t, root)
	ioctx := clicore.IO{
		Env:    hometest.Env(t.TempDir(), map[string]string{cloudTokenEnv: testMachineToken, "NO_COLOR": "1"}),
		Stderr: func(string) {},
		Client: &http.Client{Transport: fake},
	}
	text := runCacheStatus(t, root, ioctx)
	want := strings.Join([]string{
		"cache  ok  enabled",
		"",
		"  STATE  CHECK     DETAIL",
		"  ok     config    enabled from the workspace link, " + DefaultCacheURL + "; no " + CacheFileRelative,
		"  ok     identity  workspace machine token from PUTNAMI_CLOUD_TOKEN, no signed-in user",
	}, "\n")
	if text != want {
		t.Fatalf("cache status = %q, want %q", text, want)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("the cache status sent %d request(s) with the machine token, want none", len(fake.requests))
	}
}

// PUTNAMI_CACHE_TOKEN overrides every configured token source. With no
// signed-in user, the cache status names that variable instead of asking for a
// sign-in, and does not ask the ci status route for the last run.
func TestCacheTokenOverrideCacheStatusIsTruthful(t *testing.T) {
	machineTokenSession(t, "")
	t.Setenv(remotecache.TokenEnv, "cache-token-override")
	fake := ciStatusFake(CIStatusResponse{Workspace: "ws-acme", Healthy: true, Dimensions: []CIStatusDimension{}})
	root := t.TempDir()
	writeReportLinkFile(t, root)
	ioctx := clicore.IO{
		Env:    hometest.Env(t.TempDir(), map[string]string{cloudTokenEnv: testMachineToken, "NO_COLOR": "1"}),
		Stderr: func(string) {},
		Client: &http.Client{Transport: fake},
	}
	text := runCacheStatus(t, root, ioctx)
	want := strings.Join([]string{
		"cache  ok  enabled",
		"",
		"  STATE  CHECK     DETAIL",
		"  ok     config    enabled from the workspace link, " + DefaultCacheURL + "; no " + CacheFileRelative,
		"  ok     identity  cache token from " + remotecache.TokenEnv,
	}, "\n")
	if text != want {
		t.Fatalf("cache status = %q, want %q", text, want)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("the cache status sent %d request(s) with no signed-in user, want none", len(fake.requests))
	}
	if strings.Contains(text, "cache-token-override") {
		t.Fatal("the cache status carries the cache token")
	}
}

// PUTNAMI_CACHE_URL sets the cache URL a build reads. The config row names the
// override, both in an unlinked checkout with no cache config and in a linked
// checkout, so the status and the build agree on the cache they use.
func TestCacheStatusNamesTheCacheURLOverride(t *testing.T) {
	const override = "https://cache.override.test"
	for _, test := range []struct {
		name   string
		linked bool
	}{
		{name: "unlinked checkout with no cache config"},
		{name: "linked checkout with no cache config", linked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			machineTokenSession(t, "")
			t.Setenv(remotecache.URLEnv, override)
			fake := ciStatusFake(CIStatusResponse{Workspace: "ws-acme", Healthy: true, Dimensions: []CIStatusDimension{}})
			root := t.TempDir()
			if test.linked {
				writeReportLinkFile(t, root)
			}
			ioctx := clicore.IO{
				Env:    hometest.Env(t.TempDir(), map[string]string{cloudTokenEnv: testMachineToken, "NO_COLOR": "1"}),
				Stderr: func(string) {},
				Client: &http.Client{Transport: fake},
			}
			text := runCacheStatus(t, root, ioctx)
			want := strings.Join([]string{
				"cache  ok  enabled",
				"",
				"  STATE  CHECK     DETAIL",
				"  ok     config    enabled by " + remotecache.URLEnv + ", " + override,
				"  ok     identity  workspace machine token from PUTNAMI_CLOUD_TOKEN, no signed-in user",
			}, "\n")
			if text != want {
				t.Fatalf("cache status = %q, want %q", text, want)
			}
		})
	}
}

// A malformed PUTNAMI_CACHE_* override turns the remote cache off for a build.
// The status says the config is unknown and does not offer `putnami cloud
// setup`, which cannot fix the environment.
func TestCacheStatusReportsAMalformedCacheOverride(t *testing.T) {
	machineTokenSession(t, "")
	t.Setenv(remotecache.ReadOnlyEnv, "not-a-boolean")
	root := t.TempDir()
	writeCacheConfig(t, root, "https://cache.test", map[string]any{"command": cacheTokenCommand()})
	built := statusBuild(root)
	cfg, err := statusCacheConfig(built)
	if cfg != nil || err == nil {
		t.Fatalf("status config = %v, error %v; want no config and an error", cfg, err)
	}
	config := CacheStatusNodeFrom(cfg, err, nil, nil, nil).Children[0]
	if config.State != clicore.StatusUnknown || config.Fix != "" || !strings.Contains(config.Detail, remotecache.ReadOnlyEnv) {
		t.Fatalf("config check = %+v, want unknown, naming %s, with no fix", config, remotecache.ReadOnlyEnv)
	}
}

// A cache config with no URL, in an unlinked checkout with no override, leaves
// a build without a remote cache. The status says so instead of "enabled".
func TestCacheStatusReportsAConfigWithNoURL(t *testing.T) {
	machineTokenSession(t, "")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(CachePath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CachePath(root), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := statusCacheConfig(statusBuild(root))
	if err != nil {
		t.Fatal(err)
	}
	node := CacheStatusNodeFrom(cfg, nil, nil, nil, nil)
	config := node.Children[0]
	if node.Detail != "not configured" || config.State != clicore.StatusDegraded ||
		config.Detail != "not configured: no cache URL in "+CacheFileRelative || config.Fix != "putnami cloud setup" {
		t.Fatalf("cache status = %+v, config check %+v; want not configured, naming the missing URL", node, config)
	}
}

// A signed-in user is the identity even when PUTNAMI_CLOUD_TOKEN is exported
// beside the session.
func TestCacheStatusKeepsTheSignedInUser(t *testing.T) {
	machineTokenSession(t, "")
	root := t.TempDir()
	writeCacheConfig(t, root, "https://cache.test", map[string]any{"command": cacheTokenCommand()})
	home := t.TempDir()
	signIn(t, home)
	env := map[string]string{"PUTNAMI_HOME": home, cloudTokenEnv: testMachineToken}
	if cacheReadsMachineToken(statusBuild(root), env) {
		t.Fatal("a signed-in session reads the cache with the machine token")
	}
	if identity := cacheIdentity(statusBuild(root), env); identity[machineTokenIdentityKey] != nil {
		t.Fatalf("identity = %v, want no machine token marker for a signed-in user", identity)
	}
	// PUTNAMI_CACHE_TOKEN still wins over the signed-in user: a build sends it.
	t.Setenv(remotecache.TokenEnv, "cache-token-override")
	if identity := cacheIdentity(statusBuild(root), env); identity[cacheTokenIdentityKey] != remotecache.TokenEnv {
		t.Fatalf("identity = %v, want the cache token variable over the signed-in user", identity)
	}
}
