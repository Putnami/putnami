package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

// ---- scanPackageJSON ----

func TestScanPackageJSON_CollectsPutnamiDeps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	os.WriteFile(path, []byte(`{
		"dependencies": {"@putnami/web": "^1.0.0", "lodash": "^4.0.0"},
		"devDependencies": {"@putnami/utils": "^1.0.0", "typescript": "^5.0.0"}
	}`), 0644)

	seen := map[string]bool{}
	scanPackageJSON(path, seen)

	if !seen["@putnami/web"] || !seen["@putnami/utils"] {
		t.Errorf("expected @putnami deps collected, got %v", seen)
	}
	if seen["lodash"] || seen["typescript"] {
		t.Errorf("non-@putnami deps should be ignored, got %v", seen)
	}
}

func TestScanPackageJSON_MissingFile(t *testing.T) {
	seen := map[string]bool{}
	// Should not panic or add anything.
	scanPackageJSON(filepath.Join(t.TempDir(), "nope.json"), seen)
	if len(seen) != 0 {
		t.Errorf("expected empty seen for missing file, got %v", seen)
	}
}

func TestScanPackageJSON_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	os.WriteFile(path, []byte("not json"), 0644)

	seen := map[string]bool{}
	scanPackageJSON(path, seen)
	if len(seen) != 0 {
		t.Errorf("expected empty seen for invalid JSON, got %v", seen)
	}
}

// ---- collectWorkspacePackageNames ----

func TestCollectWorkspacePackageNames_ReadsMembers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"workspaces": ["packages/a", "packages/b", "packages/missing"]}`), 0644)

	mkMember := func(rel, name string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(p, 0755)
		os.WriteFile(filepath.Join(p, "package.json"), []byte(`{"name":"`+name+`"}`), 0644)
	}
	mkMember("packages/a", "@putnami/a")
	mkMember("packages/b", "@putnami/b")
	// packages/missing has no package.json — should be skipped silently.

	names := collectWorkspacePackageNames(dir)
	if !names["@putnami/a"] || !names["@putnami/b"] {
		t.Errorf("expected both member names, got %v", names)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 names, got %v", names)
	}
}

func TestCollectWorkspacePackageNames_NoRootPackageJSON(t *testing.T) {
	names := collectWorkspacePackageNames(t.TempDir())
	if len(names) != 0 {
		t.Errorf("expected empty map when no root package.json, got %v", names)
	}
}

func TestCollectWorkspacePackageNames_InvalidRootJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("not json"), 0644)
	names := collectWorkspacePackageNames(dir)
	if len(names) != 0 {
		t.Errorf("expected empty map for invalid root JSON, got %v", names)
	}
}

func TestCollectWorkspacePackageNames_MemberInvalidJSONSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"workspaces": ["packages/a"]}`), 0644)
	p := filepath.Join(dir, "packages/a")
	os.MkdirAll(p, 0755)
	os.WriteFile(filepath.Join(p, "package.json"), []byte("not json"), 0644)

	names := collectWorkspacePackageNames(dir)
	if len(names) != 0 {
		t.Errorf("expected member with invalid JSON skipped, got %v", names)
	}
}

// ---- collectPutnamiDeps ----

func TestCollectPutnamiDeps_ScansAndExcludesLocal(t *testing.T) {
	dir := t.TempDir()

	// Root package.json declares the workspace members and one external dep.
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"workspaces": ["typescript/web"],
		"dependencies": {"@putnami/cli": "^1.0.0"}
	}`), 0644)

	// A two-level-deep project package.json with an external + a local dep.
	proj := filepath.Join(dir, "typescript", "web")
	os.MkdirAll(proj, 0755)
	os.WriteFile(filepath.Join(proj, "package.json"), []byte(`{
		"name": "@putnami/web",
		"dependencies": {"@putnami/runtime": "^1.0.0", "@putnami/web": "workspace:*"}
	}`), 0644)

	// node_modules must be ignored even if it matches the glob.
	nm := filepath.Join(dir, "typescript", "node_modules")
	os.MkdirAll(nm, 0755)
	os.WriteFile(filepath.Join(nm, "package.json"), []byte(`{
		"dependencies": {"@putnami/should-be-ignored": "^1.0.0"}
	}`), 0644)

	emit := jsonl.New()
	packages := collectPutnamiDeps(dir, emit)

	got := map[string]bool{}
	for _, p := range packages {
		got[p] = true
	}
	if !got["@putnami/cli"] || !got["@putnami/runtime"] {
		t.Errorf("expected external @putnami deps, got %v", packages)
	}
	// @putnami/web is a local workspace member → excluded.
	if got["@putnami/web"] {
		t.Errorf("local workspace package should be excluded, got %v", packages)
	}
	// node_modules path must not contribute.
	if got["@putnami/should-be-ignored"] {
		t.Errorf("node_modules deps should be ignored, got %v", packages)
	}
	// Result must be sorted.
	for i := 1; i < len(packages); i++ {
		if packages[i-1] > packages[i] {
			t.Errorf("expected sorted packages, got %v", packages)
		}
	}
}

func TestCollectPutnamiDeps_None(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"dependencies": {"lodash": "^4.0.0"}}`), 0644)

	packages := collectPutnamiDeps(dir, jsonl.New())
	if len(packages) != 0 {
		t.Errorf("expected no packages, got %v", packages)
	}
}

