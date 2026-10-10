package cloudcli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	deliverycli "go.putnami.dev/cloud/extension/internal/deliverycli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
)

const testBaseURL = "https://control.test"

func TestFirstNonNilSupportsFunctionAndInterfaceSeams(t *testing.T) {
	var missing func(string)
	called := false
	want := func(string) { called = true }
	firstNonNil(missing, want)("ok")
	if !called {
		t.Fatal("firstNonNil did not skip a typed nil function")
	}

	ctx := context.Background()
	if got := firstNonNil[context.Context](nil, ctx); got != ctx {
		t.Fatalf("context = %v, want %v", got, ctx)
	}
}

// apiKeyGrantType is the OAuth2 api-key grant-type URN the aggregator's fake
// transport asserts the registry-token path sends. The registry-token command
// itself lives in internal/distributioncli (which keeps the canonical unexported
// copy); this aggregator-side fake test double keeps a local copy of the
// contract string rather than widening the lib's API surface for a test.
const apiKeyGrantType = "urn:putnami:params:oauth:grant-type:api-key" //nolint:gosec // G101: not a credential, an OAuth2 grant-type URN string

type recordedRequest struct {
	Method  string
	Path    string
	Header  http.Header
	Body    map[string]any
	RawBody string
}

type fakeTransport struct {
	t                  *testing.T
	requests           []recordedRequest
	mintedKeys         []map[string]any
	revokedIDs         []string
	omitWorkspaceScope bool
	refreshToken       string
	refreshResponse    map[string]any
	omitRawAPIKey      bool
	apiKeyGrantAud     string
	apiKeyGrantScope   string
	// workspaceSlug is the slug GET /v1/workspaces/ws-acme answers. Empty
	// answers no slug.
	workspaceSlug string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestLoginPersistsStandardCredentialStore(t *testing.T) {
	home := t.TempDir()
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("login", IO{
		Env:    hometest.Env(home, nil),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--auth-url", testBaseURL, "--no-open"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	var auth storedToken
	readJSONFile(t, filepath.Join(home, AuthFileRelative), &auth)
	if !regexp.MustCompile(`^[^.]+\.[^.]+\.sig$`).MatchString(auth.AccessToken) {
		t.Fatalf("access token shape = %q", auth.AccessToken)
	}
	if auth.RefreshToken != "refresh-token" {
		t.Fatalf("refresh token = %q", auth.RefreshToken)
	}
	if auth.TokenType != "Bearer" {
		t.Fatalf("token type = %q", auth.TokenType)
	}
	if auth.ExpiresAt != "2026-04-30T12:05:00.000Z" {
		t.Fatalf("expires_at = %q", auth.ExpiresAt)
	}
	if auth.Issuer != testBaseURL {
		t.Fatalf("issuer = %q", auth.Issuer)
	}

	joined := strings.Join(output, "\n")
	assertContains(t, joined, "To authenticate, visit:\n    https://control.test/device")
	assertContains(t, joined, "Enter code: BCDF-GHJK")
	assertContains(t, joined, "Or open this link directly:\n    https://control.test/device?code=BCDF-GHJK")
	assertContains(t, joined, "Waiting for authorization")
	assertContains(t, joined, "Signed in as dev@example.com")

	req := fake.find("/device/authorize")
	if req == nil {
		t.Fatal("device authorize request not recorded")
	}
	if req.Body["client_id"] != "putnami-cli" {
		t.Fatalf("client_id = %v", req.Body["client_id"])
	}
	if req.Body["scope"] != "openid profile email apikeys:write intelligence.read intelligence.audit intelligence.review.read intelligence.review.manage" {
		t.Fatalf("scope = %v", req.Body["scope"])
	}

	// Login is intentionally low-friction and has no registry target coordinate,
	// so it writes endpoint records but neither recipes nor broad api keys.
	if len(fake.mintedKeys) != 0 {
		t.Fatalf("mintedKeys = %d, want 0", len(fake.mintedKeys))
	}

	// registries.json records the endpoint index plus each registry's token
	// RECIPE. The recipe names only the registry kind — no owner workspace, no
	// package, no action — so it cannot widen authority: the registry derives
	// what this user may read or write per namespace from IAM.
	var state distributioncli.RegistriesState
	readJSONFile(t, filepath.Join(home, ".putnami/registries.json"), &state)
	if len(state.Keys) != 4 {
		t.Fatalf("state.Keys = %d, want 4", len(state.Keys))
	}
	wantRecipe := map[distributioncli.Registry]string{
		distributioncli.RegistryNPM: "npm", distributioncli.RegistryGomod: "go",
		distributioncli.RegistryOCI: "oci", distributioncli.RegistryPut: "put",
	}
	for _, ref := range state.Keys {
		if ref.ID != "" || ref.Prefix != "" {
			t.Fatalf("%s recipe should not have a minted key yet: %+v", ref.Registry, ref)
		}
		if ref.Token == nil {
			t.Fatalf("%s login wrote no token recipe: %+v", ref.Registry, ref)
		}
		want := []string{"putnami", "cloud", "token", "--for", wantRecipe[ref.Registry]}
		if !slices.Equal(ref.Token.Command, want) || ref.Token.URL != "" {
			t.Fatalf("%s recipe = %+v, want command %v", ref.Registry, ref.Token, want)
		}
	}
	if len(state.Auth) != 0 || len(state.PutAuth) != 0 {
		t.Fatalf("resolver auth should be empty after login: auth=%#v put_auth=%#v", state.Auth, state.PutAuth)
	}
	assertFileMode(t, filepath.Join(home, ".putnami/registries.json"), 0o600)

	// The Putnami npm registry now uses the TokenSource resolver; login must
	// not leave a static authToken in .npmrc.
	if data, err := os.ReadFile(filepath.Join(home, ".npmrc")); err == nil {
		assertNotContains(t, string(data), "//npm.putnami.dev/:_authToken=")
	} else if !os.IsNotExist(err) {
		t.Fatalf("read npmrc: %v", err)
	}

	// The gomod token is also lazy, so login must not create .netrc.
	if _, err := os.Stat(filepath.Join(home, ".netrc")); !os.IsNotExist(err) {
		t.Fatalf("netrc should not be created before resolving gomod auth: %v", err)
	}

	// OCI publish also resolves through the TokenSource recipe; login should
	// not create Docker auth or helper config for the Putnami host.
	if _, err := os.Stat(filepath.Join(home, ".docker/config.json")); !os.IsNotExist(err) {
		t.Fatalf("docker config should not be created for putnami OCI auth: %v", err)
	}
}

func TestLoginSuggestsSetupWhenWorkspaceRootIsUnlinked(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{"name": "local"})
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("login", IO{
		Env:    hometest.Env(home, map[string]string{"PUTNAMI_WORKSPACE_ROOT": workspaceRoot}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--auth-url", testBaseURL, "--no-open"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	joined := strings.Join(output, "\n")
	assertContains(t, joined, "Signed in as dev@example.com")
	assertContains(t, joined, "Next: run `putnami cloud setup` to configure this repository for Putnami Cloud.")
}

func TestLoginDoesNotSuggestSetupWhenWorkspaceRootIsLinked(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{
		"name": "local",
		"options": map[string]any{
			"@putnami/cloud": map[string]any{
				"workspace": map[string]any{
					"workspace_id":        "ws-acme",
					"control_plane_url":   testBaseURL,
					"workspace_name":      "Acme Workspace",
					"default_environment": "prod",
				},
			},
		},
	})
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("login", IO{
		Env:    hometest.Env(home, map[string]string{"PUTNAMI_WORKSPACE_ROOT": workspaceRoot}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--auth-url", testBaseURL, "--no-open"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	joined := strings.Join(output, "\n")
	assertContains(t, joined, "Signed in as dev@example.com")
	assertNotContains(t, joined, "Next: run `putnami cloud setup`")
}

func TestLogoutRevokesRegistryKeysAndRemovesConfig(t *testing.T) {
	home := t.TempDir()
	fake := newFakeTransport(t)

	if err := RunCommand("login", IO{
		Env:    hometest.Env(home, nil),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--auth-url", testBaseURL, "--no-open"}); err != nil {
		t.Fatalf("login: %v", err)
	}
	mintedIDs := make([]string, 0, len(fake.mintedKeys))
	for _, k := range fake.mintedKeys {
		mintedIDs = append(mintedIDs, stringValue(k["id"]))
	}

	if err := RunCommand("logout", IO{
		Env:    hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, nil); err != nil {
		t.Fatalf("logout: %v", err)
	}

	for _, id := range mintedIDs {
		found := false
		for _, r := range fake.revokedIDs {
			if r == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected key %s to be revoked", id)
		}
	}
	if _, err := os.Stat(filepath.Join(home, AuthFileRelative)); !os.IsNotExist(err) {
		t.Fatalf("auth.json still present after logout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".putnami/registries.json")); !os.IsNotExist(err) {
		t.Fatalf("registries.json still present after logout: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(home, ".npmrc")); err == nil {
		if strings.Contains(string(data), "//npm.putnami.dev/:_authToken=") {
			t.Fatalf("npmrc still has putnami auth line: %s", string(data))
		}
	}
}

func TestLoginIsIdempotentAndPreservesOtherEntries(t *testing.T) {
	home := t.TempDir()

	// Seed pre-existing entries the user has for OTHER registries — login
	// must not clobber these.
	mustMkdir(t, home)
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte("//npm.example.com/:_authToken=other\n//npm.putnami.dev/:_authToken=legacy\nstrict-ssl=true\n"), 0o600); err != nil {
		t.Fatalf("seed npmrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine github.com login me password ghp_xxx\n"), 0o600); err != nil {
		t.Fatalf("seed netrc: %v", err)
	}

	fake := newFakeTransport(t)
	for i := 0; i < 2; i++ {
		if err := RunCommand("login", IO{
			Env:    hometest.Env(home, nil),
			Stdout: func(string) {},
			Stderr: func(string) {},
			Client: fake.client(),
			Now:    fixedNow,
		}, []string{"--auth-url", testBaseURL, "--no-open"}); err != nil {
			t.Fatalf("login #%d: %v", i+1, err)
		}
	}

	// Login writes recipes only; explicit registries setup or token resolution
	// does the minting.
	if len(fake.mintedKeys) != 0 {
		t.Fatalf("mintedKeys = %d, want 0", len(fake.mintedKeys))
	}
	if len(fake.revokedIDs) != 0 {
		t.Fatalf("revokedIDs = %d, want 0", len(fake.revokedIDs))
	}

	npmrc := readFileString(t, filepath.Join(home, ".npmrc"))
	assertContains(t, npmrc, "//npm.example.com/:_authToken=other")
	assertContains(t, npmrc, "strict-ssl=true")
	// Putnami npm auth is resolver-based now, so re-logging in must not add a
	// static .npmrc entry while preserving third-party registry settings.
	count := strings.Count(npmrc, "//npm.putnami.dev/:_authToken=")
	if count != 0 {
		t.Fatalf("npmrc putnami entry count = %d, want 0, file:\n%s", count, npmrc)
	}

	netrc := readFileString(t, filepath.Join(home, ".netrc"))
	assertContains(t, netrc, "machine github.com login me password ghp_xxx")
	netrcCount := strings.Count(netrc, "machine go.putnami.dev")
	if netrcCount != 0 {
		t.Fatalf("netrc putnami entry count = %d, want 0", netrcCount)
	}
}

// TestLoginPreservesDockerConfigWithoutOciStaticAuth covers the macOS Docker
// Desktop case after the OCI TokenSource migration: login must leave Docker's
// native config alone except for cleaning legacy Putnami-host static auth.
func TestLoginPreservesDockerConfigWithoutOciStaticAuth(t *testing.T) {
	home := t.TempDir()
	mustMkdir(t, filepath.Join(home, ".docker"))
	writeJSONFile(t, filepath.Join(home, ".docker/config.json"), map[string]any{
		"auths": map[string]any{
			"gcr.io":          map[string]any{},
			"oci.putnami.dev": map[string]any{"auth": "legacy"},
		},
		"credHelpers": map[string]any{
			"gcr.io":          "gcloud",
			"oci.putnami.dev": "",
		},
		"credsStore":     "desktop",
		"currentContext": "desktop-linux",
	})

	fake := newFakeTransport(t)
	if err := RunCommand("login", IO{
		Env:    hometest.Env(home, nil),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--auth-url", testBaseURL, "--no-open"}); err != nil {
		t.Fatalf("login: %v", err)
	}

	cfg := map[string]any{}
	readJSONFile(t, filepath.Join(home, ".docker/config.json"), &cfg)

	// No inline auth is written for the Putnami host, and the seeded host is
	// left in place.
	auths, _ := cfg["auths"].(map[string]any)
	if _, ok := auths["oci.putnami.dev"]; ok {
		t.Fatalf("oci.putnami.dev auth should not be written: %v", auths)
	}
	if _, ok := auths["gcr.io"]; !ok {
		t.Fatalf("seeded gcr.io auth entry was dropped: %v", auths)
	}

	// No helper pin is written for the Putnami host, and the user's gcloud
	// mapping is untouched.
	helpers, _ := cfg["credHelpers"].(map[string]any)
	if _, ok := helpers["oci.putnami.dev"]; ok {
		t.Fatalf("credHelpers[oci.putnami.dev] should not be written: %v", helpers)
	}
	if helpers["gcr.io"] != "gcloud" {
		t.Fatalf("credHelpers[gcr.io] = %v, want \"gcloud\"", helpers["gcr.io"])
	}

	// Global credsStore is preserved — we only override resolution for
	// the one host.
	if cfg["credsStore"] != "desktop" {
		t.Fatalf("credsStore = %v, want \"desktop\"", cfg["credsStore"])
	}
	if cfg["currentContext"] != "desktop-linux" {
		t.Fatalf("currentContext = %v (unrelated keys must be preserved)", cfg["currentContext"])
	}

	// Logout still leaves unrelated Docker config alone.
	if err := RunCommand("logout", IO{
		Env:    hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, nil); err != nil {
		t.Fatalf("logout: %v", err)
	}

	cfg = map[string]any{}
	readJSONFile(t, filepath.Join(home, ".docker/config.json"), &cfg)
	auths, _ = cfg["auths"].(map[string]any)
	if _, ok := auths["oci.putnami.dev"]; ok {
		t.Fatalf("oci.putnami.dev auth still present after logout: %v", auths)
	}
	if _, ok := auths["gcr.io"]; !ok {
		t.Fatalf("seeded gcr.io auth entry must survive logout: %v", auths)
	}
	helpers, _ = cfg["credHelpers"].(map[string]any)
	if _, ok := helpers["oci.putnami.dev"]; ok {
		t.Fatalf("credHelpers[oci.putnami.dev] still present after logout: %v", helpers)
	}
	if helpers["gcr.io"] != "gcloud" {
		t.Fatalf("credHelpers[gcr.io] must survive logout: %v", helpers)
	}
	if cfg["credsStore"] != "desktop" {
		t.Fatalf("credsStore must survive logout: %v", cfg["credsStore"])
	}
}

// TestLoginPreservesUserConfiguredOciCredHelper guards the cleanup asymmetry:
// the CLI may remove its legacy empty helper pin, but it must not silently
// nuke a real helper the user configured for oci.putnami.dev.
func TestLoginPreservesUserConfiguredOciCredHelper(t *testing.T) {
	home := t.TempDir()
	mustMkdir(t, filepath.Join(home, ".docker"))
	writeJSONFile(t, filepath.Join(home, ".docker/config.json"), map[string]any{
		"auths": map[string]any{
			"oci.putnami.dev": map[string]any{"auth": "legacy"},
		},
		"credHelpers": map[string]any{
			"oci.putnami.dev": "some-user-helper",
		},
	})
	fake := newFakeTransport(t)

	if err := RunCommand("login", IO{
		Env:    hometest.Env(home, nil),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--auth-url", testBaseURL, "--no-open"}); err != nil {
		t.Fatalf("login: %v", err)
	}

	cfg := map[string]any{}
	readJSONFile(t, filepath.Join(home, ".docker/config.json"), &cfg)
	auths, _ := cfg["auths"].(map[string]any)
	if _, ok := auths["oci.putnami.dev"]; ok {
		t.Fatalf("legacy oci.putnami.dev auth should be removed on login: %v", auths)
	}
	helpers, _ := cfg["credHelpers"].(map[string]any)
	if helpers["oci.putnami.dev"] != "some-user-helper" {
		t.Fatalf("user-installed helper for oci.putnami.dev was clobbered on login: %v", helpers)
	}

	if err := RunCommand("logout", IO{
		Env:    hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, nil); err != nil {
		t.Fatalf("logout: %v", err)
	}

	cfg = map[string]any{}
	readJSONFile(t, filepath.Join(home, ".docker/config.json"), &cfg)
	helpers, _ = cfg["credHelpers"].(map[string]any)
	if helpers["oci.putnami.dev"] != "some-user-helper" {
		t.Fatalf("user-installed helper for oci.putnami.dev was clobbered on logout: %v", helpers)
	}
}

func TestWhoamiPrintsTheStatusNode(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home)
	workspaceRoot := t.TempDir()
	writeLinkFile(t, workspaceRoot)
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("whoami", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL": testBaseURL, "PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--json"})
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}

	var node clicore.StatusNode
	decodeJSON(t, strings.Join(output, "\n"), &node)
	var envelope map[string]any
	decodeRawJSON(t, strings.Join(output, "\n"), &envelope)
	if envelope["command"] != "cloud whoami" || envelope["status"] != "success" || envelope["exitCode"].(float64) != 0 {
		t.Fatalf("result envelope = %v", envelope)
	}
	if node.ID != "whoami" || node.State != clicore.StatusOK || len(node.Children) != 2 {
		t.Fatalf("node = %+v", node)
	}
	if node.Children[0].Detail != "signed in as dev@example.com" || node.Children[1].Detail != "linked to workspace ws-acme" {
		t.Fatalf("children = %+v", node.Children)
	}
}

func TestSetupPersistsResolvedWorkspaceConfig(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{"name": "local"})
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
	}, []string{"--workspace", "ws-acme", "--control-plane-url", testBaseURL, "--json"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var link map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, LinkFileRelative), &link)
	if link["version"].(float64) != 1 {
		t.Fatalf("version = %v", link["version"])
	}
	if link["control_plane_url"] != testBaseURL {
		t.Fatalf("control_plane_url = %v", link["control_plane_url"])
	}
	if _, hasTenant := link["tenant_id"]; hasTenant {
		t.Fatalf("link file must not include tenant_id: %v", link)
	}
	if link["workspace_id"] != "ws-acme" {
		t.Fatalf("link workspace_id = %v", link["workspace_id"])
	}
	if link["workspace_name"] != "Acme Workspace" {
		t.Fatalf("workspace_name = %v", link["workspace_name"])
	}
	if link["environment"] != "prod" {
		t.Fatalf("environment = %v", link["environment"])
	}
	if link["repository"] != "github.com/example/acme" {
		t.Fatalf("repository = %v", link["repository"])
	}
	var manifest map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), &manifest)
	options := manifest["options"].(map[string]any)
	cloud := options["@putnami/cloud"].(map[string]any)
	workspace := cloud["workspace"].(map[string]any)
	if workspace["workspace_id"] != "ws-acme" {
		t.Fatalf("manifest workspace_id = %v", workspace["workspace_id"])
	}
	// The committed manifest carries only the connection identity: the
	// workspace id and, because this run used a non-default control plane, its
	// URL. The resolved metadata above (name/environment/repository/timestamp)
	// lives only in the local cache, never in the source-controlled manifest.
	if workspace["control_plane_url"] != testBaseURL {
		t.Fatalf("manifest control_plane_url = %v, want %q", workspace["control_plane_url"], testBaseURL)
	}
	for _, key := range []string{"version", "workspace_name", "environment", "repository", "linked_at"} {
		if value, present := workspace[key]; present {
			t.Fatalf("manifest workspace block must omit %q, got %v", key, value)
		}
	}

	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got["configured"] != true {
		t.Fatalf("configured = %v, want true", got["configured"])
	}
	gotWorkspace := got["workspace"].(map[string]any)
	if gotWorkspace["workspace_id"] != "ws-acme" {
		t.Fatalf("result workspace_id = %v", gotWorkspace["workspace_id"])
	}
}

// TestSetupNeverScrubsNativeRegistryCredentials is the `cloud setup`
// counterpart to TestInstallNeverScrubsNativeRegistryCredentials: `setup`
// shares install's WriteRegistryTokenRecipes call (install falls back to
// setup when a repository has no Cloud link yet), so it must leave native
// credential files alone too. Only `cloud login`, `cloud registries setup`,
// and `cloud logout`/`cloud registries logout` are explicit enough to touch
// them.
func TestSetupNeverScrubsNativeRegistryCredentials(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{"name": "local"})

	mustMkdir(t, home)
	npmrcContent := "//npm.putnami.dev/:_authToken=pkt_valid_token_abc123\n"
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(npmrcContent), 0o600); err != nil {
		t.Fatalf("seed npmrc: %v", err)
	}
	netrcContent := "machine go.putnami.dev login _token password pkt_valid_token_def456\n"
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrcContent), 0o600); err != nil {
		t.Fatalf("seed netrc: %v", err)
	}

	fake := newFakeTransport(t)
	err := RunCommand("setup", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    func() time.Time { return time.Date(2026, 4, 30, 12, 10, 0, 0, time.UTC) },
	}, []string{"--workspace", "ws-acme", "--control-plane-url", testBaseURL, "--json"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	npmrc, statErr := os.ReadFile(filepath.Join(home, ".npmrc"))
	if statErr != nil {
		t.Fatalf("npmrc must survive setup (not be deleted): %v", statErr)
	}
	if string(npmrc) != npmrcContent {
		t.Fatalf("setup rewrote npmrc:\n got  %q\n want %q", string(npmrc), npmrcContent)
	}

	netrc, statErr := os.ReadFile(filepath.Join(home, ".netrc"))
	if statErr != nil {
		t.Fatalf("netrc must survive setup (not be deleted): %v", statErr)
	}
	if string(netrc) != netrcContent {
		t.Fatalf("setup rewrote netrc:\n got  %q\n want %q", string(netrc), netrcContent)
	}
}

// TestSetupOmitsDefaultControlPlaneURLFromManifest proves the committed
// manifest stays minimal when the workspace targets the public control plane:
// only workspace_id is written (the default URL is implied), while the local
// cache still records the resolved control_plane_url for older tooling.
func TestSetupOmitsDefaultControlPlaneURLFromManifest(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{"name": "local"})
	fake := newFakeTransport(t)

	err := RunCommand("setup", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    func() time.Time { return time.Date(2026, 4, 30, 12, 10, 0, 0, time.UTC) },
	}, []string{"--workspace", "ws-acme", "--control-plane-url", DefaultControlPlaneURL, "--no-cache", "--json"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var manifest map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), &manifest)
	options := manifest["options"].(map[string]any)
	cloud := options["@putnami/cloud"].(map[string]any)
	workspace := cloud["workspace"].(map[string]any)
	if workspace["workspace_id"] != "ws-acme" {
		t.Fatalf("manifest workspace_id = %v", workspace["workspace_id"])
	}
	if _, present := workspace["control_plane_url"]; present {
		t.Fatalf("manifest must omit control_plane_url when it equals the default, got %v", workspace["control_plane_url"])
	}
	if len(workspace) != 1 {
		t.Fatalf("manifest workspace block must contain only workspace_id, got %v", workspace)
	}

	// The local cache still records the resolved URL so the legacy read path
	// and workspace-root discovery keep working without a manifest re-resolve.
	var link map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, LinkFileRelative), &link)
	if link["control_plane_url"] != DefaultControlPlaneURL {
		t.Fatalf("cache control_plane_url = %v, want %q", link["control_plane_url"], DefaultControlPlaneURL)
	}
}

// TestInstallNeverWritesAgentWiring keeps host files out of Cloud install: the
// @putnami/intelligence extension owns the shared agent wiring.
func TestInstallNeverWritesAgentWiring(t *testing.T) {
	home, workspaceRoot := setupCacheEnv(t)
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{
		"name": "local", "options": map[string]any{"@putnami/cloud": map[string]any{
			"workspace": map[string]any{"workspace_id": "ws-acme", "control_plane_url": testBaseURL},
		}},
	})
	humanFiles := map[string]string{".mcp.json": `{"mcpServers":{"human":{"command":"custom"}}}`, ".codex/config.toml": "model = \"custom\"\n", ".gitignore": "human-private\n"}
	for path, data := range humanFiles {
		full := filepath.Join(workspaceRoot, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := readFileString(t, filepath.Join(workspaceRoot, "putnami.workspace.json"))
	err := RunCommand("install", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(string) {}, Stderr: func(string) {}, Client: newFakeTransport(t).client(), Now: fixedNow,
	}, []string{"--workspace", "ws-acme", "--control-plane-url", testBaseURL, "--no-cache"})
	if err != nil {
		t.Fatal(err)
	}
	if after := readFileString(t, filepath.Join(workspaceRoot, "putnami.workspace.json")); after != before {
		t.Fatal("install changed the manifest or selected a workspace")
	}
	for path, before := range humanFiles {
		if after := readFileString(t, filepath.Join(workspaceRoot, path)); after != before {
			t.Fatalf("install implicitly changed %s: %q", path, after)
		}
	}
}

func TestSetupManifestFailureLeavesTheRepositoryUnchanged(t *testing.T) {
	home, workspaceRoot := setupCacheEnv(t)
	manifestPath := filepath.Join(workspaceRoot, "putnami.workspace.json")
	before := readFileString(t, manifestPath)
	original := setupAtomicWrite
	setupAtomicWrite = func(string, []byte, os.FileMode) error {
		return errors.New("injected manifest write failure")
	}
	t.Cleanup(func() { setupAtomicWrite = original })

	err := RunCommand("setup", IO{
		Env:    hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL, "PUTNAMI_WORKSPACE_ROOT": workspaceRoot}),
		Stdout: func(string) {}, Stderr: func(string) {}, Client: newFakeTransport(t).client(), Now: fixedNow,
	}, []string{"--workspace", "ws-acme", "--control-plane-url", testBaseURL, "--no-cache"})
	if err == nil || !strings.Contains(err.Error(), "injected manifest write failure") {
		t.Fatalf("setup error = %v", err)
	}
	if after := readFileString(t, manifestPath); after != before {
		t.Fatalf("setup changed manifest after failed commit: before=%q after=%q", before, after)
	}
	for _, relative := range []string{".mcp.json", ".codex/config.toml"} {
		if _, statErr := os.Stat(filepath.Join(workspaceRoot, filepath.FromSlash(relative))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rollback left %s: %v", relative, statErr)
		}
	}
}

