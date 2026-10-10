package cloudcli

import (
	"net/http"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// machineTokensEnv logs in through the fake transport and returns an env that
// drives the `cloud token` family against a linked workspace (ws-acme).
func machineTokensEnv(t *testing.T) (map[string]string, *fakeTransport) {
	t.Helper()
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeLinkFile(t, workspaceRoot)
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
	return hometest.Env(home, map[string]string{
		"PUTNAMI_AUTH_URL":       testBaseURL,
		"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
	}), fake
}

func runTokens(t *testing.T, env map[string]string, fake *fakeTransport, args ...string) []string {
	t.Helper()
	var out []string
	if err := RunCommand("tokens", IO{
		Env:    env,
		Stdout: func(line string) { out = append(out, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, args); err != nil {
		t.Fatalf("cloud token %v: %v", args, err)
	}
	return out
}

// TestMachineTokensCreateShapesWorkspaceOwnedRequest proves create targets the
// CPA route for the linked workspace, sends ONLY the member-chosen
// fields (the CPA pins the owner from the path — the body must NOT carry an owner
// selector), normalizes scopes, resolves --expires, and reveals the raw token
// exactly once with the paste-ready guidance.
func TestMachineTokensCreateShapesWorkspaceOwnedRequest(t *testing.T) {
	env, fake := machineTokensEnv(t)

	out := runTokens(t, env, fake,
		"create", "--name", "ci-deploy",
		"--scopes", "deploy.read, deploy.write, delivery.ingest",
		"--expires", "30d",
	)

	req := fake.find("/v1/workspaces/ws-acme/tokens")
	if req == nil || req.Method != http.MethodPost {
		t.Fatalf("expected POST /v1/workspaces/ws-acme/tokens, got %+v", req)
	}
	// The CPA derives the owner from the authorized path — the client must not
	// send an owner selector (a confused-deputy vector if the CPA trusted it).
	for _, ownerField := range []string{"owner_kind", "owner_principal_id", "workspace_id"} {
		if _, present := req.Body[ownerField]; present {
			t.Fatalf("create body must not carry %q (the CPA pins the owner from the path): %+v", ownerField, req.Body)
		}
	}
	if req.Body["allowed_scopes"] != "deploy.read deploy.write delivery.ingest" {
		t.Fatalf("allowed_scopes = %v, want normalized space-joined string", req.Body["allowed_scopes"])
	}
	if got := stringValue(req.Body["expires_at"]); !strings.HasPrefix(got, "2026-05-30T12:00:00") {
		t.Fatalf("expires_at = %q, want 30 days after fixedNow", got)
	}

	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "pkt_01_secret_ci-deploy") {
		t.Fatalf("create output missing raw token: %q", joined)
	}
	assertContains(t, joined, "shown only once")
	assertContains(t, joined, "gh secret set PUTNAMI_CLOUD_TOKEN")
	assertContains(t, joined, "PUTNAMI_CACHE_TOKEN")
	assertContains(t, joined, "PUTNAMI_E2E_TOKEN")
}

// The CLI forwards the exact row identity rather than widening to an unbound key.
func TestMachineTokenCreatePassesExclusiveClientBinding(t *testing.T) {
	env, fake := machineTokensEnv(t)
	const clientID = "a3839d53-5b31-48d4-8924-fb29df25c743"
	runTokens(t, env, fake, "create", "--name", "review-worker", "--scopes", "review.claim", "--allowed-client-ids", clientID, "--expires", "1d")
	req := fake.find("/v1/workspaces/ws-acme/tokens")
	if req == nil {
		t.Fatal("missing workspace-owned request")
	}
	bindings, ok := req.Body["allowed_client_ids"].([]any)
	if !ok || len(bindings) != 1 || bindings[0] != clientID {
		t.Fatalf("exclusive binding lost: %v", req.Body["allowed_client_ids"])
	}
}

// --allowed-clients forwards public client names as allowed_clients, so an
// operator never looks up a row UUID; Auth resolves them. The UUID field is
// sent only when its own flag is set.
func TestMachineTokenCreatePassesClientNames(t *testing.T) {
	env, fake := machineTokensEnv(t)
	runTokens(t, env, fake, "create", "--name", "t", "--scopes", "cache.read", "--allowed-clients", "review-worker")
	req := fake.find("/v1/workspaces/ws-acme/tokens")
	if req == nil {
		t.Fatal("missing workspace-owned request")
	}
	names, ok := req.Body["allowed_clients"].([]any)
	if !ok || len(names) != 1 || names[0] != "review-worker" {
		t.Fatalf("allowed_clients = %v, want [review-worker]", req.Body["allowed_clients"])
	}
	if _, present := req.Body["allowed_client_ids"]; present {
		t.Fatalf("allowed_client_ids sent without --allowed-client-ids: %+v", req.Body)
	}
}

// Both selectors travel together (Auth binds their union), and names accept
// the same comma- or space-separated list shape as scopes.
func TestMachineTokenCreatePassesBothClientSelectors(t *testing.T) {
	env, fake := machineTokensEnv(t)
	const clientID = "a3839d53-5b31-48d4-8924-fb29df25c743"
	runTokens(t, env, fake, "create", "--name", "t", "--scopes", "cache.read",
		"--allowed-client-ids", clientID, "--allowed-clients", "review-worker, cache")
	req := fake.find("/v1/workspaces/ws-acme/tokens")
	if req == nil {
		t.Fatal("missing workspace-owned request")
	}
	ids, ok := req.Body["allowed_client_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != clientID {
		t.Fatalf("allowed_client_ids = %v, want [%s]", req.Body["allowed_client_ids"], clientID)
	}
	names, ok := req.Body["allowed_clients"].([]any)
	if !ok || len(names) != 2 || names[0] != "review-worker" || names[1] != "cache" {
		t.Fatalf("allowed_clients = %v, want [review-worker cache]", req.Body["allowed_clients"])
	}
}

// TestMachineTokensRoundTrip proves create → list → revoke → list: the token
// appears once created, the secret never shows in list, and a revoked token
// drops out of the listing.
func TestMachineTokensRoundTrip(t *testing.T) {
	env, fake := machineTokensEnv(t)

	runTokens(t, env, fake, "create", "--name", "agent-key", "--scopes", "deploy.read")

	list := strings.Join(runTokens(t, env, fake, "list"), "\n")
	assertContains(t, list, "agent-key")
	assertContains(t, list, "apikey-1")
	assertNotContains(t, list, "pkt_01_secret") // the secret never surfaces in list

	runTokens(t, env, fake, "revoke", "agent-key")
	if len(fake.revokedIDs) != 1 || fake.revokedIDs[0] != "apikey-1" {
		t.Fatalf("revokedIDs = %v, want [apikey-1]", fake.revokedIDs)
	}

	after := strings.Join(runTokens(t, env, fake, "list"), "\n")
	assertContains(t, after, "No workspace machine tokens")
	assertNotContains(t, after, "apikey-1")
}

// TestMachineTokensListStructuredOmitsSecret proves the structured surface
// carries the metadata columns and never the raw token.
func TestMachineTokensListStructuredOmitsSecret(t *testing.T) {
	env, fake := machineTokensEnv(t)
	runTokens(t, env, fake, "create", "--name", "reader", "--scopes", "cache.read")

	out := runTokens(t, env, fake, "list", "--json")
	var payload struct {
		Tokens []map[string]any `json:"tokens"`
	}
	decodeJSON(t, strings.Join(out, "\n"), &payload)
	if len(payload.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(payload.Tokens))
	}
	tok := payload.Tokens[0]
	if tok["name"] != "reader" {
		t.Fatalf("name = %v, want reader", tok["name"])
	}
	if _, leaked := tok["raw_token"]; leaked {
		t.Fatalf("list must not carry raw_token: %+v", tok)
	}
	assertNotContains(t, strings.Join(out, "\n"), "pkt_01_secret")
}

// TestMachineTokensRevokeByID proves an explicit id revokes without a name
// lookup ambiguity.
func TestMachineTokensRevokeByID(t *testing.T) {
	env, fake := machineTokensEnv(t)
	runTokens(t, env, fake, "create", "--name", "k1", "--scopes", "cache.read")

	runTokens(t, env, fake, "revoke", "apikey-1")
	if len(fake.revokedIDs) != 1 || fake.revokedIDs[0] != "apikey-1" {
		t.Fatalf("revokedIDs = %v, want [apikey-1]", fake.revokedIDs)
	}
}

// TestMachineTokensRevokeIsIdempotent proves re-revoking an already-revoked id
// converges instead of failing resolution: revoked rows are hidden from list
// output and name matching, but an exact id still resolves and the DELETE's
// 404-tolerant AllowStatuses makes the re-run a success. A revoked NAME must
// not resolve — only ids are idempotent handles.
func TestMachineTokensRevokeIsIdempotent(t *testing.T) {
	env, fake := machineTokensEnv(t)
	runTokens(t, env, fake, "create", "--name", "twice", "--scopes", "cache.read")

	runTokens(t, env, fake, "revoke", "apikey-1")
	runTokens(t, env, fake, "revoke", "apikey-1")
	if len(fake.revokedIDs) != 2 {
		t.Fatalf("revokedIDs = %v, want the DELETE re-issued on the second revoke", fake.revokedIDs)
	}

	err := RunCommand("tokens", IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"revoke", "twice"})
	if err == nil {
		t.Fatalf("expected a revoked name to no longer resolve")
	}
	assertContains(t, err.Error(), "no workspace machine token matches")
}

func TestMachineTokensCreateRequiresNameAndScopes(t *testing.T) {
	env, fake := machineTokensEnv(t)

	err := RunCommand("tokens", IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"create", "--scopes", "cache.read"})
	if err == nil {
		t.Fatalf("expected an error when --name is omitted")
	}
	assertContains(t, err.Error(), "--name")

	err = RunCommand("tokens", IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"create", "--name", "x"})
	if err == nil {
		t.Fatalf("expected an error when --scopes is omitted")
	}
	assertContains(t, err.Error(), "--scopes")
}

