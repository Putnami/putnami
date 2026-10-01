package http

import (
	"net/http/httptest"
	"testing"
)

// --- CORS Middleware ---

func TestCORS_Defaults(t *testing.T) {
	mw := CORS(CORSOptions{})
	req := httptest.NewRequest("GET", "/api", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	resp := mw(ctx, func() *Response { return JSON("ok") })
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Errorf("expected origin header, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCORS_NoOriginHeader(t *testing.T) {
	mw := CORS(CORSOptions{})
	req := httptest.NewRequest("GET", "/api", nil)
	// No Origin header set.
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	nextCalled := false
	mw(ctx, func() *Response {
		nextCalled = true
		return JSON("ok")
	})
	if !nextCalled {
		t.Error("expected next() to be called when no Origin header")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("should not set CORS headers without Origin")
	}
}

func TestCORS_DisallowedOrigin(t *testing.T) {
	mw := CORS(CORSOptions{AllowOrigins: []string{"https://allowed.com"}})
	req := httptest.NewRequest("GET", "/api", nil)
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	nextCalled := false
	mw(ctx, func() *Response {
		nextCalled = true
		return JSON("ok")
	})
	if !nextCalled {
		t.Error("expected next() to be called for disallowed origin")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("should not set CORS headers for disallowed origin")
	}
}

func TestCORS_SpecificOrigin(t *testing.T) {
	mw := CORS(CORSOptions{AllowOrigins: []string{"https://trusted.com"}})
	req := httptest.NewRequest("GET", "/api", nil)
	req.Header.Set("Origin", "https://trusted.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return JSON("ok") })
	if w.Header().Get("Access-Control-Allow-Origin") != "https://trusted.com" {
		t.Errorf("expected trusted origin, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCORS_Preflight(t *testing.T) {
	mw := CORS(CORSOptions{})
	req := httptest.NewRequest("OPTIONS", "/api", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	resp := mw(ctx, func() *Response {
		t.Error("next() should not be called for preflight")
		return nil
	})
	if resp.Status != 204 {
		t.Errorf("expected 204 for preflight, got %d", resp.Status)
	}
	if w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("expected Allow-Methods header on preflight")
	}
	if w.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Error("expected Allow-Headers header on preflight")
	}
	if w.Header().Get("Access-Control-Max-Age") != "86400" {
		t.Errorf("expected default MaxAge 86400, got %q", w.Header().Get("Access-Control-Max-Age"))
	}
}

func TestCORS_VaryOrigin(t *testing.T) {
	// A reflected Access-Control-Allow-Origin must be accompanied by Vary: Origin
	// so a shared cache does not replay one origin's ACAO to another. Assert it
	// on both the normal and preflight paths.
	mw := CORS(CORSOptions{AllowOrigins: []string{"https://a.example", "https://b.example"}})

	for _, method := range []string{"GET", "OPTIONS"} {
		req := httptest.NewRequest(method, "/api", nil)
		req.Header.Set("Origin", "https://a.example")
		w := httptest.NewRecorder()
		mw(NewContext(w, req), func() *Response { return JSON("ok") })

		vary := w.Header().Values("Vary")
		found := false
		for _, v := range vary {
			if v == "Origin" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected Vary: Origin when reflecting an origin, got %v", method, vary)
		}
	}
}

func TestCORS_Credentials(t *testing.T) {
	// Credentials with an explicit, non-wildcard origin allowlist is the
	// supported (and only safe) configuration.
	mw := CORS(CORSOptions{AllowOrigins: []string{"https://example.com"}, AllowCredentials: true})
	req := httptest.NewRequest("GET", "/api", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return JSON("ok") })
	if w.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Errorf("expected reflected origin, got %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
	if w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("expected Allow-Credentials: true for explicit origin")
	}
}

// TestCORS_CredentialsWildcardRejected verifies the secure-by-default behavior:
// when credentials are enabled but origins default to (or contain) the "*"
// wildcard, the middleware must NOT emit Access-Control-Allow-Credentials. This
// prevents the "reflect arbitrary Origin + allow credentials" misconfiguration.
func TestCORS_CredentialsWildcardRejected(t *testing.T) {
	mw := CORS(CORSOptions{AllowCredentials: true}) // wildcard default
	req := httptest.NewRequest("GET", "/api", nil)
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return JSON("ok") })
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("expected no Allow-Credentials header under wildcard origin, got %q", got)
	}
}

