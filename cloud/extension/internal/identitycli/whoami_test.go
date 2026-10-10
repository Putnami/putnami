package identitycli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// storeFreshAuth persists a non-expired base credential whose access token
// carries the given claims, so ActiveAuth serves it without a refresh round
// trip.
func storeFreshAuth(t *testing.T, home, issuer string) {
	t.Helper()
	token := jwt(t, map[string]any{
		"sub":       "user-claim",
		"email":     "claims@example.com",
		"name":      "Claims User",
		"provider":  "github",
		"scope_ref": map[string]any{"workspace_id": "ws-acme"},
	})
	writeAuthFile(t, home, clicore.StoredToken{
		AccessToken:  token,
		RefreshToken: "refresh-abc",
		TokenType:    "Bearer",
		ExpiresAt:    freshExpiry(),
		Issuer:       issuer,
		ClientID:     "putnami-cli",
	})
}

// linkWorkspace writes the .putnami/cloud-link.json `putnami cloud setup`
// leaves in a repository linked to workspace ws-acme, and returns the
// repository root.
func linkWorkspace(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	data, err := json.Marshal(map[string]any{
		"workspace_id": "ws-acme", "workspace_name": name,
		"control_plane_url": "https://api.example.test", "environment": "prod", "version": 1,
	})
	if err != nil {
		t.Fatalf("marshal link: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".putnami"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".putnami", "cloud-link.json"), data, 0o600); err != nil {
		t.Fatalf("write link: %v", err)
	}
	return root
}

func (a *authServer) whoamiUserinfoOK() {
	a.userinfoFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"sub":      "user-info",
			"email":    "info@example.com",
			"name":     "Info User",
			"provider": "google",
		})
	}
}

func TestWhoAmITextOutput(t *testing.T) {
	srv := newAuthServer(t)
	srv.whoamiUserinfoOK()

	home := t.TempDir()
	ioctx, env, stdout, _ := captureIO(t, srv.srv, home)
	storeFreshAuth(t, home, srv.srv.URL)
	root := linkWorkspace(t, "acme")

	if err := WhoAmI(map[string]any{}, root, env, ioctx); err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	for _, want := range []string{
		"whoami  ok  info@example.com, workspace acme",
		"signed in as info@example.com",
		"linked to workspace acme (ws-acme)",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stdout missing %q:\n%s", want, joined)
		}
	}
	if srv.userinfoHits == 0 {
		t.Fatalf("expected userinfo to be queried")
	}
}

