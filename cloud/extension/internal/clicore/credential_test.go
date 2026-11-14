package clicore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const credentialTestWorkspace = "11111111-aaaa-bbbb-cccc-000000000000"

// scopedTestToken is an unsigned JWT that claims workspaceID, which the
// workspace access cache requires of a cached scoped token.
func scopedTestToken(t *testing.T, workspaceID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"scope_ref": map[string]any{"workspace_id": workspaceID}})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

// credentialTestEnv is a machine with PUTNAMI_CLOUD_TOKEN set and, when
// session is true, a usable signed-in session holding a fresh token scoped to
// the test workspace.
func credentialTestEnv(t *testing.T, session bool) (map[string]string, string, time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	env := map[string]string{"PUTNAMI_HOME": t.TempDir(), CloudTokenEnv: "pkt_workspace_machine"}
	scoped := scopedTestToken(t, credentialTestWorkspace)
	if session {
		err := WriteAuth(&StoredToken{
			AccessToken: "user-base", RefreshToken: "renew", TokenType: "Bearer",
			ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
			WorkspaceAccess: map[string]StoredWorkspaceAccess{credentialTestWorkspace: {
				AccessToken: scoped, TokenType: "Bearer", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
			}},
		}, env)
		if err != nil {
			t.Fatal(err)
		}
	}
	return env, scoped, now
}

func TestWorkspaceAuthPrefersAUsableSession(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "the-signed-in-session-wins", "a-usable-session-wins-over-the-machine-token")
	env, scoped, now := credentialTestEnv(t, true)
	auth, err := WorkspaceAuth(nil, env, IO{Now: func() time.Time { return now }}, credentialTestWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if auth.AccessToken != scoped {
		t.Fatalf("WorkspaceAuth sent %q, want the session's scoped token over PUTNAMI_CLOUD_TOKEN", auth.AccessToken)
	}
	cred := ResolveCredential(env, now)
	if cred.Machine || !cred.TokenSet {
		t.Fatalf("ResolveCredential = %+v; want the session with the token set", cred)
	}
	if got := cred.String(); got != "the signed-in session, which wins over PUTNAMI_CLOUD_TOKEN" {
		t.Fatalf("credential = %q", got)
	}
}

func TestWorkspaceAuthFallsBackToTheMachineTokenWithoutAUsableSession(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "the-signed-in-session-wins", "the-machine-token-serves-without-a-usable-session")
	env, _, now := credentialTestEnv(t, false)
	io := IO{Now: func() time.Time { return now }}
	auth, err := WorkspaceAuth(nil, env, io, credentialTestWorkspace)
	if err != nil || auth.AccessToken != env[CloudTokenEnv] {
		t.Fatalf("no session: WorkspaceAuth = %+v, %v; want PUTNAMI_CLOUD_TOKEN", auth, err)
	}

	// A stored session that expired and cannot renew is not usable.
	if err := WriteAuth(&StoredToken{AccessToken: "old", TokenType: "Bearer", ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}, env); err != nil {
		t.Fatal(err)
	}
	auth, err = WorkspaceAuth(nil, env, io, credentialTestWorkspace)
	if err != nil || auth.AccessToken != env[CloudTokenEnv] {
		t.Fatalf("expired session: WorkspaceAuth = %+v, %v; want PUTNAMI_CLOUD_TOKEN", auth, err)
	}
	cred := ResolveCredential(env, now)
	if got := cred.String(); got != "PUTNAMI_CLOUD_TOKEN, the workspace machine token: no signed-in session is stored on this machine" {
		t.Fatalf("credential = %q", got)
	}

	// No token and no session: the session path answers, as before.
	delete(env, CloudTokenEnv)
	if _, err := WorkspaceAuth(nil, env, io, credentialTestWorkspace); ExitCode(err) != ExitAuth {
		t.Fatalf("no credential: err = %v, want an auth error", err)
	}
}

func TestVerboseNamesTheCredentialOnce(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "the-command-names-its-credential", "verbose-names-the-credential")
	credentialAnnounced.Store(false)
	t.Cleanup(func() { credentialAnnounced.Store(false) })
	env, _, now := credentialTestEnv(t, true)
	var lines []string
	io := IO{Now: func() time.Time { return now }, Stderr: func(line string) { lines = append(lines, line) }}
	if _, err := WorkspaceAuth(map[string]any{}, env, io, credentialTestWorkspace); err != nil || len(lines) != 0 {
		t.Fatalf("without --verbose: err %v, lines %q", err, lines)
	}
	verbose := map[string]any{"verbose": true}
	for range 2 {
		if _, err := WorkspaceAuth(verbose, env, io, credentialTestWorkspace); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 1 || lines[0] != "credential: the signed-in session, which wins over PUTNAMI_CLOUD_TOKEN" {
		t.Fatalf("--verbose lines = %q, want one line naming the session", lines)
	}
}

func TestNameCredentialNamesTheRefusedCredential(t *testing.T) {
	spectest.Proves(t, "cloud/agent-session", "the-command-names-its-credential", "an-auth-error-names-the-refused-credential")
	env, _, now := credentialTestEnv(t, true)
	refused := NewError("request failed: 401 Unauthorized", ExitAuth)
	err := NameCredential(refused, env, now)
	if ExitCode(err) != ExitAuth || !errors.Is(err, refused) {
		t.Fatalf("named error %v lost its exit code or cause", err)
	}
	if !strings.Contains(err.Error(), "credential: the signed-in session, which wins over PUTNAMI_CLOUD_TOKEN") ||
		!strings.Contains(err.Error(), "renew the session") {
		t.Fatalf("message %q does not name the session", err)
	}

	bare, _, _ := credentialTestEnv(t, false)
	forbidden := &APIError{StatusCode: http.StatusForbidden, Message: "Insufficient permissions", exit: newExitError("Insufficient permissions", ExitAPI)}
	err = NameCredential(forbidden, bare, now)
	if ExitCode(err) != ExitAPI || !strings.Contains(err.Error(), "credential: PUTNAMI_CLOUD_TOKEN, the workspace machine token") ||
		!strings.Contains(err.Error(), "run putnami cloud login to use a signed-in session") {
		t.Fatalf("403 on the machine token = %v", err)
	}
	statusLine := NewError("cloud logs: "+StatusLine(http.StatusForbidden), ExitAPI)
	if err := NameCredential(statusLine, env, now); !strings.Contains(err.Error(), "credential:") {
		t.Fatalf("a 403 status line was not named: %v", err)
	}

	other := NewError("boom", ExitAPI)
	if err := NameCredential(other, env, now); !errors.Is(err, other) {
		t.Fatalf("a non-refusal changed: %v", err)
	}
	delete(env, CloudTokenEnv)
	if err := NameCredential(refused, env, now); !errors.Is(err, refused) {
		t.Fatalf("one credential only: %v, want the error unchanged", err)
	}
	if err := NameCredential(nil, env, now); err != nil {
		t.Fatalf("nil = %v", err)
	}
}

func TestSessionUsable(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		auth *StoredToken
		want bool
	}{
		{"none", nil, false},
		{"no access token", &StoredToken{RefreshToken: "r"}, false},
		{"fresh", &StoredToken{AccessToken: "a", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano)}, true},
		{"expired, renewable", &StoredToken{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}, true},
		{"expired", &StoredToken{AccessToken: "a", ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}, false},
	} {
		if got := SessionUsable(tc.auth, now); got != tc.want {
			t.Errorf("%s: SessionUsable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
