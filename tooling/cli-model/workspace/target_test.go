package workspace

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

// targetTestRoot is the fixture workspace root, absolute on this platform:
// /workspace on Unix and \workspace on the current drive on Windows, where a
// path without a volume is not absolute and ResolveTarget's canonical cwd would
// never share a prefix with it.
var targetTestRoot = func() string {
	root, err := filepath.Abs(filepath.FromSlash("/workspace"))
	if err != nil {
		panic(err)
	}
	return root
}()

// targetTestPath is rel, a slash-separated path, under targetTestRoot.
func targetTestPath(rel string) string {
	return filepath.Join(targetTestRoot, filepath.FromSlash(rel))
}

func makeTargetTestWorkspace() *Workspace {
	projects := []*Project{
		{ID: "/typescript/framework/application", Name: "@putnami/application", Path: "typescript/framework/application", Tags: []string{"ts"}},
		{ID: "/typescript/framework/web", Name: "@putnami/web", Path: "typescript/framework/web", Tags: []string{"ts"}},
		{ID: "/typescript/framework/utils", Name: "@putnami/utils", Path: "typescript/framework/utils", Tags: []string{"ts"}},
		{ID: "/typescript/samples/hello", Name: "@example/hello", Path: "typescript/samples/hello", Tags: []string{"ts", "e2e"}},
		{ID: "/go/framework/http", Name: "go.putnami.dev/http", Path: "go/framework/http", Tags: []string{"go"}},
		{ID: "/go/framework/app", Name: "go.putnami.dev/app", Path: "go/framework/app", Tags: []string{"go"}},
		{ID: "/tooling/cli", Name: "@putnami/cli", Path: "tooling/cli", Tags: []string{"go"}},
	}

	ws := &Workspace{
		Root:        targetTestRoot,
		Projects:    projects,
		projectByID: make(map[string]*Project, len(projects)),
		projectMap:  make(map[string]*Project, len(projects)),
		Config:      &wsproto.Config{},
	}
	for _, p := range projects {
		ws.projectByID[p.ID] = p
		ws.projectMap[p.Name] = p
	}
	return ws
}

func projectIDs(projects []*Project) []string {
	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ID
	}
	return ids
}

func TestResolveTarget_ExactID(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "exact-ids-and-paths-resolve")
	ws := makeTargetTestWorkspace()
	result := ResolveTarget(ws, "/typescript/framework/application", "", nil)
	if len(result) != 1 || result[0].ID != "/typescript/framework/application" {
		t.Errorf("exact ID: got %v", projectIDs(result))
	}
}

func TestResolveTarget_RecursiveWildcard(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "wildcards-resolve")
	ws := makeTargetTestWorkspace()

	result := ResolveTarget(ws, "/typescript/framework/...", "", nil)
	if len(result) != 3 {
		t.Errorf("/typescript/framework/...: got %d projects %v, want 3", len(result), projectIDs(result))
	}

	result = ResolveTarget(ws, "/typescript/...", "", nil)
	if len(result) != 4 {
		t.Errorf("/typescript/...: got %d projects, want 4", len(result))
	}

	result = ResolveTarget(ws, "/go/...", "", nil)
	if len(result) != 2 {
		t.Errorf("/go/...: got %d projects, want 2", len(result))
	}
}

func TestResolveTarget_Union(t *testing.T) {
	ws := makeTargetTestWorkspace()
	result := ResolveTarget(ws, "/tooling/cli,/go/framework/http", "", nil)
	if len(result) != 2 {
		t.Errorf("union: got %d projects %v, want 2", len(result), projectIDs(result))
	}
}

func TestResolveTarget_Subtraction(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "subtraction-removes-its-operand")
	ws := makeTargetTestWorkspace()
	// All typescript minus samples
	result := ResolveTarget(ws, "/typescript/...,-/typescript/samples/...", "", nil)
	if len(result) != 3 {
		t.Errorf("subtraction: got %d projects %v, want 3", len(result), projectIDs(result))
	}
	for _, p := range result {
		if p.ID == "/typescript/samples/hello" {
			t.Error("samples/hello should be excluded")
		}
	}
}

func TestResolveTarget_RelativePath(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "exact-ids-and-paths-resolve")
	ws := makeTargetTestWorkspace()
	// From typescript/framework/, ./application resolves to /typescript/framework/application
	cwd := targetTestPath("typescript/framework")
	result := ResolveTarget(ws, "./application", cwd, nil)
	if len(result) != 1 || result[0].ID != "/typescript/framework/application" {
		t.Errorf("relative: got %v, want [/typescript/framework/application]", projectIDs(result))
	}
}

