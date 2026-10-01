package security

import (
	"net/http/httptest"
	"testing"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

func newCtx() *phttp.Context {
	req := httptest.NewRequest("GET", "/", nil)
	return phttp.NewContext(httptest.NewRecorder(), req)
}

func TestSecurityMiddlewareNoUser(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "authorization-decision", "missing-identity-returns-401")
	mw := Middleware(Options{Roles: []string{"admin"}})
	ctx := newCtx()

	resp := mw(ctx, func() *phttp.Response {
		return phttp.JSON("ok")
	})
	if resp.Status != 401 {
		t.Errorf("expected 401, got %d", resp.Status)
	}
}

func TestSecurityMiddlewareAllRoles(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "authorization-decision", "role-denial-returns-403")
	mw := Middleware(Options{Roles: []string{"admin", "editor"}})

	// User with both roles
	ctx := newCtx()
	ctx.User = &phttp.Claims{Roles: []string{"admin", "editor", "viewer"}}
	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 200 {
		t.Errorf("expected 200 with all roles, got %d", resp.Status)
	}

	// User missing one role
	ctx = newCtx()
	ctx.User = &phttp.Claims{Roles: []string{"admin"}}
	resp = mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 403 {
		t.Errorf("expected 403 missing role, got %d", resp.Status)
	}
}

func TestSecurityMiddlewareAnyRole(t *testing.T) {
	mw := Middleware(Options{RolesAny: []string{"admin", "editor"}})

	// User with one matching role
	ctx := newCtx()
	ctx.User = &phttp.Claims{Roles: []string{"editor"}}
	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 200 {
		t.Errorf("expected 200 with any role, got %d", resp.Status)
	}

	// User with no matching role
	ctx = newCtx()
	ctx.User = &phttp.Claims{Roles: []string{"viewer"}}
	resp = mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 403 {
		t.Errorf("expected 403 no matching role, got %d", resp.Status)
	}
}

func TestSecurityMiddlewareScopes(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "authorization-decision", "scope-denial-returns-403")
	mw := Middleware(Options{Scopes: []string{"read", "write"}})

	ctx := newCtx()
	ctx.User = &phttp.Claims{Scopes: []string{"read", "write", "delete"}}
	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 200 {
		t.Errorf("expected 200 with scopes, got %d", resp.Status)
	}
}

func TestSecurityMiddlewareScopesAny(t *testing.T) {
	mw := Middleware(Options{ScopesAny: []string{"admin", "write"}})

	ctx := newCtx()
	ctx.User = &phttp.Claims{Scopes: []string{"read", "write"}}
	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}
}

func TestSecurityMiddlewareClient(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "authorization-decision", "client-denial-returns-403")
	mw := Middleware(Options{Client: []string{"my-app", "other-app"}})

	// Allowed client
	ctx := newCtx()
	ctx.User = &phttp.Claims{ClientID: "my-app"}
	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}

	// Disallowed client
	ctx = newCtx()
	ctx.User = &phttp.Claims{ClientID: "unknown"}
	resp = mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 403 {
		t.Errorf("expected 403, got %d", resp.Status)
	}
}

func TestSecurityGuard(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "authorization-decision", "guard-denial-returns-403")
	mw := Middleware(Guard(func(user *phttp.Claims, ctx *phttp.Context) bool {
		return user.Subject == "allowed-user"
	}))

	// Allowed
	ctx := newCtx()
	ctx.User = &phttp.Claims{Subject: "allowed-user"}
	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 200 {
		t.Errorf("expected 200, got %d", resp.Status)
	}

	// Denied
	ctx = newCtx()
	ctx.User = &phttp.Claims{Subject: "other-user"}
	resp = mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 403 {
		t.Errorf("expected 403, got %d", resp.Status)
	}
}

