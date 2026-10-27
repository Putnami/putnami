package workspace

import (
	"reflect"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// A project owns a changed file only through its own directory: the nearest
// project holds a nested file, the root project holds what no deeper project
// does, and an asset claim adds no file. Each list is project-relative, sorted
// and unique, whatever spelling and order the diff used.
func TestOwnedChangedFiles_DirectoryOwnerOnly(t *testing.T) {
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/root", Name: "root", Path: "."},
		{ID: "/apps/site", Name: "site", Path: "apps/site"},
		{ID: "/apps/site/theme", Name: "theme", Path: "apps/site/theme"},
		{
			ID:   "/docs",
			Name: "docs",
			Path: "docs",
			Config: &wsproto.ProjectConfig{
				Build: &wsproto.BuildConfig{Assets: []wsproto.BuildAsset{{From: "/apps/site/assets"}}},
			},
		},
		{ID: "/lib/core", Name: "core", Path: "lib/core"},
	})

	got := OwnedChangedFiles(ws, []string{
		"apps/site/theme/main.css",
		"apps/site/index.html",
		"apps/site/b/x.go",
		"./apps/site/index.html",
		"README.md",
		"apps/site/assets/logo.svg",
		"apps/site/a.go",
	})
	want := map[string][]string{
		"/root":            {"README.md"},
		"/apps/site":       {"a.go", "assets/logo.svg", "b/x.go", "index.html"},
		"/apps/site/theme": {"main.css"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OwnedChangedFiles = %v, want %v", got, want)
	}
}

// No diff is no answer: a nil or empty list, and a nil workspace, own nothing.
func TestOwnedChangedFiles_NoDiffOwnsNothing(t *testing.T) {
	ws := NewWorkspace("/workspace", nil, []*Project{{ID: "/lib/core", Name: "core", Path: "lib/core"}})
	for name, files := range map[string][]string{"nil": nil, "empty": {}} {
		if got := OwnedChangedFiles(ws, files); got != nil {
			t.Errorf("%s: OwnedChangedFiles = %v, want nil", name, got)
		}
	}
	if got := OwnedChangedFiles(nil, []string{"lib/core/a.go"}); got != nil {
		t.Errorf("nil workspace: OwnedChangedFiles = %v, want nil", got)
	}
	if got := OwnedChangedFiles(ws, []string{"other/a.go"}); got != nil {
		t.Errorf("unowned file: OwnedChangedFiles = %v, want nil", got)
	}
}
