package distributioncli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// apiKeyStatusServer answers GET and DELETE /apikeys/key-1 the way auth-server
// does and records each request as "METHOD /path bearer".
func apiKeyStatusServer(t *testing.T, status int, payload any) (string, *[]string) {
	t.Helper()
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	return server.URL, &requests
}

func TestGetAPIKeyStatusReportsAnActiveKey(t *testing.T) {
	now := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	answer := workflowAPIKeyCreated("")
	delete(answer, "raw_token")
	answer["expires_at"], answer["last_used_at"] = "2026-10-02T13:00:00Z", "2026-10-02T11:00:00Z"
	base, requests := apiKeyStatusServer(t, http.StatusOK, answer)

	status, err := getAPIKeyStatus(context.Background(), nil, base, "session-access", "key-1", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if want := (keyStatus{Active: true, ExpiresAt: "2026-10-02T13:00:00Z", LastUsedAt: "2026-10-02T11:00:00Z"}); status != want {
		t.Fatalf("status = %+v, want %+v", status, want)
	}
	if want := "GET /apikeys/key-1 Bearer session-access"; len(*requests) != 1 || (*requests)[0] != want {
		t.Fatalf("requests = %v, want %q", *requests, want)
	}
}

func TestGetAPIKeyStatusReportsARevokedOrExpiredKey(t *testing.T) {
	now := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	for name, member := range map[string][2]string{
		"revoked": {"revoked_at", "2026-10-02T11:30:00Z"},
		"expired": {"expires_at", "2026-10-02T11:30:00Z"},
	} {
		t.Run(name, func(t *testing.T) {
			answer := workflowAPIKeyCreated("")
			delete(answer, "raw_token")
			answer[member[0]] = member[1]
			base, _ := apiKeyStatusServer(t, http.StatusOK, answer)
			status, err := getAPIKeyStatus(context.Background(), nil, base, "session-access", "key-1", now)
			if err != nil || status.Active {
				t.Fatalf("status = %+v, %v; want an inactive key", status, err)
			}
		})
	}
}

func TestGetAPIKeyStatusReportsAKeyAuthServerNoLongerKnows(t *testing.T) {
	base, _ := apiKeyStatusServer(t, http.StatusNotFound, map[string]any{"error": "not_found", "error_description": "api key not found"})
	status, err := getAPIKeyStatus(context.Background(), nil, base, "session-access", "key-1", time.Now())
	if err != nil || status != (keyStatus{NotFound: true}) {
		t.Fatalf("status = %+v, %v; want NotFound", status, err)
	}
}

func TestGetAPIKeyStatusMapsAnExpiredSessionToExitAuth(t *testing.T) {
	base, _ := apiKeyStatusServer(t, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
	if _, err := getAPIKeyStatus(context.Background(), nil, base, "session-access", "key-1", time.Now()); clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("status error = %v, want ExitAuth", err)
	}
}

func TestRevokeAPIKeyRevokesAndToleratesAKeyAlreadyGone(t *testing.T) {
	base, requests := apiKeyStatusServer(t, http.StatusOK, workflowAPIKeyRevoked)
	if err := revokeAPIKey(nil, base, "session-access", "key-1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if want := "DELETE /apikeys/key-1 Bearer session-access"; len(*requests) != 1 || (*requests)[0] != want {
		t.Fatalf("requests = %v, want %q", *requests, want)
	}
	gone, _ := apiKeyStatusServer(t, http.StatusNotFound, map[string]any{"error": "not_found"})
	if err := revokeAPIKey(nil, gone, "session-access", "key-1"); err != nil {
		t.Fatalf("revoke of a key already gone = %v, want nil", err)
	}
}
