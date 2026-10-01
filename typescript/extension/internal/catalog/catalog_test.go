package catalog

import (
	"encoding/json"
	"testing"
)

func mustManifest(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad manifest: %v", err)
	}
	return m
}

func TestParse_TopLevelDefault(t *testing.T) {
	c := Parse(mustManifest(t, `{"catalog":{"@putnami/web":"1.0.0"}}`))
	if !c.Present() {
		t.Fatal("expected catalog present")
	}
	if !c.DefaultAtTop {
		t.Error("expected top-level default catalog")
	}
	if v, ok := c.LookupIn("", "@putnami/web"); !ok || v != "1.0.0" {
		t.Errorf("LookupIn default = %q, %v; want 1.0.0, true", v, ok)
	}
}

func TestParse_WorkspacesNested(t *testing.T) {
	c := Parse(mustManifest(t, `{"workspaces":{"packages":["a"],"catalog":{"@putnami/runtime":"0.1.0-abc"}}}`))
	if c.DefaultAtTop {
		t.Error("expected nested (workspaces.catalog) default catalog")
	}
	if c.WsObj == nil {
		t.Error("expected workspaces object captured")
	}
	if v, ok := c.LookupIn("", "@putnami/runtime"); !ok || v != "0.1.0-abc" {
		t.Errorf("LookupIn = %q, %v; want 0.1.0-abc, true", v, ok)
	}
}

func TestParse_NamedCatalog(t *testing.T) {
	c := Parse(mustManifest(t, `{"catalogs":{"framework":{"@putnami/web":"1.0.0"}}}`))
	if !c.Present() {
		t.Fatal("expected catalog present via named catalogs")
	}
	if v, ok := c.LookupIn("framework", "@putnami/web"); !ok || v != "1.0.0" {
		t.Errorf("LookupIn named = %q, %v; want 1.0.0, true", v, ok)
	}
	if v, ok := c.Lookup("@putnami/web"); !ok || v != "1.0.0" {
		t.Errorf("Lookup any = %q, %v; want 1.0.0, true", v, ok)
	}
}

func TestParse_ArrayWorkspacesNoCatalog(t *testing.T) {
	c := Parse(mustManifest(t, `{"workspaces":["a","b"]}`))
	if c.Present() {
		t.Error("array workspaces without a catalog must not be present")
	}
	if c.WsObj != nil {
		t.Error("array-form workspaces must not populate WsObj")
	}
}

func TestLookupIn_Missing(t *testing.T) {
	c := Parse(mustManifest(t, `{"catalog":{"@putnami/web":"1.0.0"}}`))
	if _, ok := c.LookupIn("", "@putnami/runtime"); ok {
		t.Error("missing default-catalog entry must report not found")
	}
	if _, ok := c.LookupIn("nope", "@putnami/web"); ok {
		t.Error("lookup in a non-existent named catalog must report not found")
	}
	empty := Parse(mustManifest(t, `{"private":true}`))
	if _, ok := empty.LookupIn("", "@putnami/web"); ok {
		t.Error("lookup against an empty catalog must report not found")
	}
	if empty.Present() {
		t.Error("a manifest without catalogs must not be present")
	}
}

func TestNameFromSpec(t *testing.T) {
	cases := []struct {
		spec    string
		name    string
		catalog bool
	}{
		{"catalog:", "", true},
		{"catalog:framework", "framework", true},
		{"workspace:*", "", false},
		{"^1.2.3", "", false},
		{"0.1.0-abc", "", false},
	}
	for _, tc := range cases {
		name, ok := NameFromSpec(tc.spec)
		if ok != tc.catalog || name != tc.name {
			t.Errorf("NameFromSpec(%q) = (%q, %v); want (%q, %v)", tc.spec, name, ok, tc.name, tc.catalog)
		}
	}
}