func TestMachineTokensUnknownSubcommandErrors(t *testing.T) {
	env, fake := machineTokensEnv(t)
	err := RunCommand("tokens", IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"rotate"})
	if err == nil {
		t.Fatalf("expected an error for an unknown tokens subcommand")
	}
	assertContains(t, err.Error(), "unknown tokens subcommand")
}

func TestMachineTokensRevokeUnknownTargetErrors(t *testing.T) {
	env, fake := machineTokensEnv(t)
	runTokens(t, env, fake, "create", "--name", "present", "--scopes", "cache.read")

	err := RunCommand("tokens", IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"revoke", "missing"})
	if err == nil {
		t.Fatalf("expected an error revoking an unknown token")
	}
	assertContains(t, err.Error(), "no workspace machine token matches")
}

// TestMachineTokensCreateSurfacesServerForbidden proves a non-member caller
// surfaces the CPA's 403 cleanly (the CPA is the membership gate,
// so a non-member is rejected there with the caller's ordinary session — no
// workspace-scoped-token dance). The fresh session means the CLI never touches
// the token endpoint; it goes straight to the CPA tokens route.
func TestMachineTokensCreateSurfacesServerForbidden(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeLinkFile(t, workspaceRoot)
	writeTestAuth(t, home)
	env := hometest.Env(home, map[string]string{
		"PUTNAMI_AUTH_URL":       testBaseURL,
		"PUTNAMI_WORKSPACE_ROOT": workspaceRoot,
	})

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/v1/workspaces/ws-acme/tokens":
			return jsonResponse(http.StatusForbidden, map[string]any{
				"error":   "Forbidden",
				"message": "Insufficient permissions or workspace scope mismatch",
			}), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})}

	err := RunCommand("tokens", IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: client,
		Now:    fixedNow,
	}, []string{"create", "--name", "denied", "--scopes", "cache.read"})
	if err == nil {
		t.Fatalf("expected a 403 error")
	}
	// The generated client carries the provider's message.
	assertContains(t, err.Error(), "Insufficient permissions or workspace scope mismatch")
}