func TestSetupUsesInjectedWorkspaceObject(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeTestAuth(t, home)
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{"name": "local"})
	writeJSONFile(t, contextFile, map[string]any{
		"workspaceRoot": workspaceRoot,
		"params": map[string]any{
			"json": true,
			"workspace": map[string]any{
				"version":           1,
				"control_plane_url": testBaseURL,
				"workspace_id":      "ws-acme",
				"workspace_name":    "Acme Workspace",
				"environment":       "prod",
				"linked_at":         "2026-04-30T12:00:00Z",
			},
		},
	})
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("setup", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL": testBaseURL,
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    func() time.Time { return time.Date(2026, 4, 30, 12, 10, 0, 0, time.UTC) },
	}, []string{"--putnamiContext", contextFile, "--json"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	if req := fake.find("/v1/workspaces/ws-acme"); req == nil || req.Method != http.MethodGet {
		t.Fatalf("setup should verify /v1/workspaces/ws-acme, requests = %+v", fake.requests)
	}
	for _, req := range fake.requests {
		if strings.Contains(req.Path, "map%5B") || strings.Contains(req.Path, "map[") {
			t.Fatalf("setup stringified workspace object into request path: %+v", req)
		}
	}
	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	workspace := got["workspace"].(map[string]any)
	if workspace["workspace_id"] != "ws-acme" {
		t.Fatalf("workspace_id = %v, want ws-acme", workspace["workspace_id"])
	}
}

