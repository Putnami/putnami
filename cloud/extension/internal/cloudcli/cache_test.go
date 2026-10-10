package cloudcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
)

func setupCacheEnv(t *testing.T) (home, workspaceRoot string) {
	t.Helper()
	home = t.TempDir()
	workspaceRoot = t.TempDir()
	writeTestAuth(t, home)
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{"name": "local"})
	return home, workspaceRoot
}

func runSetup(t *testing.T, home, workspaceRoot string, args ...string) []string {
	t.Helper()
	fake := newFakeTransport(t)
	var output []string
	err := RunCommand("setup", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    func() time.Time { return time.Date(2026, 4, 30, 12, 10, 0, 0, time.UTC) },
	}, append([]string{"--workspace", "ws-acme", "--control-plane-url", testBaseURL}, args...))
	if err != nil {
		t.Fatalf("setup %v: %v", args, err)
	}
	return output
}

// TestSetupProvisionsBuildCacheConfig proves `cloud setup` writes a committable
// .putnami/cache.json with the default endpoint, mode, and a token *source* —
// never a raw bearer.
func TestSetupProvisionsBuildCacheConfig(t *testing.T) {
	home, workspaceRoot := setupCacheEnv(t)
	output := runSetup(t, home, workspaceRoot, "--json")

	var cache map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), &cache)
	if cache["enabled"] != true {
		t.Fatalf("enabled = %v, want true", cache["enabled"])
	}
	if cache["url"] != deliverycli.DefaultCacheURL {
		t.Fatalf("url = %v, want %s", cache["url"], deliverycli.DefaultCacheURL)
	}
	if cache["mode"] != "full" {
		t.Fatalf("mode = %v, want full", cache["mode"])
	}
	token, ok := cache["token"].(map[string]any)
	if !ok {
		t.Fatalf("token source missing: %v", cache)
	}
	cmd, ok := token["command"].([]any)
	if !ok || len(cmd) != 5 || cmd[0] != "putnami" || cmd[1] != "cloud" || cmd[2] != "token" || cmd[3] != "--for" || cmd[4] != "cache" {
		t.Fatalf("token command = %v, want [putnami cloud token --for cache]", token["command"])
	}

	// No secret on disk: the file carries a source, not a bearer.
	raw := readFileString(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative))
	assertNotContains(t, raw, "access_token")
	assertNotContains(t, raw, "Bearer")

	// The command result advertises the cache summary too.
	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	gotCache, ok := got["cache"].(map[string]any)
	if !ok || gotCache["url"] != deliverycli.DefaultCacheURL || gotCache["mode"] != "full" {
		t.Fatalf("result cache = %v", got["cache"])
	}
}

func TestInstallInitializesLocalCloudStateFromManifest(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{
		"name": "local",
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"version":           1,
					"control_plane_url": testBaseURL,
					"workspace_id":      "ws-acme",
					"workspace_name":    "Acme",
					"environment":       "prod",
					"linked_at":         "2026-04-30T12:00:00Z",
				},
			},
		},
	})
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("install", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--json"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("install should not call remote APIs when manifest link exists, got %d requests", len(fake.requests))
	}

	var link map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, LinkFileRelative), &link)
	if link["workspace_id"] != "ws-acme" || link["control_plane_url"] != testBaseURL {
		t.Fatalf("link = %v", link)
	}

	var cache map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), &cache)
	if cache["enabled"] != true || cache["url"] != deliverycli.DefaultCacheURL || cache["mode"] != deliverycli.DefaultCacheMode {
		t.Fatalf("cache = %v", cache)
	}

	var state distributioncli.RegistriesState
	readJSONFile(t, filepath.Join(home, ".putnami/registries.json"), &state)
	if len(state.Keys) != 4 {
		t.Fatalf("registry keys = %d, want 4", len(state.Keys))
	}
	for _, ref := range state.Keys {
		if ref.ID != "" {
			t.Fatalf("%s install minted a key: %+v", ref.Registry, ref)
		}
		if ref.Token == nil || len(ref.Token.Command) != 5 || ref.Token.Command[3] != "--for" {
			t.Fatalf("%s install wrote no kind-only token recipe: %+v", ref.Registry, ref)
		}
	}

	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got["configured"] != true || got["cache_configured"] != true {
		t.Fatalf("install output = %v", got)
	}
}