func TestResolveTarget_RelativePathFromSibling(t *testing.T) {
	ws := makeTargetTestWorkspace()
	// From typescript/framework/web, ../application resolves to /typescript/framework/application
	cwd := targetTestPath("typescript/framework/web")
	result := ResolveTarget(ws, "../application", cwd, nil)
	if len(result) != 1 || result[0].ID != "/typescript/framework/application" {
		t.Errorf("relative sibling: got %v, want [/typescript/framework/application]", projectIDs(result))
	}
}

func TestResolveTarget_RelativeWildcard(t *testing.T) {
	ws := makeTargetTestWorkspace()
	// From typescript/samples/hello, ../framework/... resolves to /typescript/framework/...
	cwd := targetTestPath("typescript/samples/hello")
	result := ResolveTarget(ws, "../../framework/...", cwd, nil)
	if len(result) != 3 {
		t.Errorf("relative wildcard: got %d projects %v, want 3", len(result), projectIDs(result))
	}
}

func TestResolveTarget_TransparentGroupFolderPaths(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "canonical-identity", "grouped-paths-resolve-to-logical-ids")
	projects := []*Project{
		{ID: "/identity/auth-server", Name: "auth-server", Path: "identity/(workloads)/auth-server"},
		{ID: "/identity/worker", Name: "worker", Path: "identity/(workloads)/worker"},
		{ID: "/identity/identity-client", Name: "identity-client", Path: "identity/(libs)/identity-client"},
	}
	ws := NewWorkspace(targetTestRoot, &wsproto.Config{}, projects)

	tests := []struct {
		name string
		expr string
		cwd  string
		want []string
	}{
		{name: "logical ID", expr: "/identity/auth-server", cwd: targetTestRoot, want: []string{"/identity/auth-server"}},
		{name: "logical wildcard", expr: "/identity/...", cwd: targetTestRoot, want: []string{"/identity/auth-server", "/identity/worker", "/identity/identity-client"}},
		{name: "relative physical path", expr: "./(workloads)/auth-server", cwd: targetTestPath("identity"), want: []string{"/identity/auth-server"}},
		{name: "relative physical wildcard", expr: "./(workloads)/...", cwd: targetTestPath("identity"), want: []string{"/identity/auth-server", "/identity/worker"}},
		{name: "current physical directory", expr: ".", cwd: targetTestPath("identity/(workloads)/auth-server"), want: []string{"/identity/auth-server"}},
		{name: "physical path is not an ID alias", expr: "/identity/(workloads)/auth-server", cwd: targetTestRoot, want: nil},
		{name: "former path-derived ID is not an alias", expr: "/identity/workloads/auth-server", cwd: targetTestRoot, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := projectIDs(ResolveTarget(ws, tt.expr, tt.cwd, nil))
			if len(got) != len(tt.want) {
				t.Fatalf("ResolveTarget(%q) = %v, want %v", tt.expr, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ResolveTarget(%q)[%d] = %q, want %q", tt.expr, i, got[i], tt.want[i])
				}
			}
		})
	}

	index := &wsproto.ScopeIndex{
		ProjectAliases: map[string]string{"auth": "/identity/auth-server"},
		Groups:         map[string]string{"identity-workloads": "/identity/auth-server,/identity/worker"},
	}
	if got := projectIDs(ResolveTarget(ws, "auth", targetTestRoot, index)); len(got) != 1 || got[0] != "/identity/auth-server" {
		t.Errorf("logical ID alias resolved to %v", got)
	}
	if got := projectIDs(ResolveTarget(ws, "identity-workloads", targetTestRoot, index)); len(got) != 2 || got[0] != "/identity/auth-server" || got[1] != "/identity/worker" {
		t.Errorf("logical ID group resolved to %v", got)
	}
}

func TestResolveTarget_Alias(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "aliases-and-groups-resolve")
	ws := makeTargetTestWorkspace()
	index := &wsproto.ScopeIndex{
		ProjectAliases: map[string]string{
			"app": "/typescript/framework/application",
			"cli": "/tooling/cli",
		},
		Groups: map[string]string{},
	}

	result := ResolveTarget(ws, "app", "", index)
	if len(result) != 1 || result[0].ID != "/typescript/framework/application" {
		t.Errorf("alias: got %v", projectIDs(result))
	}
}

func TestResolveTarget_Group(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "aliases-and-groups-resolve")
	ws := makeTargetTestWorkspace()
	index := &wsproto.ScopeIndex{
		ProjectAliases: map[string]string{},
		Groups: map[string]string{
			"ts-fw": "/typescript/framework/...",
			"go-fw": "/go/framework/...",
		},
	}

	result := ResolveTarget(ws, "ts-fw", "", index)
	if len(result) != 3 {
		t.Errorf("group: got %d projects %v, want 3", len(result), projectIDs(result))
	}
}