// TestTokenStatusFlagsExpiringAndExpiredTokens: `cloud token status` is one
// node with a child per live token. A token that expires within 7 days, or
// expired and was never revoked, is degraded with the command that fixes it.
// Degraded exits 0 unless --strict; a revoked token is left out.
func TestTokenStatusFlagsExpiringAndExpiredTokens(t *testing.T) {
	env, fake := machineTokensEnv(t)
	runTokens(t, env, fake, "create", "--name", "fresh", "--scopes", "deploy.read", "--expires", "30d")
	runTokens(t, env, fake, "create", "--name", "soon", "--scopes", "cache.read,cache.write", "--expires", "3d")
	runTokens(t, env, fake, "create", "--name", "gone", "--scopes", "cache.read")
	runTokens(t, env, fake, "revoke", "gone")
	fake.mintedKeys = append(fake.mintedKeys, map[string]any{
		"id": "apikey-9", "name": "stale", "prefix": "pkt_09", "allowed_scopes": "deploy.read",
		"created_at": "2026-01-01T00:00:00Z", "expires_at": "2026-04-01T00:00:00Z",
	})

	out := strings.Join(runTokens(t, env, fake, "status"), "\n")
	for _, want := range []string{
		"token  degraded  2 active, 1 expiring within 7 days, 1 expired, not revoked",
		"fresh  scopes deploy.read, expires 2026-05-30, never used",
		"fix: putnami cloud token create --name soon --scopes cache.read,cache.write",
		"fix: putnami cloud token revoke apikey-9",
	} {
		assertContains(t, out, want)
	}
	assertNotContains(t, out, "gone")

	err := RunCommand("token", IO{
		Env: env, Stdout: func(string) {}, Stderr: func(string) {}, Client: fake.client(), Now: fixedNow,
	}, []string{"status", "--strict"})
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("strict exit = %v, want ExitFailure", err)
	}

	var node clicore.StatusNode
	decodeResultData(t, []byte(strings.Join(runTokens(t, env, fake, "status", "--output=json"), "\n")), &node)
	if node.ID != "token" || len(node.Children) != 3 || len(node.Metrics) != 2 ||
		node.Metrics[0].Value != 2 || node.Metrics[1].Value != 1 {
		t.Fatalf("node = %+v", node)
	}
}

