package wsview

import (
	"encoding/json"
	"testing"

	workspaceproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
)

func TestWorkspaceOptionsFromContextDecodesCommittedBlocks(t *testing.T) {
	options, err := WorkspaceOptionsFromContext(&pctx.Context{Workspace: pctx.Workspace{
		Options: map[string]json.RawMessage{
			"sdd": json.RawMessage(`{"verification":{"architecture":"report"}}`),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	verification := options["sdd"]["verification"].(map[string]any)
	if verification["architecture"] != "report" {
		t.Errorf("architecture policy = %#v, want report", verification["architecture"])
	}
}

func TestWorkspaceOptionsFromContextFailsClosedOnInvalidBlocks(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"null":   json.RawMessage(`null`),
		"scalar": json.RawMessage(`"report"`),
		"array":  json.RawMessage(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := WorkspaceOptionsFromContext(&pctx.Context{Workspace: pctx.Workspace{
				Options: map[string]json.RawMessage{"sdd": raw},
			}})
			if err == nil {
				t.Fatal("invalid workspace options decoded")
			}
		})
	}
}

func TestFromContextBuildsTheViewFromSelectedProjectsOnly(t *testing.T) {
	ws, scoped := FromContext(&pctx.Context{
		WorkspaceRoot: "/repo",
		Workspace:     pctx.Workspace{Name: "putnami", Version: "1.2.3"},
		SelectedProjects: []pctx.ProjectRef{
			{
				ID: "/billing", Name: "billing", SourceName: "@acme/billing",
				Version: "4.5.6", Path: "billing", FullPath: "/repo/billing",
			},
			{ID: "/shipping", Name: "shipping", Path: "shipping", FullPath: "/repo/shipping"},
		},
		Selection: &pctx.Selection{Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing", "/shipping"}},
	})
	if ws == nil {
		t.Fatal("FromContext returned no workspace for a populated context")
	}
	if !scoped {
		t.Fatal("a narrowed selection must report scoped")
	}
	if ws.Root != "/repo" || ws.Name != "putnami" || ws.Version != "1.2.3" {
		t.Fatalf("workspace identity = %q/%q/%q", ws.Root, ws.Name, ws.Version)
	}
	if len(ws.Projects) != 2 {
		t.Fatalf("projects = %d, want one per selectedProjects entry", len(ws.Projects))
	}
	first := ws.Projects[0]
	if first.ID != "/billing" || first.Name != "billing" || first.SourceName != "@acme/billing" || first.Path != "billing" {
		t.Fatalf("first project = %+v", first)
	}
	// The version travels. A package-root source selector keys on name AND
	// version, so a view that dropped it would resolve every version-qualified
	// binding to "source unavailable" while the versionless ones beside it kept
	// working — a silent narrowing, not a crash.
	if first.Version != "4.5.6" {
		t.Fatalf("project version = %q, want the wire's 4.5.6", first.Version)
	}
	// Optional on the wire: a reference that declares no version yields none
	// here, so a selector matches by name alone rather than against a version
	// this package invented.
	if second := ws.Projects[1]; second.Version != "" {
		t.Fatalf("project version = %q, want empty — the wire carried none", second.Version)
	}
}

func TestFromContextDerivesTheProjectIDTheWireOmitted(t *testing.T) {
	ws, _ := FromContext(&pctx.Context{
		WorkspaceRoot:    "/repo",
		SelectedProjects: []pctx.ProjectRef{{Name: "cli", Path: "tooling/(group)/cli"}},
	})
	if len(ws.Projects) != 1 || ws.Projects[0].ID != "/tooling/cli" {
		t.Fatalf("derived project = %+v, want the transparent group folder dropped", ws.Projects)
	}
}

// TestFromContextOnAnInteractiveContextSeesTheSelectedProjects is the positive
// counterpart of the limit this package used to document.
//
// The interactive command path resolved a selection and wrote only its ids, so
// an extension subcommand got a list it could print and not open. It now writes
// `selectedProjects` beside the block, from the same resolver, so every id in
// `selection.projects` arrives with a name and a path.
func TestFromContextOnAnInteractiveContextSeesTheSelectedProjects(t *testing.T) {
	ws, scoped := FromContext(&pctx.Context{
		WorkspaceRoot: "/repo",
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/billing", Name: "billing", Version: "4.5.6", Path: "billing", FullPath: "/repo/billing"},
		},
		Selection: &pctx.Selection{Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing"}},
	})
	if ws == nil || ws.Root != "/repo" {
		t.Fatalf("workspace = %+v, want the root the context named", ws)
	}
	if !scoped {
		t.Fatal("the selection block still says the run was narrowed")
	}
	// Every id the selection block reports must resolve to a project this view
	// can read files from. An id with no project is the old defect.
	for _, id := range []string{"/billing"} {
		project := ws.ProjectByID(id)
		if project == nil {
			t.Fatalf("selection names %q but the view has no such project", id)
		}
		if project.Path == "" {
			t.Fatalf("project %q has no path to open a file with", id)
		}
		if project.Version != "4.5.6" {
			t.Fatalf("project %q version = %q, want the wire's 4.5.6", id, project.Version)
		}
	}
}

func TestFromContextReadsScopedFromTheSelectionBlockAlone(t *testing.T) {
	cases := []struct {
		name      string
		selection *pctx.Selection
		want      bool
	}{
		{"absent block is not scoped", nil, false},
		{"whole workspace", &pctx.Selection{Mode: pctx.SelectionModeAll}, false},
		{"impacted narrowing", &pctx.Selection{Mode: pctx.SelectionModeImpacted, Scoped: true}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, scoped := FromContext(&pctx.Context{Selection: testCase.selection}); scoped != testCase.want {
				t.Fatalf("scoped = %t, want %t", scoped, testCase.want)
			}
		})
	}
}

func TestFromContextRejectsAnAbsentContext(t *testing.T) {
	if ws, scoped := FromContext(nil); ws != nil || scoped {
		t.Fatalf("FromContext(nil) = %+v, %t, want no workspace at all", ws, scoped)
	}
}

func TestNewWorkspaceTakesIdentityFromTheConfigWhenThereIsOne(t *testing.T) {
	ws := NewWorkspace("/repo", &workspaceproto.Config{Name: "putnami"}, nil)
	if ws.Name != "putnami" || ws.Root != "/repo" {
		t.Fatalf("workspace = %+v", ws)
	}
	if bare := NewWorkspace("/repo", nil, nil); bare.Name != "" {
		t.Fatalf("workspace without config = %+v, want no invented identity", bare)
	}
}

func TestSeedRootsReturnsProjectPathsInWireOrder(t *testing.T) {
	ws := NewWorkspace("/repo", nil, []*Project{
		{Path: "shipping"},
		nil,
		{Path: "billing"},
	})
	roots := ws.SeedRoots()
	if len(roots) != 2 || roots[0] != "shipping" || roots[1] != "billing" {
		t.Fatalf("seed roots = %v, want the non-nil paths in order", roots)
	}
	var absent *Workspace
	if absent.SeedRoots() != nil {
		t.Fatal("a nil workspace must yield no roots")
	}
}

func TestProjectIDFromPathMatchesTheCoreIdentityFold(t *testing.T) {
	cases := map[string]string{
		"":                     "/",
		".":                    "/",
		"/":                    "/",
		"tooling/cli":          "/tooling/cli",
		"/tooling/cli":         "/tooling/cli",
		"(group)/cli":          "/cli",
		"tooling/(group)/cli":  "/tooling/cli",
		"tooling/()/cli":       "/tooling/()/cli",
		"tooling/cli/":         "/tooling/cli",
		"tooling//cli":         "/tooling/cli",
		"typescript/framework": "/typescript/framework",
	}
	for input, want := range cases {
		if got := ProjectIDFromPath(input); got != want {
			t.Errorf("ProjectIDFromPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCleanWorkspacePathCanonicalizesToSlashForm(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"  ":             "",
		"/":              "",
		".":              "",
		"a/./b":          "a/b",
		"a//b":           "a/b",
		"/tooling/cli/":  "tooling/cli",
		" tooling/cli  ": "tooling/cli",
	}
	for input, want := range cases {
		if got := CleanWorkspacePath(input); got != want {
			t.Errorf("CleanWorkspacePath(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestFromContextPrefersTheCompleteMembership pins the member order. A
// workspace-scoped job receives BOTH collections, and reading the selection
// when the membership is there would answer a workspace-wide question with a
// subset — which is the whole reason the membership member exists.
func TestFromContextPrefersTheCompleteMembership(t *testing.T) {
	ws, scoped := FromContext(&pctx.Context{
		WorkspaceRoot: "/repo",
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/api", Name: "@acme/api", Path: "api", FullPath: "/repo/api"},
		},
		WorkspaceProjects: []pctx.ProjectRef{
			{ID: "/api", Name: "@acme/api", Path: "api", FullPath: "/repo/api", Dependencies: []string{"/core"}},
			{ID: "/core", Name: "@acme/core", Path: "core", FullPath: "/repo/core"},
			{ID: "/legacy", Name: "@acme/legacy", Path: "legacy", FullPath: "/repo/legacy"},
		},
		Selection: &pctx.Selection{Mode: pctx.SelectionModeImpacted, Scoped: true, ProjectIDs: []string{"/api"}},
	})
	if len(ws.Projects) != 3 {
		t.Fatalf("projects = %d, want the whole membership rather than the selection", len(ws.Projects))
	}
	if ws.ProjectByID("/legacy") == nil {
		t.Fatal("a member outside the selection is missing; a workspace-wide claim would be a lie")
	}
	// The run is still narrowed. Membership and scope are independent facts: a
	// validator may see the whole workspace and still owe its report to a
	// subset.
	if !scoped {
		t.Fatal("a narrowed run must still report scoped")
	}
	if ws.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Fatalf("a complete membership raised the incomplete-view warning: %v", ws.WarningCodes)
	}
}

func TestFromContextDecodesSiblingConfigAndPreservesOwnAuthority(t *testing.T) {
	ws, _ := FromContext(&pctx.Context{
		WorkspaceRoot: "/repo",
		Project: pctx.Project{
			Name: "@acme/api", Path: "api", FullPath: "/repo/api",
			Publish: json.RawMessage(`["npm"]`),
			Options: map[string]json.RawMessage{
				"publish": json.RawMessage(`{"npm":true}`),
			},
		},
		WorkspaceProjects: []pctx.ProjectRef{
			{
				ID: "/api", Name: "@acme/api", Path: "api", FullPath: "/repo/api",
				Config: json.RawMessage(`{"name":"@acme/api","featureAuthority":{"none":"reviewed as infrastructure only"}}`),
			},
			{
				ID: "/client", Name: "@acme/client", Path: "client", FullPath: "/repo/client",
				Config: json.RawMessage(`{"name":"@acme/client","featureAuthority":{"owner":"api/contracts"}}`),
			},
		},
	})

	api := ws.ProjectByID("/api")
	if api == nil || api.Config == nil || api.Config.FeatureAuthority == nil {
		t.Fatalf("own project lost its decoded config: %+v", api)
	}
	if got := api.Config.FeatureAuthority.None; got != "reviewed as infrastructure only" {
		t.Errorf("own featureAuthority = %q, want the raw workspace config's reviewed answer", got)
	}
	if api.Config.Options["publish"]["npm"] != true {
		t.Errorf("own options were not merged into the decoded config: %+v", api.Config.Options)
	}
	client := ws.ProjectByID("/client")
	if client == nil || client.Config == nil || client.Config.FeatureAuthority == nil ||
		client.Config.FeatureAuthority.Owner != "api/contracts" {
		t.Fatalf("sibling featureAuthority did not cross the job wire: %+v", client)
	}
}

// TestFromContextOnAProjectScopedJobSeesItsOwnProject pins the third source.
//
// A project-scoped task receives neither collection: the orchestrator attaches
// selectedProjects only to the jobs that run once for the workspace. Before
// this, such a job built a view with ZERO projects, so discovery had one root
// (the workspace root), read none of the project's own documents, and reported
// `valid: true` over a project whose manifest and spec were both broken.
func TestFromContextOnAProjectScopedJobSeesItsOwnProject(t *testing.T) {
	ws, scoped := FromContext(&pctx.Context{
		WorkspaceRoot: "/repo",
		Project: pctx.Project{
			Name:     "@acme/api",
			Path:     "api",
			FullPath: "/repo/api",
			Type:     "library",
			Publish:  []byte(`["npm"]`),
			Options: map[string]json.RawMessage{
				"publish": json.RawMessage(`{"npm":true,"archives":false}`),
			},
		},
		Selection: &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: []string{"/api"}},
	})
	if len(ws.Projects) != 1 {
		t.Fatalf("projects = %+v, want the job's own project", ws.Projects)
	}
	own := ws.Projects[0]
	if own.ID != "/api" || own.Name != "@acme/api" || own.Path != "api" || own.Type != "library" {
		t.Fatalf("own project = %+v", own)
	}
	// Publication travels, in BOTH shapes the workspace accepts: a TypeScript
	// package declares it only through options.publish.npm, so a view that read
	// the array alone would report a publishable project as unpublished and
	// never assess its completeness gap.
	if len(own.Publish) != 1 || own.Publish[0] != "npm" {
		t.Fatalf("publish channels = %v, want the wire's [npm]", own.Publish)
	}
	if own.Config == nil {
		t.Fatal("the authored options did not travel; options.publish is invisible")
	}
	if enabled, ok := own.Config.Options["publish"]["npm"].(bool); !ok || !enabled {
		t.Fatalf("options = %+v, want publish.npm true", own.Config.Options)
	}
	// A project-scoped view is not an incomplete WORKSPACE view: it was never
	// asked a workspace-wide question, so it raises no fail-closed warning.
	if ws.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Fatalf("a project-scoped view raised the incomplete-view warning: %v", ws.WarningCodes)
	}
	if scoped {
		t.Fatal("mode=all is not a narrowed run")
	}
}

// TestFromContextIgnoresTheSyntheticWorkspaceProject keeps the own-project
// fallback from inventing a member. A workspace-once job whose orchestrator
// published no membership runs for a SYNTHETIC project rooted at ".", and
// minting a workspace member out of it would add a project no putnami.json
// declares.
func TestFromContextIgnoresTheSyntheticWorkspaceProject(t *testing.T) {
	ws, _ := FromContext(&pctx.Context{
		WorkspaceRoot: "/repo",
		Project:       pctx.Project{Name: "putnami", Path: ".", FullPath: "/repo"},
	})
	if len(ws.Projects) != 0 {
		t.Fatalf("projects = %+v, want no member minted from the synthetic project", ws.Projects)
	}
}
