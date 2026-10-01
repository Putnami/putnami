package http

import (
	"testing"
)

func TestClaimsFromMap_StandardClaims(t *testing.T) {
	m := map[string]any{
		"sub":       "user-123",
		"iss":       "https://auth.example.com",
		"client_id": "web-app",
		"roles":     []any{"admin", "editor"},
		"scope":     []any{"read", "write"},
		"custom":    "value",
	}

	c := ClaimsFromMap(m)

	if c.Subject != "user-123" {
		t.Errorf("Subject = %q, want %q", c.Subject, "user-123")
	}
	if c.Issuer != "https://auth.example.com" {
		t.Errorf("Issuer = %q, want %q", c.Issuer, "https://auth.example.com")
	}
	if c.ClientID != "web-app" {
		t.Errorf("ClientID = %q, want %q", c.ClientID, "web-app")
	}
	if len(c.Roles) != 2 || c.Roles[0] != "admin" || c.Roles[1] != "editor" {
		t.Errorf("Roles = %v, want [admin editor]", c.Roles)
	}
	if len(c.Scopes) != 2 || c.Scopes[0] != "read" || c.Scopes[1] != "write" {
		t.Errorf("Scopes = %v, want [read write]", c.Scopes)
	}
	if c.Extra["custom"] != "value" {
		t.Errorf("Extra[custom] = %v, want %q", c.Extra["custom"], "value")
	}
}

func TestClaimsFromMap_EmptyMap(t *testing.T) {
	c := ClaimsFromMap(map[string]any{})

	if c.Subject != "" {
		t.Errorf("Subject = %q, want empty", c.Subject)
	}
	if c.Extra == nil {
		t.Error("Extra should be initialized")
	}
}

func TestClaimsFromMap_RolesAsStringSlice(t *testing.T) {
	c := ClaimsFromMap(map[string]any{
		"roles": []string{"admin"},
	})
	if len(c.Roles) != 1 || c.Roles[0] != "admin" {
		t.Errorf("Roles = %v, want [admin]", c.Roles)
	}
}

func TestClaimsFromMap_ScopeAsSpaceSeparatedString(t *testing.T) {
	c := ClaimsFromMap(map[string]any{
		"scope": "read write delete",
	})
	if len(c.Scopes) != 3 {
		t.Errorf("Scopes = %v, want [read write delete]", c.Scopes)
	}
}

func TestClaimsFromMap_ScopeEmptyString(t *testing.T) {
	c := ClaimsFromMap(map[string]any{
		"scope": "",
	})
	if c.Scopes != nil {
		t.Errorf("Scopes = %v, want nil", c.Scopes)
	}
}

func TestClaimsFromMap_NonStringSubIgnored(t *testing.T) {
	c := ClaimsFromMap(map[string]any{
		"sub": 123,
	})
	if c.Subject != "" {
		t.Errorf("Subject = %q, want empty for non-string value", c.Subject)
	}
}

func TestClaims_HasRole(t *testing.T) {
	c := &Claims{Roles: []string{"admin", "editor"}}

	if !c.HasRole("admin") {
		t.Error("HasRole(admin) = false, want true")
	}
	if !c.HasRole("editor") {
		t.Error("HasRole(editor) = false, want true")
	}
	if c.HasRole("viewer") {
		t.Error("HasRole(viewer) = true, want false")
	}
}

func TestClaims_HasRole_Empty(t *testing.T) {
	c := &Claims{}
	if c.HasRole("admin") {
		t.Error("HasRole on empty roles = true, want false")
	}
}

func TestClaims_HasScope(t *testing.T) {
	c := &Claims{Scopes: []string{"read", "write"}}

	if !c.HasScope("read") {
		t.Error("HasScope(read) = false, want true")
	}
	if c.HasScope("delete") {
		t.Error("HasScope(delete) = true, want false")
	}
}

func TestClaims_Get_TypedFields(t *testing.T) {
	c := &Claims{
		Subject:  "user-1",
		Issuer:   "auth.example.com",
		ClientID: "app-1",
		Roles:    []string{"admin"},
		Scopes:   []string{"read"},
		Extra:    map[string]any{"tenant": "acme"},
	}

	if c.Get("sub") != "user-1" {
		t.Errorf("Get(sub) = %v, want user-1", c.Get("sub"))
	}
	if c.Get("iss") != "auth.example.com" {
		t.Errorf("Get(iss) = %v, want auth.example.com", c.Get("iss"))
	}
	if c.Get("client_id") != "app-1" {
		t.Errorf("Get(client_id) = %v, want app-1", c.Get("client_id"))
	}
	if c.Get("tenant") != "acme" {
		t.Errorf("Get(tenant) = %v, want acme", c.Get("tenant"))
	}
}

func TestClaims_Get_MissingExtraKey(t *testing.T) {
	c := &Claims{Extra: map[string]any{}}
	if c.Get("nonexistent") != nil {
		t.Errorf("Get(nonexistent) = %v, want nil", c.Get("nonexistent"))
	}
}
