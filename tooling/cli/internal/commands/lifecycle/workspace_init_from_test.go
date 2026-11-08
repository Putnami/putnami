package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

const initFromFeature = "cli/init-from-a-monorepo-tool"

func TestInitFromPantsDeclaresThePantsExtensionAndSyncsTheProjects(t *testing.T) {
	spectest.Proves(t, initFromFeature, "init-from-pants", "init-from-pants-declares-the-pants-extension-and-syncs-the-projects")
	dir := t.TempDir()
	stubWorkspaceInitStages(t, "", 0)
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "pants.toml"), []byte("[GLOBAL]\npants_version = \"2.25.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var installed []string
	installExtension = func(_ context.Context, _ string, _ *wsproto.Config, args []string, _ string) error {
		installed = append(installed, args...)
		return nil
	}
	origSync := syncInitProjects
	t.Cleanup(func() { syncInitProjects = origSync })
	var synced *wsproto.Config
	syncInitProjects = func(_ context.Context, root string, cfg *wsproto.Config, _ LifecycleEnv) error {
		if root != dir {
			t.Errorf("sync root = %s, want %s", root, dir)
		}
		synced = cfg
		return nil
	}

	out, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", []string{"--from", "pants", "--workspace", "shop"}, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("WorkspaceInit: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	var cfg initConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Extensions, []string{"@putnami/pants"}) || len(cfg.Templates) != 0 || len(cfg.Registries) != 0 {
		t.Errorf("workspace config = %s, want @putnami/pants declared alone", data)
	}
	if !slices.Equal(installed, []string{"@putnami/pants"}) {
		t.Errorf("installed = %v, want @putnami/pants", installed)
	}
	if synced == nil || !slices.Contains(synced.Extensions.Names(), "@putnami/pants") {
		t.Errorf("projects sync ran on %+v, want the workspace that declares @putnami/pants", synced)
	}
	if !strings.Contains(out, "putnami lint,test,build --impacted") {
		t.Errorf("the summary names the next command:\n%s", out)
	}
}

func TestInitFromRefusesWhatItCannotAdopt(t *testing.T) {
	spectest.Proves(t, initFromFeature, "init-from-pants", "init-from-pants-declares-the-pants-extension-and-syncs-the-projects")
	for name, c := range map[string]struct {
		args      []string
		pantsTOML bool
		want      string
	}{
		"no pants.toml":   {[]string{"--from", "pants"}, false, "holds no pants.toml"},
		"an unknown tool": {[]string{"--from", "gradle"}, true, `unknown --from tool "gradle" (choose: pants)`},
		"a project":       {[]string{"--from", "pants", "--project", "web"}, true, "does not combine with --project"},
		"an extension":    {[]string{"--from", "pants", "--extension", "go"}, true, "does not combine with --project, --project-path or --extension"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if c.pantsTOML {
				if err := os.WriteFile(filepath.Join(dir, "pants.toml"), []byte("[GLOBAL]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := WorkspaceInit(context.Background(), "", c.args, LifecycleEnv{})
			if err == nil || !strings.Contains(err.Error(), c.want) || !errors.Is(err, protocolcli.ErrUsage) {
				t.Fatalf("err = %v, want a usage error with %q", err, c.want)
			}
			if _, err := os.Stat(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); !os.IsNotExist(err) {
				t.Errorf("a refused init writes no workspace config: %v", err)
			}
		})
	}
}
