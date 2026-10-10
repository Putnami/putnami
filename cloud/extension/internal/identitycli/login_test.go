package identitycli

import (
	"net/http"
	"os"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// deviceOK is the standard device-authorization response carrying an explicit
// expiry and poll interval so the timeout/interval fallbacks are not exercised.
func (a *authServer) deviceOK() {
	a.deviceFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":               "dev-123",
			"user_code":                 "BCDF-GHJK",
			"verification_uri":          "https://verify.test/device",
			"verification_uri_complete": "https://verify.test/device?code=BCDF-GHJK",
			"expires_in":                900,
			"interval":                  0.001,
		})
	}
}

// tokenClaims are the access-token JWT claims shared by the success responders.
func tokenClaims(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"sub":       "user-claim",
		"email":     "claims@example.com",
		"name":      "Claims User",
		"provider":  "github",
		"scope_ref": map[string]any{"workspace_id": "ws-acme"},
	}
}

// tokenSuccess replies with a valid token response on every call.
func (a *authServer) tokenSuccess(t *testing.T) {
	t.Helper()
	a.tokenFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  jwt(t, tokenClaims(t)),
			"refresh_token": "refresh-abc",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}
}

// userinfoOK replies with the OIDC userinfo document.
func (a *authServer) userinfoOK() {
	a.userinfoFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"sub":      "user-info",
			"email":    "info@example.com",
			"name":     "Info User",
			"provider": "google",
		})
	}
}

func TestLoginHappyPathTextOutput(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.tokenSuccess(t)
	srv.userinfoOK()

	home := t.TempDir()
	ioctx, _, stdout, stderr := captureIO(t, srv.srv, home)

	var phases, ttys []string
	var progressCalls int
	ioctx.Phase = func(s string) { phases = append(phases, s) }
	ioctx.TTY = func(s string) { ttys = append(ttys, s) }
	ioctx.Progress = func(_, _ float64, _ string) { progressCalls++ }

	result, err := Login(map[string]any{"open": false}, ioctx.Env, ioctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// userinfo wins over claims for the resolved identity.
	if result.User.Email != "info@example.com" {
		t.Fatalf("user email = %q, want info@example.com", result.User.Email)
	}
	if result.Message != "Signed in as info@example.com." {
		t.Fatalf("message = %q", result.Message)
	}
	if result.Auth == nil || result.Auth.AccessToken == "" {
		t.Fatalf("expected persisted auth token")
	}

	// The credential must have been written to disk.
	if _, statErr := os.Stat(authPath(home)); statErr != nil {
		t.Fatalf("auth.json not written: %v", statErr)
	}

	joined := strings.Join(*stdout, "\n")
	for _, want := range []string{"To authenticate, visit:", "https://verify.test/device", "Enter code: BCDF-GHJK", "Waiting for authorization..."} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stdout missing %q:\n%s", want, joined)
		}
	}
	if len(phases) == 0 || len(ttys) == 0 || progressCalls == 0 {
		t.Fatalf("expected phase/tty/progress hooks to fire (phases=%d ttys=%d progress=%d)", len(phases), len(ttys), progressCalls)
	}
	if len(*stderr) != 0 {
		t.Fatalf("unexpected stderr: %v", *stderr)
	}
}

func TestLoginDefaultsWhenDeviceOmitsTimings(t *testing.T) {
	srv := newAuthServer(t)
	// Device response without expires_in/interval exercises the 900s/5s
	// fallbacks; the token succeeds immediately so the 5s interval never sleeps.
	srv.deviceFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":      "dev-123",
			"user_code":        "BCDF-GHJK",
			"verification_uri": "https://verify.test/device",
		})
	}
	srv.tokenSuccess(t)
	srv.userinfoOK()

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	if _, err := Login(map[string]any{"open": false}, ioctx.Env, ioctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
}

