package lifecycle

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// crossScopeProbe answers a workspace in which apps/api imports libs/core
// through its go.mod, across the apps and libs scopes.
type crossScopeProbe struct{}

func (crossScopeProbe) Name() string { return "@acme/go" }

func (crossScopeProbe) Probe(wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	return wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: "@acme/go",
		Projects: []wsproto.ProbeProject{
			{Path: "libs/core", SourceName: "acme/core", SourceFile: "libs/core/go.mod"},
			{
				Path: "apps/api", SourceName: "acme/api", SourceFile: "apps/api/go.mod",
				Dependencies:      []string{"libs/core"},
				DependencySources: map[string]wsproto.DependencySource{"libs/core": wsproto.DependencySourceGoModule},
			},
		},
	}, nil
}

// A refused graph is printed finding by finding, and the refresh does not claim
// the index was left unbuilt.
func TestRefreshWorkspaceSnapshotListsEveryRefusedImport(t *testing.T) {
	root := t.TempDir()
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"includes":["libs/core","apps/api"]}`)
	writeRawFile(t, filepath.Join(root, "libs", "putnami.json"), `{"tags":["lib"]}`)
	writeRawFile(t, filepath.Join(root, "apps", "putnami.json"), `{"tags":["app"]}`)
	writeRawFile(t, filepath.Join(root, "libs", "core", "putnami.json"), `{"name":"acme/core"}`)
	writeRawFile(t, filepath.Join(root, "libs", "core", "go.mod"), "module acme/core\n\ngo 1.25\n")
	writeRawFile(t, filepath.Join(root, "apps", "api", "putnami.json"), `{"name":"acme/api"}`)
	writeRawFile(t, filepath.Join(root, "apps", "api", "go.mod"), "module acme/api\n\ngo 1.25\n\nrequire acme/core v0.0.0\n")
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	scope, ok := workspace.NewProviderScope("@acme/go", &extproto.WorkspaceAdapter{Markers: []string{"go.mod"}, Inputs: []string{"**/go.mod"}})
	if !ok {
		t.Fatal("the fixture adapter was rejected")
	}
	adapters := workspaceAdapters{adapters: []workspaceAdapter{{
		binding: workspace.ProviderBinding{Scope: scope, Provider: crossScopeProbe{}},
	}}}

	out, err := captureStdout(t, func() error {
		refreshWorkspaceSnapshot(context.Background(), root, adapters, nil, false, nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"warning: visibility-violation: 1 import(s)",
		"[error] /apps/api: /apps/api imports /libs/core across a scope boundary",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(out, "the index was not rebuilt") {
		t.Fatalf("output = %q, want no claim that the index was not rebuilt", out)
	}
}