// The cloud extension being enabled no longer force-pins
// @putnami/cloud: nothing imports it at the root (it resolves via the
// lock/artifact-store path), so a same-version root dep+override was pure npm
// EOVERRIDE bait. It is now managed only when a workspace package actually
// declares it as a dependency.
func TestCollectPutnamiDeps_CloudExtensionDoesNotForcePin(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"extensions": ["/typescript/extension", "@putnami/cloud"]
	}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"workspaces": ["packages/runtime"]
	}`), 0644)

	runtimeDir := filepath.Join(dir, "packages", "runtime")
	os.MkdirAll(runtimeDir, 0755)
	os.WriteFile(filepath.Join(runtimeDir, "package.json"), []byte(`{
		"name": "@putnami/runtime"
	}`), 0644)

	if packages := collectPutnamiDeps(dir, jsonl.New()); len(packages) != 0 {
		t.Errorf("cloud extension alone must not force-add @putnami/cloud, got %v", packages)
	}
}

func TestCollectPutnamiDeps_CloudCollectedWhenDeclaredDependency(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"extensions": ["@putnami/cloud"]
	}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"dependencies": {"@putnami/cloud": "^1.0.0"}
	}`), 0644)

	packages := collectPutnamiDeps(dir, jsonl.New())
	if len(packages) != 1 || packages[0] != "@putnami/cloud" {
		t.Errorf("expected @putnami/cloud collected from a real dependency, got %v", packages)
	}
}

// A catalog entry that no workspace member imports (an "orphan") is still
// managed: the catalog is the workspace's committed version policy, so upgrade
// must keep it current instead of letting it drift stale. Regression for
// catalog-only @putnami/* packages (e.g. client/events/storage) that
// `putnami upgrade` stranded at an old version while re-pinning only the
// member-referenced subset.
func TestCollectPutnamiDeps_IncludesOrphanCatalogEntries(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"private": true,
		"workspaces": ["packages/web"],
		"catalog": {
			"@putnami/application": "0.1.0-old",
			"@putnami/events": "0.1.0-old",
			"react": "^19.0.0"
		}
	}`), 0644)

	// The member imports @putnami/application via catalog:; nothing imports
	// @putnami/events — it lives in the catalog only.
	member := filepath.Join(dir, "packages", "web")
	os.MkdirAll(member, 0755)
	os.WriteFile(filepath.Join(member, "package.json"), []byte(`{
		"name": "@acme/web",
		"dependencies": {"@putnami/application": "catalog:"}
	}`), 0644)

	got := map[string]bool{}
	for _, p := range collectPutnamiDeps(dir, jsonl.New()) {
		got[p] = true
	}
	if !got["@putnami/application"] {
		t.Errorf("member-referenced catalog package missing, got %v", got)
	}
	if !got["@putnami/events"] {
		t.Errorf("orphan catalog entry must be managed so it upgrades, got %v", got)
	}
	if got["react"] {
		t.Errorf("non-@putnami catalog entry must not be collected, got %v", got)
	}
}

// A local workspace package that also appears in the catalog stays excluded —
// it is resolved from source, not fetched from the registry.
func TestCollectPutnamiDeps_ExcludesLocalCatalogEntries(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"workspaces": ["packages/ui"],
		"catalog": { "@putnami/ui": "0.1.0-old" }
	}`), 0644)
	member := filepath.Join(dir, "packages", "ui")
	os.MkdirAll(member, 0755)
	os.WriteFile(filepath.Join(member, "package.json"), []byte(`{"name": "@putnami/ui"}`), 0644)

	for _, p := range collectPutnamiDeps(dir, jsonl.New()) {
		if p == "@putnami/ui" {
			t.Errorf("local workspace package in the catalog must be excluded, got %v", p)
		}
	}
}

// ---- resolvedPackages ----

