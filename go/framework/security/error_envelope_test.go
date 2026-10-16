package security

import (
	"encoding/json"
	"net/http"
	"testing"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

// TestSecurityDenialAnswersTheCanonicalEnvelope sends requests through a real
// server whose routes the security gate guards, and reads each refusal as the
// canonical error envelope.
func TestSecurityDenialAnswersTheCanonicalEnvelope(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "authorization-decision", "a-denial-answers-the-canonical-envelope")

	server := phttp.NewServerPlugin(phttp.ServerConfig{Port: 0})
	// A request that carries "Authorization: Bearer viewer" is authenticated
	// with no role; any other request carries no identity.
	server.Use(func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
		if ctx.Header("Authorization") == "Bearer viewer" {
			ctx.User = &phttp.Claims{Subject: "viewer"}
		}
		return next()
	})
	server.Use(Middleware(Options{Roles: []string{"admin"}}))
	server.GET("/admin", func(*phttp.Context) *phttp.Response { return phttp.JSON("ok") })
	ts := server.TestServer()
	defer ts.Close()

	cases := []struct {
		name          string
		authorization string
		status        int
		code          string
		message       string
	}{
		{"no token", "", http.StatusUnauthorized, "unauthorized", "Authentication required"},
		{"missing role", "Bearer viewer", http.StatusForbidden, "forbidden", "Insufficient permissions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/admin", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.authorization != "" {
				req.Header.Set("Authorization", tc.authorization)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			want := map[string]any{
				"code":    tc.code,
				"error":   http.StatusText(tc.status),
				"message": tc.message,
			}
			for key, value := range want {
				if body[key] != value {
					t.Errorf("body[%q] = %v, want %v (body %v)", key, body[key], value, body)
				}
			}
			if len(body) != len(want) {
				t.Errorf("body has %d fields, want %d: %v", len(body), len(want), body)
			}
		})
	}
}
