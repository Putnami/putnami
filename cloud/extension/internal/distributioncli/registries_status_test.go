package distributioncli

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func TestRegistriesStatusNodeFromMapsEveryKeyState(t *testing.T) {
	node := RegistriesStatusNodeFrom([]RegistryAnswer{
		{Registry: RegistryNPM, Configured: true, Host: "npm.putnami.dev"},
		{Registry: RegistryGomod, Configured: true, Host: "go.putnami.dev"},
		{Registry: RegistryOCI},
		{Registry: RegistryPut, Configured: true, Host: "put.putnami.dev", KeyID: "key-1", KeyPrefix: "pkt_ephe", Active: true},
	})
	want := strings.Join([]string{
		"registries  degraded  npm, go on demand; oci not configured; put key active",
		"",
		"  METRIC                 VALUE         WINDOW",
		"  registries configured  3 of 4 (75%)  now",
		"",
		"  STATE     CHECK  DETAIL",
		"  ok        npm    on demand, npm.putnami.dev",
		"  ok        go     on demand, go.putnami.dev",
		"  degraded  oci    not configured on this machine",
		"                   fix: putnami cloud registries setup",
		"  ok        put    key pkt_ephe active, put.putnami.dev",
	}, "\n")
	if got := strings.Join(clicore.RenderStatusReport(node), "\n"); got != want {
		t.Fatalf("registries status = %q, want %q", got, want)
	}

	for _, tc := range []struct {
		answer RegistryAnswer
		state  clicore.StatusState
		detail string
		fix    string
	}{
		{RegistryAnswer{NotFound: true}, clicore.StatusDegraded, "stale key pkt_ephe: auth-server no longer knows it", "putnami cloud registries setup"},
		{RegistryAnswer{RevokedAt: "2026-10-02T11:30:00Z"}, clicore.StatusFailing, "key pkt_ephe revoked at 2026-10-02T11:30:00Z", "putnami cloud registries setup"},
		{RegistryAnswer{ExpiresAt: "2026-10-02T11:30:00Z"}, clicore.StatusFailing, "key pkt_ephe expired at 2026-10-02T11:30:00Z", "putnami cloud registries setup"},
		{RegistryAnswer{Err: errors.New("auth-server unavailable")}, clicore.StatusUnknown, "key pkt_ephe not read: auth-server unavailable", ""},
	} {
		answer := tc.answer
		answer.Registry, answer.Configured, answer.KeyID, answer.KeyPrefix = RegistryPut, true, "key-1", "pkt_ephe"
		node := RegistriesStatusNodeFrom([]RegistryAnswer{answer})
		child := node.Children[0]
		if node.State != tc.state || child.State != tc.state || child.Detail != tc.detail || child.Fix != tc.fix {
			t.Fatalf("%+v = %+v, want %s %q fix %q", tc.answer, child, tc.state, tc.detail, tc.fix)
		}
	}
}

func TestRegistryStatusNodeFromShowsTheEndpointAndTheKey(t *testing.T) {
	node := RegistryStatusNodeFrom(RegistryAnswer{
		Registry: RegistryPut, Configured: true, Host: "put.putnami.dev", URL: "https://put.putnami.dev",
		KeyID: "key-1", KeyPrefix: "pkt_ephe", Active: true, ExpiresAt: "2026-10-02T13:00:00Z",
	})
	if node.ID != "registries.put" || node.State != clicore.StatusOK || len(node.Children) != 2 ||
		node.Children[0].Detail != "https://put.putnami.dev" || node.Children[1].Detail != "pkt_ephe, expires 2026-10-02T13:00:00Z, last used never" {
		t.Fatalf("node = %+v", node)
	}
}

// TestRegistriesStatusReadsEachStoredKey drives `registries status` against
// auth-server answering a stored key with JSON null members, an answer that
// must not read as "invalid JSON response".
func TestRegistriesStatusReadsEachStoredKey(t *testing.T) {
	_, env, ioctx := distributionWorkflowFixture(t)
	answer := workflowAPIKeyCreated("")
	delete(answer, "raw_token")
	answer["revoked_at"], answer["expires_at"], answer["last_used_at"], answer["tenant_id"] = nil, nil, nil, nil
	base, requests := apiKeyStatusServer(t, http.StatusOK, answer)
	env["PUTNAMI_AUTH_URL"] = base
	if err := WriteRegistriesState(env, &RegistriesState{Version: 1, Keys: []KeyRef{
		{Registry: RegistryNPM, Host: "npm.putnami.dev"},
		{Registry: RegistryPut, Host: "put.putnami.dev", ID: "key-1", Prefix: "pkt_ephe"},
	}}); err != nil {
		t.Fatal(err)
	}
	var stdout []string
	ioctx.Stdout = func(line string) { stdout = append(stdout, line) }
	args := []string{"status"}
	if err := Registries(clicore.MergeParams(clicore.ParseFlags(args)), args, env, ioctx); err != nil {
		t.Fatalf("registries status: %v", err)
	}
	if text := strings.Join(stdout, "\n"); !strings.HasPrefix(text, "registries  degraded  npm on demand; go, oci not configured; put key active") {
		t.Fatalf("registries status = %q", text)
	}
	if len(*requests) != 1 || !strings.HasPrefix((*requests)[0], "GET /apikeys/key-1 ") {
		t.Fatalf("requests = %v, want one key read", *requests)
	}

	stdout = nil
	args = []string{"status", "go"}
	if err := Registries(clicore.MergeParams(clicore.ParseFlags(args)), args, env, ioctx); err != nil {
		t.Fatalf("registries status go: %v", err)
	}
	if text := strings.Join(stdout, "\n"); text != "go  degraded  not configured on this machine\n\nnext: putnami cloud registries setup" {
		t.Fatalf("registries status go = %q", text)
	}
	args = []string{"status", "pip"}
	if err := Registries(clicore.MergeParams(clicore.ParseFlags(args)), args, env, ioctx); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("an unknown registry = %v, want a usage error", err)
	}

	node := RegistriesStatusNode(map[string]any{}, "", env, ioctx)
	if node.State != clicore.StatusDegraded || node.Children[3].Detail != "key pkt_ephe active, put.putnami.dev" {
		t.Fatalf("node = %+v, want the same node the command prints", node)
	}
}

func TestRegistriesStatusKeepsTheAuthError(t *testing.T) {
	_, env, ioctx := distributionWorkflowFixture(t)
	if err := clicore.WriteAuth(&clicore.StoredToken{AccessToken: "expired", ExpiresAt: "2020-01-01T00:00:00Z"}, env); err != nil {
		t.Fatal(err)
	}
	if err := WriteRegistriesState(env, &RegistriesState{Version: 1, Keys: []KeyRef{
		{Registry: RegistryPut, Host: "put.putnami.dev", ID: "key-1", Prefix: "pkt_ephe"},
	}}); err != nil {
		t.Fatal(err)
	}
	args := []string{"status"}
	if err := Registries(clicore.MergeParams(clicore.ParseFlags(args)), args, env, ioctx); clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("an expired session = %v, want exit %d", err, clicore.ExitAuth)
	}
	if node := RegistriesStatusNode(map[string]any{}, "", env, ioctx); node.State != clicore.StatusUnknown || node.Fix != "putnami cloud login" {
		t.Fatalf("node = %+v, want unknown with the login command", node)
	}
}