func TestSetupPreservesManifestOrderAndFormatting(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	manifestPath := filepath.Join(workspaceRoot, "putnami.workspace.json")
	manifest := `{
  "name": "local",
  "version": "1.0.0",
  "type": "workspace",
  "projects": [
    "apps/auth-server"
  ],
  "options": {
    "existing": true,
    "@putnami/cloud": {
      "enabled": true
    }
  }
}
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	fake := newFakeTransport(t)

	err := RunCommand("setup", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    func() time.Time { return time.Date(2026, 4, 30, 12, 10, 0, 0, time.UTC) },
	}, []string{"--workspace", "ws-acme", "--control-plane-url", testBaseURL})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	updated := readFileString(t, manifestPath)
	for _, want := range []string{
		`  "projects": [
    "apps/auth-server"
  ],`,
		`    "existing": true,`,
		`      "enabled": true,`,
		`      "workspace": {`,
		`        "workspace_id": "ws-acme"`,
	} {
		assertContains(t, updated, want)
	}
	nameAt := strings.Index(updated, `"name"`)
	versionAt := strings.Index(updated, `"version"`)
	typeAt := strings.Index(updated, `"type"`)
	projectsAt := strings.Index(updated, `"projects"`)
	optionsAt := strings.Index(updated, `"options"`)
	if !(nameAt < versionAt && versionAt < typeAt && typeAt < projectsAt && projectsAt < optionsAt) {
		t.Fatalf("manifest top-level key order changed:\n%s", updated)
	}
}

func TestCloudTokenMintsScopedAccessFromManifestLink(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	fake := newFakeTransport(t)
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
	}, []string{"--json"})
	if err != nil {
		t.Fatalf("cloud token: %v", err)
	}

	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got["workspace_id"] != "ws-acme" {
		t.Fatalf("workspace_id = %v", got["workspace_id"])
	}
	claims := decodeJWT(stringValue(got["access_token"]))
	if claimedWorkspaceID(claims) != "ws-acme" {
		t.Fatalf("token workspace = %v", claims["scope_ref"])
	}

	if err := RunCommand("token", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--json"}); err != nil {
		t.Fatalf("cloud token second call: %v", err)
	}
	refreshes := 0
	for _, req := range fake.requests {
		if req.Path == "/token" && req.Body["grant_type"] == "refresh_token" {
			refreshes++
		}
	}
	if refreshes != 1 {
		t.Fatalf("cloud token should reuse cached scoped access token; refresh calls = %d, want 1", refreshes)
	}
}

// TestCloudTokenGlobalMovedToTheOperatorCLI pins that the base
// login bearer is an operator credential, so the public command refuses
// --global before any request and names the command that mints it now.
func TestCloudTokenGlobalMovedToTheOperatorCLI(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home)
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("token", IO{
		Env:    hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--global", "--output=json"})
	var exitErr *cliError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitUsage {
		t.Fatalf("error = %T %v, want a usage error", err, err)
	}
	assertContains(t, err.Error(), "putnami operator token --global")
	if len(fake.requests) != 0 || len(output) != 0 {
		t.Fatalf("requests = %#v, stdout = %q, want neither", fake.requests, output)
	}
}

func TestCloudTokenSkipsInjectedPutnamiContextFlag(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{"json": true}})
	fake := newFakeTransport(t)
	var output []string

	err := RunCommand("token", IO{
		Env:    hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--putnamiContext", contextFile, "--json"})
	if err != nil {
		t.Fatalf("cloud token with injected context: %v", err)
	}

	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got["workspace_id"] != "ws-acme" {
		t.Fatalf("workspace_id = %v, want ws-acme", got["workspace_id"])
	}
}

func TestWorkspaceAuthPersistsRotatedRefreshWhenScopedTokenMissing(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	fake := newFakeTransport(t)
	fake.omitWorkspaceScope = true
	fake.refreshToken = "rotated-refresh-token"

	err := RunCommand("token", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--json"})
	if err == nil {
		t.Fatalf("expected missing workspace scope error")
	}
	assertContains(t, err.Error(), "without scope_ref.workspace_id")

	var auth storedToken
	readJSONFile(t, filepath.Join(home, AuthFileRelative), &auth)
	if auth.RefreshToken != "rotated-refresh-token" {
		t.Fatalf("refresh token = %q, want rotated-refresh-token", auth.RefreshToken)
	}
	if claimedWorkspaceID(decodeJWT(auth.AccessToken)) != "" {
		t.Fatalf("unscoped refresh response must not be cached as workspace access")
	}
	if _, ok := auth.WorkspaceAccess["ws-acme"]; ok {
		t.Fatalf("workspace cache should not be populated from an unscoped token: %+v", auth.WorkspaceAccess)
	}
}

func TestRefreshStoredAuthWithRetryWaitsForConcurrentWorkspaceRotation(t *testing.T) {
	home := t.TempDir()
	env := hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL})
	writeTestAuth(t, home)
	stale, err := readAuth(env, true)
	if err != nil {
		t.Fatalf("read stale auth: %v", err)
	}
	scopedToken := stringValue(tokenResponse(map[string]any{
		"scope_ref": map[string]any{"workspace_id": "ws-acme"},
	})["access_token"])
	rotated := &storedToken{
		AccessToken:  stringValue(tokenResponse(nil)["access_token"]),
		RefreshToken: "rotated-refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    "2026-04-30T13:00:00.000Z",
		Issuer:       testBaseURL,
		ClientID:     "putnami-cli",
		WorkspaceAccess: map[string]storedWorkspaceAccess{
			"ws-acme": {
				AccessToken: scopedToken,
				TokenType:   "Bearer",
				ExpiresAt:   "2026-04-30T13:00:00.000Z",
			},
		},
	}
	writeErr := make(chan error, 1)
	timer := time.AfterFunc(5*time.Millisecond, func() {
		writeErr <- writeAuth(rotated, env)
	})
	defer timer.Stop()

	tokenRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if req.Body != nil {
			data, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()
			_ = json.Unmarshal(data, &body)
		}
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/.well-known/openid-configuration":
			return jsonResponse(http.StatusOK, map[string]any{
				"issuer":         testBaseURL,
				"token_endpoint": testBaseURL + "/token",
			}), nil
		case req.Method == http.MethodPost && req.URL.Path == "/token":
			tokenRequests++
			if body["refresh_token"] != "refresh-token" {
				t.Fatalf("unexpected second refresh with token %q", body["refresh_token"])
			}
			return jsonResponse(http.StatusBadRequest, map[string]any{
				"error":             "invalid_grant",
				"error_description": "Invalid or expired refresh token",
			}), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})}

	next, base, err := refreshStoredAuthWithRetry(map[string]any{}, env, IO{Client: client, Now: fixedNow}, stale, "ws-acme")
	if writeErr := <-writeErr; writeErr != nil {
		t.Fatalf("write rotated auth: %v", writeErr)
	}
	if err != nil {
		t.Fatalf("refresh with concurrent rotation: %v", err)
	}
	if tokenRequests != 1 {
		t.Fatalf("token refresh requests = %d, want 1", tokenRequests)
	}
	if base.RefreshToken != "rotated-refresh-token" {
		t.Fatalf("base refresh token = %q, want rotated-refresh-token", base.RefreshToken)
	}
	if next.AccessToken != scopedToken {
		t.Fatalf("returned access token did not use the concurrently cached workspace token")
	}
}

func TestRefreshStoredAuthFreshWithRetryNeverReturnsConcurrentCachedGlobalAccess(t *testing.T) {
	home := t.TempDir()
	env := hometest.Env(home, map[string]string{"PUTNAMI_AUTH_URL": testBaseURL})
	writeTestAuth(t, home)
	stale, err := readAuth(env, true)
	if err != nil {
		t.Fatalf("read stale auth: %v", err)
	}
	cachedAccess := stringValue(tokenResponse(map[string]any{"concurrent_cache": true})["access_token"])
	latest := *stale
	latest.AccessToken = cachedAccess
	latest.ExpiresAt = fixedNow().Add(time.Hour).Format(time.RFC3339Nano)

	writeErr := make(chan error, 1)
	timer := time.AfterFunc(5*time.Millisecond, func() {
		writeErr <- writeAuth(&latest, env)
	})
	defer timer.Stop()

	tokenRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/.well-known/openid-configuration":
			return jsonResponse(http.StatusOK, map[string]any{
				"issuer": testBaseURL, "token_endpoint": testBaseURL + "/token",
			}), nil
		case req.Method == http.MethodPost && req.URL.Path == "/token":
			tokenRequests++
			return jsonResponse(http.StatusBadRequest, map[string]any{
				"error": "invalid_grant", "error_description": "Invalid or expired refresh token",
			}), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})}

	next, _, err := refreshStoredAuthFreshWithRetry(map[string]any{}, env, IO{Client: client, Now: fixedNow}, stale, "")
	if writeErr := <-writeErr; writeErr != nil {
		t.Fatalf("write concurrent auth: %v", writeErr)
	}
	if err == nil {
		t.Fatalf("strict refresh returned cached access %q as a fresh mint", next.AccessToken)
	}
	if next != nil {
		t.Fatalf("strict refresh result = %+v, want nil on invalid_grant", next)
	}
	if tokenRequests != 1 {
		t.Fatalf("token refresh requests = %d, want one failed mint and no cache success", tokenRequests)
	}
}

func TestInvalidGrantErrorSuggestsCloudLogin(t *testing.T) {
	err := newOAuthTokenError("invalid_grant", "Invalid or expired refresh token", ExitAuth)

	if !isOAuthTokenError(err, "invalid_grant") {
		t.Fatalf("expected invalid_grant oauth error, got %v", err)
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) {
		t.Fatalf("expected cliError, got %T", err)
	}
	if cliErr.Code != ExitAuth {
		t.Fatalf("exit code = %d, want %d", cliErr.Code, ExitAuth)
	}
	message := err.Error()
	assertContains(t, message, "Invalid or expired refresh token")
	assertContains(t, message, "putnami cloud login")
	assertContains(t, message, "refresh cloud credentials")
}

// TestSetupAutoCreatesWorkspaceFromPutnamiJSON proves that `cloud setup`
// without --workspace POSTs a new workspace using putnami.json's name,
// and writes the link file pointing at the server-generated id.
func TestSetupAutoCreatesWorkspaceFromPutnamiJSON(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	if err := os.WriteFile(filepath.Join(workspaceRoot, "putnami.json"), []byte(`{"name":"apps/api"}`), 0o644); err != nil {
		t.Fatalf("write putnami.json: %v", err)
	}
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
	}, []string{"--control-plane-url", testBaseURL, "--json"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	req := fake.find("/v1/workspaces")
	if req == nil || req.Method != http.MethodPost {
		t.Fatalf("expected POST /v1/workspaces, got %+v", req)
	}
	if req.Body["name"] != "apps/api" {
		t.Fatalf("body name = %v, want %q", req.Body["name"], "apps/api")
	}
	if _, present := req.Body["organization_id"]; present {
		t.Fatalf("organization_id should be omitted, got %v", req.Body["organization_id"])
	}

	var link map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, LinkFileRelative), &link)
	if link["workspace_id"] != "ws-generated-1" {
		t.Fatalf("workspace_id = %v, want ws-generated-1", link["workspace_id"])
	}
	if link["workspace_name"] != "apps/api" {
		t.Fatalf("workspace_name = %v", link["workspace_name"])
	}

	_ = output
}

// TestSetupFailsWithoutWorkspaceOrPutnamiJSON proves the auto-create path
// surfaces a clear usage error when no --workspace and no putnami.json are
// available to derive a name from.
func TestSetupFailsWithoutWorkspaceOrPutnamiJSON(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	fake := newFakeTransport(t)

	err := RunCommand("setup", IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":       testBaseURL,
			"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
		}),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    func() time.Time { return time.Date(2026, 4, 30, 12, 10, 0, 0, time.UTC) },
	}, []string{"--control-plane-url", testBaseURL})
	if err == nil {
		t.Fatalf("expected usage error")
	}
}

// TestSetupAutoConfiguresLinkedWorkspace proves `cloud setup --auto` with a
// known workspace id verifies and links it and enables the build cache, exactly
// like a plain setup for automation callers.
func TestSetupAutoConfiguresLinkedWorkspace(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeJSONFile(t, filepath.Join(workspaceRoot, "putnami.workspace.json"), map[string]any{"name": "local"})
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
	}, []string{"--auto", "--workspace", "ws-acme", "--control-plane-url", testBaseURL, "--json"})
	if err != nil {
		t.Fatalf("setup --auto: %v", err)
	}

	if req := fake.find("/v1/workspaces/ws-acme"); req == nil || req.Method != http.MethodGet {
		t.Fatalf("setup --auto should verify /v1/workspaces/ws-acme, requests = %+v", fake.requests)
	}
	if req := fake.find("/v1/workspaces"); req != nil && req.Method != http.MethodGet {
		t.Fatalf("setup --auto must not create a workspace, got %+v", req)
	}

	var link map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, LinkFileRelative), &link)
	if link["workspace_id"] != "ws-acme" {
		t.Fatalf("link workspace_id = %v, want ws-acme", link["workspace_id"])
	}

	var cache map[string]any
	readJSONFile(t, filepath.Join(workspaceRoot, deliverycli.CacheFileRelative), &cache)
	if cache["enabled"] != true {
		t.Fatalf("cache enabled = %v, want true", cache["enabled"])
	}

	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got["configured"] != true {
		t.Fatalf("configured = %v, want true", got["configured"])
	}
}

// TestSetupAutoSkipsWithoutLinkedWorkspace proves `--auto` never auto-creates a
// workspace: with no link and no --workspace it exits 0 without touching the
// control plane or writing a link file. This keeps automation safe to run on
// every fresh checkout.
func TestSetupAutoSkipsWithoutLinkedWorkspace(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
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
	}, []string{"--auto", "--control-plane-url", testBaseURL, "--json"})
	if err != nil {
		t.Fatalf("setup --auto should not error when unlinked: %v", err)
	}

	if req := fake.find("/v1/workspaces"); req != nil {
		t.Fatalf("setup --auto must not create a workspace, got %+v", req)
	}
	if _, statErr := os.Stat(filepath.Join(workspaceRoot, LinkFileRelative)); !os.IsNotExist(statErr) {
		t.Fatalf("setup --auto must not write a link file when skipping (stat err = %v)", statErr)
	}

	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got["configured"] != false || got["skipped"] != true {
		t.Fatalf("expected configured=false skipped=true, got %+v", got)
	}
}

// TestSetupAutoSkipsWhenUnauthenticated proves `--auto` skips cleanly (exit 0,
// no network) when there is no stored credential, so automation never fails on
// a checkout that has not run `cloud login`.
func TestSetupAutoSkipsWhenUnauthenticated(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	// No writeTestAuth: the checkout is unauthenticated.
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
	}, []string{"--auto", "--workspace", "ws-acme", "--control-plane-url", testBaseURL, "--json"})
	if err != nil {
		t.Fatalf("setup --auto should not error when unauthenticated: %v", err)
	}

	if len(fake.requests) != 0 {
		t.Fatalf("setup --auto must not reach the network when unauthenticated, requests = %+v", fake.requests)
	}

	var got map[string]any
	decodeJSON(t, strings.Join(output, "\n"), &got)
	if got["configured"] != false || got["skipped"] != true {
		t.Fatalf("expected configured=false skipped=true, got %+v", got)
	}
}

// TestExtensionManifestExposesSetupAutoFlag proves the published extension keeps
// the explicit `putnami cloud setup --auto` automation mode while install-time
// initialization is handled by the workspace-install command.
func TestExtensionManifestExposesSetupAutoFlag(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}

	if _, ok := manifest["hooks"]; ok {
		t.Fatalf("manifest should use workspace-install instead of legacy hooks")
	}

	commands := manifest["commands"].(map[string]any)
	setupCommand := commands["cloud-setup"].(map[string]any)
	flags := setupCommand["flags"].(map[string]any)
	autoFlag, ok := flags["auto"].(map[string]any)
	if !ok {
		t.Fatalf("cloud-setup missing --auto flag")
	}
	if autoFlag["type"] != "boolean" {
		t.Fatalf("--auto type = %v, want boolean", autoFlag["type"])
	}
}

// TestExtensionManifestBuildsImageLayersBeforePackaging pins the bootstrap
// edge that lets the currently selected worker publish its own replacement.
// The trusted publisher cannot depend on a newer entrypoint already being in
// the selected image: the checkout's ordinary build must materialize the
// gitignored layers first, and package must independently verify them.
func TestExtensionManifestBuildsImageLayersBeforePackaging(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}

	commands := manifest["commands"].(map[string]any)
	assertImageLayersContribution := func(command, task string, activationFiles []any, reservesBuildCPU bool) {
		t.Helper()
		contribution, ok := commands[command].(map[string]any)
		if !ok {
			t.Fatalf("manifest command %q is missing", command)
		}
		if got := contribution["activationFiles"]; !reflect.DeepEqual(got, activationFiles) {
			t.Fatalf("%s activationFiles = %v, want %v", command, got, activationFiles)
		}
		runs, ok := contribution["run"].([]any)
		if !ok {
			t.Fatalf("%s run = %v, want image-layer step", command, contribution["run"])
		}
		var run map[string]any
		for _, candidate := range runs {
			typed, candidateOK := candidate.(map[string]any)
			if candidateOK && typed["task"] == task {
				run = typed
				break
			}
		}
		if run == nil {
			t.Fatalf("%s run = %v, want task %s", command, runs, task)
		}
		if reservesBuildCPU {
			if run["heavy"] != true || run["cpuWeight"] != float64(2) {
				t.Fatalf("%s run = %v, want heavy=true and cpuWeight=2 for the producer's two-way Go builds", command, run)
			}
		}
	}

	assertImageLayersContribution("cloud-image-layers", "cloud-image-layers", []any{"image-layers.json"}, true)
	assertImageLayersContribution("build", "cloud-image-layers", []any{"image-layers.json"}, true)
	assertImageLayersContribution("package", "cloud-image-layers-check", []any{"*/index.md", ".gen/config-schema.json", "image-layers.json", "schema/config.json"}, false)

	tasks := manifest["tasks"].(map[string]any)
	producer := tasks["cloud-image-layers"].(map[string]any)
	if producer["cache"] != false {
		t.Fatalf("cloud-image-layers cache = %v, want false so a cold checkout is never served another worktree's layers", producer["cache"])
	}
}

// TestExtensionManifestDeclaresV3RuntimeAndTypedCloudContributions pins the
// Cloud half of the contract-v3 handoff. The runtime declaration describes the
// published binary and source preparation, and every task and tool runs that
// runtime through {extensionRuntime}, with no exception. The CLI resolves it to
// the archive's compiled/putnami-cloud, or compiled/putnami-cloud.exe on
// Windows, so those commands do not depend on a POSIX shell. That includes the
// remote build-cache provider: see TestCacheProviderTaskStartsTheNativeRuntime.
// The task ports themselves remain exact, typed v3 declarations.
func TestExtensionManifestDeclaresV3RuntimeAndTypedCloudContributions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}

	if got := manifest["name"]; got != cloudRuntimeIdentity {
		t.Fatalf("manifest name = %v, want %s", got, cloudRuntimeIdentity)
	}
	if got := manifest["version"]; got != cloudRuntimeVersion {
		t.Fatalf("manifest version = %v, want %s", got, cloudRuntimeVersion)
	}
	if got := manifest["cliContract"]; got != float64(cloudCLIContract) {
		t.Fatalf("cliContract = %v, want %d", got, cloudCLIContract)
	}
	runtime, ok := manifest["runtime"].(map[string]any)
	if !ok {
		t.Fatal("manifest is missing its contract-v3 runtime declaration")
	}
	if got := runtime["executable"]; got != "compiled/putnami-cloud" {
		t.Fatalf("runtime.executable = %v, want compiled/putnami-cloud", got)
	}
	projectData, err := os.ReadFile(filepath.Join("..", "..", "putnami.json"))
	if err != nil {
		t.Fatalf("read project manifest: %v", err)
	}
	var project struct {
		Options struct {
			Package struct {
				BinaryName string `json:"binary-name"`
			} `json:"package"`
		} `json:"options"`
	}
	if err := json.Unmarshal(projectData, &project); err != nil {
		t.Fatalf("decode project manifest: %v", err)
	}
	if got, want := runtime["executable"], "compiled/"+project.Options.Package.BinaryName; got != want {
		t.Fatalf("runtime.executable = %v, want %s, the binary the archive packager writes", got, want)
	}
	prepare, ok := runtime["prepare"].(map[string]any)
	if !ok {
		t.Fatal("runtime must prepare compiled/putnami-cloud in a source checkout")
	}
	if got := prepare["command"]; got != "{extensionRoot}/bin/prepare-runtime" {
		t.Fatalf("runtime.prepare.command = %v, want {extensionRoot}/bin/prepare-runtime", got)
	}
	if got := prepare["args"]; !reflect.DeepEqual(got, []any{"--output", "{runtimeOutput}"}) {
		t.Fatalf("runtime.prepare.args = %v, want [--output {runtimeOutput}]", got)
	}
	if got := prepare["inputs"]; !reflect.DeepEqual(got, []any{"bin/prepare-runtime", "cmd/**", "go.mod", "go.sum", "internal/**"}) {
		t.Fatalf("runtime.prepare.inputs = %v, want source digest inputs", got)
	}
	preparePath := filepath.Join("..", "..", "bin", "prepare-runtime")
	if info, err := os.Stat(preparePath); err != nil {
		t.Fatalf("runtime prepare launcher %s: %v", preparePath, err)
	} else if info.Mode()&0o111 == 0 {
		t.Fatalf("runtime prepare launcher %s is not executable", preparePath)
	}
	if _, ok := manifest["workspace"]; !ok {
		t.Fatal("Cloud must declare its reserved archive-member workspace probe")
	}
	if _, ok := manifest["ecosystems"]; !ok {
		t.Fatal("Cloud must declare the archive and put ecosystems used by its publishers")
	}

	commands, ok := manifest["commands"].(map[string]any)
	if !ok {
		t.Fatal("manifest commands are missing")
	}
	if _, ok := commands["publish-provider"]; ok {
		t.Fatal("manifest must not expose the retired publish-provider capability marker")
	}
	tasks, ok := manifest["tasks"].(map[string]any)
	if !ok {
		t.Fatal("manifest tasks are missing")
	}
	if _, ok := tasks["cloud-publish-provider"]; ok {
		t.Fatal("manifest must not expose the retired cloud-publish-provider marker task")
	}
	for name, rawTask := range tasks {
		task, ok := rawTask.(map[string]any)
		if !ok {
			t.Fatalf("task %q is not an object", name)
		}
		if got := task["command"]; got != "{extensionRuntime}" {
			t.Fatalf("task %q command = %v, want {extensionRuntime}: a script command cannot start on Windows", name, got)
		}
	}
	// The Intelligence MCP tools belong to the @putnami/intelligence extension.
	if _, ok := manifest["tools"]; ok {
		t.Fatal("Cloud must not declare MCP tools: they ship with @putnami/intelligence")
	}

	assertSchemaInput := func(taskName string) {
		t.Helper()
		task, ok := tasks[taskName].(map[string]any)
		if !ok {
			t.Fatalf("task %q is missing", taskName)
		}
		inputs, ok := task["inputs"].(map[string]any)
		if !ok {
			t.Fatalf("task %q has no input ports", taskName)
		}
		schema, ok := inputs["schema"].(map[string]any)
		if !ok {
			t.Fatalf("task %q has no schema input port", taskName)
		}
		if got := schema["from"]; got != "task" {
			t.Fatalf("task %q schema.from = %v, want task", taskName, got)
		}
		if got := schema["optional"]; got != true {
			t.Fatalf("task %q schema.optional = %v, want true", taskName, got)
		}
		if _, ok := schema["files"]; ok {
			t.Fatalf("task %q schema must be a typed task port, not a file-scan fallback", taskName)
		}
	}
	assertSchemaInput("cloud-publish-config")
	assertSchemaInput("cloud-publish-config-project")

	assertEffects := func(taskName string, want ...string) {
		t.Helper()
		task, ok := tasks[taskName].(map[string]any)
		if !ok {
			t.Fatalf("task %q is missing", taskName)
		}
		declares, ok := task["declares"].(map[string]any)
		if !ok {
			t.Fatalf("task %q has no v3 declaration", taskName)
		}
		effects, ok := declares["effects"].([]any)
		if !ok {
			t.Fatalf("task %q has no declared effects", taskName)
		}
		if len(effects) != len(want) {
			t.Fatalf("task %q effects = %v, want %v", taskName, effects, want)
		}
		for i, expected := range want {
			if effects[i] != expected {
				t.Fatalf("task %q effects = %v, want %v", taskName, effects, want)
			}
		}
	}
	assertEffects("cloud-publish-config-project", "cloud", "network")
	assertEffects("cloud-cache-provider", "process")
	assertEffects("cloud-credential-provider", "process", "network")
	assertEffects("cloud-session-reporter", "process", "network")
	assertEffects("cloud-log-reporter", "process", "network")
	assertEffects("cloud-image-layers", "network")
	assertEffects("cloud-deploy", "cloud", "network")
	assertMigrationPublishedOutput := func(taskName string) {
		t.Helper()
		task, ok := tasks[taskName].(map[string]any)
		if !ok {
			t.Fatalf("task %q is missing", taskName)
		}
		declares, ok := task["declares"].(map[string]any)
		if !ok {
			t.Fatalf("task %q has no v3 declaration", taskName)
		}
		// The retired publish sentinel lived at .gen/migration-published.json,
		// inside the build~generate/package~generate "gen" output subtree.
		// Declaring it as a task output violates the planner's
		// one-owner-per-output invariant and fails every deploy plan for a
		// migration-carrying project, so the task must never claim it again.
		if outputs, ok := declares["outputs"].(map[string]any); ok {
			if _, ok := outputs["migrationPublished"]; ok {
				t.Fatalf("task %q must not declare the migrationPublished output: .gen/migration-published.json is owned by the generate tasks' gen output", taskName)
			}
		}
	}
	assertMigrationPublishedOutput("cloud-publish-migration-project")
	assertSourceMutation := func(taskName string) {
		t.Helper()
		task, ok := tasks[taskName].(map[string]any)
		if !ok {
			t.Fatalf("task %q is missing", taskName)
		}
		writes, ok := task["writes"].([]any)
		if !ok || len(writes) != 1 || writes[0] != "sources" {
			t.Fatalf("task %q writes = %v, want [sources]", taskName, task["writes"])
		}
		declares, ok := task["declares"].(map[string]any)
		if !ok || declares["mutatesSources"] != true {
			t.Fatalf("task %q must declare mutatesSources=true", taskName)
		}
	}
	assertSourceMutation("cloud-publish-config")
	assertSourceMutation("cloud-publish-config-project")

	deploy, ok := tasks["cloud-deploy"].(map[string]any)
	if !ok {
		t.Fatal("cloud-deploy task is missing")
	}
	if _, ok := deploy["inputs"]; ok {
		t.Fatal("cloud-deploy reads no project file: publish-v2 carries its request file")
	}
}

func TestRuntimeInfoHandshake(t *testing.T) {
	var output []string
	code := RunMain([]string{"__putnami", "runtime-info"}, IO{
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
	})
	if code != ExitSuccess {
		t.Fatalf("runtime-info exit code = %d, want %d", code, ExitSuccess)
	}
	if len(output) != 1 {
		t.Fatalf("runtime-info emitted %d lines, want one: %v", len(output), output)
	}
	var got runtimeInfoResponse
	if err := json.Unmarshal([]byte(output[0]), &got); err != nil {
		t.Fatalf("decode runtime-info: %v\n%s", err, output[0])
	}
	if got.Extension != cloudRuntimeIdentity {
		t.Fatalf("runtime extension = %q, want %q", got.Extension, cloudRuntimeIdentity)
	}
	if got.Version != cloudRuntimeVersion {
		t.Fatalf("runtime version = %q, want %q", got.Version, cloudRuntimeVersion)
	}
	if got.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("runtime platform = %q, want %q", got.Platform, runtime.GOOS+"/"+runtime.GOARCH)
	}
	if got.CLIContract != cloudCLIContract {
		t.Fatalf("runtime cliContract = %d, want %d", got.CLIContract, cloudCLIContract)
	}
	if got.RuntimeProtocol != cloudRuntimeProtocol {
		t.Fatalf("runtime protocol = %d, want %d", got.RuntimeProtocol, cloudRuntimeProtocol)
	}
	if got.RuntimeABI != cloudRuntimeABI {
		t.Fatalf("runtime ABI = %d, want %d", got.RuntimeABI, cloudRuntimeABI)
	}
}

func TestRunMainEmitsPutnamiJSONLResultEvents(t *testing.T) {
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{"json": true}})
	var output []string

	code := RunMain([]string{"help", "--putnamiContext", contextFile}, IO{
		Env:    hometest.Env(t.TempDir(), nil),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
	})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	events := decodeEvents(t, output)
	result := findEvent(events, "result")
	data := result["data"].(map[string]any)
	if data["status"] != "OK" {
		t.Fatalf("status = %v", data["status"])
	}
	payload := data["data"].(map[string]any)
	commands := payload["commands"].([]any)
	if len(commands) != 20 {
		t.Fatalf("help lists %d commands, want the 20 public entries", len(commands))
	}
	first := commands[0].(map[string]any)
	if first["command"] != "cloud login" {
		t.Fatalf("first command = %v", first["command"])
	}
}

// TestRunMainStandaloneStructuredFailureUsesDefaultOutput pins the standalone
// error path: the extension entrypoint is called with a zero IO in production,
// so a structured auth failure must render through the default stdout sink
// instead of dereferencing the caller's nil Stdout function.
func TestRunMainStandaloneStructuredFailureUsesDefaultOutput(t *testing.T) {
	code := RunMain([]string{"whoami", "--output=jsonl"}, IO{
		Env: hometest.Env(t.TempDir(), map[string]string{"PUTNAMI_WORKSPACE_ROOT": t.TempDir()}),
	})
	if code != clicore.ExitFailure {
		t.Fatalf("code = %d, want the failing-status exit %d", code, clicore.ExitFailure)
	}
}

// TestRunMainStampsTheNegotiatedEventVersion pins the event version stamp. The
// invoking CLI advertises the event protocol version it accepts and, from
// extension contract 3, accepts EXACTLY that version, dropping anything else
// before a renderer sees it. Stamping a hand-written 1 would make every event
// this extension emits invisible: logs, diagnostics, phases and
// the result alike.
func TestRunMainStampsTheNegotiatedEventVersion(t *testing.T) {
	run := func(t *testing.T, advertised string) []map[string]any {
		t.Helper()
		workspaceRoot := t.TempDir()
		contextFile := filepath.Join(t.TempDir(), "context.json")
		writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{}})
		env := hometest.Env(t.TempDir(), nil)
		if advertised != "" {
			env["PUTNAMI_RUNTIME_EVENTS"] = advertised
		}
		var output []string
		code := RunMain([]string{"help", "--putnamiContext", contextFile}, IO{
			Env:    env,
			Stdout: func(line string) { output = append(output, line) },
			Stderr: func(string) {},
		})
		if code != 0 {
			t.Fatalf("code = %d", code)
		}
		events := decodeEvents(t, output)
		if len(events) == 0 {
			t.Fatal("no events emitted")
		}
		return events
	}

	t.Run("answers at the advertised version", func(t *testing.T) {
		for _, event := range run(t, "2") {
			if event["v"] != float64(2) {
				t.Fatalf("event %v stamped v=%v, want the advertised 2", event["type"], event["v"])
			}
		}
	})

	// Absent or unparsable both mean v1 — the version every consumer has always
	// understood — so a directly invoked binary and an older CLI still receive a
	// stream they can read.
	for name, advertised := range map[string]string{"absent": "", "unparsable": "not-a-number"} {
		t.Run("falls back to v1 when the advertisement is "+name, func(t *testing.T) {
			for _, event := range run(t, advertised) {
				if event["v"] != float64(1) {
					t.Fatalf("event %v stamped v=%v, want the fail-closed 1", event["type"], event["v"])
				}
			}
		})
	}
}

// TestRunMainFailedResultEventCarriesTheCause pins the failure cause at the
// forwarding layer. The invoking CLI describes a failed task
// from the result event's `error` member and falls back to the subprocess's own
// wait error when there is none — which is how a whole harness's verdict was
// recorded as `exit status 1`.
func TestRunMainFailedResultEventCarriesTheCause(t *testing.T) {
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{}})
	var output []string

	code := RunMain([]string{"whoami", "--putnamiContext", contextFile}, IO{
		Env: hometest.Env(t.TempDir(), map[string]string{
			"PUTNAMI_RUNTIME_EVENTS": "2",
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
	})
	if code != clicore.ExitFailure {
		t.Fatalf("code = %d, want the failing-status exit %d", code, clicore.ExitFailure)
	}

	result := findEvent(decodeEvents(t, output), "result")
	if result == nil {
		t.Fatal("a failing command must emit a result event")
	}
	data := result["data"].(map[string]any)
	if data["status"] != "FAILED" {
		t.Fatalf("status = %v, want FAILED", data["status"])
	}
	cause, ok := data["error"].(map[string]any)
	if !ok {
		t.Fatalf("FAILED result carries no error member: %v", data)
	}
	message, _ := cause["message"].(string)
	if message == "" {
		t.Fatalf("error member carries no message: %v", cause)
	}
	diagnostics := eventsOfType(decodeEvents(t, output), "diagnostic")
	if len(diagnostics) != 1 {
		t.Fatalf("terminal diagnostics = %d, want exactly one: %v", len(diagnostics), diagnostics)
	}
}

func TestPutnamiEventIOKeepsStderrAsLogsUntilTerminalFailure(t *testing.T) {
	state := &eventState{}
	var output []string
	runtimeIO := putnamiEventIO(IO{Stdout: func(line string) { output = append(output, line) }}, state, 2)

	runtimeIO.Stderr("go: downloading example.test/module v1.2.3")
	runtimeIO.Stderr("not ok 4 - worktree guard reports the mutated file")
	if diagnostics := eventsOfType(decodeEvents(t, output), "diagnostic"); len(diagnostics) != 0 {
		t.Fatalf("successful stderr emitted diagnostics before a terminal failure: %v", diagnostics)
	}
	state.emitFailureDiagnostic(errors.New("cloud shell-test: tests/run.sh failed with exit status 4"))

	events := decodeEvents(t, output)
	logs := eventsOfType(events, "log")
	if len(logs) != 2 {
		t.Fatalf("stderr logs = %d, want 2: %v", len(logs), logs)
	}
	if diagnostics := eventsOfType(events, "diagnostic"); len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %d, want the single terminal diagnostic: %v", len(diagnostics), diagnostics)
	} else {
		message, _ := diagnostics[0]["message"].(string)
		for _, want := range []string{"failed with exit status 4", "not ok 4 - worktree guard reports the mutated file"} {
			if !strings.Contains(message, want) {
				t.Fatalf("terminal diagnostic missing %q: %q", want, message)
			}
		}
	}
}

func TestPutnamiEventIOBoundsTerminalFailureDiagnostic(t *testing.T) {
	state := &eventState{}
	var output []string
	runtimeIO := putnamiEventIO(IO{Stdout: func(line string) { output = append(output, line) }}, state, 2)
	for i := 0; i < runtimeStderrTailLines+5; i++ {
		runtimeIO.Stderr(fmt.Sprintf("stderr-%02d-%s", i, strings.Repeat("x", runtimeStderrTailLineRunes)))
	}
	state.emitFailureDiagnostic(errors.New(strings.Repeat("failure ", runtimeFailureDiagnosticRunes)))

	diagnostics := eventsOfType(decodeEvents(t, output), "diagnostic")
	if len(diagnostics) != 1 {
		t.Fatalf("diagnostics = %d, want 1", len(diagnostics))
	}
	message, _ := diagnostics[0]["message"].(string)
	if got := len([]rune(message)); got > runtimeFailureDiagnosticRunes {
		t.Fatalf("diagnostic runes = %d, want <= %d", got, runtimeFailureDiagnosticRunes)
	}
	if !strings.Contains(message, "stderr-20-") {
		t.Fatalf("diagnostic must preserve the newest stderr evidence: %q", message)
	}
	if strings.Contains(message, "stderr-00-") {
		t.Fatalf("diagnostic retained stderr outside the %d-line tail", runtimeStderrTailLines)
	}
}

func eventsOfType(events []map[string]any, kind string) []map[string]any {
	var matches []map[string]any
	for _, event := range events {
		if event["type"] == kind {
			matches = append(matches, event)
		}
	}
	return matches
}

func TestExtensionManifestRegistersSetupAndTokenCommands(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}

	commandGroups := manifest["commandGroups"].(map[string]any)
	cloudGroup := commandGroups["cloud"].(map[string]any)
	subcommands := cloudGroup["subcommands"].(map[string]any)
	setup := subcommands["setup"].(map[string]any)
	if setup["command"] != "cloud-setup" {
		t.Fatalf("cloud setup command = %v, want cloud-setup", setup["command"])
	}
	token := subcommands["token"].(map[string]any)
	if token["command"] != "cloud-token" {
		t.Fatalf("cloud token command = %v, want cloud-token", token["command"])
	}
	if _, ok := subcommands["workspace"]; ok {
		t.Fatalf("cloud workspace subcommand should not be registered")
	}
	if _, ok := subcommands["link"]; ok {
		t.Fatalf("cloud link subcommand should not be registered")
	}

	commands := manifest["commands"].(map[string]any)
	setupCommand := commands["cloud-setup"].(map[string]any)
	setupRun := setupCommand["run"].([]any)[0].(map[string]any)
	if setupRun["task"] != "cloud-setup" {
		t.Fatalf("cloud-setup run task = %v, want cloud-setup", setupRun["task"])
	}
	tokenCommand := commands["cloud-token"].(map[string]any)
	tokenFlags := tokenCommand["flags"].(map[string]any)
	if _, ok := tokenFlags["purpose"]; ok {
		t.Fatal("cloud-token must not expose the grant-only reserved purpose")
	}
	// The user contract is kind-only. Target coordinates are not flags any more:
	// the registry derives per-namespace authority from the caller's identity,
	// so a target flag could only lie about what the token can reach.
	registryTokenCommand := commands["cloud-registry-token"].(map[string]any)
	registryTokenFlags := registryTokenCommand["flags"].(map[string]any)
	for _, gone := range []string{"owner-workspace", "package", "action", "channel"} {
		if _, ok := tokenFlags[gone]; ok {
			t.Fatalf("cloud-token must not expose the retired target flag --%s", gone)
		}
		if _, ok := registryTokenFlags[gone]; ok {
			t.Fatalf("cloud-registry-token must not expose the retired target flag --%s", gone)
		}
	}
	if _, ok := registryTokenFlags["host"]; !ok {
		t.Fatal("cloud-registry-token must keep --host: it is the whole host-form contract")
	}
	if _, ok := tokenFlags["for"]; !ok {
		t.Fatal("cloud-token must keep --for: it is the whole kind-form contract")
	}
	tokenRun := tokenCommand["run"].([]any)[0].(map[string]any)
	if tokenRun["task"] != "cloud-token" {
		t.Fatalf("cloud-token run task = %v, want cloud-token", tokenRun["task"])
	}
	distributionCommand := commands["cloud-distribution"].(map[string]any)
	distributionFlags := distributionCommand["flags"].(map[string]any)
	for _, retired := range []string{"purpose", "action", "grantee-kind", "grantee-id"} {
		if _, ok := distributionFlags[retired]; ok {
			t.Fatalf("cloud-distribution must not expose retired grant authority --%s", retired)
		}
	}
	// --package is retired as grant authority and is still refused by grants
	// and by every mirrors command but add. It is declared only as the one
	// package an archive mirror target copies.
	packageFlag, _ := distributionFlags["package"].(map[string]any)
	if description, _ := packageFlag["description"].(string); packageFlag["type"] != "string" ||
		!strings.Contains(description, "archive mirror target") || strings.Contains(description, "grant") {
		t.Fatalf("cloud-distribution package flag = %v, want the archive mirror target's package only", distributionFlags["package"])
	}
	for _, name := range []string{"ecosystem", "id", "destination", "alias", "username"} {
		if flag, ok := distributionFlags[name].(map[string]any); !ok || flag["type"] != "string" {
			t.Fatalf("cloud-distribution %s flag = %v, want a declared string flag for mirrors add", name, distributionFlags[name])
		}
	}
	for _, name := range []string{"grantee-workspace-id", "channel", "registry-put-url"} {
		flag, ok := distributionFlags[name].(map[string]any)
		if !ok || flag["type"] != "string" {
			t.Fatalf("cloud-distribution %s flag = %v, want a string flag", name, distributionFlags[name])
		}
	}
	installCommand := commands["workspace-install"].(map[string]any)
	installRun := installCommand["run"].([]any)[0].(map[string]any)
	if installRun["task"] != "cloud-install" {
		t.Fatalf("workspace-install run task = %v, want cloud-install", installRun["task"])
	}
	if _, ok := commands["cloud-workspace"]; ok {
		t.Fatalf("cloud-workspace command should not be registered")
	}
	if _, ok := commands["cloud-link"]; ok {
		t.Fatalf("cloud-link command should not be registered")
	}

	tasks := manifest["tasks"].(map[string]any)
	setupTask := tasks["cloud-setup"].(map[string]any)
	setupArgs := setupTask["args"].([]any)
	if len(setupArgs) != 1 || setupArgs[0] != "setup" {
		t.Fatalf("cloud-setup task args = %v, want [setup]", setupArgs)
	}
	tokenTask := tasks["cloud-token"].(map[string]any)
	tokenArgs := tokenTask["args"].([]any)
	if len(tokenArgs) != 1 || tokenArgs[0] != "token" {
		t.Fatalf("cloud-token task args = %v, want [token]", tokenArgs)
	}
	installTask := tasks["cloud-install"].(map[string]any)
	installArgs := installTask["args"].([]any)
	if len(installArgs) != 1 || installArgs[0] != "install" {
		t.Fatalf("cloud-install task args = %v, want [install]", installArgs)
	}
	if _, ok := tasks["cloud-workspace"]; ok {
		t.Fatalf("cloud-workspace task should not be registered")
	}
	if _, ok := tasks["cloud-link"]; ok {
		t.Fatalf("cloud-link task should not be registered")
	}
}

func TestRunMainEmitsVisiblePutnamiPhaseAndTTYPrompt(t *testing.T) {
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{}})
	fake := newFakeTransport(t)
	var output []string
	var tty []string

	code := RunMain([]string{"login", "--auth-url", testBaseURL, "--no-open", "--putnamiContext", contextFile}, IO{
		Env:    hometest.Env(t.TempDir(), nil),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		TTY:    func(text string) { tty = append(tty, text) },
		Client: fake.client(),
		Now:    fixedNow,
	})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	events := decodeEvents(t, output)
	phase := findEvent(events, "phase")
	if phase["name"] != "Visit https://control.test/device and enter code BCDF-GHJK" {
		t.Fatalf("phase name = %v", phase["name"])
	}
	progress := findEvent(events, "progress")
	if progress["message"] != "Open https://control.test/device?code=BCDF-GHJK to authorize Putnami Cloud" {
		t.Fatalf("progress message = %v", progress["message"])
	}
	ttyOutput := strings.Join(tty, "")
	if !strings.HasPrefix(ttyOutput, "\r\x1b[2K") {
		t.Fatalf("tty prompt did not clear line: %q", ttyOutput)
	}
	assertContains(t, ttyOutput, "To authenticate, visit:")
	assertContains(t, ttyOutput, "Signed in as dev@example.com")
}

func TestRunMainInteractiveContextWritesPlainOutput(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home)
	workspaceRoot := t.TempDir()
	writeLinkFile(t, workspaceRoot)
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": workspaceRoot, "params": map[string]any{}})
	fake := newFakeTransport(t)
	var output []string

	code := RunMain([]string{"whoami", "--putnamiContext", contextFile}, IO{
		Env: hometest.Env(home, map[string]string{
			"PUTNAMI_AUTH_URL":    testBaseURL,
			"PUTNAMI_INTERACTIVE": "1",
		}),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}

	joined := strings.Join(output, "\n")
	assertContains(t, joined, "signed in as dev@example.com")
	if strings.Contains(joined, `"type"`) {
		t.Fatalf("interactive output should not contain JSONL events: %s", joined)
	}
}

func TestDefaultConfirmReadsInteractiveStdin(t *testing.T) {
	oldStdin := os.Stdin
	oldStderr := os.Stderr
	oldTTY := ttyDevice
	// Force the os.Stdin fallback so the test never blocks on the real
	// controlling terminal when run interactively (e.g. `putnami t`).
	ttyDevice = ""
	t.Cleanup(func() {
		os.Stdin = oldStdin
		os.Stderr = oldStderr
		ttyDevice = oldTTY
	})

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	defer inR.Close()
	defer errR.Close()

	os.Stdin = inR
	os.Stderr = errW
	if _, err := inW.WriteString("N\r"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := inW.Close(); err != nil {
		t.Fatalf("close stdin writer: %v", err)
	}

	answer, ok := defaultConfirm("Reveal? ", map[string]string{"PUTNAMI_INTERACTIVE": "1"})
	if !ok || answer != "N" {
		t.Fatalf("answer, ok = %q, %v; want N, true", answer, ok)
	}
	if err := errW.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	prompt, err := io.ReadAll(errR)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if string(prompt) != "Reveal? " {
		t.Fatalf("prompt = %q, want Reveal? ", string(prompt))
	}
}

func TestReadConfirmAnswerAcceptsNewline(t *testing.T) {
	answer, ok := readConfirmAnswer(strings.NewReader("yes\n"))
	if !ok || answer != "yes" {
		t.Fatalf("answer, ok = %q, %v; want yes, true", answer, ok)
	}
}

func newFakeTransport(t *testing.T) *fakeTransport {
	t.Helper()
	return &fakeTransport{t: t}
}

func (f *fakeTransport) client() *http.Client {
	return &http.Client{Transport: f}
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	body := map[string]any{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			f.t.Fatalf("decode request body: %v", err)
		}
	}
	f.requests = append(f.requests, recordedRequest{
		Method:  req.Method,
		Path:    req.URL.Path,
		Header:  req.Header.Clone(),
		Body:    body,
		RawBody: string(bodyBytes),
	})

	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/.well-known/openid-configuration":
		return jsonResponse(http.StatusOK, map[string]any{
			"issuer":                        testBaseURL,
			"device_authorization_endpoint": testBaseURL + "/device/authorize",
			"token_endpoint":                testBaseURL + "/token",
			"userinfo_endpoint":             testBaseURL + "/userinfo",
			"revocation_endpoint":           testBaseURL + "/revoke",
		}), nil
	case req.Method == http.MethodPost && req.URL.Path == "/device/authorize":
		return jsonResponse(http.StatusOK, map[string]any{
			"device_code":               "device-1",
			"user_code":                 "BCDF-GHJK",
			"verification_uri":          testBaseURL + "/device",
			"verification_uri_complete": testBaseURL + "/device?code=BCDF-GHJK",
			"expires_in":                60,
			"interval":                  0,
		}), nil
	case req.Method == http.MethodPost && req.URL.Path == "/token":
		if body["grant_type"] == "refresh_token" {
			extra := map[string]any{"refreshed": true}
			if workspaceID := stringValue(body["workspace_id"]); workspaceID != "" && !f.omitWorkspaceScope {
				extra["scope_ref"] = map[string]any{"workspace_id": workspaceID}
			}
			response := tokenResponse(extra)
			if f.refreshToken != "" {
				response["refresh_token"] = f.refreshToken
			}
			for key, value := range f.refreshResponse {
				response[key] = value
			}
			return jsonResponse(http.StatusOK, response), nil
		}
		if body["grant_type"] == apiKeyGrantType {
			clientID := stringValue(body["client_id"])
			if clientID != distributioncli.DefaultRegistryTokenClientID && clientID != cacheOAuthClientID {
				f.t.Fatalf("api-key token client_id = %v", body["client_id"])
			}
			apiKey := stringValue(body["api_key"])
			if !strings.HasPrefix(apiKey, "pkt_") {
				f.t.Fatalf("api_key = %q, want pkt_*", apiKey)
			}
			claims := map[string]any{
				"sub":            "user-1",
				"aud":            body["client_id"],
				"scope":          body["scope"],
				"principal_kind": "user",
				"principal_id":   "user-1",
				"apikey_id":      "apikey-exchanged",
			}
			if f.apiKeyGrantAud != "" {
				claims["aud"] = f.apiKeyGrantAud
			}
			if f.apiKeyGrantScope != "" {
				claims["scope"] = f.apiKeyGrantScope
			}
			if workspaceID := stringValue(body["workspace_id"]); workspaceID != "" {
				claims["scope_ref"] = map[string]any{"workspace_id": workspaceID}
			}
			return jsonResponse(http.StatusOK, map[string]any{
				"access_token": jwt(claims),
				"token_type":   "Bearer",
				"expires_in":   300,
			}), nil
		}
		if body["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" {
			f.t.Fatalf("grant_type = %v", body["grant_type"])
		}
		if body["client_id"] != "putnami-cli" {
			f.t.Fatalf("client_id = %v", body["client_id"])
		}
		return jsonResponse(http.StatusOK, tokenResponse(nil)), nil
	case req.Method == http.MethodGet && req.URL.Path == "/userinfo":
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header")
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"sub":      "user-1",
			"email":    "dev@example.com",
			"name":     "Dev User",
			"provider": "github",
		}), nil
	// The caller's workspace listing backs --workspace <slug|name> resolution:
	// the fixtures pass "ws-acme", which is not a canonical id, so setup
	// resolves it as the slug of the one workspace the fake knows.
	case req.Method == http.MethodGet && req.URL.Path == "/v1/workspaces" && req.URL.Query().Get("name") == "":
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header")
		}
		return jsonResponse(http.StatusOK, map[string]any{"workspaces": []map[string]any{{
			"id": "ws-acme", "slug": "ws-acme", "name": "Acme Workspace", "organization_id": "org-acme",
		}}}), nil
	case req.Method == http.MethodGet && req.URL.Path == "/v1/workspaces/ws-acme":
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header")
		}
		workspace := map[string]any{
			"id":              "ws-acme",
			"organization_id": "org-acme",
			"name":            "Acme Workspace",
			"repository":      "github.com/example/acme",
		}
		if f.workspaceSlug != "" {
			workspace["slug"] = f.workspaceSlug
		}
		return jsonResponse(http.StatusOK, workspace), nil
	case req.Method == http.MethodPost && req.URL.Path == "/v1/workspaces":
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header on POST /v1/workspaces")
		}
		return jsonResponse(http.StatusCreated, map[string]any{
			"id":              "ws-generated-1",
			"organization_id": "org-personal",
			"name":            stringValue(body["name"]),
			"repository":      stringValue(body["repository"]),
		}), nil
	// Registry api-key minting (`cloud registry-token`) still drives the
	// auth-server /apikeys surface directly — only workspace machine-token
	// management goes through the CPA.
	case req.Method == http.MethodPost && req.URL.Path == "/apikeys":
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header on /apikeys")
		}
		name := stringValue(body["name"])
		id := fmt.Sprintf("apikey-%d", len(f.mintedKeys)+1)
		prefix := fmt.Sprintf("pkt_%02d", len(f.mintedKeys)+1)
		minted := map[string]any{
			"id":                 id,
			"name":               name,
			"prefix":             prefix,
			"raw_token":          prefix + "_secret_" + strings.ReplaceAll(name, ":", "_"),
			"created_at":         "2026-04-30T12:00:00.000Z",
			"allowed_scopes":     stringValue(body["allowed_scopes"]),
			"owner_kind":         stringValue(body["owner_kind"]),
			"owner_principal_id": stringValue(body["owner_principal_id"]),
			"workspace_id":       stringValue(body["workspace_id"]),
			"expires_at":         stringValue(body["expires_at"]),
		}
		if f.omitRawAPIKey {
			// The contract requires raw_token, so an incomplete answer
			// carries it empty.
			minted["raw_token"] = ""
		}
		f.mintedKeys = append(f.mintedKeys, minted)
		// auth-server's 201 answer names the owner as owner_principal_kind
		// and always carries allowed_client_ids.
		answer := maps.Clone(minted)
		delete(answer, "owner_kind")
		answer["owner_principal_kind"] = "user"
		answer["allowed_client_ids"] = []string{}
		return jsonResponse(http.StatusCreated, answer), nil
	case req.Method == http.MethodGet && req.URL.Path == "/apikeys":
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header on GET /apikeys")
		}
		keys := make([]map[string]any, 0, len(f.mintedKeys))
		for _, k := range f.mintedKeys {
			id := stringValue(k["id"])
			revoked := ""
			for _, r := range f.revokedIDs {
				if r == id {
					revoked = "2026-04-30T12:05:00.000Z"
				}
			}
			keys = append(keys, map[string]any{
				"id":                   id,
				"name":                 k["name"],
				"prefix":               k["prefix"],
				"owner_principal_kind": k["owner_kind"],
				"owner_principal_id":   k["owner_principal_id"],
				"workspace_id":         k["workspace_id"],
				"allowed_scopes":       k["allowed_scopes"],
				"expires_at":           k["expires_at"],
				"created_at":           k["created_at"],
				"revoked_at":           revoked,
			})
		}
		return jsonResponse(http.StatusOK, map[string]any{"keys": keys}), nil
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/apikeys/"):
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header on DELETE /apikeys")
		}
		id := strings.TrimPrefix(req.URL.Path, "/apikeys/")
		f.revokedIDs = append(f.revokedIDs, id)
		return jsonResponse(http.StatusOK, map[string]any{"revoked": true}), nil
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/apikeys/"):
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header on GET /apikeys/{id}")
		}
		id := strings.TrimPrefix(req.URL.Path, "/apikeys/")
		for _, k := range f.mintedKeys {
			if stringValue(k["id"]) != id {
				continue
			}
			key := map[string]any{
				"id":                   id,
				"name":                 k["name"],
				"prefix":               k["prefix"],
				"owner_principal_kind": "user",
				"owner_principal_id":   k["owner_principal_id"],
				"allowed_scopes":       k["allowed_scopes"],
				"allowed_client_ids":   []string{},
				"created_at":           k["created_at"],
			}
			// auth-server omits revoked_at on a live key.
			if slices.Contains(f.revokedIDs, id) {
				key["revoked_at"] = "2026-04-30T12:05:00.000Z"
			}
			return jsonResponse(http.StatusOK, key), nil
		}
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"}), nil
	// Workspace machine tokens flow through the CPA at
	// /v1/workspaces/{workspace}/tokens, which membership-gates the
	// caller and proxies to the auth-server. The fake stands in for the CPA: the
	// owner is pinned server-side, so the create body carries only name/scopes.
	case req.Method == http.MethodPost && req.URL.Path == "/v1/workspaces/ws-acme/tokens":
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header on POST tokens")
		}
		name := stringValue(body["name"])
		id := fmt.Sprintf("apikey-%d", len(f.mintedKeys)+1)
		prefix := fmt.Sprintf("pkt_%02d", len(f.mintedKeys)+1)
		minted := map[string]any{
			"id":             id,
			"name":           name,
			"prefix":         prefix,
			"raw_token":      prefix + "_secret_" + strings.ReplaceAll(name, ":", "_"),
			"created_at":     "2026-04-30T12:00:00.000Z",
			"allowed_scopes": stringValue(body["allowed_scopes"]),
			"workspace_id":   "ws-acme",
			"expires_at":     stringValue(body["expires_at"]),
		}
		f.mintedKeys = append(f.mintedKeys, minted)
		return jsonResponse(http.StatusCreated, minted), nil
	case req.Method == http.MethodGet && req.URL.Path == "/v1/workspaces/ws-acme/tokens":
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header on GET tokens")
		}
		tokens := make([]map[string]any, 0, len(f.mintedKeys))
		for _, k := range f.mintedKeys {
			id := stringValue(k["id"])
			revoked := ""
			for _, r := range f.revokedIDs {
				if r == id {
					revoked = "2026-04-30T12:05:00.000Z"
				}
			}
			tokens = append(tokens, map[string]any{
				"id":             id,
				"name":           k["name"],
				"prefix":         k["prefix"],
				"workspace_id":   "ws-acme",
				"allowed_scopes": k["allowed_scopes"],
				"expires_at":     k["expires_at"],
				"created_at":     k["created_at"],
				"revoked_at":     revoked,
			})
		}
		return jsonResponse(http.StatusOK, map[string]any{"tokens": tokens}), nil
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/v1/workspaces/ws-acme/tokens/"):
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
			f.t.Fatalf("missing bearer header on DELETE tokens")
		}
		id := strings.TrimPrefix(req.URL.Path, "/v1/workspaces/ws-acme/tokens/")
		f.revokedIDs = append(f.revokedIDs, id)
		return jsonResponse(http.StatusOK, map[string]any{"revoked": true, "id": id, "workspace_id": "ws-acme"}), nil
	case req.Method == http.MethodGet && req.URL.Path == "/v1/workspaces/ws-acme/ci/status":
		return jsonResponse(http.StatusOK, map[string]any{
			"lastRunCache": map[string]any{"tasks": 40, "hits": 30, "misses": 10, "hitPercent": 75, "servedMs": 90000, "executedMs": 30000},
		}), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found", "path": req.URL.Path}), nil
	}
}

func (f *fakeTransport) find(path string) *recordedRequest {
	for i := range f.requests {
		if f.requests[i].Path == path {
			return &f.requests[i]
		}
	}
	return nil
}

func fixedNow() time.Time {
	return time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
}

func writeTestAuth(t *testing.T, home string) {
	t.Helper()
	file := filepath.Join(home, AuthFileRelative)
	mustMkdir(t, filepath.Dir(file))
	writeJSONFile(t, file, map[string]any{
		"access_token":  tokenResponse(nil)["access_token"],
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expires_at":    "2026-04-30T13:00:00.000Z",
		"issuer":        testBaseURL,
		"client_id":     "putnami-cli",
	})
}

func tokenResponse(extra map[string]any) map[string]any {
	claims := map[string]any{
		"sub":      "user-1",
		"email":    "dev@example.com",
		"name":     "Dev User",
		"provider": "github",
		"scope":    "openid profile email apikeys:write intelligence.read intelligence.audit intelligence.review.read intelligence.review.manage",
		"iat":      fixedNow().Unix(),
		"exp":      fixedNow().Add(5 * time.Minute).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	return map[string]any{
		"access_token":  jwt(claims),
		"refresh_token": "refresh-token",
		"id_token":      jwt(map[string]any{"sub": "user-1", "scope_ref": map[string]any{"workspace_id": "ws-acme"}}),
		"token_type":    "Bearer",
		"expires_in":    300,
	}
}

func jwt(payload map[string]any) string {
	return base64url(map[string]any{"alg": "none", "typ": "JWT"}) + "." + base64url(payload) + ".sig"
}

func base64url(value map[string]any) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func jsonResponse(status int, body any) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}

func readJSONFile(t *testing.T, file string, dest any) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
}

func writeJSONFile(t *testing.T, file string, data any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode json: %v", err)
	}
	if err := os.WriteFile(file, encoded, 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

func decodeJSON(t *testing.T, text string, dest any) {
	t.Helper()
	data := []byte(text)
	if decodeResultData(t, data, dest) {
		return
	}
	if err := json.Unmarshal(data, dest); err != nil {
		t.Fatalf("decode json %q: %v", text, err)
	}
}

func decodeRawJSON(t *testing.T, text string, dest any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), dest); err != nil {
		t.Fatalf("decode raw json %q: %v", text, err)
	}
}

func decodeResultData(t *testing.T, data []byte, dest any) bool {
	t.Helper()
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	if _, ok := probe["status"]; !ok {
		return false
	}
	if _, ok := probe["exitCode"]; !ok {
		return false
	}
	payload, ok := probe["data"]
	if !ok {
		payload = []byte("null")
	}
	if err := json.Unmarshal(payload, dest); err != nil {
		t.Fatalf("decode result data %q: %v", string(payload), err)
	}
	return true
}

func decodeEvents(t *testing.T, lines []string) []map[string]any {
	t.Helper()
	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var event map[string]any
		decodeJSON(t, line, &event)
		events = append(events, event)
	}
	return events
}

func findEvent(events []map[string]any, eventType string) map[string]any {
	for _, event := range events {
		if event["type"] == eventType {
			return event
		}
	}
	return nil
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
	}
}

func assertContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("expected %q to contain %q", text, want)
	}
}

func assertNotContains(t *testing.T, text, want string) {
	t.Helper()
	if strings.Contains(text, want) {
		t.Fatalf("expected %q not to contain %q", text, want)
	}
}

// TestAdoptContextProject covers the primary native-dispatch resolution:
// the native putnami CLI resolves the active project from cwd and passes it
// in the job context as `project.name`. `cd <workload> && putnami cloud
// <verb>` then resolves the app with no --app flag, matching `putnami
// build` ergonomics.
//
// The native CLI conflates context.workspace.name with context.project.name
// when invoked from a workload, so the distinction between "this is a
// workload" and "this is the workspace root" comes from reading the real
// workspace name out of putnami.workspace.json — hence the tempdir setup.
func TestAdoptContextProject(t *testing.T) {
	mkWorkspace := func(t *testing.T, name string) string {
		t.Helper()
		root := t.TempDir()
		writeJSONFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"name": name})
		return root
	}
	mkCtx := func(root, project string) putnamiContext {
		var c putnamiContext
		c.WorkspaceRoot = root
		c.Project.Name = project
		return c
	}

	t.Run("uses context project when it's a workload", func(t *testing.T) {
		root := mkWorkspace(t, "putnami-cloud")
		params := map[string]any{}
		adoptContextProject(params, mkCtx(root, "apps/api"))
		if params["app"] != "apps/api" {
			t.Errorf("params[app] = %v, want apps/api", params["app"])
		}
		if params["_contextApp"] != "apps/api" {
			t.Errorf("params[_contextApp] = %v, want the marker that distinguishes ambient context from explicit --app", params["_contextApp"])
		}
	})

	t.Run("ignores an ambiguous project-tail context", func(t *testing.T) {
		root := mkWorkspace(t, "putnami-cloud")
		writeNamedProject(t, root, "libs/core", "libs/core")
		writeNamedProject(t, root, "policy/libs/core", "policy/libs/core")

		params := map[string]any{}
		adoptContextProject(params, mkCtx(root, "core"))
		if got, ok := params["app"]; ok {
			t.Errorf("params[app] = %v, want unset for ambiguous task context", got)
		}
	})

	t.Run("ignores context project equal to the workspace's real name", func(t *testing.T) {
		// Invocation from the workspace root: project.name == the name in
		// putnami.workspace.json. Not a deployable app.
		root := mkWorkspace(t, "putnami-cloud")
		params := map[string]any{}
		adoptContextProject(params, mkCtx(root, "putnami-cloud"))
		if got, ok := params["app"]; ok {
			t.Errorf("params[app] = %v, want unset at workspace root", got)
		}
	})

	t.Run("does not overwrite explicit --app", func(t *testing.T) {
		root := mkWorkspace(t, "putnami-cloud")
		params := map[string]any{"app": "explicit"}
		adoptContextProject(params, mkCtx(root, "apps/api"))
		if params["app"] != "explicit" {
			t.Errorf("params[app] = %v, want explicit (must not overwrite)", params["app"])
		}
		if _, marked := params["_contextApp"]; marked {
			t.Error("explicit --app must not be marked as ambient context")
		}
	})

	t.Run("no-op on empty context project", func(t *testing.T) {
		root := mkWorkspace(t, "putnami-cloud")
		params := map[string]any{}
		adoptContextProject(params, mkCtx(root, ""))
		if got, ok := params["app"]; ok {
			t.Errorf("params[app] = %v, want unset", got)
		}
	})

	t.Run("uses project when workspace.json is absent (can't confirm it's the workspace)", func(t *testing.T) {
		// No putnami.workspace.json → workspaceName returns "" → the guard
		// can't prove the project is the workspace, so it's used. Safe: a
		// wrong value surfaces a clear downstream error rather than silently
		// resolving to the workspace.
		root := t.TempDir()
		params := map[string]any{}
		adoptContextProject(params, mkCtx(root, "some/workload"))
		if params["app"] != "some/workload" {
			t.Errorf("params[app] = %v, want some/workload", params["app"])
		}
	})
}

