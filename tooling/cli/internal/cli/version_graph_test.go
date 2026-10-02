package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	supportproto "go.putnami.dev/protocol/support"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A support catalog promotes an unlisted project a stable one depends on, and a
// provider's dependency edges live only in the recorded view, so `version`
// records that view before it computes a version. Without a catalog, no edge
// is read and nothing is recorded.
func TestVersionRecordsTheDependencyGraphOnlyWithASupportCatalog(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		withCatalog bool
		want        workspace.RecordedFreshness
	}{
		{name: "with a catalog", withCatalog: true, want: workspace.RecordedFresh},
		{name: "without a catalog", withCatalog: false, want: workspace.RecordedAbsent},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			wsRoot := versionGraphRepo(t, testCase.withCatalog)
			registered, ok := lookupCommand("version")
			if !ok {
				t.Fatal("version command is not registered")
			}
			err := registered.run(&CommandEnv{
				Ctx:    context.Background(),
				Cfg:    wsproto.Load(wsRoot),
				WsRoot: wsRoot,
				Sub:    "get",
			})
			if err != nil {
				t.Fatalf("version get: %v", err)
			}
			if got := workspace.RecordedIndexView(wsRoot, time.Now()).Freshness; got != testCase.want {
				t.Fatalf("recorded view = %s, want %s", got, testCase.want)
			}
		})
	}
}

// versionGraphRepo is a committed workspace with one version line and one
// project, and, when withCatalog is set, a catalog that lists that project as
// stable.
func versionGraphRepo(t *testing.T, withCatalog bool) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		wsproto.WorkspaceConfigFilename:                         `{"name": "test", "includes": ["tooling"]}`,
		filepath.Join("tooling", wsproto.ConfigFilename):        `{"line": {}, "includes": ["cli"]}`,
		filepath.Join("tooling", "cli", wsproto.ConfigFilename): `{"name": "@putnami/cli"}`,
	}
	if withCatalog {
		catalog, err := supportproto.MarshalCatalog(&supportproto.Catalog{
			ProtocolVersion: 1,
			Entries: []supportproto.Entry{
				{ID: "@putnami/cli", Kind: supportproto.SubjectKindPackage, Status: supportproto.StatusStable},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		files[supportproto.CatalogFilename] = string(catalog)
	}
	for name, contents := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "test@test.com"}, {"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"}, {"checkout", "-b", "main"},
		{"add", "-A"}, {"commit", "-m", "feat: the workspace"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}
