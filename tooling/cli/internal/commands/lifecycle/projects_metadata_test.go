package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

// setupTaggableProject creates a minimal workspace containing one project and
// returns the workspace root and the path to its putnami.json.
//
// The tags live in putnami.json. They used to be
// written into a package.json `putnami` section, which made the answer depend on
// which language the project happened to be written in and put core's authored
// values inside a manifest another writer owns.
func setupTaggableProject(t *testing.T, initialTags string) (string, string) {
	t.Helper()
	ws := t.TempDir()

	wsCfg := filepath.Join(ws, "putnami.workspace.json")
	if err := os.WriteFile(wsCfg, []byte(`{"includes":["packages/api"]}`), 0o644); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}

	projDir := filepath.Join(ws, "packages", "api")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	cfg := `{"name":"@demo/api"` + initialTags + `}`
	cfgPath := filepath.Join(projDir, "putnami.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write putnami.json: %v", err)
	}
	workspace.InvalidateLoadCache(ws)
	t.Cleanup(func() { workspace.InvalidateLoadCache(ws) })
	return ws, cfgPath
}

// readPutnamiTags reads the tags array from a project's putnami.json.
func readPutnamiTags(t *testing.T, cfgPath string) []string {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read putnami.json: %v", err)
	}
	var raw struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse putnami.json: %v", err)
	}
	return raw.Tags
}

func tagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProjectsTag_Add(t *testing.T) {
	ws, pkgPath := setupTaggableProject(t, "")

	if err := ProjectsTag(ws, nil, []string{"@demo/api", "backend,go"}); err != nil {
		t.Fatalf("ProjectsTag add: %v", err)
	}

	// Stored sorted.
	if got := readPutnamiTags(t, pkgPath); !tagsEqual(got, []string{"backend", "go"}) {
		t.Errorf("tags after add = %v, want [backend go]", got)
	}
}

func TestProjectsTag_AddDedupes(t *testing.T) {
	ws, pkgPath := setupTaggableProject(t, `,"tags":["go"]`)

	if err := ProjectsTag(ws, nil, []string{"@demo/api", "go,api"}); err != nil {
		t.Fatalf("ProjectsTag add: %v", err)
	}
	if got := readPutnamiTags(t, pkgPath); !tagsEqual(got, []string{"api", "go"}) {
		t.Errorf("tags after dedup add = %v, want [api go]", got)
	}
}

func TestProjectsTag_Remove(t *testing.T) {
	ws, pkgPath := setupTaggableProject(t, `,"tags":["api","backend","go"]`)

	if err := ProjectsTag(ws, nil, []string{"@demo/api", "backend", "--remove"}); err != nil {
		t.Fatalf("ProjectsTag remove: %v", err)
	}
	if got := readPutnamiTags(t, pkgPath); !tagsEqual(got, []string{"api", "go"}) {
		t.Errorf("tags after remove = %v, want [api go]", got)
	}
}

func TestProjectsTag_Set(t *testing.T) {
	ws, pkgPath := setupTaggableProject(t, `,"tags":["old"]`)

	if err := ProjectsTag(ws, nil, []string{"@demo/api", "x,y", "--set"}); err != nil {
		t.Fatalf("ProjectsTag set: %v", err)
	}
	if got := readPutnamiTags(t, pkgPath); !tagsEqual(got, []string{"x", "y"}) {
		t.Errorf("tags after set = %v, want [x y]", got)
	}
}

func TestProjectsTag_RemoveAllClears(t *testing.T) {
	ws, pkgPath := setupTaggableProject(t, `,"tags":["only"]`)

	if err := ProjectsTag(ws, nil, []string{"@demo/api", "only", "--remove"}); err != nil {
		t.Fatalf("ProjectsTag remove last: %v", err)
	}
	// Clearing all tags leaves no tag behind. The field itself SURVIVES, as an
	// explicit empty array — see TestProjectsTag_RemoveAllClearsProviderTags for
	// why the difference between "cleared" and "absent" has to be on disk.
	if got := readPutnamiTags(t, pkgPath); len(got) != 0 {
		t.Errorf("tags after clearing = %v, want none", got)
	}
}

// recordProviderTags writes a workspace index whose single provider reports tags
// for a project, which is where a TypeScript project's tags actually live: in
// package.json's `putnami.tags`, reported by the TS probe and unioned into the
// merged view by MergeProbeResults. Load adopts that recorded view, so this is
// the production path with the extension process taken out of it.
func recordProviderTags(t *testing.T, wsRoot, projectPath string, tags []string) {
	t.Helper()
	index := map[string]any{
		"version":        1,
		"probeDigest":    "wp1:test",
		"identityDigest": "wsid1:test",
		"providers": []map[string]any{{
			"extension": "@fixture/ts",
			"digest":    "wp1:test",
			"result": map[string]any{
				"version":   1,
				"extension": "@fixture/ts",
				"projects": []map[string]any{{
					"path": projectPath,
					"tags": tags,
				}},
			},
		}},
	}
	encoded, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wsRoot, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, ".putnami", "workspace-index.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(wsRoot)
}