// TestInstallNeverScrubsNativeRegistryCredentials guards the regression
// reported 2026-09-02/03: every `putnami install` deleted the user's real
// `//npm.putnami.dev/:_authToken=` line from ~/.npmrc (and the `machine
// go.putnami.dev` entry from ~/.netrc), because install shared the same
// WriteRegistryTokenRecipes call as `cloud login` — a function whose
// contract is to migrate AWAY legacy static credentials. A `bun install` of
// a private @putnami/* package then 404s until the line is restored by
// hand. install runs routinely and non-interactively as part of dependency
// installation; tearing down a valid credential belongs only to
// `cloud logout` (or an explicit `cloud registries` command), never to
// install. See TestLoginIsIdempotentAndPreservesOtherEntries for login's own
// (still scrub-on-migrate) contract, which is unchanged by this fix.
func TestInstallNeverScrubsNativeRegistryCredentials(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{
		"name": "local",
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"version":           1,
					"control_plane_url": testBaseURL,
					"workspace_id":      "ws-acme",
					"workspace_name":    "Acme",
					"environment":       "prod",
					"linked_at":         "2026-04-30T12:00:00Z",
				},
			},
		},
	})

	mustMkdir(t, home)
	// The npmrc holds ONLY the putnami auth line — the exact shape that used
	// to make removeNpmrcAuth delete the whole file once the line was
	// stripped (rewriteLines removes a file that ends up with zero lines).
	npmrcContent := "//npm.putnami.dev/:_authToken=pkt_valid_token_abc123\n"
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(npmrcContent), 0o600); err != nil {
		t.Fatalf("seed npmrc: %v", err)
	}
	netrcContent := "machine go.putnami.dev login _token password pkt_valid_token_def456\n"
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrcContent), 0o600); err != nil {
		t.Fatalf("seed netrc: %v", err)
	}

	fake := newFakeTransport(t)
	var output []string
	err := RunCommand("install", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--json"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	npmrc, statErr := os.ReadFile(filepath.Join(home, ".npmrc"))
	if statErr != nil {
		t.Fatalf("npmrc must survive install (not be deleted): %v", statErr)
	}
	if string(npmrc) != npmrcContent {
		t.Fatalf("install rewrote npmrc:\n got  %q\n want %q", string(npmrc), npmrcContent)
	}

	netrc, statErr := os.ReadFile(filepath.Join(home, ".netrc"))
	if statErr != nil {
		t.Fatalf("netrc must survive install (not be deleted): %v", statErr)
	}
	if string(netrc) != netrcContent {
		t.Fatalf("install rewrote netrc:\n got  %q\n want %q", string(netrc), netrcContent)
	}
}