func TestWhoAmIJSONOutputIsTheNode(t *testing.T) {
	srv := newAuthServer(t)
	srv.whoamiUserinfoOK()

	home := t.TempDir()
	ioctx, env, stdout, _ := captureIO(t, srv.srv, home)
	storeFreshAuth(t, home, srv.srv.URL)
	root := linkWorkspace(t, "")

	if err := WhoAmI(map[string]any{"output": "json"}, root, env, ioctx); err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	var envelope struct {
		Data clicore.StatusNode `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.Join(*stdout, "\n")), &envelope); err != nil {
		t.Fatalf("decode: %v\n%s", err, strings.Join(*stdout, "\n"))
	}
	node := envelope.Data
	if node.ID != "whoami" || node.State != clicore.StatusOK || len(node.Children) != 2 {
		t.Fatalf("node = %+v", node)
	}
	if node.Children[0].Detail != "signed in as info@example.com" || node.Children[1].Detail != "linked to workspace ws-acme" {
		t.Fatalf("children = %+v", node.Children)
	}
}

func TestWhoAmINotSignedInFailsWithTheLoginFix(t *testing.T) {
	srv := newAuthServer(t)
	srv.whoamiUserinfoOK()

	home := t.TempDir() // no auth.json written
	ioctx, env, stdout, _ := captureIO(t, srv.srv, home)
	root := linkWorkspace(t, "")

	err := WhoAmI(map[string]any{}, root, env, ioctx)
	if clicore.ExitCode(err) != clicore.ExitFailure {
		t.Fatalf("exit code = %d (%v), want ExitFailure", clicore.ExitCode(err), err)
	}
	if !strings.Contains(err.Error(), "whoami is failing: not signed in") {
		t.Fatalf("error = %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	if !strings.Contains(joined, "fix: putnami cloud login") {
		t.Fatalf("stdout missing the login fix:\n%s", joined)
	}
}

func TestWhoAmIUserinfoRefusalEndsTheSession(t *testing.T) {
	srv := newAuthServer(t)
	srv.userinfoFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
	}

	home := t.TempDir()
	ioctx, env, _, _ := captureIO(t, srv.srv, home)
	storeFreshAuth(t, home, srv.srv.URL)
	root := linkWorkspace(t, "")

	node := WhoamiStatusNode(map[string]any{}, root, env, ioctx)
	if node.State != clicore.StatusFailing || node.Children[0].Detail != "not signed in" {
		t.Fatalf("node = %+v", node)
	}
}

func TestWhoAmIUserinfoOutageFallsBackToTheClaims(t *testing.T) {
	srv := newAuthServer(t)
	srv.userinfoFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "boom"})
	}

	home := t.TempDir()
	ioctx, env, _, _ := captureIO(t, srv.srv, home)
	storeFreshAuth(t, home, srv.srv.URL)
	root := linkWorkspace(t, "")

	node := WhoamiStatusNode(map[string]any{}, root, env, ioctx)
	if node.State != clicore.StatusOK || node.Children[0].Detail != "signed in as claims@example.com" {
		t.Fatalf("node = %+v", node)
	}
}

func TestWhoamiStatusNodeFrom(t *testing.T) {
	at := fixedNow()
	linked := WhoamiLink{WorkspaceID: "11111111-aaaa-bbbb-cccc-000000000000"}
	cases := []struct {
		name       string
		session    WhoamiSession
		link       WhoamiLink
		state      clicore.StatusState
		detail     string
		fix        string
		metricSecs float64
	}{
		{
			name:    "signed in and linked",
			session: WhoamiSession{User: User{Email: "dev@example.com"}, ExpiresAt: at.Add(90 * time.Minute)},
			link:    linked, state: clicore.StatusOK, detail: "dev@example.com, workspace 11111111", metricSecs: 5400,
		},
		{
			name:    "CI token",
			session: WhoamiSession{CIToken: true},
			link:    linked, state: clicore.StatusOK, detail: "PUTNAMI_CLOUD_TOKEN, workspace 11111111",
		},
		{
			name:    "not signed in",
			session: WhoamiSession{Err: clicore.NewError("not authenticated", clicore.ExitAuth)},
			link:    linked, state: clicore.StatusFailing, detail: "not signed in, workspace 11111111", fix: "putnami cloud login",
		},
		{
			name:    "sign-in service down",
			session: WhoamiSession{Err: errors.New("refresh failed: connection refused")},
			link:    linked, state: clicore.StatusUnknown, detail: "refresh failed: connection refused, workspace 11111111", fix: "putnami cloud login",
		},
		{
			name:    "not linked",
			session: WhoamiSession{User: User{Name: "Dev"}},
			link:    WhoamiLink{Err: errors.New("workspace not configured")},
			state:   clicore.StatusFailing, detail: "Dev, this repository is not linked to a workspace", fix: "putnami cloud setup",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := WhoamiStatusNodeFrom(tc.session, tc.link, at)
			if node.ID != "whoami" || node.State != tc.state || node.Detail != tc.detail || node.Fix != tc.fix {
				t.Fatalf("node = %+v", node)
			}
			if tc.metricSecs == 0 {
				if len(node.Metrics) != 0 {
					t.Fatalf("metrics = %+v, want none", node.Metrics)
				}
				return
			}
			if len(node.Metrics) != 1 || node.Metrics[0].ID != "session_expires_in" || node.Metrics[0].Value != tc.metricSecs {
				t.Fatalf("metrics = %+v", node.Metrics)
			}
		})
	}
}

func TestSessionExpiryReadsTheRefreshToken(t *testing.T) {
	refresh := jwt(t, map[string]any{"exp": float64(fixedNow().Add(24 * time.Hour).Unix())})
	if got := sessionExpiry(&clicore.StoredToken{RefreshToken: refresh}); !got.Equal(fixedNow().Add(24 * time.Hour)) {
		t.Fatalf("expiry = %s", got)
	}
	if got := sessionExpiry(&clicore.StoredToken{RefreshToken: "opaque"}); !got.IsZero() {
		t.Fatalf("opaque refresh expiry = %s, want zero", got)
	}
	if got := sessionExpiry(&clicore.StoredToken{ExpiresAt: freshExpiry()}); !got.Equal(fixedNow().Add(time.Hour)) {
		t.Fatalf("access expiry = %s", got)
	}
}

func TestSignedInWording(t *testing.T) {
	if got := loginSuccessMessage(User{Email: "dev@example.com"}); got != "Signed in as dev@example.com." {
		t.Fatalf("login message = %q", got)
	}
	if got := loginSuccessMessage(User{}); got != "Signed in." {
		t.Fatalf("anonymous login message = %q", got)
	}
}
