package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func mustManifest(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad manifest: %v", err)
	}
	return m
}

// ---- parseCatalogModel ----

func TestParseCatalogModel_TopLevel(t *testing.T) {
	m := parseCatalogModel(mustManifest(t, `{"catalog":{"@putnami/web":"1.0.0"}}`))
	if !m.present() {
		t.Fatal("expected catalog present")
	}
	if v, ok := m.lookup("@putnami/web"); !ok || v != "1.0.0" {
		t.Errorf("lookup = %q, %v; want 1.0.0, true", v, ok)
	}
	if !m.defaultAtTop {
		t.Error("expected top-level default catalog")
	}
}

func TestParseCatalogModel_WorkspacesNested(t *testing.T) {
	m := parseCatalogModel(mustManifest(t, `{"workspaces":{"packages":["a"],"catalog":{"@putnami/web":"1.0.0"}}}`))
	if !m.present() {
		t.Fatal("expected catalog present")
	}
	if m.defaultAtTop {
		t.Error("expected nested (workspaces.catalog) default catalog")
	}
	if m.wsObj == nil {
		t.Error("expected workspaces object to be captured for write-back")
	}
}

func TestParseCatalogModel_NamedCatalogs(t *testing.T) {
	m := parseCatalogModel(mustManifest(t, `{"catalogs":{"framework":{"@putnami/web":"1.0.0"}}}`))
	if !m.present() {
		t.Fatal("expected catalog present via named catalogs")
	}
	if v, ok := m.lookup("@putnami/web"); !ok || v != "1.0.0" {
		t.Errorf("lookup in named catalog = %q, %v", v, ok)
	}
}

func TestParseCatalogModel_ArrayWorkspacesNoCatalog(t *testing.T) {
	m := parseCatalogModel(mustManifest(t, `{"workspaces":["a","b"]}`))
	if m.present() {
		t.Error("array workspaces without a catalog must not be present")
	}
}

// ---- catalogModel mutation + writeBack ----

func TestCatalogModel_SetUpdatesNamedInPlace(t *testing.T) {
	pkg := mustManifest(t, `{"catalogs":{"framework":{"@putnami/web":"1.0.0"}}}`)
	m := parseCatalogModel(pkg)
	if !m.set("@putnami/web", "2.0.0") {
		t.Fatal("expected set to place the package")
	}
	if err := m.writeBack(pkg); err != nil {
		t.Fatalf("writeBack: %v", err)
	}
	var got struct {
		Catalogs map[string]map[string]string `json:"catalogs"`
	}
	json.Unmarshal(mustMarshal(t, pkg), &got)
	if got.Catalogs["framework"]["@putnami/web"] != "2.0.0" {
		t.Errorf("named catalog not updated in place: %v", got.Catalogs)
	}
}

func TestCatalogModel_AddToDefault_TopLevelForArrayForm(t *testing.T) {
	pkg := mustManifest(t, `{"workspaces":["a"]}`)
	m := parseCatalogModel(pkg)
	m.addToDefault("@putnami/web", "1.2.3")
	if !m.defaultAtTop {
		t.Error("a new default catalog for an array-form workspace should be top-level")
	}
	if err := m.writeBack(pkg); err != nil {
		t.Fatalf("writeBack: %v", err)
	}
	var got map[string]string
	json.Unmarshal(pkg["catalog"], &got)
	if got["@putnami/web"] != "1.2.3" {
		t.Errorf("writeBack catalog = %v", got)
	}
}

func TestCatalogModel_AddToDefault_NestedForObjectForm(t *testing.T) {
	pkg := mustManifest(t, `{"workspaces":{"packages":["a"]}}`)
	m := parseCatalogModel(pkg)
	m.addToDefault("@putnami/web", "1.2.3")
	if m.defaultAtTop {
		t.Error("a new default catalog for an object-form workspace should be nested")
	}
	if err := m.writeBack(pkg); err != nil {
		t.Fatalf("writeBack: %v", err)
	}
	if _, ok := pkg["catalog"]; ok {
		t.Error("object-form workspace must not get a top-level catalog")
	}
	var got struct {
		Catalog map[string]string `json:"catalog"`
	}
	json.Unmarshal(pkg["workspaces"], &got)
	if got.Catalog["@putnami/web"] != "1.2.3" {
		t.Errorf("expected workspaces.catalog seeded, got %v", got.Catalog)
	}
}