func TestResolvedPackages_KeepsResolvedPreservesOrder(t *testing.T) {
	packages := []string{"@putnami/application", "@putnami/events", "@putnami/web"}
	versions := map[string]string{
		"@putnami/application": "1.2.3",
		// @putnami/events failed to resolve (skipped) — no entry.
		"@putnami/web": "1.2.3",
	}
	got := resolvedPackages(packages, versions)
	want := []string{"@putnami/application", "@putnami/web"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// ---- runDepsUpgrade ----

func testUpgradeReleaseSet(t *testing.T, npmVersions map[string]string, goVersions map[string]string) json.RawMessage {
	t.Helper()
	members := make([]distribution.ReleaseSetMember, 0, len(npmVersions)+len(goVersions))
	for name, version := range npmVersions {
		members = append(members, distribution.ReleaseSetMember{
			Ecosystem:            "npm",
			Coordinate:           name,
			Version:              version,
			ArtifactDigest:       "sha256:" + strings.Repeat("a", 64),
			Dependencies:         []distribution.ReleaseSetDependency{},
			SourceRevision:       strings.Repeat("1", 40),
			SelectionFingerprint: "sha256:" + strings.Repeat("c", 64),
		})
	}
	for name, version := range goVersions {
		members = append(members, distribution.ReleaseSetMember{
			Ecosystem:            "go",
			Coordinate:           name,
			Version:              version,
			ArtifactDigest:       "sha256:" + strings.Repeat("b", 64),
			Dependencies:         []distribution.ReleaseSetDependency{},
			SourceRevision:       strings.Repeat("1", 40),
			SelectionFingerprint: "sha256:" + strings.Repeat("d", 64),
		})
	}
	releaseSet := distribution.NormalizeReleaseSet(&distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members:         members,
	})
	ref, diagnostics := distribution.DeriveReleaseSetRef(releaseSet)
	if len(diagnostics) != 0 {
		t.Fatalf("derive test release set: %v", diagnostics)
	}
	// The upgrade hands the extension the CHANNEL HEAD shape, not a resolve
	// envelope: an immutable release is named by no channel, so its generation
	// is 0.
	raw, err := json.Marshal(distribution.ChannelHead{
		Ref:        ref,
		Generation: 0,
		ReleaseSet: releaseSet,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRunDepsUpgrade_ReleaseSetPinsMixedNPMVersionsWithoutTagQueries(t *testing.T) {
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	ctx.Workspace.Name = "putnami"
	ctx.Params[upgradeReleaseSetParam] = testUpgradeReleaseSet(t, map[string]string{
		"@putnami/application": "1.2.3",
		"@putnami/web":         "2.4.6-canary.1",
	}, map[string]string{"go.putnami.dev/http": "v3.5.7"})
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"catalog": {
			"@putnami/application": "0.1.0",
			"@putnami/web": "0.1.0"
		}
	}`), 0o644)

	// Set mode must not reach npm metadata at all. An exact immutable member map
	// is already the resolution result; per-package tag reads could mix heads.
	originalClient := npmMetadataClient
	t.Cleanup(func() { npmMetadataClient = originalClient })
	npmMetadataClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("release-set upgrade queried npm metadata")
		return nil, errors.New("unexpected metadata query")
	})}
	mockAllExec(t, successExec)

	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("runDepsUpgrade = %q, %v", status, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Catalog map[string]string `json:"catalog"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Catalog["@putnami/application"] != "1.2.3" || manifest.Catalog["@putnami/web"] != "2.4.6-canary.1" {
		t.Fatalf("catalog = %#v, want heterogeneous release-set versions", manifest.Catalog)
	}
}

func TestRunDepsUpgrade_ReleaseSetMissingReferencedPackageFailsClosed(t *testing.T) {
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	ctx.Workspace.Name = "putnami"
	ctx.Params[upgradeReleaseSetParam] = testUpgradeReleaseSet(t,
		map[string]string{"@putnami/application": "1.2.3"}, nil)
	original := `{"dependencies":{"@putnami/web":"0.1.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(original), 0o644)
	executed := false
	mockAllExec(t, func(string, []string, ...exec.Option) (*exec.Result, error) {
		executed = true
		return &exec.Result{Success: true}, nil
	})

	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("missing member is a reported job failure, got process error: %v", err)
	}
	if status != "FAILED" || executed {
		t.Fatalf("status = %q, executed = %v; want fail before install", status, executed)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if string(data) != original {
		t.Fatalf("manifest changed on missing member:\n%s", data)
	}
}

func TestResolveUpgradeNPMVersions_RejectsInvalidTypedSnapshot(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Workspace.Name = "putnami"
	raw := testUpgradeReleaseSet(t, map[string]string{"@putnami/web": "1.2.3"}, nil)
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	ref := document["ref"].(map[string]any)
	ref["digest"] = "sha256:" + strings.Repeat("f", 64)
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	ctx.Params[upgradeReleaseSetParam] = tampered

	versions, _, setMode, err := resolveUpgradeNPMVersions(ctx, dir, []string{"@putnami/web"}, "canary", jsonl.New())
	if err == nil || !strings.Contains(err.Error(), "invalid release-set snapshot") {
		t.Fatalf("resolve error = %v, want strict validation failure", err)
	}
	if !setMode || versions != nil {
		t.Fatalf("setMode = %v, versions = %#v; want failed set mode", setMode, versions)
	}
}

// pinViaDependencies (catalog-free workspaces) pins @putnami/* as direct
// dependencies only and strips redundant @putnami/* overrides — the direct pin
// is the sole version policy and a same-version override is pure npm EOVERRIDE
// bait. Non-@putnami overrides are preserved.
func TestPinWorkspacePutnamiDeps_StripsPutnamiOverrides(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"dependencies": {
			"left-alone": "^1.0.0",
			"@putnami/web": "^1.0.0"
		},
		"overrides": {
			"left-alone": "2.0.0",
			"nested": {"child": "3.0.0"},
			"@putnami/web": "0.0.1"
		}
	}`), 0644)

	err := pinWorkspacePutnamiDeps(dir, []string{"@putnami/application", "@putnami/web"}, "1.2.3", false, jsonl.New())
	if err != nil {
		t.Fatalf("pinWorkspacePutnamiDeps: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg struct {
		Dependencies map[string]string          `json:"dependencies"`
		Overrides    map[string]json.RawMessage `json:"overrides"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}

	// Direct dependencies are pinned.
	if pkg.Dependencies["@putnami/application"] != "1.2.3" || pkg.Dependencies["@putnami/web"] != "1.2.3" {
		t.Errorf("expected @putnami dependencies pinned, got %v", pkg.Dependencies)
	}
	if pkg.Dependencies["left-alone"] != "^1.0.0" {
		t.Errorf("non-Putnami dependency should be preserved, got %q", pkg.Dependencies["left-alone"])
	}

	// @putnami/* overrides are stripped — no duplicate-override EOVERRIDE bait.
	if _, ok := pkg.Overrides["@putnami/web"]; ok {
		t.Errorf("@putnami/web override should be stripped, got %v", pkg.Overrides)
	}
	if _, ok := pkg.Overrides["@putnami/application"]; ok {
		t.Errorf("@putnami/application override should not be written, got %v", pkg.Overrides)
	}

	// Non-Putnami overrides are preserved.
	var leftAlone string
	if err := json.Unmarshal(pkg.Overrides["left-alone"], &leftAlone); err != nil || leftAlone != "2.0.0" {
		t.Errorf("non-Putnami override should be preserved, got %q (err %v)", leftAlone, err)
	}
	var nested map[string]string
	if err := json.Unmarshal(pkg.Overrides["nested"], &nested); err != nil || nested["child"] != "3.0.0" {
		t.Errorf("nested override should be preserved, got %v (err %v)", nested, err)
	}
}

// TestPinWorkspacePutnamiDeps_CatalogTopLevel is the core regression: a
// catalog-first workspace (top-level catalog, package-level catalog: consumers,
// no root @putnami/* dependencies or overrides) must have its versions updated
// in the catalog only — never reintroducing root dependencies/overrides.
func TestPinWorkspacePutnamiDeps_CatalogTopLevel(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"private": true,
		"workspaces": ["packages/api"],
		"catalog": {
			"@putnami/application": "1.0.0",
			"@putnami/web": "1.0.0",
			"react": "^19.2.3",
			"react-dom": "^19.2.3"
		}
	}`), 0644)

	err := pinWorkspacePutnamiDeps(dir, []string{"@putnami/application", "@putnami/web"}, "1.2.3", false, jsonl.New())
	if err != nil {
		t.Fatalf("pinWorkspacePutnamiDeps: %v", err)
	}

	var pkg map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}

	var catalog map[string]string
	if err := json.Unmarshal(pkg["catalog"], &catalog); err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	if catalog["@putnami/application"] != "1.2.3" || catalog["@putnami/web"] != "1.2.3" {
		t.Errorf("expected @putnami/* pinned in catalog, got %v", catalog)
	}
	if catalog["react"] != "^19.2.3" || catalog["react-dom"] != "^19.2.3" {
		t.Errorf("non-Putnami catalog entries must be preserved, got %v", catalog)
	}
	if _, ok := pkg["dependencies"]; ok {
		t.Errorf("catalog mode must not introduce root dependencies, got %s", pkg["dependencies"])
	}
	if _, ok := pkg["overrides"]; ok {
		t.Errorf("catalog mode must not introduce root overrides, got %s", pkg["overrides"])
	}
}