// TestAdoptPositionalApp covers the convention that lets users write
// `putnami cloud publish-config <app>` rather than the more verbose
// `--app <app>` — matches the native putnami CLI (`putnami build <app>`).
func TestAdoptPositionalApp(t *testing.T) {
	t.Run("picks up positional app", func(t *testing.T) {
		params := map[string]any{}
		adoptPositionalApp(params, []string{"apps/auth-server"})
		if params["app"] != "apps/auth-server" {
			t.Errorf("params[app] = %v, want apps/auth-server", params["app"])
		}
	})

	t.Run("does not overwrite explicit --app", func(t *testing.T) {
		params := map[string]any{"app": "explicit-name"}
		adoptPositionalApp(params, []string{"positional-name"})
		if params["app"] != "explicit-name" {
			t.Errorf("params[app] = %v, want explicit-name (must not overwrite)", params["app"])
		}
	})

	t.Run("does not overwrite explicit --appName", func(t *testing.T) {
		params := map[string]any{"appName": "from-context"}
		adoptPositionalApp(params, []string{"positional-name"})
		if got, ok := params["app"]; ok {
			t.Errorf("params[app] = %v, expected not set when appName is present", got)
		}
	})

	t.Run("ignores flag tokens", func(t *testing.T) {
		params := map[string]any{}
		adoptPositionalApp(params, []string{"--wait", "--json"})
		if got, ok := params["app"]; ok {
			t.Errorf("params[app] = %v, expected not set when args[0] is a flag", got)
		}
	})

	t.Run("skips parent-CLI --putnamiContext to find the real positional", func(t *testing.T) {
		// The native putnami CLI injects `--putnamiContext <path>` into the
		// args slice when invoking the extension. It can land before the
		// user's positional. Without the firstPositional-aware scan the
		// extension reads args[0]="--putnamiContext", bails on the flag
		// guard, and reports "could not determine app" even though the
		// user wrote `putnami cloud publish-config <app>`.
		params := map[string]any{}
		adoptPositionalApp(params, []string{"--putnamiContext", "/tmp/ctx.json", "apps/api"})
		if params["app"] != "apps/api" {
			t.Errorf("params[app] = %v, want apps/api (must look past --putnamiContext)", params["app"])
		}
	})

	t.Run("skips flag-with-value pairs", func(t *testing.T) {
		// `--env prod` is a flag followed by its value; the positional
		// app comes after it. Ensure firstPositional's flag-pair skip
		// logic flows through to adoptPositionalApp.
		params := map[string]any{}
		adoptPositionalApp(params, []string{"--env", "prod", "my-app"})
		if params["app"] != "my-app" {
			t.Errorf("params[app] = %v, want my-app", params["app"])
		}
	})

	t.Run("ignores empty args", func(t *testing.T) {
		params := map[string]any{}
		adoptPositionalApp(params, nil)
		if got, ok := params["app"]; ok {
			t.Errorf("params[app] = %v, expected not set when args is empty", got)
		}
	})

	t.Run("ignores empty positional", func(t *testing.T) {
		params := map[string]any{}
		adoptPositionalApp(params, []string{""})
		if got, ok := params["app"]; ok {
			t.Errorf("params[app] = %v, expected not set when args[0] is empty", got)
		}
	})
}

