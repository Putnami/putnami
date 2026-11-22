package cloudcli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	"go.putnami.dev/protocol/features/spectest"
)

const ingestTokenFeature = "cloud/ingest-token"

// organizationRoutes stands in for identity-api's organization token routes
// on the control plane, with the wire contract of its handlers: create takes
// {name, expires_at?} and answers 201 with the one-time raw_token, list
// answers {organization_id, tokens} without secrets, delete answers
// {revoked, id, organization_id} and 404 for an id that is not the
// organization's. Every other call goes to the shared fake (sign-in).
type organizationRoutes struct {
	t        *testing.T
	fallback *fakeTransport
	refuse   int
	created  []map[string]any
	revoked  []string
	calls    []recordedRequest
}

func (o *organizationRoutes) RoundTrip(req *http.Request) (*http.Response, error) {
	const base = "/v1/organizations/org-acme/tokens"
	if !strings.HasPrefix(req.URL.Path, "/v1/organizations/") {
		return o.fallback.RoundTrip(req)
	}
	body := map[string]any{}
	raw := ""
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		raw = string(data)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				o.t.Fatalf("decode request body: %v", err)
			}
		}
	}
	o.calls = append(o.calls, recordedRequest{Method: req.Method, Path: req.URL.Path, Header: req.Header.Clone(), Body: body, RawBody: raw})
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") || len(req.Header.Get("Authorization")) < 10 {
		o.t.Fatalf("%s %s without the session bearer: %q", req.Method, req.URL.Path, req.Header.Get("Authorization"))
	}
	if o.refuse != 0 {
		return jsonResponse(o.refuse, map[string]any{"error": "forbidden", "message": "the caller does not manage this organization"}), nil
	}
	switch {
	case req.Method == http.MethodPost && req.URL.Path == base:
		name := stringValue(body["name"])
		token := map[string]any{
			"id":              "key-" + name,
			"name":            name,
			"prefix":          "pkt_ab",
			"organization_id": "org-acme",
			"tenant_id":       "tenant-acme",
			"allowed_scopes":  "delivery.ingest",
			"expires_at":      stringValue(body["expires_at"]),
			"created_at":      "2026-05-01T12:00:00Z",
		}
		o.created = append(o.created, token)
		answer := map[string]any{}
		for key, value := range token {
			answer[key] = value
		}
		answer["raw_token"] = "pkt_ab_secret_" + name
		return jsonResponse(http.StatusCreated, answer), nil
	case req.Method == http.MethodGet && req.URL.Path == base:
		rows := make([]map[string]any, 0, len(o.created))
		for _, token := range o.created {
			row := map[string]any{}
			for key, value := range token {
				row[key] = value
			}
			for _, id := range o.revoked {
				if id == token["id"] {
					row["revoked_at"] = "2026-05-01T13:00:00Z"
				}
			}
			rows = append(rows, row)
		}
		return jsonResponse(http.StatusOK, map[string]any{"organization_id": "org-acme", "tokens": rows}), nil
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, base+"/"):
		id := strings.TrimPrefix(req.URL.Path, base+"/")
		for _, done := range o.revoked {
			if done == id {
				return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found", "message": "no ingest token with that id in this organization"}), nil
			}
		}
		o.revoked = append(o.revoked, id)
		return jsonResponse(http.StatusOK, map[string]any{"revoked": true, "id": id, "organization_id": "org-acme"}), nil
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"}), nil
}

func (o *organizationRoutes) find(method, path string) *recordedRequest {
	for i := range o.calls {
		if o.calls[i].Method == method && o.calls[i].Path == path {
			return &o.calls[i]
		}
	}
	return nil
}