// TestPinWorkspacePutnamiDeps_CatalogStripsDuplicates verifies that a partially
// migrated manifest (catalog plus leftover root @putnami/* deps/overrides) is
// cleaned up: the catalog becomes the only @putnami/* version policy.
func TestPinWorkspacePutnamiDeps_CatalogStripsDuplicates(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"catalog": { "@putnami/web": "1.0.0" },
		"dependencies": { "@putnami/web": "1.0.0", "left-alone": "^1.0.0" },
		"overrides": { "@putnami/web": "1.0.0", "left-alone": "2.0.0" }
	}`), 0644)

	err := pinWorkspacePutnamiDeps(dir, []string{"@putnami/web"}, "1.2.3", false, jsonl.New())
	if err != nil {
		t.Fatalf("pinWorkspacePutnamiDeps: %v", err)
	}

	var pkg map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	json.Unmarshal(data, &pkg)

	var catalog map[string]string
	json.Unmarshal(pkg["catalog"], &catalog)
	if catalog["@putnami/web"] != "1.2.3" {
		t.Errorf("expected catalog pinned, got %v", catalog)
	}

	var deps map[string]string
	json.Unmarshal(pkg["dependencies"], &deps)
	if _, ok := deps["@putnami/web"]; ok {
		t.Errorf("@putnami/web should be stripped from dependencies, got %v", deps)
	}
	if deps["left-alone"] != "^1.0.0" {
		t.Errorf("non-Putnami dependency must be preserved, got %v", deps)
	}

	var overrides map[string]string
	json.Unmarshal(pkg["overrides"], &overrides)
	if _, ok := overrides["@putnami/web"]; ok {
		t.Errorf("@putnami/web should be stripped from overrides, got %v", overrides)
	}
	if overrides["left-alone"] != "2.0.0" {
		t.Errorf("non-Putnami override must be preserved, got %v", overrides)
	}
}

// TestPinWorkspacePutnamiDeps_CatalogSeedsMissing verifies that a package
// referenced via catalog: but absent from the catalog is added — otherwise
// `bun install` would fail to resolve it.
func TestPinWorkspacePutnamiDeps_CatalogSeedsMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"catalog": { "@putnami/web": "1.0.0" }
	}`), 0644)

	err := pinWorkspacePutnamiDeps(dir, []string{"@putnami/events", "@putnami/web"}, "1.2.3", false, jsonl.New())
	if err != nil {
		t.Fatalf("pinWorkspacePutnamiDeps: %v", err)
	}

	var pkg map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	json.Unmarshal(data, &pkg)
	var catalog map[string]string
	json.Unmarshal(pkg["catalog"], &catalog)
	if catalog["@putnami/events"] != "1.2.3" {
		t.Errorf("expected @putnami/events seeded into catalog, got %v", catalog)
	}
	if catalog["@putnami/web"] != "1.2.3" {
		t.Errorf("expected @putnami/web pinned, got %v", catalog)
	}
}