func TestCatalogModel_AddToCatalog_NestedForObjectForm(t *testing.T) {
	pkg := mustManifest(t, `{"workspaces":{"packages":["a"],"catalog":{"@putnami/web":"1.0.0"}}}`)
	m := parseCatalogModel(pkg)
	m.addToCatalog("framework", "@putnami/ui", "1.2.3")
	if m.namedAtTop {
		t.Error("a new named catalog for nested catalog workspaces should be nested")
	}
	if err := m.writeBack(pkg); err != nil {
		t.Fatalf("writeBack: %v", err)
	}
	if _, ok := pkg["catalogs"]; ok {
		t.Error("object-form workspace must not get a top-level catalogs key")
	}
	var got struct {
		Catalogs map[string]map[string]string `json:"catalogs"`
	}
	json.Unmarshal(pkg["workspaces"], &got)
	if got.Catalogs["framework"]["@putnami/ui"] != "1.2.3" {
		t.Errorf("expected workspaces.catalogs.framework seeded, got %v", got.Catalogs)
	}
}

// ---- stripPutnamiEntries ----

func TestStripPutnamiEntries_PreservesOthers(t *testing.T) {
	pkg := mustManifest(t, `{"dependencies":{"@putnami/web":"1.0.0","left":"^1.0.0"}}`)
	removed, err := stripPutnamiEntries(pkg, "dependencies", false, jsonl.New())
	if err != nil {
		t.Fatalf("stripPutnamiEntries: %v", err)
	}
	if !removed {
		t.Error("expected removal")
	}
	var deps map[string]string
	json.Unmarshal(pkg["dependencies"], &deps)
	if _, ok := deps["@putnami/web"]; ok {
		t.Error("@putnami/web should be removed")
	}
	if deps["left"] != "^1.0.0" {
		t.Errorf("non-Putnami dependency must be preserved, got %v", deps)
	}
}

func TestStripPutnamiEntries_DropsEmptiedKey(t *testing.T) {
	pkg := mustManifest(t, `{"overrides":{"@putnami/web":"1.0.0"}}`)
	removed, err := stripPutnamiEntries(pkg, "overrides", false, jsonl.New())
	if err != nil {
		t.Fatalf("stripPutnamiEntries: %v", err)
	}
	if !removed {
		t.Error("expected removal")
	}
	if _, ok := pkg["overrides"]; ok {
		t.Error("an emptied overrides key should be dropped entirely")
	}
}

func TestStripPutnamiEntries_PreservesRedirectOverride(t *testing.T) {
	pkg := mustManifest(t, `{"overrides":{"@putnami/client":"catalog:","@putnami/web":"1.0.0","@putnami/events":"catalog:framework"}}`)
	removed, err := stripPutnamiEntries(pkg, "overrides", false, jsonl.New())
	if err != nil {
		t.Fatalf("stripPutnamiEntries: %v", err)
	}
	if !removed {
		t.Error("expected the version-pinned @putnami/web override to be removed")
	}
	var overrides map[string]string
	json.Unmarshal(pkg["overrides"], &overrides)
	if overrides["@putnami/client"] != "catalog:" {
		t.Errorf("catalog: redirect override must be preserved, got %v", overrides)
	}
	if overrides["@putnami/events"] != "catalog:framework" {
		t.Errorf("named-catalog redirect override must be preserved, got %v", overrides)
	}
	if _, ok := overrides["@putnami/web"]; ok {
		t.Errorf("version-pinned override must still be stripped, got %v", overrides)
	}
}

func TestStripPutnamiEntries_RedirectOnlyOverridesKept(t *testing.T) {
	pkg := mustManifest(t, `{"overrides":{"@putnami/client":"catalog:"}}`)
	removed, err := stripPutnamiEntries(pkg, "overrides", false, jsonl.New())
	if err != nil {
		t.Fatalf("stripPutnamiEntries: %v", err)
	}
	if removed {
		t.Error("a redirect-only overrides block must be left untouched")
	}
	if _, ok := pkg["overrides"]; !ok {
		t.Error("redirect-only overrides key must not be dropped")
	}
}