// TestFindCloudLinkRoot covers the walk-up that lets `cd <subdir> &&
// putnami cloud …` resolve the linked workspace the same way `cd <subdir>
// && putnami build` does. Without it the user has to either run from the
// workspace root or set PUTNAMI_WORKSPACE_ROOT.
func TestFindCloudLinkRoot(t *testing.T) {
	t.Run("finds link from a subdir", func(t *testing.T) {
		root := t.TempDir()
		linkDir := filepath.Join(root, ".putnami")
		mustMkdir(t, linkDir)
		writeJSONFile(t, filepath.Join(linkDir, "cloud-link.json"), map[string]any{"workspace_id": "w"})
		nested := filepath.Join(root, "auth", "workloads", "server")
		mustMkdir(t, nested)
		got, ok := findCloudLinkRoot(nested)
		if !ok {
			t.Fatalf("findCloudLinkRoot(%s) = false; want true", nested)
		}
		if got != root {
			t.Errorf("findCloudLinkRoot = %s, want %s", got, root)
		}
	})

	t.Run("finds link at the start dir itself", func(t *testing.T) {
		root := t.TempDir()
		mustMkdir(t, filepath.Join(root, ".putnami"))
		writeJSONFile(t, filepath.Join(root, ".putnami", "cloud-link.json"), map[string]any{"workspace_id": "w"})
		got, ok := findCloudLinkRoot(root)
		if !ok || got != root {
			t.Errorf("findCloudLinkRoot(%s) = (%q, %v), want (%q, true)", root, got, ok, root)
		}
	})

	t.Run("returns false when no link exists", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "auth", "server")
		mustMkdir(t, nested)
		got, ok := findCloudLinkRoot(nested)
		if ok || got != "" {
			t.Errorf("findCloudLinkRoot(%s) = (%q, %v), want (\"\", false)", nested, got, ok)
		}
	})

	t.Run("empty start returns false", func(t *testing.T) {
		if got, ok := findCloudLinkRoot(""); ok || got != "" {
			t.Errorf("findCloudLinkRoot(\"\") = (%q, %v), want (\"\", false)", got, ok)
		}
	})
}