func TestInstallSkipsWithoutManifestLinkOrAuth(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("install", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--json"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("install should not call remote APIs when unauthenticated, got %d requests", len(fake.requests))
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, LinkFileRelative)); !os.IsNotExist(err) {
		t.Fatalf("link should not be written, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, deliverycli.CacheFileRelative)); !os.IsNotExist(err) {
		t.Fatalf("cache should not be written, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".putnami/registries.json")); !os.IsNotExist(err) {
		t.Fatalf("registries should not be written, stat err = %v", err)
	}

	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got["configured"] != false || got["skipped"] != true {
		t.Fatalf("install output = %v", got)
	}
}

func TestInstallPreservesExistingCacheConfigUnlessForced(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{
		"name": "local",
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"workspace_id":      "ws-acme",
					"control_plane_url": testBaseURL,
				},
			},
		},
	})
	if err := os.MkdirAll(filepath.Join(workspaceRoot, ".putnami"), 0o755); err != nil {
		t.Fatalf("mkdir .putnami: %v", err)
	}
	writeJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), map[string]any{
		"enabled": false,
		"url":     "https://cache.custom",
		"mode":    "minimal",
	})

	runInstall := func(args ...string) {
		t.Helper()
		err := RunCommand("install", IO{
			Env: hometest.Env(home, map[string]string{
				"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
			}),
			Stdout: func(string) {},
			Stderr: func(string) {},
			Client: newFakeTransport(t).client(),
			Now:    fixedNow,
		}, args)
		if err != nil {
			t.Fatalf("install %v: %v", args, err)
		}
	}

	runInstall()
	var cache map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), &cache)
	if cache["enabled"] != false || cache["url"] != "https://cache.custom" || cache["mode"] != "minimal" {
		t.Fatalf("cache should be preserved, got %v", cache)
	}

	runInstall("--force", "--cache-url", "https://cache.new/", "--cache-mode", "toplevel")
	readJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), &cache)
	if cache["enabled"] != true || cache["url"] != "https://cache.new" || cache["mode"] != "toplevel" {
		t.Fatalf("forced cache = %v", cache)
	}
}

// TestSetupSkipsCacheWithNoCacheFlag proves --no-cache leaves the cache
// unprovisioned (and any existing file untouched).
func TestSetupSkipsCacheWithNoCacheFlag(t *testing.T) {
	home, workspaceRoot := setupCacheEnv(t)
	runSetup(t, home, workspaceRoot, "--no-cache")

	if _, err := os.Stat(filepath.Join(workspaceRoot, deliverycli.CacheFileRelative)); !os.IsNotExist(err) {
		t.Fatalf("expected no cache.json with --no-cache, stat err = %v", err)
	}
}

// TestSetupHonorsCacheModeAndURL proves the mode/url overrides land in the file
// and an unknown mode is a usage error rather than a silently inert config.
func TestSetupHonorsCacheModeAndURL(t *testing.T) {
	home, workspaceRoot := setupCacheEnv(t)
	runSetup(t, home, workspaceRoot, "--cache-mode", "toplevel", "--cache-url", "https://cache.test/")

	var cache map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), &cache)
	if cache["mode"] != "toplevel" {
		t.Fatalf("mode = %v, want toplevel", cache["mode"])
	}
	if cache["url"] != "https://cache.test" {
		t.Fatalf("url = %v, want trimmed https://cache.test", cache["url"])
	}

	badHome, badRoot := setupCacheEnv(t)
	fake := newFakeTransport(t)
	err := RunCommand("setup", IO{
		Env: hometest.Env(badHome, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": badRoot,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    func() time.Time { return time.Date(2026, 4, 30, 12, 10, 0, 0, time.UTC) },
	}, []string{"--workspace", "ws-acme", "--control-plane-url", testBaseURL, "--cache-mode", "turbo"})
	if err == nil {
		t.Fatalf("expected usage error for invalid cache mode")
	}
	assertContains(t, err.Error(), "invalid cache mode")
	if len(fake.requests) != 0 {
		t.Fatalf("invalid cache mode made %d API requests, want 0", len(fake.requests))
	}
	for _, file := range []string{LinkFileRelative, deliverycli.CacheFileRelative} {
		if _, statErr := os.Stat(filepath.Join(badRoot, file)); !os.IsNotExist(statErr) {
			t.Fatalf("invalid cache mode should not write %s, stat err = %v", file, statErr)
		}
	}
}