// organizationTokensEnv signs in and returns an env with NO linked workspace:
// an organization token needs none.
func organizationTokensEnv(t *testing.T) (map[string]string, *organizationRoutes) {
	t.Helper()
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
	env := hometest.Env(home, map[string]string{
		"PUTNAMI_AUTH_URL":       testBaseURL,
		"PUTNAMI_WORKSPACE_ROOT": t.TempDir(),
	})
	return env, &organizationRoutes{t: t, fallback: fake}
}

func runOrganizationTokens(env map[string]string, routes *organizationRoutes, args ...string) ([]string, []string, error) {
	var out, errs []string
	err := RunCommand("token", IO{
		Env:    env,
		Stdout: func(line string) { out = append(out, line) },
		Stderr: func(line string) { errs = append(errs, line) },
		Client: &http.Client{Transport: routes},
		Now:    fixedNow,
	}, args)
	return out, errs, err
}

func TestOrganizationCreatePostsTheNameAndExpiry(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "create-calls-the-organization-route", "organization-create-posts-the-name-and-expiry")
	env, routes := organizationTokensEnv(t)
	if _, _, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", "ci", "--expires", "30d"); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := routes.find(http.MethodPost, "/v1/organizations/org-acme/tokens")
	if req == nil {
		t.Fatalf("no POST /v1/organizations/org-acme/tokens: %+v", routes.calls)
	}
	if len(req.Body) != 2 || req.Body["name"] != "ci" || !strings.HasPrefix(stringValue(req.Body["expires_at"]), "2026-05-30T12:00:00") {
		t.Fatalf("body = %v, want name and expires_at only", req.Body)
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", req.Header.Get("Content-Type"))
	}
	if _, _, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", "nightly"); err != nil {
		t.Fatalf("create without expiry: %v", err)
	}
	last := routes.calls[len(routes.calls)-1]
	if _, present := last.Body["expires_at"]; present || last.Body["name"] != "nightly" {
		t.Fatalf("body without --expires = %v", last.Body)
	}
}

func TestOrganizationCreateNeedsNoLinkedWorkspace(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "create-calls-the-organization-route", "organization-create-needs-no-linked-workspace")
	env, routes := organizationTokensEnv(t)
	delete(env, "PUTNAMI_WORKSPACE_ROOT")
	if _, _, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", "ci"); err != nil {
		t.Fatalf("create outside a workspace: %v", err)
	}
	if routes.find(http.MethodPost, "/v1/organizations/org-acme/tokens") == nil {
		t.Fatal("create sent no organization call")
	}
	// Without --organization, the same checkout still needs a linked workspace.
	if _, _, err := runOrganizationTokens(env, routes, "create", "--name", "ci", "--scopes", "delivery.ingest"); err == nil {
		t.Fatal("a workspace token was created with no linked workspace")
	}
}

func TestOrganizationCreatePrintsTheTokenOnce(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "create-shows-the-token-once", "organization-create-prints-the-token-once")
	env, routes := organizationTokensEnv(t)
	out, errs, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", "ci")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	joined := strings.Join(out, "\n")
	assertContains(t, joined, `Created organization ingest token "ci" for organization org-acme, scope delivery.ingest.`)
	assertContains(t, joined, "\n  pkt_ab_secret_ci\n")
	assertContains(t, joined, "shown only once")
	assertContains(t, joined, `gh secret set PUTNAMI_CLOUD_TOKEN --body "pkt_ab_secret_ci"`)
	assertContains(t, joined, `export PUTNAMI_CLOUD_TOKEN="pkt_ab_secret_ci"`)
	if strings.Contains(strings.Join(errs, "\n"), "pkt_ab_secret_ci") {
		t.Fatalf("stderr prints the token: %q", errs)
	}
	// The list after it never prints the secret again.
	listed, _, err := runOrganizationTokens(env, routes, "list", "--organization", "org-acme")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	assertNotContains(t, strings.Join(listed, "\n"), "secret")
}