// Clearing the LAST tag of a project whose tags come from the provider view must
// actually clear them.
//
// It used to be a silent no-op: clearing left tagValue nil, so
// UpdateProjectConfigField DELETED the `tags` key — and for a project whose tags
// the provider also reports, resolveProjectIdentity's `len(p.Tags) == 0`
// fallthrough could not tell "explicitly cleared" from "never authored" and
// handed the provider's tags straight back. Verified against the real command:
// `tags=[api web]` → "tags cleared" → `tags=[api web]`.
//
// The existing fixtures never caught it because they seed tags into putnami.json
// only, where there is no second source to fall back to.
func TestProjectsTag_RemoveAllClearsProviderTags(t *testing.T) {
	ws, cfgPath := setupTaggableProject(t, "")
	recordProviderTags(t, ws, "packages/api", []string{"api", "web"})
	t.Cleanup(func() { workspace.InvalidateLoadCache(ws) })

	loaded, err := workspace.Load(ws)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if got := loaded.ProjectByName("@demo/api").Tags; !tagsEqual(got, []string{"api", "web"}) {
		t.Fatalf("fixture tags = %v, want the provider view's [api web]", got)
	}

	if err := ProjectsTag(ws, nil, []string{"@demo/api", "api,web", "--remove"}); err != nil {
		t.Fatalf("ProjectsTag remove all: %v", err)
	}

	// On disk: the authored statement "no tags", not an absent key.
	if got := readPutnamiTags(t, cfgPath); len(got) != 0 {
		t.Errorf("tags in putnami.json after clearing = %v, want none", got)
	}
	if !strings.Contains(readText(t, cfgPath), `"tags"`) {
		t.Errorf("the cleared state is not representable — the key was deleted:\n%s", readText(t, cfgPath))
	}

	// And resolved: the provider view must not put them back.
	workspace.InvalidateLoadCache(ws)
	recleared, err := workspace.Load(ws)
	if err != nil {
		t.Fatalf("reload workspace: %v", err)
	}
	if got := recleared.ProjectByName("@demo/api").Tags; len(got) != 0 {
		t.Fatalf("tags came back from the provider view after an explicit clear: %v", got)
	}
}

// A PARTIAL remove still leaves an authored non-empty list, which bypasses the
// fallthrough entirely — the nil-vs-empty distinction must not change that.
func TestProjectsTag_PartialRemoveKeepsTheRest(t *testing.T) {
	ws, cfgPath := setupTaggableProject(t, "")
	recordProviderTags(t, ws, "packages/api", []string{"api", "web"})
	t.Cleanup(func() { workspace.InvalidateLoadCache(ws) })

	if err := ProjectsTag(ws, nil, []string{"@demo/api", "web", "--remove"}); err != nil {
		t.Fatalf("ProjectsTag remove: %v", err)
	}
	if got := readPutnamiTags(t, cfgPath); !tagsEqual(got, []string{"api"}) {
		t.Fatalf("tags after partial remove = %v, want [api]", got)
	}

	workspace.InvalidateLoadCache(ws)
	reloaded, err := workspace.Load(ws)
	if err != nil {
		t.Fatalf("reload workspace: %v", err)
	}
	if got := reloaded.ProjectByName("@demo/api").Tags; !tagsEqual(got, []string{"api"}) {
		t.Fatalf("resolved tags after partial remove = %v, want [api]", got)
	}
}

// A project that never authored tags still inherits the provider's — the clear
// is what suppresses them, not the mere presence of a provider view.
func TestProjectsTag_UnauthoredTagsStillComeFromTheProviderView(t *testing.T) {
	ws, _ := setupTaggableProject(t, "")
	recordProviderTags(t, ws, "packages/api", []string{"api", "web"})
	t.Cleanup(func() { workspace.InvalidateLoadCache(ws) })

	loaded, err := workspace.Load(ws)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if got := loaded.ProjectByName("@demo/api").Tags; !tagsEqual(got, []string{"api", "web"}) {
		t.Fatalf("tags = %v, want the provider view's [api web]", got)
	}
}

func TestProjectsTag_MissingNameIsUsageError(t *testing.T) {
	ws, _ := setupTaggableProject(t, "")
	err := ProjectsTag(ws, nil, nil)
	if err == nil {
		t.Fatal("ProjectsTag with no project name should error")
	}
}

func TestProjectsTag_UnknownProjectNotFound(t *testing.T) {
	ws, _ := setupTaggableProject(t, "")
	err := ProjectsTag(ws, nil, []string{"@demo/nope", "x"})
	if err == nil {
		t.Fatal("ProjectsTag with unknown project should error")
	}
}