// TestFindActiveProject covers the workload-resolution walk that drops the
// need for an explicit --app flag when invoked from inside a workload
// directory. Mirrors the native `putnami` CLI's project resolution; the
// extension was previously the odd one out.
func TestFindActiveProject(t *testing.T) {
	t.Run("finds nearest workload putnami.json", func(t *testing.T) {
		root := t.TempDir()
		writeJSONFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"name": "ws"})
		workload := filepath.Join(root, "auth", "workloads", "server")
		mustMkdir(t, workload)
		writeJSONFile(t, filepath.Join(workload, "putnami.json"), map[string]any{"name": "apps/auth-server"})
		got, ok := findActiveProject(workload, root)
		if !ok || got != "apps/auth-server" {
			t.Errorf("findActiveProject = (%q, %v), want (\"apps/auth-server\", true)", got, ok)
		}
	})

	t.Run("finds workload from a deeper subdir", func(t *testing.T) {
		root := t.TempDir()
		writeJSONFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"name": "ws"})
		workload := filepath.Join(root, "auth", "workloads", "server")
		mustMkdir(t, filepath.Join(workload, "src", "api"))
		writeJSONFile(t, filepath.Join(workload, "putnami.json"), map[string]any{"name": "apps/auth-server"})
		got, ok := findActiveProject(filepath.Join(workload, "src", "api"), root)
		if !ok || got != "apps/auth-server" {
			t.Errorf("findActiveProject = (%q, %v), want (\"apps/auth-server\", true)", got, ok)
		}
	})

	t.Run("stops at workspace root marker without a workload above", func(t *testing.T) {
		root := t.TempDir()
		writeJSONFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"name": "ws"})
		// No workload anywhere in the tree.
		nested := filepath.Join(root, "scripts")
		mustMkdir(t, nested)
		got, ok := findActiveProject(nested, root)
		if ok || got != "" {
			t.Errorf("findActiveProject = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("does not return the workspace's putnami.workspace.json as a project", func(t *testing.T) {
		// Even if a putnami.json sits alongside a putnami.workspace.json
		// (rare; would mean the workspace root is also a project),
		// findActiveProject should stop at the workspace marker BEFORE
		// reading the project file. Avoids surfacing the workspace name
		// as an app name to the cloud API.
		root := t.TempDir()
		writeJSONFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"name": "ws"})
		writeJSONFile(t, filepath.Join(root, "putnami.json"), map[string]any{"name": "@putnami/cloud"})
		got, ok := findActiveProject(root, root)
		if ok || got != "" {
			t.Errorf("findActiveProject at workspace root = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("does not cross the workspace boundary", func(t *testing.T) {
		// start is outside workspaceRoot — the walk-up shouldn't pull in
		// putnami.json files from unrelated trees. Matters for tests that
		// run from the cloud-cli package directory while exercising a
		// sandboxed workspaceRoot.
		root := t.TempDir()
		mustMkdir(t, filepath.Join(root, "ws"))
		writeJSONFile(t, filepath.Join(root, "ws", "putnami.workspace.json"), map[string]any{"name": "ws"})
		// A putnami.json sits OUTSIDE the workspace root.
		writeJSONFile(t, filepath.Join(root, "putnami.json"), map[string]any{"name": "outside-project"})
		got, ok := findActiveProject(root, filepath.Join(root, "ws"))
		if ok || got != "" {
			t.Errorf("findActiveProject outside workspace = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("returns false at filesystem root when nothing found", func(t *testing.T) {
		// Sandbox with no putnami.* files; walk must terminate cleanly.
		root := t.TempDir()
		got, ok := findActiveProject(root, "")
		if ok || got != "" {
			t.Errorf("findActiveProject(%s) = (%q, %v), want (\"\", false)", root, got, ok)
		}
	})
}

// TestExtensionManifestShellTestVerbIsFileActivated pins the `test` verb this
// extension contributes for projects no language extension covers: the CI
// runner image's subject under test is a bash entrypoint,
// so nothing else provides it a `test` job.
//
// The whole safety of adding a verb to a workspace-wide extension is its
// ACTIVATION: it must schedule only for a project that actually commits the
// harness it runs. Losing the activation gate would silently schedule a
// nonexistent script for every project in the workspace.
func TestExtensionManifestShellTestVerbIsFileActivated(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}
	commands, _ := manifest["commands"].(map[string]any)
	tasks, _ := manifest["tasks"].(map[string]any)

	command, ok := commands["test"].(map[string]any)
	if !ok {
		t.Fatal("manifest contributes no test command")
	}
	if got := command["activationFiles"]; !reflect.DeepEqual(got, []any{"tests/run.sh"}) {
		t.Fatalf("test.activationFiles = %v, want [tests/run.sh] — the verb must never activate without the harness it runs", got)
	}
	if _, workspaceWide := command["activation"]; workspaceWide {
		t.Fatalf("test must stay file-activated per project, got activation=%v", command["activation"])
	}
	run, ok := command["run"].([]any)
	if !ok || len(run) != 1 {
		t.Fatalf("test.run = %v, want exactly one step", command["run"])
	}
	step, _ := run[0].(map[string]any)
	if got := step["task"]; got != "cloud-shell-test" {
		t.Fatalf("test step task = %v, want cloud-shell-test", got)
	}
	// No upstream edge, deliberately. `^cross-compile` resolves within the SAME
	// command, so under `test` it would name a nonexistent upstream test step and
	// plan nothing — a dependency declaration that reads as ordering but is not.
	// The runtime covers the real case instead: the task runs {extensionRuntime},
	// which never reads .putnami/out (the runner purges it on every pinned
	// checkout). It is the installed archive's binary, or, for an extension
	// linked from source, a binary the CLI prepares from the current source.
	if _, declared := step["dependsOn"]; declared {
		t.Fatalf("test step declares dependsOn = %v; ^ refs do not resolve across commands, so this would be inert",
			step["dependsOn"])
	}

	task, ok := tasks["cloud-shell-test"].(map[string]any)
	if !ok {
		t.Fatal("manifest declares no cloud-shell-test task")
	}
	if got := task["args"]; !reflect.DeepEqual(got, []any{"shell-test"}) {
		t.Fatalf("cloud-shell-test args = %v, want the shell-test subcommand", got)
	}
	if got := task["cwd"]; got != "{projectRoot}" {
		t.Fatalf("cloud-shell-test cwd = %v, want {projectRoot}", got)
	}
	// Uncached on purpose: a shell harness drives real git/process fixtures, so
	// its inputs are wider than any declarable file set and a stale hit would
	// report a green gate for an unexercised entrypoint.
	if got, ok := task["cache"].(bool); !ok || got {
		t.Fatalf("cloud-shell-test cache = %v, want an explicit false", task["cache"])
	}
}

