package clicore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// createdAPIKeyAnswer is auth-server's 201 answer to POST /apikeys: every
// member its contract requires, with the one-time raw token.
func createdAPIKeyAnswer(id, rawToken string) map[string]any {
	return map[string]any{
		"id": id, "name": "cli:distribution:ws-mint", "prefix": "pkt_ephe",
		"owner_principal_kind": "user", "owner_principal_id": "user-1",
		"allowed_scopes": mintTestScope, "allowed_client_ids": []string{},
		"workspace_id": mintTestWorkspace, "created_at": "2026-10-02T12:00:00Z",
		"expires_at": "2026-10-02T12:10:00Z", "raw_token": rawToken,
	}
}

// apiKeyServer is an auth-server fake for one key route. It records the
// request and answers with status and payload.
type apiKeyServer struct {
	method, path, authorization string
	status                      int
	payload                     any
}

func (s *apiKeyServer) start(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.method, s.path, s.authorization = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_ = json.NewEncoder(w).Encode(s.payload)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestReadAPIKeyReadsTheKeyWithTheUserBearer(t *testing.T) {
	answer := createdAPIKeyAnswer("key/1", "")
	delete(answer, "raw_token")
	answer["revoked_at"], answer["last_used_at"] = "2026-10-02T12:05:00Z", "2026-10-02T12:01:00Z"
	server := &apiKeyServer{status: http.StatusOK, payload: answer}
	base := server.start(t)

	state, err := ReadAPIKey(context.Background(), nil, base+"/", "session-access", "key/1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if server.method != http.MethodGet || server.path != "/apikeys/key%2F1" || server.authorization != "Bearer session-access" {
		t.Fatalf("request = %s %s with %q", server.method, server.path, server.authorization)
	}
	want := APIKeyState{RevokedAt: "2026-10-02T12:05:00Z", ExpiresAt: "2026-10-02T12:10:00Z", LastUsedAt: "2026-10-02T12:01:00Z"}
	if state != want {
		t.Fatalf("state = %+v, want %+v", state, want)
	}
}

func TestReadAPIKeyReportsAKeyAuthServerNoLongerKnows(t *testing.T) {
	server := &apiKeyServer{status: http.StatusNotFound, payload: map[string]any{"error": "not_found", "error_description": "api key not found"}}
	state, err := ReadAPIKey(context.Background(), nil, server.start(t), "session-access", "key-1")
	if err != nil || state != (APIKeyState{NotFound: true}) {
		t.Fatalf("read = %+v, %v; want NotFound", state, err)
	}
}

func TestReadAPIKeyMapsAnExpiredSessionToExitAuth(t *testing.T) {
	server := &apiKeyServer{status: http.StatusUnauthorized, payload: map[string]any{"error": "invalid_token"}}
	base := server.start(t)
	_, err := ReadAPIKey(context.Background(), nil, base, "session-access", "key-1")
	if ExitCode(err) != ExitAuth || err.Error() != "request failed for "+base+"/apikeys/key-1: invalid_token" {
		t.Fatalf("read = %v (exit %d), want an ExitAuth refusal naming the key and auth-server's cause", err, ExitCode(err))
	}
}

func TestReadAPIKeyNamesTheErrorDescriptionOfAnOAuthRefusal(t *testing.T) {
	server := &apiKeyServer{status: http.StatusBadRequest, payload: map[string]any{"error": "invalid_request", "error_description": "id must be a uuid"}}
	base := server.start(t)
	_, err := ReadAPIKey(context.Background(), nil, base, "session-access", "key-1")
	if ExitCode(err) != ExitAPI || err.Error() != "request failed for "+base+"/apikeys/key-1: id must be a uuid" {
		t.Fatalf("read = %v (exit %d), want error_description", err, ExitCode(err))
	}
}

func TestReadAPIKeyNamesTheStatusLineOfARefusalWithoutACause(t *testing.T) {
	server := &apiKeyServer{status: http.StatusForbidden, payload: map[string]any{}}
	base := server.start(t)
	_, err := ReadAPIKey(context.Background(), nil, base, "session-access", "key-1")
	if ExitCode(err) != ExitAPI || err.Error() != "request failed for "+base+"/apikeys/key-1: 403 Forbidden" {
		t.Fatalf("read = %v (exit %d), want the status line", err, ExitCode(err))
	}
}

func TestOAuthRefusalWithMessage(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"description first", `{"error":"invalid_token","error_description":"token expired"}`, `{"error":"invalid_token","error_description":"token expired","message":"token expired"}`},
		{"error when no description", `{"error":"invalid_token","error_description":" "}`, `{"error":"invalid_token","error_description":" ","message":"invalid_token"}`},
		{"message kept", `{"error":"forbidden","message":"owned by another principal"}`, `{"error":"forbidden","message":"owned by another principal"}`},
		{"no cause", `{"code":"x"}`, `{"code":"x"}`},
		{"cause not a string", `{"error":{"reason":"x"}}`, `{"error":{"reason":"x"}}`},
		{"not an object", `["invalid_token"]`, `["invalid_token"]`},
		{"not JSON", `upstream connect error`, `upstream connect error`},
		{"null", `null`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(oauthRefusalWithMessage([]byte(tc.body))); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// stubTransport answers every request with a fixed status and body.
type stubTransport struct {
	status int
	body   io.Reader
}

func (s stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	header := http.Header{"Content-Length": []string{"0"}}
	return &http.Response{StatusCode: s.status, Header: header, Body: io.NopCloser(s.body)}, nil
}

func TestOAuthRefusalMessagesRewritesOnlyRefusals(t *testing.T) {
	refusal := `{"error":"invalid_token"}`
	resp, err := authServerCompat{base: stubTransport{status: http.StatusUnauthorized, body: strings.NewReader(refusal)}}.
		RoundTrip(httptest.NewRequest(http.MethodGet, "https://auth.test/apikeys/key-1", nil))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	want := `{"error":"invalid_token","message":"invalid_token"}`
	if string(data) != want || resp.ContentLength != int64(len(want)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(want)) {
		t.Fatalf("refusal = %s (length %d, header %q), want %s", data, resp.ContentLength, resp.Header.Get("Content-Length"), want)
	}

	success := `{"error":"not a refusal"}`
	resp, err = authServerCompat{base: stubTransport{status: http.StatusOK, body: strings.NewReader(success)}}.
		RoundTrip(httptest.NewRequest(http.MethodGet, "https://auth.test/apikeys/key-1", nil))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if data, _ := io.ReadAll(resp.Body); string(data) != success {
		t.Fatalf("success = %s, want it untouched", data)
	}
}

func TestOAuthRefusalMessagesPassesAnOversizedRefusalThrough(t *testing.T) {
	large := `{"error":"` + strings.Repeat("x", refusalBodyLimit) + `"}`
	resp, err := authServerCompat{base: stubTransport{status: http.StatusBadRequest, body: strings.NewReader(large)}}.
		RoundTrip(httptest.NewRequest(http.MethodGet, "https://auth.test/apikeys/key-1", nil))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	if string(data) != large {
		t.Fatalf("oversized refusal changed: %d bytes, want %d", len(data), len(large))
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestOAuthRefusalMessagesKeepsTheTransportError(t *testing.T) {
	wrapped := withAuthServerCompat(nil)
	if wrapped == http.DefaultClient {
		t.Fatal("wrapped the shared default client")
	}
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/apikeys/key-1", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := wrapped.Transport.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("round trip to a closed port succeeded")
	}
}

func TestReadAPIKeySurfacesTheProviderMessage(t *testing.T) {
	server := &apiKeyServer{status: http.StatusForbidden, payload: map[string]any{"error": "forbidden", "message": "cannot read api key owned by another principal"}}
	_, err := ReadAPIKey(context.Background(), nil, server.start(t), "session-access", "key-1")
	if ExitCode(err) != ExitAPI || !strings.Contains(err.Error(), "cannot read api key owned by another principal") {
		t.Fatalf("read = %v (exit %d), want the provider's message", err, ExitCode(err))
	}
}

// TestReadAPIKeyReadsAKeyWithNullMembers pins a past regression: auth-server
// sends an unset key column as JSON null, and the generated client refused the
// whole answer as "invalid JSON response".
func TestReadAPIKeyReadsAKeyWithNullMembers(t *testing.T) {
	answer := createdAPIKeyAnswer("key-1", "")
	delete(answer, "raw_token")
	answer["revoked_at"], answer["expires_at"], answer["last_used_at"], answer["tenant_id"], answer["updated_at"] = nil, nil, nil, nil, nil
	server := &apiKeyServer{status: http.StatusOK, payload: answer}
	state, err := ReadAPIKey(context.Background(), nil, server.start(t), "session-access", "key-1")
	if err != nil || state != (APIKeyState{}) {
		t.Fatalf("read = %+v, %v; want an active key with no dates", state, err)
	}
}

func TestWithoutNullMembers(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"id":"key-1","revoked_at":null,"expires_at":"2026-10-02T12:10:00Z"}`, `{"expires_at":"2026-10-02T12:10:00Z","id":"key-1"}`},
		{`{"id":"key-1"}`, `{"id":"key-1"}`},
		{`{"nested":{"a":null}}`, `{"nested":{"a":null}}`},
		{`[null]`, `[null]`},
		{`not json`, `not json`},
	} {
		if got := string(withoutNullMembers([]byte(tc.body))); got != tc.want {
			t.Fatalf("withoutNullMembers(%s) = %s, want %s", tc.body, got, tc.want)
		}
	}
}

func TestReadAPIKeyRefusesAnAnswerOutsideTheContract(t *testing.T) {
	server := &apiKeyServer{status: http.StatusOK, payload: map[string]any{"id": "key-1"}}
	base := server.start(t)
	_, err := ReadAPIKey(context.Background(), nil, base, "session-access", "key-1")
	if ExitCode(err) != ExitAPI || !strings.Contains(err.Error(), "invalid JSON response from "+base+"/apikeys/key-1") {
		t.Fatalf("read = %v, want an invalid response", err)
	}
}

func TestRevokeAPIKeyRevokesWithTheUserBearer(t *testing.T) {
	server := &apiKeyServer{status: http.StatusOK, payload: map[string]any{"revoked": true}}
	if err := RevokeAPIKey(context.Background(), nil, server.start(t), "session-access", "key-1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if server.method != http.MethodDelete || server.path != "/apikeys/key-1" || server.authorization != "Bearer session-access" {
		t.Fatalf("request = %s %s with %q", server.method, server.path, server.authorization)
	}
}

func TestRevokeAPIKeyToleratesAKeyAlreadyGone(t *testing.T) {
	server := &apiKeyServer{status: http.StatusNotFound, payload: map[string]any{"error": "not_found", "error_description": "api key not found"}}
	if err := RevokeAPIKey(context.Background(), nil, server.start(t), "session-access", "key-1"); err != nil {
		t.Fatalf("revoke = %v, want a key already gone to count as revoked", err)
	}
}

func TestRevokeAPIKeyMapsAnExpiredSessionToExitAuth(t *testing.T) {
	server := &apiKeyServer{status: http.StatusUnauthorized, payload: map[string]any{"error": "invalid_token"}}
	err := RevokeAPIKey(context.Background(), nil, server.start(t), "session-access", "key-1")
	if ExitCode(err) != ExitAuth {
		t.Fatalf("revoke = %v (exit %d), want ExitAuth", err, ExitCode(err))
	}
}

func TestRevokeAPIKeySurfacesTheProviderMessage(t *testing.T) {
	server := &apiKeyServer{status: http.StatusForbidden, payload: map[string]any{"error": "forbidden", "message": "cannot revoke api key owned by another principal"}}
	err := RevokeAPIKey(context.Background(), nil, server.start(t), "session-access", "key-1")
	if ExitCode(err) != ExitAPI || !strings.Contains(err.Error(), "cannot revoke api key owned by another principal") {
		t.Fatalf("revoke = %v (exit %d), want the provider's message", err, ExitCode(err))
	}
}
