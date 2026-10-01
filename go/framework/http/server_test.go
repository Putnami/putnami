package http

import (
	"net/http/httptest"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestNewServerPlugin(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{Port: 9090})
	if sp.Name() != "http" {
		t.Errorf("Name() = %q, want %q", sp.Name(), "http")
	}
	if sp.config.Port != 9090 {
		t.Errorf("Port = %d, want 9090", sp.config.Port)
	}
}

func TestServerPlugin_RouteRegistration(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	sp.GET("/users", func(_ *Context) *Response { return JSON("get") })
	sp.POST("/users", func(_ *Context) *Response { return JSON("post") })
	sp.PUT("/users/{id}", func(_ *Context) *Response { return JSON("put") })
	sp.DELETE("/users/{id}", func(_ *Context) *Response { return JSON("delete") })
	sp.PATCH("/users/{id}", func(_ *Context) *Response { return JSON("patch") })

	if sp.routes.lookup("GET", "/users") == nil {
		t.Error("expected GET /users match")
	}
	if sp.routes.lookup("POST", "/users") == nil {
		t.Error("expected POST /users match")
	}
	if sp.routes.lookup("PUT", "/users/1") == nil {
		t.Error("expected PUT /users/1 match")
	}
	if sp.routes.lookup("DELETE", "/users/1") == nil {
		t.Error("expected DELETE /users/1 match")
	}
	if sp.routes.lookup("PATCH", "/users/1") == nil {
		t.Error("expected PATCH /users/1 match")
	}
}

func TestServerPlugin_Use(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	called := false
	sp.Use(func(_ *Context, next func() *Response) *Response {
		called = true
		return next()
	})
	if len(sp.middlewares) != 1 {
		t.Errorf("expected 1 middleware, got %d", len(sp.middlewares))
	}
	_ = called // just verifying registration
}

func TestServerPlugin_RouteChaining(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	result := sp.
		GET("/a", func(_ *Context) *Response { return nil }).
		POST("/b", func(_ *Context) *Response { return nil }).
		PUT("/c", func(_ *Context) *Response { return nil }).
		DELETE("/d", func(_ *Context) *Response { return nil }).
		PATCH("/e", func(_ *Context) *Response { return nil })

	if result != sp {
		t.Error("expected chaining to return same ServerPlugin")
	}
}

// --- OPTIONS preflight reaches the CORS middleware via the router ---