func TestOrganizationCreateJSONCarriesTheToken(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "create-shows-the-token-once", "organization-create-json-carries-the-token")
	env, routes := organizationTokensEnv(t)
	out, _, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", "ci", "--output=json")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(strings.Join(out, "\n")), &result); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	data, _ := result["data"].(map[string]any)
	if data == nil {
		data = result
	}
	if data["raw_token"] != "pkt_ab_secret_ci" || data["organization_id"] != "org-acme" || data["allowed_scopes"] != "delivery.ingest" || data["id"] != "key-ci" {
		t.Fatalf("json result = %v", result)
	}
}

func TestOrganizationCreateRefusesAnotherScope(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "create-refuses-another-scope", "organization-create-refuses-another-scope")
	env, routes := organizationTokensEnv(t)
	for _, extra := range [][]string{
		{"--scopes", "cache.read"},
		{"--scopes", "delivery.ingest,cache.read"},
		{"--allowed-clients", "review-worker"},
		{"--allowed-client-ids", "a3839d53-5b31-48d4-8924-fb29df25c743"},
	} {
		args := append([]string{"create", "--organization", "org-acme", "--name", "ci"}, extra...)
		_, _, err := runOrganizationTokens(env, routes, args...)
		if err == nil || clicore.ExitCode(err) != ExitUsage {
			t.Fatalf("%v: err = %v, want a usage error", extra, err)
		}
		if extra[0] == "--scopes" {
			assertContains(t, err.Error(), "delivery.ingest scope only")
		}
	}
	if len(routes.calls) != 0 {
		t.Fatalf("a refused create sent %d call(s)", len(routes.calls))
	}
	if _, _, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", "ci", "--scopes", "delivery.ingest"); err != nil {
		t.Fatalf("--scopes delivery.ingest: %v", err)
	}
}

func TestOrganizationListPrintsLiveTokensWithoutSecret(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "list-never-shows-the-secret", "organization-list-prints-live-tokens-without-secret")
	env, routes := organizationTokensEnv(t)
	for _, name := range []string{"ci", "old"} {
		if _, _, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", name); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	routes.revoked = append(routes.revoked, "key-old")
	out, _, err := runOrganizationTokens(env, routes, "list", "--organization", "org-acme")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	joined := strings.Join(out, "\n")
	assertContains(t, joined, "NAME")
	assertContains(t, joined, "key-ci")
	assertContains(t, joined, "delivery.ingest")
	assertNotContains(t, joined, "key-old")
	assertNotContains(t, joined, "pkt_ab_secret")
	if routes.find(http.MethodGet, "/v1/organizations/org-acme/tokens") == nil {
		t.Fatal("list sent no GET")
	}
	out, _, err = runOrganizationTokens(env, routes, "list", "--organization", "org-acme", "--output=json")
	if err != nil {
		t.Fatalf("list json: %v", err)
	}
	joined = strings.Join(out, "\n")
	assertContains(t, joined, `"key-ci"`)
	assertNotContains(t, joined, "key-old")
	assertNotContains(t, joined, "raw_token")
}

func TestOrganizationRevokeResolvesANameAndDeletes(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "revoke-resolves-the-token", "organization-revoke-resolves-a-name-and-deletes")
	env, routes := organizationTokensEnv(t)
	if _, _, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", "ci"); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, _, err := runOrganizationTokens(env, routes, "revoke", "ci", "--organization", "org-acme")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if routes.find(http.MethodDelete, "/v1/organizations/org-acme/tokens/key-ci") == nil {
		t.Fatalf("no DELETE of key-ci: %+v", routes.calls)
	}
	assertContains(t, strings.Join(out, "\n"), "Revoked organization ingest token key-ci.")
	// A second revoke of the same id converges on the server's 404.
	if _, _, err := runOrganizationTokens(env, routes, "revoke", "key-ci", "--organization", "org-acme"); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	_, _, err = runOrganizationTokens(env, routes, "revoke", "missing", "--organization", "org-acme")
	if err == nil || clicore.ExitCode(err) != ExitUsage {
		t.Fatalf("unknown name: err = %v", err)
	}
	assertContains(t, err.Error(), `no organization ingest token matches "missing"`)
}