// TestPinWorkspacePutnamiDeps_CatalogWorkspacesNested covers the nested
// workspaces.catalog placement Bun also accepts.
func TestPinWorkspacePutnamiDeps_CatalogWorkspacesNested(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"workspaces": {
			"packages": ["packages/*"],
			"catalog": { "@putnami/web": "1.0.0" }
		}
	}`), 0644)

	err := pinWorkspacePutnamiDeps(dir, []string{"@putnami/web"}, "1.2.3", false, jsonl.New())
	if err != nil {
		t.Fatalf("pinWorkspacePutnamiDeps: %v", err)
	}

	var pkg struct {
		Workspaces struct {
			Packages []string          `json:"packages"`
			Catalog  map[string]string `json:"catalog"`
		} `json:"workspaces"`
	}
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	if pkg.Workspaces.Catalog["@putnami/web"] != "1.2.3" {
		t.Errorf("expected nested workspaces.catalog pinned, got %v", pkg.Workspaces.Catalog)
	}
	if len(pkg.Workspaces.Packages) != 1 || pkg.Workspaces.Packages[0] != "packages/*" {
		t.Errorf("expected workspaces.packages preserved, got %v", pkg.Workspaces.Packages)
	}
}

// TestPinWorkspacePutnamiDeps_CatalogDryRun verifies `--dry-run` reports the
// catalog changes without writing the manifest.
func TestPinWorkspacePutnamiDeps_CatalogDryRun(t *testing.T) {
	dir := t.TempDir()
	original := `{
		"catalog": { "@putnami/web": "1.0.0" },
		"dependencies": { "@putnami/web": "1.0.0" }
	}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(original), 0644)

	err := pinWorkspacePutnamiDeps(dir, []string{"@putnami/web"}, "1.2.3", true, jsonl.New())
	if err != nil {
		t.Fatalf("pinWorkspacePutnamiDeps (dry-run): %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if string(data) != original {
		t.Errorf("dry-run must not modify package.json\n got: %s", data)
	}
}

// TestPinWorkspacePutnamiDeps_PreservesTopLevelKeyOrder is the key-order
// regression: encoding/json always marshals map keys alphabetically, so
// writeRootManifest used to reorder every top-level key of the root
// package.json on every rewrite — silently scrambling unmanaged keys like
// "overrides" and letting an overrides drop slip by unnoticed. A
// realistic large-workspace-shaped manifest whose keys are deliberately NOT
// alphabetical must come out of a catalog version bump with the exact same
// top-level key order (and the redirect override untouched), never
// alphabetized.
func TestPinWorkspacePutnamiDeps_PreservesTopLevelKeyOrder(t *testing.T) {
	dir := t.TempDir()
	original := `{
  "name": "@putnami/cloud",
  "private": true,
  "workspaces": [
    "packages/*"
  ],
  "dependencies": {
    "react": "^19.2.3"
  },
  "overrides": {
    "@putnami/client": "catalog:"
  },
  "catalog": {
    "@putnami/client": "1.0.0",
    "@putnami/web": "1.0.0"
  }
}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(original), 0644)

	err := pinWorkspacePutnamiDeps(dir, []string{"@putnami/client"}, "2.0.0", false, jsonl.New())
	if err != nil {
		t.Fatalf("pinWorkspacePutnamiDeps: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}

	// Top-level keys must keep their original order — NOT come out
	// alphabetically reordered (which would put "catalog" before "name").
	gotOrder, err := topLevelKeyOrder(data)
	if err != nil {
		t.Fatalf("topLevelKeyOrder: %v", err)
	}
	wantOrder := []string{"name", "private", "workspaces", "dependencies", "overrides", "catalog"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("top-level key order = %v, want %v (unchanged from source)", gotOrder, wantOrder)
	}

	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}

	// The catalog:-redirect override must still be there, byte-for-byte.
	var overrides map[string]string
	if err := json.Unmarshal(pkg["overrides"], &overrides); err != nil {
		t.Fatalf("parse overrides: %v", err)
	}
	if overrides["@putnami/client"] != "catalog:" {
		t.Errorf("catalog: redirect override must be preserved, got %v", overrides)
	}

	// The catalog bump itself must still have happened.
	var catalog map[string]string
	if err := json.Unmarshal(pkg["catalog"], &catalog); err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	if catalog["@putnami/client"] != "2.0.0" {
		t.Errorf("expected @putnami/client bumped in catalog, got %v", catalog)
	}

	// Formatting stays 2-space indented with a single trailing newline.
	if !strings.HasSuffix(string(data), "\n") || strings.HasSuffix(string(data), "\n\n") {
		t.Errorf("expected exactly one trailing newline, got %q", string(data))
	}
	if !strings.Contains(string(data), "\n  \"name\": ") {
		t.Errorf("expected 2-space-indented top-level keys, got:\n%s", data)
	}
	if !strings.Contains(string(data), "\n    \"@putnami/client\": ") {
		t.Errorf("expected 2-space-indented nesting for object values, got:\n%s", data)
	}
}

// TestRunDepsUpgrade_CatalogMode exercises the full scan → resolve → pin path on
// a catalog-first workspace with a package-level catalog: consumer.
func TestRunDepsUpgrade_CatalogMode(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params["putnami-version"] = []byte(`"0.1.0-abc"`)

	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"private": true,
		"workspaces": ["packages/web"],
		"catalog": { "@putnami/web": "0.0.1" }
	}`), 0644)
	memberDir := filepath.Join(dir, "packages", "web")
	os.MkdirAll(memberDir, 0755)
	os.WriteFile(filepath.Join(memberDir, "package.json"), []byte(`{
		"name": "@app/web",
		"dependencies": { "@putnami/web": "catalog:" }
	}`), 0644)

	var gotArgs []string
	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		gotArgs = args
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "install" {
		t.Errorf("expected bun install, got %v", gotArgs)
	}

	var pkg map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	json.Unmarshal(data, &pkg)
	var catalog map[string]string
	json.Unmarshal(pkg["catalog"], &catalog)
	if catalog["@putnami/web"] != "0.1.0-abc" {
		t.Errorf("expected catalog @putnami/web pinned, got %v", catalog)
	}
	if _, ok := pkg["dependencies"]; ok {
		t.Errorf("catalog mode must not add root dependencies, got %s", pkg["dependencies"])
	}
	if _, ok := pkg["overrides"]; ok {
		t.Errorf("catalog mode must not add root overrides, got %s", pkg["overrides"])
	}
}