// TestExtensionManifestHasNoClassicDeploy pins that the classic deploy is gone:
// no `putnami deploy` workspace verb, no `cloud sync`, no deploy-publish task,
// and `cloud deploy` declares only the flags that `deploy status` and
// `deploy publish-v2` read.
func TestExtensionManifestHasNoClassicDeploy(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}
	commands, _ := manifest["commands"].(map[string]any)
	tasks, _ := manifest["tasks"].(map[string]any)
	for _, name := range []string{"deploy", "cloud-sync"} {
		if _, ok := commands[name]; ok {
			t.Fatalf("manifest still contributes the %q command", name)
		}
	}
	for _, name := range []string{"cloud-sync", "cloud-deploy-publish"} {
		if _, ok := tasks[name]; ok {
			t.Fatalf("manifest still declares the %q task", name)
		}
	}
	groups, _ := manifest["commandGroups"].(map[string]any)
	cloud, _ := groups["cloud"].(map[string]any)
	entries, _ := cloud["commands"].(map[string]any)
	if _, ok := entries["sync"]; ok {
		t.Fatal("the cloud group still lists the sync alias")
	}

	deploy, ok := commands["cloud-deploy"].(map[string]any)
	if !ok {
		t.Fatal("manifest contributes no cloud-deploy command")
	}
	flags, _ := deploy["flags"].(map[string]any)
	got := slices.Sorted(maps.Keys(flags))
	want := []string{"auth-url", "client-id", "control-plane-url", "env", "request-file", "strict", "timeout", "wait"}
	if !slices.Equal(got, want) {
		t.Fatalf("cloud-deploy flags = %v, want %v", got, want)
	}
	env, _ := commands["cloud-env"].(map[string]any)
	envFlags, _ := env["flags"].(map[string]any)
	for _, name := range []string{"apply", "prune", "allow-dirty", "skip-migration", "wait", "timeout"} {
		if _, ok := envFlags[name]; ok {
			t.Fatalf("cloud-env still declares the env sync flag %q", name)
		}
	}

	publish, ok := commands["publish"].(map[string]any)
	if !ok {
		t.Fatal("manifest contributes no publish command")
	}
	publishFlags, _ := publish["flags"].(map[string]any)
	if _, ok := publishFlags["dry-run"]; !ok {
		t.Fatal("publish command must declare dry-run before any side-effecting publisher may execute under putnami publish --dry-run")
	}
	publishRun, _ := publish["run"].([]any)
	configStep, _ := publishRun[0].(map[string]any)
	migrationStep, _ := publishRun[1].(map[string]any)
	if got := configStep["if"]; got != "!params.skip-publish-config && !params.skipPublishConfig" {
		t.Fatalf("publish config condition = %v, want only the skip flags", got)
	}
	if got := migrationStep["if"]; got != "!params.skip-publish-migration && !params.skipPublishMigration" {
		t.Fatalf("publish migration condition = %v, want only the skip flags", got)
	}
}