func TestOrganizationRefusalNamesTheRouteAndReason(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "refusal-names-the-route", "organization-refusal-names-the-route-and-reason")
	env, routes := organizationTokensEnv(t)
	routes.refuse = http.StatusForbidden
	_, _, err := runOrganizationTokens(env, routes, "create", "--organization", "org-acme", "--name", "ci")
	if err == nil {
		t.Fatal("a refused create succeeded")
	}
	message := err.Error()
	assertContains(t, message, "/v1/organizations/org-acme/tokens")
	assertContains(t, message, "platform.organization.manage")
	if clicore.ExitCode(err) == 0 {
		t.Fatal("a refusal exits zero")
	}
	routes.refuse = http.StatusBadGateway
	_, _, err = runOrganizationTokens(env, routes, "list", "--organization", "org-acme")
	if err == nil {
		t.Fatal("a refused list succeeded")
	}
	assertContains(t, err.Error(), "/v1/organizations/org-acme/tokens")
	assertContains(t, err.Error(), "does not manage this organization")
}

// An answer the CLI cannot decode is named by its route and status only: the
// body of a create answer may hold the token.
func TestOrganizationUndecodableAnswerNeverQuotesTheBody(t *testing.T) {
	server := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"raw_token":"pkt_leak"`)),
		}, nil
	})
	_, err := organizationTokenCall(&http.Client{Transport: server}, http.MethodPost, "https://api.test/v1/organizations/o/tokens", "access", map[string]any{"name": "ci"}, http.StatusCreated)
	if err == nil {
		t.Fatal("an undecodable answer was accepted")
	}
	assertNotContains(t, err.Error(), "pkt_leak")
}

func TestOrganizationFlagRefusesOtherSubcommands(t *testing.T) {
	env, routes := organizationTokensEnv(t)
	for _, args := range [][]string{
		{"status", "--organization", "org-acme"},
		{"--organization", "org-acme"},
		{"cache", "--organization", "org-acme"},
	} {
		_, _, err := runOrganizationTokens(env, routes, args...)
		if err == nil || clicore.ExitCode(err) != ExitUsage {
			t.Fatalf("%v: err = %v, want a usage error", args, err)
		}
	}
	if len(routes.calls) != 0 {
		t.Fatalf("a refused --organization sent %d call(s)", len(routes.calls))
	}
}

func TestTokenHelpNamesTheOrganizationCommands(t *testing.T) {
	env, routes := organizationTokensEnv(t)
	out, _, err := runOrganizationTokens(env, routes, "help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	joined := strings.Join(out, "\n")
	for _, want := range []string{"token create --organization <id> --name <name>", "token list --organization <id>", "token revoke <id|name> --organization <id>"} {
		assertContains(t, joined, want)
	}
}

// putnami cloud login, logout and token are interactive and
// workspace-optional, so the CLI runs them outside any workspace (CLI ADR
// 0051): a person with no workspace signs in, then creates the token.
func TestSignInAndTokenCommandsRunWithoutAWorkspace(t *testing.T) {
	spectest.Proves(t, ingestTokenFeature, "signs-in-outside-a-workspace", "sign-in-and-token-commands-run-without-a-workspace")
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest struct {
		CommandGroups map[string]struct {
			Subcommands map[string]struct {
				Interactive bool   `json:"interactive"`
				Workspace   string `json:"workspace"`
			} `json:"subcommands"`
		} `json:"commandGroups"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}
	for _, name := range []string{"login", "logout", "token"} {
		sub, ok := manifest.CommandGroups["cloud"].Subcommands[name]
		if !ok || !sub.Interactive || sub.Workspace != "optional" {
			t.Errorf("cloud %s = %+v, want interactive and workspace optional", name, sub)
		}
	}
}