func TestStripPutnamiEntries_DryRunDoesNotMutate(t *testing.T) {
	pkg := mustManifest(t, `{"dependencies":{"@putnami/web":"1.0.0"}}`)
	removed, _ := stripPutnamiEntries(pkg, "dependencies", true, jsonl.New())
	if !removed {
		t.Error("dry-run should still report a pending removal")
	}
	var deps map[string]string
	json.Unmarshal(pkg["dependencies"], &deps)
	if deps["@putnami/web"] != "1.0.0" {
		t.Error("dry-run must not mutate the manifest")
	}
}

// ---- collectCatalogPutnamiRefs ----

func TestCollectCatalogPutnamiRefs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"workspaces":["packages/web","packages/api"]}`), 0644)

	writeMember(t, dir, "packages/web", `{
		"name": "@app/web",
		"dependencies": {"@putnami/web": "catalog:", "@putnami/events": "catalog:framework", "react": "^19.2.3"},
		"devDependencies": {"@putnami/utils": "catalog:"}
	}`)
	writeMember(t, dir, "packages/api", `{
		"name": "@app/api",
		"dependencies": {"@putnami/application": "1.0.0"}
	}`)

	refs := collectCatalogPutnamiRefs(dir)
	want := map[string]bool{
		"\x00@putnami/web":             true,
		"\x00@putnami/utils":           true,
		"framework\x00@putnami/events": true,
	}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
	for _, r := range refs {
		if !want[r.key()] {
			t.Errorf("unexpected ref %q", r.label())
		}
	}
}

// ---- ensureWorkspaceCatalog ----

func TestEnsureWorkspaceCatalog_SeedsMissing(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "a-missing-catalog-entry-is-seeded")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"private":true,"workspaces":["packages/web"]}`), 0644)
	writeMember(t, dir, "packages/web", `{
		"name": "@app/web",
		"dependencies": {"@putnami/web": "catalog:", "@putnami/ui": "catalog:", "react": "^19.2.3"}
	}`)

	ctx := &pctx.Context{WorkspaceRoot: dir}
	if err := ensureWorkspaceCatalog(ctx, jsonl.New()); err != nil {
		t.Fatalf("ensureWorkspaceCatalog: %v", err)
	}

	catalog := readCatalog(t, dir)
	if catalog["@putnami/web"] != "latest" || catalog["@putnami/ui"] != "latest" {
		t.Errorf("expected catalog: @putnami refs seeded to latest, got %v", catalog)
	}
	if _, ok := catalog["react"]; ok {
		t.Errorf("react (direct range, not catalog:) must not be seeded, got %v", catalog)
	}
}

// TestEnsureWorkspaceCatalog_NewTopLevelKeyAppended is the other half of the
// Key-order fix: a top-level key the tooling creates for the first time (here,
// seeding "catalog" into a manifest that had none) must be appended after the
// manifest's existing keys — not inserted alphabetically — and every existing
// key must keep its original relative order.
func TestEnsureWorkspaceCatalog_NewTopLevelKeyAppended(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "the-root-manifest-top-level-key-order-is-preserved")
	dir := t.TempDir()
	original := `{"name":"@app/root","private":true,"workspaces":["packages/web"]}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(original), 0644)
	writeMember(t, dir, "packages/web", `{
		"name": "@app/web",
		"dependencies": {"@putnami/web": "catalog:"}
	}`)

	ctx := &pctx.Context{WorkspaceRoot: dir}
	if err := ensureWorkspaceCatalog(ctx, jsonl.New()); err != nil {
		t.Fatalf("ensureWorkspaceCatalog: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	gotOrder, err := topLevelKeyOrder(data)
	if err != nil {
		t.Fatalf("topLevelKeyOrder: %v", err)
	}
	wantOrder := []string{"name", "private", "workspaces", "catalog"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("top-level key order = %v, want %v (existing keys unchanged, new key appended)", gotOrder, wantOrder)
	}
}

func TestEnsureWorkspaceCatalog_PreservesExisting(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "an-existing-entry-is-left-alone")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"workspaces": ["packages/web"],
		"catalog": {"@putnami/web": "1.2.3", "react": "^19.2.3"}
	}`), 0644)
	writeMember(t, dir, "packages/web", `{
		"name": "@app/web",
		"dependencies": {"@putnami/web": "catalog:", "@putnami/ui": "catalog:"}
	}`)

	ctx := &pctx.Context{WorkspaceRoot: dir}
	if err := ensureWorkspaceCatalog(ctx, jsonl.New()); err != nil {
		t.Fatalf("ensureWorkspaceCatalog: %v", err)
	}

	catalog := readCatalog(t, dir)
	if catalog["@putnami/web"] != "1.2.3" {
		t.Errorf("existing catalog entry must not be overwritten, got %q", catalog["@putnami/web"])
	}
	if catalog["@putnami/ui"] != "latest" {
		t.Errorf("missing @putnami/ui should be seeded, got %v", catalog)
	}
	if catalog["react"] != "^19.2.3" {
		t.Errorf("non-Putnami catalog entry must be preserved, got %v", catalog)
	}
}

