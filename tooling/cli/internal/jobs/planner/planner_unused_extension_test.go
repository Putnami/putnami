package planner

import (
	"reflect"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// UnusedExtensions names only the projects that use no extension serving a
// command, keeps the commands in the order asked, and names nothing for a
// project another serving extension already covers, for a workspace-wide
// job, for a disabled job or extension, or for a command no extension
// declares.
func TestUnusedExtensions(t *testing.T) {
	t.Parallel()
	site := &workspace.Project{ID: "/site", Name: "site", Extensions: []string{"@putnami/typescript"}}
	docs := &workspace.Project{ID: "/docs", Name: "docs"}
	web := &workspace.Project{ID: "/web", Name: "web", Extensions: []string{"@putnami/web"}}
	webExtension := &extension.ExtensionDescription{Name: "@putnami/web", Jobs: map[string]*extension.JobDefinition{
		"web-i18n-sync":      {Name: "web-i18n-sync", ExtensionName: "@putnami/web"},
		"web-i18n-translate": {Name: "web-i18n-translate", ExtensionName: "@putnami/web"},
	}}
	only := []*extension.ExtensionDescription{webExtension}

	got := UnusedExtensions([]string{"web-i18n-translate", "web-i18n-sync"}, []*workspace.Project{site, web, docs}, only, nil, nil)
	want := []UnusedExtension{{
		Name:     "@putnami/web",
		Commands: []string{"web-i18n-translate", "web-i18n-sync"},
		Projects: []*workspace.Project{site, docs},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnusedExtensions = %+v, want %+v", got, want)
	}

	none := map[string]struct {
		commands   []string
		projects   []*workspace.Project
		extensions []*extension.ExtensionDescription
		jobs, exts []string
	}{
		"a project that uses the extension": {[]string{"web-i18n-sync"}, []*workspace.Project{web}, only, nil, nil},
		"a command no extension declares":   {[]string{"build"}, []*workspace.Project{site}, only, nil, nil},
		"another serving extension the project uses": {[]string{"serve"}, []*workspace.Project{site}, []*extension.ExtensionDescription{
			{Name: "@putnami/typescript", Jobs: map[string]*extension.JobDefinition{"serve": {Name: "serve", ExtensionName: "@putnami/typescript"}}},
			{Name: "@putnami/go", Jobs: map[string]*extension.JobDefinition{"serve": {Name: "serve", ExtensionName: "@putnami/go"}}},
		}, nil, nil},
		"a workspace-wide job": {[]string{"web-i18n-sync"}, []*workspace.Project{site}, []*extension.ExtensionDescription{
			webExtension,
			{Name: "@acme/audit", Jobs: map[string]*extension.JobDefinition{"web-i18n-sync": {Name: "web-i18n-sync", ExtensionName: "@acme/audit", Activation: "workspace"}}},
		}, nil, nil},
		"a disabled extension": {[]string{"web-i18n-sync"}, []*workspace.Project{site}, only, nil, []string{"@putnami/web"}},
		"a disabled job":       {[]string{"web-i18n-sync"}, []*workspace.Project{site}, only, []string{"@putnami/web:web-i18n-sync"}, nil},
	}
	for name, tc := range none {
		if got := UnusedExtensions(tc.commands, tc.projects, tc.extensions, tc.jobs, tc.exts); len(got) != 0 {
			t.Errorf("%s: UnusedExtensions = %+v, want none", name, got)
		}
	}
}