func TestRunDepsUpgrade_BunResolutionFails(t *testing.T) {
	orig := resolveBunBin
	t.Cleanup(func() { resolveBunBin = orig })
	resolveBunBin = func() (string, error) { return "", errors.New("no bun") }

	ctx, _ := makeTestCtx(t)
	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when bun resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunDepsUpgrade_NoPackages(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, dir := makeTestCtx(t)
	// A workspace whose @putnami/* packages are all LOCAL members has nothing to
	// fetch from the registry, so deps-upgrade is a no-op.
	frameworkMembers := []string{"@putnami/application", "@putnami/runtime", "@putnami/web"}
	workspaces := make([]string, 0, len(frameworkMembers))
	for _, name := range frameworkMembers {
		rel := filepath.Join("packages", strings.TrimPrefix(name, "@putnami/"))
		workspaces = append(workspaces, filepath.ToSlash(rel))
		pkgDir := filepath.Join(dir, rel)
		os.MkdirAll(pkgDir, 0755)
		os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"`+name+`"}`), 0644)
	}
	rootPkg := `{"workspaces":[` + quoteList(workspaces) + `]}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(rootPkg), 0644)

	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK when no @putnami deps, got %q", status)
	}
}

func TestRunDepsUpgrade_Success(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params["putnami-version"] = []byte(`"0.1.0-abc"`)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"dependencies": {"@putnami/cli": "^1.0.0", "@putnami/web": "^1.0.0"}
	}`), 0644)

	var gotArgs []string
	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		gotArgs = args
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "install" {
		t.Errorf("expected 'install' as first arg, got %v", gotArgs)
	}
	if !containsArg(gotArgs, "--force") {
		t.Errorf("expected --force in install args, got %v", gotArgs)
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
		Overrides    map[string]string `json:"overrides"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	// Legacy shape (no catalog): the referenced @putnami/* packages are pinned as
	// direct dependencies only — no duplicate override (EOVERRIDE bait).
	// Packages the workspace does not reference are not force-added.
	if pkg.Dependencies["@putnami/web"] != "0.1.0-abc" || pkg.Dependencies["@putnami/cli"] != "0.1.0-abc" {
		t.Errorf("expected referenced @putnami deps pinned, got %v", pkg.Dependencies)
	}
	if len(pkg.Overrides) != 0 {
		t.Errorf("legacy shape must not write @putnami/* overrides, got %v", pkg.Overrides)
	}
	if _, ok := pkg.Dependencies["@putnami/application"]; ok {
		t.Errorf("unreferenced @putnami/application should not be force-added, got %v", pkg.Dependencies)
	}
}

// Enabling the cloud extension no longer introduces @putnami/cloud into the
// root manifest: only packages actually referenced as dependencies are
// managed, and each is pinned as a direct dependency with no duplicate override.
func TestRunDepsUpgrade_CloudExtensionDoesNotPinCloud(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params["putnami-version"] = []byte(`"0.1.0-abc"`)
	os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"extensions": ["@putnami/cloud"]
	}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"dependencies": {"@putnami/web": "^1.0.0"}
	}`), 0644)

	mockAllExec(t, successExec)

	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
		Overrides    map[string]string `json:"overrides"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	if _, ok := pkg.Dependencies["@putnami/cloud"]; ok {
		t.Errorf("cloud extension must not introduce an @putnami/cloud dependency, got %v", pkg.Dependencies)
	}
	if pkg.Dependencies["@putnami/web"] != "0.1.0-abc" {
		t.Errorf("web dependency = %q, want 0.1.0-abc", pkg.Dependencies["@putnami/web"])
	}
	if len(pkg.Overrides) != 0 {
		t.Errorf("no @putnami/* overrides should be written, got %v", pkg.Overrides)
	}
}