func TestEnsureWorkspaceCatalog_SeedsNamedCatalogRefs(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "a-requested-named-catalog-is-selected")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"workspaces": {
			"packages": ["packages/web"],
			"catalog": {"@putnami/web": "1.2.3"},
			"catalogs": {"framework": {"@putnami/application": "1.2.3"}}
		}
	}`), 0644)
	writeMember(t, dir, "packages/web", `{
		"name": "@app/web",
		"dependencies": {
			"@putnami/web": "catalog:framework",
			"@putnami/application": "catalog:framework",
			"@putnami/ui": "catalog:ui"
		}
	}`)

	ctx := &pctx.Context{WorkspaceRoot: dir}
	if err := ensureWorkspaceCatalog(ctx, jsonl.New()); err != nil {
		t.Fatalf("ensureWorkspaceCatalog: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	var pkg struct {
		Workspaces struct {
			Catalog  map[string]string            `json:"catalog"`
			Catalogs map[string]map[string]string `json:"catalogs"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	if pkg.Workspaces.Catalog["@putnami/web"] != "1.2.3" {
		t.Errorf("default catalog entry should be preserved, got %v", pkg.Workspaces.Catalog)
	}
	if pkg.Workspaces.Catalogs["framework"]["@putnami/application"] != "1.2.3" {
		t.Errorf("existing named catalog entry should be preserved, got %v", pkg.Workspaces.Catalogs)
	}
	if pkg.Workspaces.Catalogs["framework"]["@putnami/web"] != "latest" {
		t.Errorf("named catalog ref should seed framework catalog, got %v", pkg.Workspaces.Catalogs)
	}
	if pkg.Workspaces.Catalogs["ui"]["@putnami/ui"] != "latest" {
		t.Errorf("missing named catalog should be created, got %v", pkg.Workspaces.Catalogs)
	}
}

func TestEnsureWorkspaceCatalog_NoRefsCreatesNothing(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "catalog-seeding", "no-catalog-references-seeds-nothing")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"workspaces":["packages/web"]}`), 0644)
	writeMember(t, dir, "packages/web", `{
		"name": "@app/web",
		"dependencies": {"@putnami/web": "1.0.0"}
	}`)

	ctx := &pctx.Context{WorkspaceRoot: dir}
	if err := ensureWorkspaceCatalog(ctx, jsonl.New()); err != nil {
		t.Fatalf("ensureWorkspaceCatalog: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	var pkg map[string]json.RawMessage
	json.Unmarshal(data, &pkg)
	if _, ok := pkg["catalog"]; ok {
		t.Errorf("no catalog: refs → no catalog should be created, got %s", pkg["catalog"])
	}
}

func writeMember(t *testing.T, wsRoot, rel, content string) {
	t.Helper()
	dir := filepath.Join(wsRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readCatalog(t *testing.T, wsRoot string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wsRoot, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	var catalog map[string]string
	json.Unmarshal(pkg["catalog"], &catalog)
	return catalog
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