// TestCORS_CredentialsExplicitWildcardRejected covers an explicit "*" entry
// (not just the default) combined with credentials.
func TestCORS_CredentialsExplicitWildcardRejected(t *testing.T) {
	mw := CORS(CORSOptions{AllowOrigins: []string{"*"}, AllowCredentials: true})
	req := httptest.NewRequest("GET", "/api", nil)
	req.Header.Set("Origin", "https://anything.example")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return JSON("ok") })
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("expected no Allow-Credentials header for explicit wildcard, got %q", got)
	}
}

func TestCORS_CredentialsMixedWildcardAndExplicitOrigin(t *testing.T) {
	mw := CORS(CORSOptions{
		AllowOrigins:     []string{"*", "https://trusted.example"},
		AllowCredentials: true,
	})

	trustedReq := httptest.NewRequest("GET", "/api", nil)
	trustedReq.Header.Set("Origin", "https://trusted.example")
	trustedW := httptest.NewRecorder()
	mw(NewContext(trustedW, trustedReq), func() *Response { return JSON("ok") })
	if got := trustedW.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("trusted explicit origin credentials = %q, want true", got)
	}

	otherReq := httptest.NewRequest("GET", "/api", nil)
	otherReq.Header.Set("Origin", "https://other.example")
	otherW := httptest.NewRecorder()
	mw(NewContext(otherW, otherReq), func() *Response { return JSON("ok") })
	if got := otherW.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("wildcard-only match credentials = %q, want empty", got)
	}
}

func TestCORS_ExposeHeaders(t *testing.T) {
	mw := CORS(CORSOptions{ExposeHeaders: []string{"X-Request-ID", "X-Total-Count"}})
	req := httptest.NewRequest("GET", "/api", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return JSON("ok") })
	expose := w.Header().Get("Access-Control-Expose-Headers")
	if expose != "X-Request-ID, X-Total-Count" {
		t.Errorf("expected expose headers, got %q", expose)
	}
}

func TestCORS_CustomMethods(t *testing.T) {
	mw := CORS(CORSOptions{AllowMethods: []string{"GET", "POST"}})
	req := httptest.NewRequest("OPTIONS", "/api", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return nil })
	methods := w.Header().Get("Access-Control-Allow-Methods")
	if methods != "GET, POST" {
		t.Errorf("expected 'GET, POST', got %q", methods)
	}
}

func TestCORS_CustomMaxAge(t *testing.T) {
	mw := CORS(CORSOptions{MaxAge: 3600})
	req := httptest.NewRequest("OPTIONS", "/api", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	mw(ctx, func() *Response { return nil })
	if w.Header().Get("Access-Control-Max-Age") != "3600" {
		t.Errorf("expected MaxAge 3600, got %q", w.Header().Get("Access-Control-Max-Age"))
	}
}

// --- SecurityHeaders Middleware ---

func TestSecurityHeaders(t *testing.T) {
	mw := SecurityHeaders()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	nextCalled := false
	mw(ctx, func() *Response {
		nextCalled = true
		return JSON("ok")
	})

	if !nextCalled {
		t.Error("expected next() to be called")
	}

	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"X-XSS-Protection":       "0",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for header, expected := range checks {
		got := w.Header().Get(header)
		if got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
}

func TestSecurityHeaders_PresistThroughNext(t *testing.T) {
	mw := SecurityHeaders()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	ctx := NewContext(w, req)

	// Headers should be set before next() is called.
	mw(ctx, func() *Response {
		if w.Header().Get("X-Frame-Options") != "DENY" {
			t.Error("headers should be set before next()")
		}
		return JSON("ok")
	})
}
