package security

import (
	"net/http/httptest"
	"testing"

	phttp "go.putnami.dev/http"
)

func TestAPIKey_ValidKey(t *testing.T) {
	mw := APIKey(APIKeyConfig{Keys: []string{"key-1", "key-2"}})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("X-Api-Key", "key-1")
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set")
	}
	if ctx.User.Subject != "apikey" {
		t.Errorf("subject = %q, want apikey", ctx.User.Subject)
	}
}

func TestAPIKey_InvalidKey(t *testing.T) {
	mw := APIKey(APIKeyConfig{Keys: []string{"key-1"}})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	req.Header.Set("X-Api-Key", "wrong-key")
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil for invalid API key")
	}
}

func TestAPIKey_NoHeader(t *testing.T) {
	mw := APIKey(APIKeyConfig{Keys: []string{"key-1"}})

	req := httptest.NewRequest("POST", "/v1/traces", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User != nil {
		t.Error("expected user to be nil when no header")
	}
}

func TestAPIKey_CustomHeader(t *testing.T) {
	mw := APIKey(APIKeyConfig{
		Keys:   []string{"secret"},
		Header: "X-Custom-Key",
	})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Custom-Key", "secret")
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set with custom header")
	}
}

func TestAPIKey_CustomSubject(t *testing.T) {
	mw := APIKey(APIKeyConfig{
		Keys:    []string{"key-1"},
		Subject: "service-account",
	})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Api-Key", "key-1")
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set")
	}
	if ctx.User.Subject != "service-account" {
		t.Errorf("subject = %q, want service-account", ctx.User.Subject)
	}
}

func TestAPIKey_Scopes(t *testing.T) {
	mw := APIKey(APIKeyConfig{
		Keys:   []string{"key-1"},
		Scopes: []string{"ingest", "read"},
	})

	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Api-Key", "key-1")
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })

	if ctx.User == nil {
		t.Fatal("expected user to be set")
	}
	if len(ctx.User.Scopes) != 2 || ctx.User.Scopes[0] != "ingest" {
		t.Errorf("scopes = %v, want [ingest read]", ctx.User.Scopes)
	}
}