func TestExtensionManifestTargetedPublishCanSkipWorkspaceDocs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}
	commands, _ := manifest["commands"].(map[string]any)
	publish, ok := commands["publish"].(map[string]any)
	if !ok {
		t.Fatal("manifest contributes no publish command")
	}
	flags, _ := publish["flags"].(map[string]any)
	if _, ok := flags["skip-publish-doc"]; !ok {
		t.Fatal("publish must expose skip-publish-doc so a docs project's site-content step can be skipped")
	}
	// Docs no longer publish as a workspace prerequisite of every publish: a
	// package-scoped publish never reaches them, and the docs project's own
	// step honors the skip flag.
	if prerequisites, present := publish["sessionPrerequisites"]; present {
		t.Fatalf("publish.sessionPrerequisites = %v, want none", prerequisites)
	}
	for _, raw := range publish["run"].([]any) {
		step := raw.(map[string]any)
		if step["id"] != cloudSiteContentPublishStep {
			continue
		}
		if got, want := step["if"], "params.site-content && !params.skip-publish-doc && !params.skipPublishDoc"; got != want {
			t.Fatalf("publish site-content condition = %v, want %v", got, want)
		}
		return
	}
	t.Fatalf("publish run = %v, want a %s step", publish["run"], cloudSiteContentPublishStep)
}