// TestCloudTokenForCacheEmitsBareBearer proves the cache-purpose contract end
// to end: an ordinary fresh workspace cache cannot satisfy it, the ephemeral
// key and OAuth exchange both request exactly cache.read+cache.write for the
// configured workspace, the backing key is cleaned up, and stdout is one bare
// least-privilege bearer even with --json present.
func TestCloudTokenForCacheEmitsBareBearer(t *testing.T) {
	for _, extra := range [][]string{{"--for", "cache"}, {"--for", "cache", "--json"}} {
		home := t.TempDir()
		workspaceRoot := t.TempDir()
		writeTestAuth(t, home)
		writeLinkFile(t, workspaceRoot)
		env := hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		})
		ordinaryWorkspaceToken := stringValue(tokenResponse(map[string]any{
			"scope_ref": map[string]any{"workspace_id": "ws-acme"},
		})["access_token"])
		stored, err := readAuth(env, true)
		if err != nil {
			t.Fatalf("read auth: %v", err)
		}
		stored.WorkspaceAccess = map[string]storedWorkspaceAccess{
			"ws-acme": {
				AccessToken: ordinaryWorkspaceToken,
				TokenType:   "Bearer",
				ExpiresAt:   fixedNow().Add(time.Hour).Format(time.RFC3339Nano),
				Scope:       "openid profile email apikeys:write intelligence.read intelligence.audit intelligence.review.read intelligence.review.manage",
			},
		}
		if err := writeAuth(stored, env); err != nil {
			t.Fatalf("write auth: %v", err)
		}
		fake := newFakeTransport(t)
		var output []string

		err = RunCommand("token", IO{
			Env:    env,
			Stdout: func(line string) { output = append(output, line) },
			Stderr: func(string) {},
			Client: fake.client(),
			Now:    fixedNow,
		}, extra)
		if err != nil {
			t.Fatalf("cloud token %v: %v", extra, err)
		}
		if len(output) != 1 {
			t.Fatalf("cloud token %v emitted %d lines, want 1: %v", extra, len(output), output)
		}
		bare := output[0]
		if strings.ContainsAny(bare, "{ ") || strings.Contains(bare, "Minted") {
			t.Fatalf("cloud token %v emitted decorated output: %q", extra, bare)
		}
		if claimedWorkspaceID(decodeJWT(bare)) != "ws-acme" {
			t.Fatalf("cloud token %v bearer not workspace-scoped: %q", extra, bare)
		}
		if bare == ordinaryWorkspaceToken {
			t.Fatalf("cloud token %v reused the insufficient ordinary workspace token", extra)
		}
		claims := decodeJWT(bare)
		if claims["aud"] != cacheOAuthClientID {
			t.Fatalf("cloud token %v aud = %v, want %q", extra, claims["aud"], cacheOAuthClientID)
		}
		if claims["scope"] != cacheAllowedScopes {
			t.Fatalf("cloud token %v scope = %v, want %q", extra, claims["scope"], cacheAllowedScopes)
		}
		if len(fake.mintedKeys) != 1 {
			t.Fatalf("cloud token %v minted keys = %d, want 1", extra, len(fake.mintedKeys))
		}
		minted := fake.mintedKeys[0]
		if minted["allowed_scopes"] != cacheAllowedScopes || minted["workspace_id"] != "ws-acme" {
			t.Fatalf("cloud token %v key scope/workspace = %v/%v, want %q/ws-acme", extra, minted["allowed_scopes"], minted["workspace_id"], cacheAllowedScopes)
		}
		if minted["expires_at"] != fixedNow().Add(10*time.Minute).Format(time.RFC3339Nano) {
			t.Fatalf("cloud token %v key expires_at = %v, want ten-minute bound", extra, minted["expires_at"])
		}
		var grant *recordedRequest
		for i := range fake.requests {
			request := &fake.requests[i]
			if request.Path == "/token" && request.Body["grant_type"] == apiKeyGrantType {
				grant = request
				break
			}
		}
		if grant == nil {
			t.Fatalf("cloud token %v did not exchange the cache api key", extra)
		}
		if grant.Body["client_id"] != cacheOAuthClientID || grant.Body["scope"] != cacheAllowedScopes || grant.Body["workspace_id"] != "ws-acme" {
			t.Fatalf("cloud token %v grant client/scope/workspace = %v/%v/%v", extra, grant.Body["client_id"], grant.Body["scope"], grant.Body["workspace_id"])
		}
		if len(fake.revokedIDs) != 1 || fake.revokedIDs[0] != minted["id"] {
			t.Fatalf("cloud token %v revoked ids = %v, want [%v]", extra, fake.revokedIDs, minted["id"])
		}
	}
}

