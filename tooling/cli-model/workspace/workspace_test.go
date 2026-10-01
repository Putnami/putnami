package workspace

import (
	"strings"
	"testing"
)

func TestWorkspaceWarningCodesDoNotDependOnMessageText(t *testing.T) {
	ws := NewWorkspace("", nil, nil)
	ws.AddWarning(WarningCodeProviderViewUnavailable, "first wording")
	ws.AddWarning(WarningCodeProviderViewUnavailable, "reworded")
	if !ws.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Fatal("typed provider-view warning was not retained")
	}
	if len(ws.Warnings) != 2 || len(ws.WarningCodes) != 1 {
		t.Fatalf("warnings=%v codes=%v", ws.Warnings, ws.WarningCodes)
	}
}

func TestWorkspace_ProjectByName(t *testing.T) {
	ws := NewWorkspace("", nil, []*Project{
		{Name: "app", Path: "apps/app"},
		{Name: "lib", Path: "libs/lib"},
	})

	got := ws.ProjectByName("app")
	if got == nil || got.Name != "app" {
		t.Error("ProjectByName(app) should find project")
	}

	got = ws.ProjectByName("nonexistent")
	if got != nil {
		t.Error("ProjectByName(nonexistent) should return nil")
	}
}

func TestWorkspace_ProjectByID(t *testing.T) {
	ws := &Workspace{
		Projects: []*Project{
			{ID: "/apps/app", Name: "app", Path: "apps/app"},
			{ID: "/libs/lib", Name: "lib", Path: "libs/lib"},
		},
		projectByID: map[string]*Project{},
	}
	for _, p := range ws.Projects {
		ws.projectByID[p.ID] = p
	}

	got := ws.ProjectByID("/apps/app")
	if got == nil || got.Name != "app" {
		t.Error("ProjectByID(/apps/app) should find project")
	}

	got = ws.ProjectByID("/nonexistent")
	if got != nil {
		t.Error("ProjectByID(/nonexistent) should return nil")
	}

	// Name should NOT match as ID
	got = ws.ProjectByID("app")
	if got != nil {
		t.Error("ProjectByID(app) should return nil — bare name is not an ID")
	}
}

func TestWorkspace_ProjectsByPattern_Exact(t *testing.T) {
	ws := &Workspace{
		Projects: []*Project{
			{ID: "/typescript/frameworks/application", Name: "@putnami/application", Path: "typescript/frameworks/application"},
			{ID: "/typescript/frameworks/web", Name: "@putnami/web", Path: "typescript/frameworks/web"},
			{ID: "/go/frameworks/http", Name: "go.putnami.dev/http", Path: "go/frameworks/http"},
		},
		projectByID: map[string]*Project{},
	}
	for _, p := range ws.Projects {
		ws.projectByID[p.ID] = p
	}

	result := ws.ProjectsByPattern("/typescript/frameworks/application")
	if len(result) != 1 || result[0].ID != "/typescript/frameworks/application" {
		t.Errorf("exact match: got %d projects, want 1", len(result))
	}

	result = ws.ProjectsByPattern("/nonexistent")
	if len(result) != 0 {
		t.Errorf("nonexistent: got %d projects, want 0", len(result))
	}
}

func TestWorkspace_ProjectsByPattern_Recursive(t *testing.T) {
	ws := &Workspace{
		Projects: []*Project{
			{ID: "/typescript/frameworks/application", Path: "typescript/frameworks/application"},
			{ID: "/typescript/frameworks/web", Path: "typescript/frameworks/web"},
			{ID: "/typescript/frameworks/utils", Path: "typescript/frameworks/utils"},
			{ID: "/typescript/samples/hello", Path: "typescript/samples/hello"},
			{ID: "/go/frameworks/http", Path: "go/frameworks/http"},
		},
	}

	// /typescript/frameworks/... should match all 3 TS frameworks
	result := ws.ProjectsByPattern("/typescript/frameworks/...")
	if len(result) != 3 {
		t.Errorf("/typescript/frameworks/...: got %d projects, want 3", len(result))
	}

	// /typescript/... should match all 4 TS projects
	result = ws.ProjectsByPattern("/typescript/...")
	if len(result) != 4 {
		t.Errorf("/typescript/...: got %d projects, want 4", len(result))
	}

	// /go/... should match 1 Go project
	result = ws.ProjectsByPattern("/go/...")
	if len(result) != 1 {
		t.Errorf("/go/...: got %d projects, want 1", len(result))
	}

	// /nonexistent/... should match nothing
	result = ws.ProjectsByPattern("/nonexistent/...")
	if len(result) != 0 {
		t.Errorf("/nonexistent/...: got %d projects, want 0", len(result))
	}
}

