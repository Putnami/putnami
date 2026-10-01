package engine

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	workspacepb "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The selection stage owns --no-cache-projects: it is where the workspace, the
// shared --projects selector grammar, and the usage-error exit already live.

// noCacheProjectsFixture is a workspace whose config disables one tag, and a
// request that selects everything. The tagged project is what proves a
// default-excluded target stays nameable as a cache bypass: the flag states
// cache policy, not the run's scope, and @putnami/clientgen's provider check
// depends on exactly that (its providers carry the disabled `e2e` tag).
func noCacheProjectsFixture(t *testing.T) (*workspace.Workspace, *Request) {
	t.Helper()
	config := &workspacepb.Config{Disable: &workspacepb.DisableConfig{Tags: []string{"e2e"}}}
	ws := workspace.NewWorkspace(t.TempDir(), config, []*workspace.Project{
		{ID: "/app", Name: "@scope/app", Path: "app"},
		{ID: "/lib", Name: "@scope/lib", Path: "lib"},
		{ID: "/samples/items", Name: "@example/items", Path: "samples/items", Tags: []string{"e2e"}},
	})
	// `--all`, not a bare `--projects *`: an explicitly targeted selection
	// overrides the workspace's default tag exclusions, which is precisely the
	// behavior the tagged project has to stay excluded from here.
	req := &Request{Config: config}
	req.Global.All = true
	req.Global.Projects = "*"
	return ws, req
}

// TestResolveNoCacheProjects_ResolvesTheSameSelectorGrammarAsProjects pins that
// the flag reuses the --projects parser — both the id and the package-name
// spelling — and that workspace default tag exclusions do not silently drop a
// named target.
func TestResolveNoCacheProjects_ResolvesTheSameSelectorGrammarAsProjects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "scoped-cache-bypass",
		"no-cache-projects-is-execution-policy-only")
	ws, req := noCacheProjectsFixture(t)
	req.Global.NoCacheProjects = "/app,@example/items"
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("selection exit code = %d, want %d", code, ExitSuccess)
	}
	for _, project := range selected {
		if project.ID == "/samples/items" {
			t.Fatal("the fixture no longer excludes the tagged project by default, so it proves nothing")
		}
	}
	if len(req.noCacheProjects) != 2 || !req.noCacheProjects["/app"] || !req.noCacheProjects["/samples/items"] {
		t.Fatalf("resolved cache bypass = %v, want /app and the default-excluded /samples/items",
			req.noCacheProjects)
	}
	// The run's own scope is untouched: the flag is cache policy, not selection.
	if req.Global.Projects != "*" || !req.Global.All {
		t.Errorf("the cache-bypass selector rewrote the run selection: %q", req.Global.Projects)
	}

	// Absent, it resolves to nothing rather than to everything.
	_, bare := noCacheProjectsFixture(t)
	if _, code := selectProjects(bare, ws); code != ExitSuccess {
		t.Fatalf("bare selection exit code = %d", code)
	}
	if len(bare.noCacheProjects) != 0 {
		t.Errorf("a run without the flag resolved %v", bare.noCacheProjects)
	}
}

// TestResolveNoCacheProjects_UnmatchedSelectorIsAUsageError is the fail-closed
// half. The flag exists to take a project OUT of the cache, so a typo that
// silently left it in would hand back a cached verdict for exactly the task the
// caller asked to re-derive.
func TestResolveNoCacheProjects_UnmatchedSelectorIsAUsageError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "scoped-cache-bypass",
		"a-selector-that-matches-nothing-fails-the-run")
	ws, req := noCacheProjectsFixture(t)
	req.Global.NoCacheProjects = "@scope/lbi"
	selected, code := selectProjects(req, ws)
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d for a selector that names no project", code, ExitUsage)
	}
	if selected != nil {
		t.Errorf("a refused run still returned %d selected project(s)", len(selected))
	}
	if len(req.noCacheProjects) != 0 {
		t.Errorf("a refused selector still resolved %v", req.noCacheProjects)
	}
}

// TestResolveNoCacheProjects_NoCacheSubsumesTheScopedForm keeps the broader
// statement of the same intent authoritative: --no-cache already refuses every
// task, so the scoped list has nothing left to add and an unmatched selector
// beside it is not a reason to refuse the run.
func TestResolveNoCacheProjects_NoCacheSubsumesTheScopedForm(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "scoped-cache-bypass",
		"no-cache-projects-is-execution-policy-only")
	ws, req := noCacheProjectsFixture(t)
	req.Global.NoCache = true
	req.Global.NoCacheProjects = "@scope/lbi"
	if _, code := selectProjects(req, ws); code != ExitSuccess {
		t.Fatalf("exit code = %d, want %d: --no-cache subsumes the scoped form", code, ExitSuccess)
	}
	if len(req.noCacheProjects) != 0 {
		t.Errorf("--no-cache still resolved a scoped list: %v", req.noCacheProjects)
	}
}