func TestRunDepsUpgrade_BunUpdateExecError(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params["putnami-version"] = []byte(`"0.1.0-abc"`)
	original := `{"dependencies": {"@putnami/cli": "^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(original), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		// A failed installer may already have rewritten/created its lockfile.
		os.WriteFile(filepath.Join(dir, "bun.lock"), []byte("partial"), 0o644)
		return nil, errors.New("exec boom")
	})

	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error to propagate from bun install")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
	if data, readErr := os.ReadFile(filepath.Join(dir, "package.json")); readErr != nil || string(data) != original {
		t.Errorf("package.json was not rolled back: %v\n%s", readErr, data)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "bun.lock")); !os.IsNotExist(statErr) {
		t.Errorf("new partial bun.lock survived rollback: %v", statErr)
	}
}

func TestRunDepsUpgrade_BunUpdateUnsuccessful(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params["putnami-version"] = []byte(`"0.1.0-abc"`)
	originalManifest := `{"dependencies": {"@putnami/cli": "^1.0.0"}}`
	originalLock := "original-lock"
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(originalManifest), 0644)
	os.WriteFile(filepath.Join(dir, "bun.lockb"), []byte(originalLock), 0o600)
	// The mode the file has on disk: 0600 on Unix, and 0666 on Windows, which
	// records no permission but read-only.
	originalInfo, err := os.Stat(filepath.Join(dir, "bun.lockb"))
	if err != nil {
		t.Fatal(err)
	}

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		os.WriteFile(filepath.Join(dir, "bun.lockb"), []byte("partial-lock"), 0o644)
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "registry unreachable"}, nil
	})

	status, _, err := runDepsUpgrade(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED when bun install is unsuccessful, got %q", status)
	}
	if data, readErr := os.ReadFile(filepath.Join(dir, "package.json")); readErr != nil || string(data) != originalManifest {
		t.Errorf("package.json was not rolled back: %v\n%s", readErr, data)
	}
	if data, readErr := os.ReadFile(filepath.Join(dir, "bun.lockb")); readErr != nil || string(data) != originalLock {
		t.Errorf("bun.lockb was not rolled back: %v\n%s", readErr, data)
	}
	if info, statErr := os.Stat(filepath.Join(dir, "bun.lockb")); statErr != nil || info.Mode().Perm() != originalInfo.Mode().Perm() {
		t.Errorf("bun.lockb mode was not restored to %v: %v, %v", originalInfo.Mode().Perm(), info, statErr)
	}
}

func TestResolvePutnamiNPMVersionUsesCLIUserAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(cliUserAgentEnv, "putnami-cli/v1.2.3")

	origClient := npmMetadataClient
	t.Cleanup(func() { npmMetadataClient = origClient })

	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"dist-tags":{"latest":"1.2.3"}}`))
	}))
	t.Cleanup(srv.Close)
	npmMetadataClient = srv.Client()

	got, err := resolvePutnamiNPMVersion(context.Background(), dir, scopeRegistries(srv.URL), "@putnami/web", "latest")
	if err != nil {
		t.Fatalf("resolvePutnamiNPMVersion: %v", err)
	}
	if got != "1.2.3" {
		t.Fatalf("version = %q, want 1.2.3", got)
	}
	if gotUA != "putnami-cli/v1.2.3" {
		t.Fatalf("User-Agent = %q, want putnami-cli/v1.2.3", gotUA)
	}
}