func TestResolveTarget_WorkspaceConfigAliases(t *testing.T) {
	ws := makeTargetTestWorkspace()
	ws.Config.ProjectAliases = map[string]string{
		"site-cli": "/tooling/cli",
	}

	result := ResolveTarget(ws, "site-cli", "", nil)
	if len(result) != 1 || result[0].ID != "/tooling/cli" {
		t.Errorf("workspace alias: got %v", projectIDs(result))
	}
}

func TestResolveTarget_LegacyNameFallback(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "canonical-identity", "legacy-names-resolve-as-a-fallback")
	ws := makeTargetTestWorkspace()
	// Should still match by Name for backward compat
	result := ResolveTarget(ws, "@putnami/web", "", nil)
	if len(result) != 1 || result[0].ID != "/typescript/framework/web" {
		t.Errorf("legacy name: got %v", projectIDs(result))
	}
}

func TestResolveTarget_CurrentProject(t *testing.T) {
	ws := makeTargetTestWorkspace()
	cwd := targetTestPath("tooling/cli")
	result := ResolveTarget(ws, ".", cwd, nil)
	if len(result) != 1 || result[0].ID != "/tooling/cli" {
		t.Errorf("current project: got %v", projectIDs(result))
	}
}

func TestResolveTarget_Empty(t *testing.T) {
	ws := makeTargetTestWorkspace()
	cwd := targetTestPath("tooling/cli")
	result := ResolveTarget(ws, "", cwd, nil)
	if len(result) != 1 || result[0].ID != "/tooling/cli" {
		t.Errorf("empty target: got %v, want current project", projectIDs(result))
	}
}

func TestResolveTarget_Nonexistent(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "an-unknown-target-is-an-error")
	ws := makeTargetTestWorkspace()
	result := ResolveTarget(ws, "/nonexistent/project", "", nil)
	if len(result) != 0 {
		t.Errorf("nonexistent: got %v, want empty", projectIDs(result))
	}
}

func TestResolveTarget_PreservesOrder(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "unions-preserve-order-and-dedupe")
	ws := makeTargetTestWorkspace()
	// Union of multiple — should follow ws.Projects order
	result := ResolveTarget(ws, "/tooling/cli,/typescript/framework/application", "", nil)
	if len(result) != 2 {
		t.Fatalf("got %d, want 2", len(result))
	}
	// application comes before cli in ws.Projects
	if result[0].ID != "/typescript/framework/application" {
		t.Errorf("first = %s, want /typescript/framework/application (ws.Projects order)", result[0].ID)
	}
	if result[1].ID != "/tooling/cli" {
		t.Errorf("second = %s, want /tooling/cli", result[1].ID)
	}
}

func TestResolveTarget_DeduplicatesUnion(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "unions-preserve-order-and-dedupe")
	ws := makeTargetTestWorkspace()
	// Same project via two paths — should only appear once
	result := ResolveTarget(ws, "/tooling/cli,/tooling/cli", "", nil)
	if len(result) != 1 {
		t.Errorf("deduplicate: got %d, want 1", len(result))
	}
}

func TestResolveTarget_SuffixWildcard(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "wildcards-resolve")
	ws := makeTargetTestWorkspace()

	// .../web should match projects ending in /web
	result := ResolveTarget(ws, ".../web", "", nil)
	if len(result) != 1 || result[0].ID != "/typescript/framework/web" {
		t.Errorf(".../web: got %v, want [/typescript/framework/web]", projectIDs(result))
	}

	// .../framework/web should match more specifically
	result = ResolveTarget(ws, ".../framework/web", "", nil)
	if len(result) != 1 || result[0].ID != "/typescript/framework/web" {
		t.Errorf(".../framework/web: got %v, want [/typescript/framework/web]", projectIDs(result))
	}

	// .../http should match the Go project
	result = ResolveTarget(ws, ".../http", "", nil)
	if len(result) != 1 || result[0].ID != "/go/framework/http" {
		t.Errorf(".../http: got %v, want [/go/framework/http]", projectIDs(result))
	}

	// .../cli should match tooling/cli
	result = ResolveTarget(ws, ".../cli", "", nil)
	if len(result) != 1 || result[0].ID != "/tooling/cli" {
		t.Errorf(".../cli: got %v, want [/tooling/cli]", projectIDs(result))
	}
}