func TestCloudTokenForCacheFailsClosedAndCleansUpEphemeralKey(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeTransport)
		wantError string
	}{
		{
			name: "incomplete key response",
			configure: func(fake *fakeTransport) {
				fake.omitRawAPIKey = true
			},
			wantError: "incomplete scoped-token api key",
		},
		{
			name: "wrong token audience",
			configure: func(fake *fakeTransport) {
				fake.apiKeyGrantAud = "distribution"
			},
			wantError: "expected \"cache\"",
		},
		{
			name: "missing required token scope",
			configure: func(fake *fakeTransport) {
				fake.apiKeyGrantScope = "cache.read"
			},
			wantError: "without required scopes \"cache.read cache.write\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			workspaceRoot := t.TempDir()
			writeTestAuth(t, home)
			writeLinkFile(t, workspaceRoot)
			fake := newFakeTransport(t)
			tc.configure(fake)
			var output []string

			err := RunCommand("token", IO{
				Env: hometest.Env(home, map[string]string{
					"PUTNAMI_AUTH_URL":       testBaseURL,
					"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
				}),
				Stdout: func(line string) { output = append(output, line) },
				Stderr: func(string) {},
				Client: fake.client(),
				Now:    fixedNow,
			}, []string{"--for", "cache"})
			if err == nil {
				t.Fatal("cloud token --for cache succeeded with an invalid auth response")
			}
			assertContains(t, err.Error(), tc.wantError)
			if len(output) != 0 {
				t.Fatalf("invalid cache mint wrote bearer output: %v", output)
			}
			if len(fake.mintedKeys) != 1 || len(fake.revokedIDs) != 1 || fake.revokedIDs[0] != fake.mintedKeys[0]["id"] {
				t.Fatalf("ephemeral key cleanup minted=%v revoked=%v", fake.mintedKeys, fake.revokedIDs)
			}
		})
	}
}

func TestCloudTokenForCacheCompatibilityFlagIsRemoved(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)

	err := RunCommand("token", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: newFakeTransport(t).client(),
		Now:    fixedNow,
	}, []string{"--for-cache"})
	if err == nil {
		t.Fatalf("expected --for-cache to be rejected")
	}
	assertContains(t, err.Error(), "--for cache")
}

// TestCacheStatusReportsConfig proves `cloud cache status` surfaces the
// persisted config, bound workspace, and local identity.
func TestCacheStatusReportsConfig(t *testing.T) {
	home, workspaceRoot := setupCacheEnv(t)
	runSetup(t, home, workspaceRoot)

	var output []string
	err := RunCommand("cache", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: newFakeTransport(t).client(),
		Now:    fixedNow,
	}, []string{"status", "--json"})
	if err != nil {
		t.Fatalf("cache status: %v", err)
	}

	var got clicore.StatusNode
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got.ID != "cache" || got.State != clicore.StatusOK {
		t.Fatalf("status = %+v, want an ok cache node", got)
	}
	config, _ := got.Child("cache.config")
	if config.State != clicore.StatusOK || !strings.HasPrefix(config.Detail, "enabled in ") || !strings.HasSuffix(config.Detail, "(mode full)") {
		t.Fatalf("config = %+v", config)
	}
	identity, _ := got.Child("cache.identity")
	if identity.State != clicore.StatusOK || !strings.Contains(identity.Detail, "dev@example.com") {
		t.Fatalf("identity = %+v", identity)
	}
	if got.Detail != "enabled; last run reused 30 of 40 tasks" || len(got.Metrics) != 3 || got.Metrics[0].ID != "reused_tasks" {
		t.Fatalf("detail = %q, metrics = %+v", got.Detail, got.Metrics)
	}
}

