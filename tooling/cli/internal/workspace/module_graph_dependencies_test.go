package workspace

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// A target is a module-graph dependency only when every backing of it is a
// module-graph entry: an import, a putnami.json entry (merged into the
// provider's entry or spelled apart), a contract edge or a declared file input
// that reads it each keep it a dependency the code takes.
func TestModuleGraphDependenciesKeepEveryOtherBacking(t *testing.T) {
	root := t.TempDir()
	for _, module := range []string{"libs/core", "libs/client", "services/catalog"} {
		writeFileAt(t, filepath.Join(root, filepath.FromSlash(module), "go.mod"), "module acme/"+module+"\n")
	}
	moduleGraph := func(entries ...string) map[string]wsproto.DependencySource {
		sources := map[string]wsproto.DependencySource{}
		for _, entry := range entries {
			sources[entry] = wsproto.DependencySourceModuleGraph
		}
		return sources
	}
	core := &Project{ID: "/libs/core", Name: "acme/core", Path: "libs/core"}
	client := &Project{ID: "/libs/client", Name: "acme/client", Path: "libs/client"}
	catalog := &Project{ID: "/services/catalog", Name: "acme/catalog", Path: "services/catalog",
		ContractServiceID: "catalog"}
	projects := []*Project{core, client, catalog,
		// An indirect require alone: a module-graph dependency.
		{ID: "/apps/indirect", Name: "acme/indirect", Path: "apps/indirect",
			Dependencies: []string{"acme/core"}, DependencySources: moduleGraph("acme/core")},
		// The same target reached by a module-graph entry and an import.
		{ID: "/apps/imported", Name: "acme/imported", Path: "apps/imported",
			Dependencies: []string{"/libs/core", "acme/core"},
			DependencySources: map[string]wsproto.DependencySource{
				"/libs/core": wsproto.DependencySourceModuleGraph, "acme/core": wsproto.DependencySourceGoModule}},
		// A putnami.json entry the provider's module-graph entry merged into.
		{ID: "/apps/authored", Name: "acme/authored", Path: "apps/authored",
			Config:       &wsproto.ProjectConfig{Name: "acme/authored", Dependencies: []string{"acme/core"}},
			Dependencies: []string{"acme/core"}, DependencySources: moduleGraph("acme/core")},
		// A declared file input that reads inside the target.
		{ID: "/apps/reader", Name: "acme/reader", Path: "apps/reader",
			Config:       readerConfig(t),
			Dependencies: []string{"acme/core", "acme/client"}, DependencySources: moduleGraph("acme/core", "acme/client")},
		// A generated client whose provider its go.mod also requires indirectly.
		{ID: "/clients/catalog-go", Name: "acme/catalog-client", Path: "clients/catalog-go",
			Dependencies: []string{"acme/catalog"}, DependencySources: moduleGraph("acme/catalog"),
			GeneratedClient: &GeneratedClientBinding{ServiceID: "catalog"}},
	}
	got := ModuleGraphDependencies(NewWorkspace(root, nil, projects))
	want := map[string][]string{
		"/apps/indirect": {"/libs/core"},
		"/apps/reader":   {"/libs/client"},
	}
	if len(got) != len(want) {
		t.Fatalf("module-graph dependencies = %v, want %v", got, want)
	}
	for id, ids := range want {
		if !slices.Equal(got[id], ids) {
			t.Errorf("%s module-graph dependencies = %v, want %v", id, got[id], ids)
		}
	}
}

// readerConfig is a putnami.json whose test layer reads inside libs/core.
func readerConfig(t *testing.T) *wsproto.ProjectConfig {
	t.Helper()
	var config wsproto.ProjectConfig
	if err := json.Unmarshal([]byte(`{"name":"acme/reader","options":{"test":{"filePatterns":["../../libs/core/**"]}}}`), &config); err != nil {
		t.Fatal(err)
	}
	return &config
}