func TestWorkspace_ProjectsByPattern_RecursiveSelf(t *testing.T) {
	// /typescript/... should also include /typescript itself if it's a project
	ws := &Workspace{
		Projects: []*Project{
			{ID: "/typescript", Path: "typescript"},
			{ID: "/typescript/frameworks/web", Path: "typescript/frameworks/web"},
		},
	}

	result := ws.ProjectsByPattern("/typescript/...")
	if len(result) != 2 {
		t.Errorf("/typescript/...: got %d projects, want 2 (including /typescript itself)", len(result))
	}
}

func TestWorkspace_ProjectsByPattern_SuffixWildcard(t *testing.T) {
	ws := &Workspace{
		Projects: []*Project{
			{ID: "/typescript/frameworks/web", Path: "typescript/frameworks/web"},
			{ID: "/typescript/frameworks/utils", Path: "typescript/frameworks/utils"},
			{ID: "/go/frameworks/http", Path: "go/frameworks/http"},
			{ID: "/sites/web", Path: "sites/web"},
		},
	}

	// .../web should match projects ending in /web
	result := ws.ProjectsByPattern(".../web")
	if len(result) != 2 {
		t.Errorf(".../web: got %d projects, want 2", len(result))
	}
	for _, p := range result {
		if p.ID != "/typescript/frameworks/web" && p.ID != "/sites/web" {
			t.Errorf("unexpected match: %s", p.ID)
		}
	}

	// .../frameworks/web should match more specifically
	result = ws.ProjectsByPattern(".../frameworks/web")
	if len(result) != 1 || result[0].ID != "/typescript/frameworks/web" {
		t.Errorf(".../frameworks/web: got %v, want [/typescript/frameworks/web]", result)
	}

	// .../nonexistent should match nothing
	result = ws.ProjectsByPattern(".../nonexistent")
	if len(result) != 0 {
		t.Errorf(".../nonexistent: got %d projects, want 0", len(result))
	}
}

func TestWorkspace_ProjectsByPattern_InfixWildcard(t *testing.T) {
	ws := &Workspace{
		Projects: []*Project{
			{ID: "/typescript/frameworks/web", Path: "typescript/frameworks/web"},
			{ID: "/typescript/frameworks/utils", Path: "typescript/frameworks/utils"},
			{ID: "/go/frameworks/http", Path: "go/frameworks/http"},
			{ID: "/go/frameworks/app", Path: "go/frameworks/app"},
			{ID: "/typescript/samples/hello", Path: "typescript/samples/hello"},
			{ID: "/tooling/cli", Path: "tooling/cli"},
		},
	}

	// .../frameworks/... should match all projects under any frameworks/ dir
	result := ws.ProjectsByPattern(".../frameworks/...")
	if len(result) != 4 {
		t.Errorf(".../frameworks/...: got %d projects, want 4", len(result))
	}
	for _, p := range result {
		if !strings.Contains(p.ID, "/frameworks/") {
			t.Errorf("unexpected match: %s", p.ID)
		}
	}

	// .../samples/... should match only the samples project
	result = ws.ProjectsByPattern(".../samples/...")
	if len(result) != 1 || result[0].ID != "/typescript/samples/hello" {
		t.Errorf(".../samples/...: got %v, want [/typescript/samples/hello]", result)
	}

	// .../nonexistent/... should match nothing
	result = ws.ProjectsByPattern(".../nonexistent/...")
	if len(result) != 0 {
		t.Errorf(".../nonexistent/...: got %d projects, want 0", len(result))
	}
}
