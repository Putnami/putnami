package jobs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// The two steps of a hosted install's dependency fetch run disjoint
// extensions. The dependency fetch sees only the extensions installed from the
// artifact store, so no path extension's runtime, probe or job starts beside
// the job credential; its skip records stay. The path-extension fetch
// discovers every extension and plans only the others. Any other run sees and
// plans every extension.
func TestTheHostedFetchStepsRunDisjointExtensions(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "path-extension-fetch-runs-after-custody")
	wsRoot, storeRoot := t.TempDir(), t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)
	stored := &extension.ExtensionDescription{Name: "@acme/cloud", Path: filepath.Join(storeRoot, "extensions", "acme-cloud@1.0.0")}
	path := &extension.ExtensionDescription{Name: "@acme/local", Path: filepath.Join(wsRoot, "tools", "local"), RelPath: "tools/local", LocalSource: true}
	for _, ext := range []*extension.ExtensionDescription{stored, path} {
		if err := os.MkdirAll(ext.Path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	all := []*extension.ExtensionDescription{path, stored}
	discovered := &extension.DiscoveryResult{Extensions: all, Skipped: []extension.SkippedExtension{{Name: "@acme/planted"}}}
	names := func(exts []*extension.ExtensionDescription) []string {
		out := make([]string, 0, len(exts))
		for _, ext := range exts {
			out = append(out, ext.Name)
		}
		return out
	}

	for _, c := range []struct {
		name      string
		ctx       context.Context
		step      string
		discovers []string
		plans     []string
	}{
		{"the dependency fetch", WithDependencyFetch(context.Background()), "dependency fetch", []string{"@acme/cloud"}, []string{"@acme/cloud"}},
		{"the path-extension fetch", WithPathExtensionFetch(context.Background()), "path-extension fetch", []string{"@acme/local", "@acme/cloud"}, []string{"@acme/local"}},
		{"any other run", context.Background(), "", []string{"@acme/local", "@acme/cloud"}, []string{"@acme/local", "@acme/cloud"}},
	} {
		if got := HostedFetchStep(c.ctx); got != c.step {
			t.Errorf("%s: HostedFetchStep = %q, want %q", c.name, got, c.step)
		}
		view := CredentialedFetchView(c.ctx, wsRoot, discovered)
		if got := names(view.Extensions); !slices.Equal(got, c.discovers) {
			t.Errorf("%s: discovers %v, want %v", c.name, got, c.discovers)
		}
		if len(view.Skipped) != 1 {
			t.Errorf("%s: the skip records are %+v, want the discovery's", c.name, view.Skipped)
		}
		if got := names(PathExtensionFetchPlan(c.ctx, wsRoot, view.Extensions)); !slices.Equal(got, c.plans) {
			t.Errorf("%s: plans %v, want %v", c.name, got, c.plans)
		}
	}
	if !slices.Equal(names(discovered.Extensions), []string{"@acme/local", "@acme/cloud"}) {
		t.Errorf("the dependency fetch changed the discovery it scoped: %v", names(discovered.Extensions))
	}
	if CredentialedFetchView(WithDependencyFetch(context.Background()), wsRoot, nil) != nil {
		t.Error("the dependency fetch made a discovery out of none")
	}
}