func TestLoginPendingThenSuccess(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.userinfoOK()
	srv.tokenFn = func(w http.ResponseWriter, _ *http.Request) {
		srv.mu.Lock()
		hit := srv.tokenHits
		srv.mu.Unlock()
		if hit == 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "authorization_pending"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": jwt(t, tokenClaims(t)),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	// A negative interval is clamped to 0, so the pending retry is immediate.
	result, err := Login(map[string]any{"open": false, "poll-interval-ms": -5, "poll-timeout-ms": 60000}, ioctx.Env, ioctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if result.Auth == nil {
		t.Fatalf("expected auth after pending->success")
	}
	if srv.tokenHits < 2 {
		t.Fatalf("token endpoint hits = %d, want >= 2", srv.tokenHits)
	}
}

func TestLoginSlowDownThenSuccess(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.userinfoOK()
	srv.tokenFn = func(w http.ResponseWriter, _ *http.Request) {
		srv.mu.Lock()
		hit := srv.tokenHits
		srv.mu.Unlock()
		if hit == 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "slow_down"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": jwt(t, tokenClaims(t)),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	result, err := Login(map[string]any{"open": false, "poll-interval-ms": 0, "poll-timeout-ms": 60000}, ioctx.Env, ioctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if result.Auth == nil {
		t.Fatalf("expected auth after slow_down->success")
	}
	if srv.tokenHits < 2 {
		t.Fatalf("token endpoint hits = %d, want >= 2", srv.tokenHits)
	}
}

func TestLoginOpensBrowser(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.tokenSuccess(t)
	srv.userinfoOK()

	home := t.TempDir()
	ioctx, _, stdout, _ := captureIO(t, srv.srv, home)
	ioctx.TTY = func(string) {}

	var opened string
	ioctx.OpenBrowser = func(url string) error {
		opened = url
		return nil
	}

	if _, err := Login(map[string]any{"open": true}, ioctx.Env, ioctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	// openURL prefers verification_uri_complete.
	if opened != "https://verify.test/device?code=BCDF-GHJK" {
		t.Fatalf("browser opened %q", opened)
	}
	if !strings.Contains(strings.Join(*stdout, "\n"), "Browser opened automatically.") {
		t.Fatalf("expected browser-opened confirmation, got:\n%s", strings.Join(*stdout, "\n"))
	}
}

func TestLoginBrowserOpenErrorIsSilent(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.tokenSuccess(t)
	srv.userinfoOK()

	home := t.TempDir()
	ioctx, _, stdout, _ := captureIO(t, srv.srv, home)
	ioctx.OpenBrowser = func(string) error { return os.ErrPermission }

	if _, err := Login(map[string]any{"open": true}, ioctx.Env, ioctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if strings.Contains(strings.Join(*stdout, "\n"), "Browser opened automatically.") {
		t.Fatalf("did not expect browser-opened confirmation on open error")
	}
}

func TestLoginJSONModeSuppressesHumanOutput(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.tokenSuccess(t)
	srv.userinfoOK()

	home := t.TempDir()
	ioctx, _, stdout, _ := captureIO(t, srv.srv, home)
	ioctx.OpenBrowser = func(string) error { return nil }

	if _, err := Login(map[string]any{"open": true, "json": true}, ioctx.Env, ioctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	joined := strings.Join(*stdout, "\n")
	for _, unwanted := range []string{"To authenticate, visit:", "Waiting for authorization...", "Browser opened automatically."} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("json mode must suppress %q, got:\n%s", unwanted, joined)
		}
	}
}

func TestLoginDeviceRequestError(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "server_error"})
	}
	srv.tokenSuccess(t)
	srv.userinfoOK()

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	if _, err := Login(map[string]any{"open": false}, ioctx.Env, ioctx); err == nil {
		t.Fatalf("expected error when device authorization fails")
	}
	if _, statErr := os.Stat(authPath(home)); statErr == nil {
		t.Fatalf("auth.json must not be written on device failure")
	}
}

func TestLoginTokenErrorWithDescription(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.userinfoOK()
	srv.tokenFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "access_denied",
			"error_description": "the user denied the request",
		})
	}

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	_, err := Login(map[string]any{"open": false, "poll-interval-ms": 0, "poll-timeout-ms": 60000}, ioctx.Env, ioctx)
	if err == nil || !strings.Contains(err.Error(), "the user denied the request") {
		t.Fatalf("expected denial error, got: %v", err)
	}
	if clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("exit code = %d, want ExitAuth", clicore.ExitCode(err))
	}
}

func TestLoginTokenErrorWithoutDescription(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.userinfoOK()
	srv.tokenFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expired_token"})
	}

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	_, err := Login(map[string]any{"open": false, "poll-interval-ms": 0, "poll-timeout-ms": 60000}, ioctx.Env, ioctx)
	if err == nil || !strings.Contains(err.Error(), "authorization failed: expired_token") {
		t.Fatalf("expected synthesized failure message, got: %v", err)
	}
}

func TestLoginTimesOut(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.userinfoOK()
	// The token endpoint would keep the flow pending, but a zero timeout trips
	// before the first poll.
	srv.tokenFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "authorization_pending"})
	}

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	_, err := Login(map[string]any{"open": false, "poll-timeout-ms": 0}, ioctx.Env, ioctx)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
	if clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("exit code = %d, want ExitAuth", clicore.ExitCode(err))
	}
}

func TestLoginWorkspaceFlagIsDeprecated(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.tokenSuccess(t)
	srv.userinfoOK()

	home := t.TempDir()
	ioctx, _, _, stderr := captureIO(t, srv.srv, home)

	if _, err := Login(map[string]any{"open": false, "workspace": "ws-old"}, ioctx.Env, ioctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !strings.Contains(strings.Join(*stderr, "\n"), "deprecated") {
		t.Fatalf("expected deprecation warning on stderr, got: %v", *stderr)
	}
}

func TestLoginFallsBackToClaimsWhenUserinfoFails(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.tokenSuccess(t)
	srv.userinfoFn = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "boom"})
	}

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	result, err := Login(map[string]any{"open": false}, ioctx.Env, ioctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	// With userinfo down, the identity is resolved from the JWT claims.
	if result.User.Email != "claims@example.com" {
		t.Fatalf("user email = %q, want claims@example.com", result.User.Email)
	}
	if result.Message != "Signed in as claims@example.com." {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestLoginCustomClientIDAndScope(t *testing.T) {
	srv := newAuthServer(t)
	srv.deviceOK()
	srv.userinfoOK()
	var gotClientID, gotScope string
	srv.deviceFn = func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBody(t, r)
		gotClientID, _ = body["client_id"].(string)
		gotScope, _ = body["scope"].(string)
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":      "dev-123",
			"user_code":        "BCDF-GHJK",
			"verification_uri": "https://verify.test/device",
			"expires_in":       900,
			"interval":         0.001,
		})
	}
	srv.tokenSuccess(t)

	home := t.TempDir()
	ioctx, _, _, _ := captureIO(t, srv.srv, home)

	if _, err := Login(map[string]any{"open": false, "client-id": "custom-cli", "scope": "openid custom"}, ioctx.Env, ioctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotClientID != "custom-cli" {
		t.Fatalf("client_id = %q, want custom-cli", gotClientID)
	}
	if gotScope != "openid custom" {
		t.Fatalf("scope = %q, want 'openid custom'", gotScope)
	}
}