func TestCacheStatusNotConfigured(t *testing.T) {
	workspaceRoot := t.TempDir()
	var output []string
	err := RunCommand("cache", IO{
		Env:    hometest.Env(t.TempDir(), map[string]string{"PUTNAMI_WORKSPACE_ROOT": workspaceRoot}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Now:    fixedNow,
	}, []string{"status", "--json"})
	if err != nil {
		t.Fatalf("cache status: %v", err)
	}
	var got clicore.StatusNode
	decodeJSON(t, strings.Join(output, "\n"), &got)
	config, _ := got.Child("cache.config")
	if got.State != clicore.StatusDegraded || config.Fix != "putnami cloud setup" || got.Detail != "not configured" {
		t.Fatalf("status = %+v, want degraded naming the setup", got)
	}
}

// TestCacheDisable proves both teardown paths: enabled:false (default) and full
// file removal (--remove).
func TestCacheDisable(t *testing.T) {
	home, workspaceRoot := setupCacheEnv(t)
	runSetup(t, home, workspaceRoot)

	cacheIO := func(out *[]string) IO {
		return IO{
			Env:    hometest.Env(home, map[string]string{"PUTNAMI_WORKSPACE_ROOT": workspaceRoot}),
			Stdout: func(line string) { *out = append(*out, line) },
			Stderr: func(string) {},
			Now:    fixedNow,
		}
	}

	var disableOut []string
	if err := RunCommand("cache", cacheIO(&disableOut), []string{"disable"}); err != nil {
		t.Fatalf("cache disable: %v", err)
	}
	var cache map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), &cache)
	if cache["enabled"] != false {
		t.Fatalf("enabled = %v, want false after disable", cache["enabled"])
	}

	var removeOut []string
	if err := RunCommand("cache", cacheIO(&removeOut), []string{"disable", "--remove"}); err != nil {
		t.Fatalf("cache disable --remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, deliverycli.CacheFileRelative)); !os.IsNotExist(err) {
		t.Fatalf("expected cache.json removed, stat err = %v", err)
	}
}

// TestExtensionManifestRegistersCacheSurface locks the manifest contract for
// the cache command, the setup cache flags, and the token --for flag.
func TestExtensionManifestRegistersCacheSurface(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}

	subcommands := manifest["commandGroups"].(map[string]any)["cloud"].(map[string]any)["subcommands"].(map[string]any)
	cache, ok := subcommands["cache"].(map[string]any)
	if !ok || cache["command"] != "cloud-cache" {
		t.Fatalf("cloud cache subcommand = %v, want cloud-cache", subcommands["cache"])
	}

	commands := manifest["commands"].(map[string]any)
	cacheCommand, ok := commands["cloud-cache"].(map[string]any)
	if !ok {
		t.Fatalf("cloud-cache command missing")
	}
	cacheRun := cacheCommand["run"].([]any)[0].(map[string]any)
	if cacheRun["task"] != "cloud-cache" {
		t.Fatalf("cloud-cache run task = %v", cacheRun["task"])
	}

	tokenFlags := commands["cloud-token"].(map[string]any)["flags"].(map[string]any)
	if _, ok := tokenFlags["for"]; !ok {
		t.Fatalf("cloud-token missing --for flag")
	}
	if _, ok := tokenFlags["for-cache"]; ok {
		t.Fatalf("cloud-token should not advertise removed --for-cache flag")
	}

	setupFlags := commands["cloud-setup"].(map[string]any)["flags"].(map[string]any)
	for _, flag := range []string{"cache", "cache-mode", "cache-url"} {
		if _, ok := setupFlags[flag]; !ok {
			t.Fatalf("cloud-setup missing --%s flag", flag)
		}
	}

	cacheTask := manifest["tasks"].(map[string]any)["cloud-cache"].(map[string]any)
	cacheArgs := cacheTask["args"].([]any)
	if len(cacheArgs) != 1 || cacheArgs[0] != "cache" {
		t.Fatalf("cloud-cache task args = %v, want [cache]", cacheArgs)
	}
}