// TestTokenStatusRoutesBeforeTheTarget: `cloud token status` reads the
// tokens; it is never minted as a bearer for a target named "status".
func TestTokenStatusRoutesBeforeTheTarget(t *testing.T) {
	env, fake := machineTokensEnv(t)
	var out []string
	if err := RunCommand("token", IO{
		Env: env, Stdout: func(line string) { out = append(out, line) }, Stderr: func(string) {}, Client: fake.client(), Now: fixedNow,
	}, []string{"status"}); err != nil {
		t.Fatalf("token status: %v", err)
	}
	if joined := strings.Join(out, "\n"); !strings.HasPrefix(joined, "token  ok  no workspace machine token") {
		t.Fatalf("token status output:\n%s", joined)
	}
	if fake.find("/v1/workspaces/ws-acme/tokens") == nil {
		t.Fatal("token status did not read the workspace tokens")
	}
}

func TestTokenStatusNodeFrom(t *testing.T) {
	now := fixedNow()
	node := TokenStatusNodeFrom([]map[string]any{
		{"id": "k1", "name": "ci", "allowed_scopes": "deploy.read", "expires_at": "", "last_used_at": "2026-04-29T08:00:00Z"},
		{"id": "k2", "name": "", "allowed_scopes": "cache.read", "expires_at": now.Add(6 * 24 * time.Hour).Format(time.RFC3339)},
	}, now)
	if node.State != clicore.StatusDegraded || node.Fix != "putnami cloud token create --name k2 --scopes cache.read" {
		t.Fatalf("node = %+v", node)
	}
	if node.Children[0].State != clicore.StatusOK || node.Children[0].Detail != "scopes deploy.read, never expires, last used 2026-04-29" {
		t.Fatalf("ci = %+v", node.Children[0])
	}
	if node.Children[1].Title != "k2" || node.Children[1].State != clicore.StatusDegraded {
		t.Fatalf("k2 = %+v", node.Children[1])
	}
}