// A dist-tag IS the npm channel. Resolving one is the whole npm half of
// `upgrade --channel`: no release set, no namespace.
func TestResolvePutnamiNPMVersion_ResolvesTheChannelDistTag(t *testing.T) {
	dir := t.TempDir()
	originalClient := npmMetadataClient
	t.Cleanup(func() { npmMetadataClient = originalClient })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"dist-tags":{"latest":"1.2.3","canary":"0.1.0-20260902173000-cafe1234"}}`))
	}))
	t.Cleanup(server.Close)
	npmMetadataClient = server.Client()

	got, err := resolvePutnamiNPMVersion(context.Background(), dir, scopeRegistries(server.URL), "@putnami/web", "canary")
	if err != nil {
		t.Fatalf("resolvePutnamiNPMVersion: %v", err)
	}
	if got != "0.1.0-20260902173000-cafe1234" {
		t.Fatalf("version = %q, want the canary dist-tag", got)
	}
}

// A private registry hides versions from an anonymous client, so the metadata
// read presents the same _authToken npm itself would use. Without it the
// published channel looks missing.
func TestResolvePutnamiNPMVersion_SendsTheNpmrcAuthToken(t *testing.T) {
	const token = "npm-secret-token"
	dir := t.TempDir()
	originalClient := npmMetadataClient
	t.Cleanup(func() { npmMetadataClient = originalClient })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			_, _ = w.Write([]byte(`{"dist-tags":{"latest":"1.0.0"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"dist-tags":{"latest":"1.0.0","canary":"0.1.0-private"}}`))
	}))
	t.Cleanup(server.Close)
	npmMetadataClient = server.Client()

	writeScopeNpmrc(t, dir, server.URL, "")
	if _, err := resolvePutnamiNPMVersion(context.Background(), dir, scopeRegistries(server.URL), "@putnami/web", "canary"); err == nil {
		t.Fatal("anonymous read resolved a private channel")
	}

	writeScopeNpmrc(t, dir, server.URL, token)
	got, err := resolvePutnamiNPMVersion(context.Background(), dir, scopeRegistries(server.URL), "@putnami/web", "canary")
	if err != nil {
		t.Fatalf("authenticated resolvePutnamiNPMVersion: %v", err)
	}
	if got != "0.1.0-private" {
		t.Fatalf("version = %q, want the private canary version", got)
	}
}

func TestResolvePutnamiNPMVersion_ErrorNamesPackageChannelHostAndStatus(t *testing.T) {
	dir := t.TempDir()
	originalClient := npmMetadataClient
	t.Cleanup(func() { npmMetadataClient = originalClient })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "gone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"dist-tags":{"latest":"1.0.0"}}`))
	}))
	t.Cleanup(server.Close)
	npmMetadataClient = server.Client()
	host := strings.TrimPrefix(server.URL, "http://")

	_, err := resolvePutnamiNPMVersion(context.Background(), dir, scopeRegistries(server.URL), "@putnami/gone", "canary")
	if err == nil {
		t.Fatal("unknown package resolved")
	}
	for _, want := range []string{"@putnami/gone", "canary", host, "HTTP 404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	_, err = resolvePutnamiNPMVersion(context.Background(), dir, scopeRegistries(server.URL), "@putnami/web", "canary")
	if err == nil {
		t.Fatal("missing dist-tag resolved")
	}
	for _, want := range []string{"@putnami/web", "canary", host, "no dist-tag with that name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// A consumer workspace is not the publisher: a downstream consumer workspace
// consuming a `putnami` snapshot is the normal case, and rejecting a foreign
// release-set namespace is a defect on the extension side.
func TestResolveUpgradeNPMVersions_AcceptsAForeignReleaseSetNamespace(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	ctx.Workspace.Name = "acme-consumer"
	ctx.Params[upgradeReleaseSetParam] = testUpgradeReleaseSet(t,
		map[string]string{"@putnami/web": "1.2.3"}, nil)

	versions, _, setMode, err := resolveUpgradeNPMVersions(ctx, dir, []string{"@putnami/web"}, "canary", jsonl.New())
	if err != nil {
		t.Fatalf("resolveUpgradeNPMVersions: %v", err)
	}
	if !setMode || versions["@putnami/web"] != "1.2.3" {
		t.Fatalf("setMode = %v, versions = %#v; want the publisher's snapshot accepted", setMode, versions)
	}
}

// Versions are derived from git, so an ordered pre-release ends in the commit
// it was cut from. The REVISION column is that suffix: it is the only thing
// that tells two builds of one base version apart, and a stable version has
// none.
func TestUpgradeTableShowsTheSourceRevision(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "upgrade-table-shows-source-revision", "revision-column-follows-the-version-suffix")

	for _, tc := range []struct{ version, want string }{
		{"1.2.3", "-"},
		{"0.3.0-5336de185", "5336de185"},
		{"0.1.0-20260902173000-cafe1234", "cafe1234"},
		{"1.0.0-rc.1", "rc.1"},
		{"1.2.3+build.7", "-"},
		{"0.3.0-abc1234+build.7", "abc1234"},
		{"", "-"},
	} {
		if got := npmVersionRevision(tc.version); got != tc.want {
			t.Errorf("revision of %q = %q, want %q", tc.version, got, tc.want)
		}
	}

	rows := renderUpgradeTable(
		[]string{"@putnami/web", "@putnami/utils"},
		map[string]string{"@putnami/web": "0.3.0-5336de185", "@putnami/utils": "1.2.3"},
		npmReleaseSetSource,
	)
	if len(rows) != 3 {
		t.Fatalf("rows = %#v, want a header and two packages", rows)
	}
	if !strings.HasPrefix(rows[0], "PACKAGE") || !strings.Contains(rows[0], "REVISION") {
		t.Fatalf("header = %q", rows[0])
	}
	if !strings.Contains(rows[1], "0.3.0-5336de185") || !strings.Contains(rows[1], "5336de185") {
		t.Fatalf("pre-release row = %q", rows[1])
	}
	if !strings.Contains(rows[2], "1.2.3") || !strings.Contains(rows[2], "-          release-set") {
		t.Fatalf("stable row = %q, want a dash revision", rows[2])
	}
}

// scopeRegistries is the workspace registries declaration a test publishes for
// the @putnami scope — the source the upgrade resolves its registry from now.
func scopeRegistries(registry string) npmRegistries {
	return npmRegistries{Scopes: map[string]string{"@putnami": registry}}
}

// writeScopeNpmrc writes only the CREDENTIAL line: token discovery still reads
// the .npmrc, exactly as npm does. The registry itself comes from the workspace
// declaration.
func writeScopeNpmrc(t *testing.T, dir, registry, token string) {
	t.Helper()
	content := ""
	if token != "" {
		content += "//" + strings.TrimPrefix(registry, "http://") + "/:_authToken=" + token + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".npmrc"), []byte(content), 0o600); err != nil {
		t.Fatalf("write .npmrc: %v", err)
	}
}

func quoteList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, `"`+item+`"`)
	}
	return strings.Join(quoted, ",")
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