func TestIdentityResolver(t *testing.T) {
	resolver := IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		auth := ctx.Header("Authorization")
		if auth == "Bearer valid-token" {
			return &phttp.Claims{Subject: "user-1", Roles: []string{"admin"}}
		}
		return nil
	})

	// With valid token
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	ctx := phttp.NewContext(httptest.NewRecorder(), req)

	resolver(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if ctx.User == nil {
		t.Error("user should be set")
	}
	if ctx.User.Subject != "user-1" {
		t.Errorf("expected 'user-1', got %v", ctx.User.Subject)
	}

	// Without token
	ctx = newCtx()
	resolver(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if ctx.User != nil {
		t.Error("user should be nil without token")
	}
}

func TestClaimsFromMap(t *testing.T) {
	m := map[string]any{
		"sub":       "user-1",
		"iss":       "https://auth.example.com",
		"client_id": "my-app",
		"roles":     []string{"admin", "editor"},
		"scope":     "read write delete",
		"custom":    42,
	}
	c := phttp.ClaimsFromMap(m)
	if c.Subject != "user-1" {
		t.Errorf("expected Subject='user-1', got %q", c.Subject)
	}
	if c.Issuer != "https://auth.example.com" {
		t.Errorf("expected Issuer, got %q", c.Issuer)
	}
	if c.ClientID != "my-app" {
		t.Errorf("expected ClientID='my-app', got %q", c.ClientID)
	}
	if len(c.Roles) != 2 || c.Roles[0] != "admin" {
		t.Errorf("expected Roles=[admin, editor], got %v", c.Roles)
	}
	if len(c.Scopes) != 3 || c.Scopes[0] != "read" {
		t.Errorf("expected Scopes=[read, write, delete], got %v", c.Scopes)
	}
	if c.Extra["custom"] != 42 {
		t.Errorf("expected Extra[custom]=42, got %v", c.Extra["custom"])
	}
}

func TestClaimsGet(t *testing.T) {
	c := &phttp.Claims{
		Subject:  "user-1",
		ClientID: "app",
		Extra:    map[string]any{"org": "acme"},
	}
	if c.Get("sub") != "user-1" {
		t.Errorf("expected sub=user-1, got %v", c.Get("sub"))
	}
	if c.Get("client_id") != "app" {
		t.Errorf("expected client_id=app, got %v", c.Get("client_id"))
	}
	if c.Get("org") != "acme" {
		t.Errorf("expected org=acme, got %v", c.Get("org"))
	}
}

func TestSecurityMiddlewareExcludePaths(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "authorization-decision", "only-excluded-paths-bypass-the-rule")
	mw := Middleware(Options{
		Scopes:       []string{"admin"},
		ExcludePaths: []string{"/_/"},
	})

	// Excluded path — no user, should pass.
	req := httptest.NewRequest("GET", "/_/health", nil)
	ctx := phttp.NewContext(httptest.NewRecorder(), req)
	resp := mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 200 {
		t.Errorf("excluded path: expected 200, got %d", resp.Status)
	}

	// Non-excluded path, no user — 401.
	ctx = newCtx()
	resp = mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 401 {
		t.Errorf("non-excluded no user: expected 401, got %d", resp.Status)
	}

	// Non-excluded path, user without scope — 403.
	req = httptest.NewRequest("POST", "/v1/traces", nil)
	ctx = phttp.NewContext(httptest.NewRecorder(), req)
	ctx.User = &phttp.Claims{Scopes: []string{"read"}}
	resp = mw(ctx, func() *phttp.Response { return phttp.JSON("ok") })
	if resp.Status != 403 {
		t.Errorf("non-excluded missing scope: expected 403, got %d", resp.Status)
	}
}

func TestClaimsHasRoleAndScope(t *testing.T) {
	c := &phttp.Claims{
		Roles:  []string{"admin", "editor"},
		Scopes: []string{"read", "write"},
	}
	if !c.HasRole("admin") {
		t.Error("expected HasRole(admin)=true")
	}
	if c.HasRole("viewer") {
		t.Error("expected HasRole(viewer)=false")
	}
	if !c.HasScope("read") {
		t.Error("expected HasScope(read)=true")
	}
	if c.HasScope("delete") {
		t.Error("expected HasScope(delete)=false")
	}
}