func TestServerPlugin_CORSPreflightThroughRouter(t *testing.T) {
	spectest.Proves(t, "go/http-services", "method-fallbacks", "options-preflight-passes-through-the-middleware-chain")
	sp := NewServerPlugin(ServerConfig{})
	sp.GET("/api/data", func(_ *Context) *Response { return JSON("ok") })
	sp.POST("/api/data", func(_ *Context) *Response { return JSON("ok") })
	sp.Use(CORS(CORSOptions{AllowOrigins: []string{"https://app.example.com"}}))
	sp.wrapRoutesOnce()
	handler := sp.buildHandler()

	// Browser preflight: OPTIONS with Origin, but no OPTIONS route registered.
	req := httptest.NewRequest("OPTIONS", "/api/data", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != 204 {
		t.Errorf("preflight status = %d, want 204 (CORS middleware must answer)", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request origin", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("expected Access-Control-Allow-Methods on the preflight response")
	}
}

func TestServerPlugin_OptionsFallbackAndUnknownPath(t *testing.T) {
	spectest.Proves(t, "go/http-services", "method-fallbacks", "options-fallback-answers-a-known-path")
	sp := NewServerPlugin(ServerConfig{})
	sp.GET("/api/data", func(_ *Context) *Response { return JSON("ok") })
	sp.wrapRoutesOnce()
	handler := sp.buildHandler()

	// Known path, no CORS, no explicit OPTIONS handler: default 204 + Allow.
	req := httptest.NewRequest("OPTIONS", "/api/data", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != 204 {
		t.Errorf("OPTIONS status = %d, want 204", w.Code)
	}
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "OPTIONS") {
		t.Errorf("Allow = %q, want it to list GET and OPTIONS", allow)
	}

	// Unknown path still 404s under OPTIONS (synthesis only for known paths).
	req2 := httptest.NewRequest("OPTIONS", "/nope", nil)
	w2 := httptest.NewRecorder()
	handler(w2, req2)
	if w2.Code != 404 {
		t.Errorf("OPTIONS to unknown path status = %d, want 404", w2.Code)
	}
}

// --- WithAccept route option tests ---

func TestWithAccept_MatchingContentType(t *testing.T) {
	handler := func(_ *Context) *Response { return JSON("ok") }
	wrapped := applyRouteOptions(handler, routeConfig{accept: []string{"application/json"}})

	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp := wrapped(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200 for matching content type, got %d", resp.Status)
	}
}

func TestWithAccept_NonMatchingContentType(t *testing.T) {
	handler := func(_ *Context) *Response { return JSON("ok") }
	wrapped := applyRouteOptions(handler, routeConfig{accept: []string{"application/json"}})

	req := httptest.NewRequest("POST", "/", strings.NewReader("<xml/>"))
	req.Header.Set("Content-Type", "application/xml")
	resp := wrapped(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 415 {
		t.Errorf("expected 415 for non-matching content type, got %d", resp.Status)
	}
	if resp.Headers.Get("Accept") != "application/json" {
		t.Errorf("expected Accept header, got %q", resp.Headers.Get("Accept"))
	}
}

func TestWithAccept_NoContentType(t *testing.T) {
	handler := func(_ *Context) *Response { return JSON("ok") }
	wrapped := applyRouteOptions(handler, routeConfig{accept: []string{"application/json"}})

	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	// No Content-Type header
	resp := wrapped(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200 when no Content-Type header, got %d", resp.Status)
	}
}

func TestWithAccept_MultipleTypes(t *testing.T) {
	handler := func(_ *Context) *Response { return JSON("ok") }
	wrapped := applyRouteOptions(handler, routeConfig{accept: []string{"application/json", "text/plain"}})

	req := httptest.NewRequest("POST", "/", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	resp := wrapped(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200 for text/plain (one of accepted types), got %d", resp.Status)
	}
}

// --- WithStatusCode route option tests ---

func TestWithStatusCode(t *testing.T) {
	handler := func(_ *Context) *Response { return JSON(map[string]string{"id": "1"}) }
	wrapped := applyRouteOptions(handler, routeConfig{statusCode: 201})

	req := httptest.NewRequest("POST", "/", nil)
	resp := wrapped(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 201 {
		t.Errorf("expected 201, got %d", resp.Status)
	}
}

func TestWithStatusCode_NoOverrideNon200(t *testing.T) {
	handler := func(_ *Context) *Response { return JSONStatus(400, map[string]string{"error": "bad"}) }
	wrapped := applyRouteOptions(handler, routeConfig{statusCode: 201})

	req := httptest.NewRequest("POST", "/", nil)
	resp := wrapped(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 400 {
		t.Errorf("expected 400 (not overridden), got %d", resp.Status)
	}
}

func TestApplyRouteOptions_NoOptions(t *testing.T) {
	handler := func(_ *Context) *Response { return JSON("ok") }
	wrapped := applyRouteOptions(handler, routeConfig{})

	// Should return the same handler (no wrapping)
	req := httptest.NewRequest("GET", "/", nil)
	resp := wrapped(NewContext(httptest.NewRecorder(), req))
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

// --- matchesAccept tests ---

func TestMatchesAccept(t *testing.T) {
	tests := []struct {
		contentType string
		accepted    []string
		want        bool
	}{
		{"application/json", []string{"application/json"}, true},
		{"text/html", []string{"application/json"}, false},
		{"application/json; charset=utf-8", []string{"application/json"}, true},
		{"APPLICATION/JSON", []string{"application/json"}, true},
		{"text/plain", []string{"application/json", "text/plain"}, true},
		{"", []string{"application/json"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.contentType, func(t *testing.T) {
			got := matchesAccept(tt.contentType, tt.accepted)
			if got != tt.want {
				t.Errorf("matchesAccept(%q, %v) = %v, want %v", tt.contentType, tt.accepted, got, tt.want)
			}
		})
	}
}

// --- RouteOption function tests ---

func TestRouteOptionFunctions(t *testing.T) {
	t.Run("WithAccept", func(t *testing.T) {
		cfg := routeConfig{}
		WithAccept("application/json", "text/plain")(&cfg)
		if len(cfg.accept) != 2 {
			t.Errorf("accept count = %d, want 2", len(cfg.accept))
		}
	})

	t.Run("WithStatusCode", func(t *testing.T) {
		cfg := routeConfig{}
		WithStatusCode(201)(&cfg)
		if cfg.statusCode != 201 {
			t.Errorf("statusCode = %d, want 201", cfg.statusCode)
		}
	})

	t.Run("WithMaxBodySize", func(t *testing.T) {
		cfg := routeConfig{}
		WithMaxBodySize(8192)(&cfg)
		if cfg.maxBodySize != 8192 {
			t.Errorf("maxBodySize = %d, want 8192", cfg.maxBodySize)
		}
	})
}

// --- RouteController edge cases ---

func TestRouteController_NoMatch(t *testing.T) {
	rc := newRouteController()
	if rc.lookup("GET", "/anything") != nil {
		t.Error("expected nil for unregistered method")
	}
}

func TestRouteController_MultipleHandlersPerMethod(t *testing.T) {
	rc := newRouteController()
	rc.Add("GET", "/a", func(_ *Context) *Response { return Text("a") })
	rc.Add("GET", "/b", func(_ *Context) *Response { return Text("b") })

	if rc.lookup("GET", "/a") == nil {
		t.Error("expected match for /a")
	}
	if rc.lookup("GET", "/b") == nil {
		t.Error("expected match for /b")
	}
}