func TestResolveTarget_InfixWildcard(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "target-expression-grammar", "wildcards-resolve")
	ws := makeTargetTestWorkspace()

	// .../framework/... should match all framework projects (TS + Go)
	result := ResolveTarget(ws, ".../framework/...", "", nil)
	if len(result) != 5 {
		t.Errorf(".../framework/...: got %d projects %v, want 5", len(result), projectIDs(result))
	}

	// .../samples/... should match only the samples project
	result = ResolveTarget(ws, ".../samples/...", "", nil)
	if len(result) != 1 || result[0].ID != "/typescript/samples/hello" {
		t.Errorf(".../samples/...: got %v, want [/typescript/samples/hello]", projectIDs(result))
	}
}

func TestResolveTarget_SuffixWildcardInUnion(t *testing.T) {
	ws := makeTargetTestWorkspace()

	// Union with suffix wildcard
	result := ResolveTarget(ws, ".../web,.../http", "", nil)
	if len(result) != 2 {
		t.Errorf(".../web,.../http: got %d projects %v, want 2", len(result), projectIDs(result))
	}
}

func TestResolveTarget_SuffixWildcardWithSubtraction(t *testing.T) {
	ws := makeTargetTestWorkspace()

	// All frameworks minus Go frameworks
	result := ResolveTarget(ws, ".../framework/...,-/go/...", "", nil)
	if len(result) != 3 {
		t.Errorf(".../framework/...,-/go/...: got %d projects %v, want 3", len(result), projectIDs(result))
	}
	for _, p := range result {
		if !hasPrefix(p.ID, "/typescript/") {
			t.Errorf("unexpected project in result: %s", p.ID)
		}
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestFilterProjects_WithTargetExpression(t *testing.T) {
	ws := makeTargetTestWorkspace()
	result := FilterProjects(ws, FilterOptions{
		Projects: "/typescript/framework/...",
	})
	if len(result) != 3 {
		t.Errorf("filter with target expr: got %d, want 3", len(result))
	}
}

func TestFilterProjects_WithTargetAndTagFilter(t *testing.T) {
	ws := makeTargetTestWorkspace()
	result := FilterProjects(ws, FilterOptions{
		Projects:   "/typescript/...",
		ExcludeTag: "e2e",
	})
	// 4 TS projects, minus 1 e2e sample = 3
	if len(result) != 3 {
		t.Errorf("target + tag filter: got %d projects %v, want 3", len(result), projectIDs(result))
	}
}

// makeActivatedScopeWorkspace produces a workspace where /cloud is an
// activated scope with two child workloads, mirroring the canonical
// workspace-infra use case.
func makeActivatedScopeWorkspace() *Workspace {
	projects := []*Project{
		{ID: "/cloud", Name: "cloud", Path: "cloud", ActivatedScope: true,
			ScopeIncludes: []string{"/cloud/workloads/api", "/cloud/workloads/worker"}},
		{ID: "/cloud/workloads/api", Name: "api", Path: "cloud/workloads/api"},
		{ID: "/cloud/workloads/worker", Name: "worker", Path: "cloud/workloads/worker"},
	}
	ws := &Workspace{
		Root:        "/workspace",
		Projects:    projects,
		projectByID: make(map[string]*Project, len(projects)),
		projectMap:  make(map[string]*Project, len(projects)),
		Config:      &wsproto.Config{},
	}
	for _, p := range projects {
		ws.projectByID[p.ID] = p
		ws.projectMap[p.Name] = p
	}
	return ws
}

// TestResolveTarget_ActivatedScope_ExactPath confirms that /cloud resolves to
// scope-self only — the user-clarified "domain is the self target" semantics.
func TestResolveTarget_ActivatedScope_ExactPath(t *testing.T) {
	ws := makeActivatedScopeWorkspace()
	result := ResolveTarget(ws, "/cloud", "", nil)
	if len(result) != 1 || result[0].ID != "/cloud" {
		t.Errorf("/cloud: got %v; want [/cloud] only", projectIDs(result))
	}
}

// TestResolveTarget_ActivatedScope_RecursivePath confirms /cloud/... covers
// scope-self plus all children.
func TestResolveTarget_ActivatedScope_RecursivePath(t *testing.T) {
	ws := makeActivatedScopeWorkspace()
	result := ResolveTarget(ws, "/cloud/...", "", nil)
	got := projectIDs(result)
	want := map[string]bool{"/cloud": true, "/cloud/workloads/api": true, "/cloud/workloads/worker": true}
	if len(got) != 3 {
		t.Fatalf("/cloud/...: got %v; want 3 projects", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected project %q in /cloud/... result", id)
		}
	}
}
