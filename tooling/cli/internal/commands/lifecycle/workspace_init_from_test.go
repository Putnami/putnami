package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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
	assertInitFrom(t, "pants", "pants.toml", "@putnami/pants")
}

func TestInitFromBazelDeclaresTheBazelExtensionAndSyncsTheProjects(t *testing.T) {
	spectest.Proves(t, initFromFeature, "init-from-bazel", "init-from-bazel-declares-the-bazel-extension-and-syncs-the-projects")
	for _, marker := range []string{"MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel"} {
		t.Run(marker, func(t *testing.T) {
			assertInitFrom(t, "bazel", marker, "@putnami/bazel")
		})
	}
}

// assertInitFrom runs putnami init --from tool in a directory that holds the
// marker, and checks that it declares and installs the extension alone, then
// synchronizes the projects.
func assertInitFrom(t *testing.T, tool, marker, extension string) {
	t.Helper()
	dir := t.TempDir()
	stubWorkspaceInitStages(t, "", 0)
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("\n"), 0o644); err != nil {
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
		return WorkspaceInit(context.Background(), "", []string{"--from", tool, "--workspace", "shop"}, LifecycleEnv{})
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
	if !slices.Equal(cfg.Extensions, []string{extension}) || len(cfg.Templates) != 0 || len(cfg.Registries) != 0 {
		t.Errorf("workspace config = %s, want %s declared alone", data, extension)
	}
	if !slices.Equal(installed, []string{extension}) {
		t.Errorf("installed = %v, want %s", installed, extension)
	}
	if synced == nil || !slices.Contains(synced.Extensions.Names(), extension) {
		t.Errorf("projects sync ran on %+v, want the workspace that declares %s", synced, extension)
	}
	if !strings.Contains(out, "putnami lint,test,build --impacted") {
		t.Errorf("the summary names the next command:\n%s", out)
	}
}

func TestInitFromRefusesWhatItCannotAdopt(t *testing.T) {
	spectest.Proves(t, initFromFeature, "init-from-pants", "init-from-pants-declares-the-pants-extension-and-syncs-the-projects")
	spectest.Proves(t, initFromFeature, "init-from-bazel", "init-from-bazel-declares-the-bazel-extension-and-syncs-the-projects")
	for name, c := range map[string]struct {
		args      []string
		pantsTOML bool
		want      string
	}{
		"no pants.toml":   {[]string{"--from", "pants"}, false, "holds no pants.toml"},
		"no Bazel root":   {[]string{"--from", "bazel"}, true, "holds no MODULE.bazel, WORKSPACE, WORKSPACE.bazel"},
		"no tool":         {[]string{"--from"}, true, "--from needs a tool (choose: bazel, nx, pants)"},
		"an empty tool":   {[]string{"--from="}, true, "--from needs a tool (choose: bazel, nx, pants)"},
		"an unknown tool": {[]string{"--from", "gradle"}, true, `unknown --from tool "gradle" (choose: bazel, nx, pants)`},
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

// stubInitFrom stubs the stages of an init --from run: the extension
// install, the sync, the preview and the terminal. It returns what they saw.
type initFromCalls struct {
	installed []string
	synced    *wsproto.Config
	previewed []string
}

func stubInitFrom(t *testing.T, terminal bool, answer string) *initFromCalls {
	t.Helper()
	stubWorkspaceInitStages(t, "", 0)
	calls := &initFromCalls{}
	installExtension = func(_ context.Context, _ string, _ *wsproto.Config, args []string, _ string) error {
		calls.installed = append(calls.installed, args...)
		return nil
	}
	origSync, origPreview, origTerminal, origInput := syncInitProjects, previewInitSync, initIsTerminal, initPromptInput
	t.Cleanup(func() {
		syncInitProjects, previewInitSync, initIsTerminal, initPromptInput = origSync, origPreview, origTerminal, origInput
	})
	syncInitProjects = func(_ context.Context, _ string, cfg *wsproto.Config, _ LifecycleEnv) error {
		calls.synced = cfg
		return nil
	}
	previewInitSync = func(_ context.Context, _ string, _ *wsproto.Config, mode string, _ LifecycleEnv) error {
		calls.previewed = append(calls.previewed, mode)
		return nil
	}
	initIsTerminal = func() bool { return terminal }
	initPromptInput = strings.NewReader(answer)
	return calls
}

func nxRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "nx.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readInitConfig(t *testing.T, dir string) initConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	var cfg initConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestInitFromNxDeclaresTheNxExtensionRecordsTheModeAndSyncs(t *testing.T) {
	spectest.Proves(t, initFromFeature, "init-from-nx", "init-from-nx-declares-the-nx-extension-records-the-mode-and-syncs")
	for mode, want := range map[string][]string{
		"import":   {"@putnami/nx", "@putnami/typescript"},
		"delegate": {"@putnami/nx"},
	} {
		t.Run(mode, func(t *testing.T) {
			dir := nxRepository(t)
			calls := stubInitFrom(t, false, "")
			out, err := captureStdout(t, func() error {
				return WorkspaceInit(context.Background(), "", []string{"--from", "nx", "--mode", mode, "--workspace", "shop"}, LifecycleEnv{})
			})
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, out)
			}
			cfg := readInitConfig(t, dir)
			if !slices.Equal(cfg.Extensions, want) || cfg.Options["@putnami/nx"].Mode != mode {
				t.Errorf("workspace config = %+v, want %v and the mode %s", cfg, want, mode)
			}
			if !slices.Equal(calls.installed, want) {
				t.Errorf("installed = %v, want %v", calls.installed, want)
			}
			if calls.synced == nil || !slices.Contains(calls.synced.Extensions.Names(), "@putnami/nx") {
				t.Errorf("projects sync ran on %+v", calls.synced)
			}
		})
	}

	// Without nx.json, init refuses.
	t.Chdir(t.TempDir())
	if err := WorkspaceInit(context.Background(), "", []string{"--from", "nx"}, LifecycleEnv{}); err == nil || !strings.Contains(err.Error(), "holds no nx.json") || !errors.Is(err, protocolcli.ErrUsage) {
		t.Errorf("err = %v, want a usage error naming nx.json", err)
	}
}

func TestInitFromAsksTheModeWithImportPreselected(t *testing.T) {
	spectest.Proves(t, initFromFeature, "init-asks-the-mode", "init-from-asks-the-mode-with-import-preselected")
	for name, c := range map[string]struct {
		terminal bool
		answer   string
		args     []string
		want     string
		asked    bool
	}{
		"an empty answer imports":    {true, "\n", nil, "import", true},
		"2 delegates":                {true, "2\n", nil, "delegate", true},
		"a second try":               {true, "maybe\ndelegate\n", nil, "delegate", true},
		"no terminal imports":        {false, "", nil, "import", false},
		"--mode skips the question":  {true, "", []string{"--mode", "delegate"}, "delegate", false},
		"--mode= skips the question": {true, "", []string{"--mode=import"}, "import", false},
		"a closed input imports":     {true, "", nil, "import", true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := nxRepository(t)
			stubInitFrom(t, c.terminal, c.answer)
			out, err := captureStdout(t, func() error {
				return WorkspaceInit(context.Background(), "", append([]string{"--from", "nx"}, c.args...), LifecycleEnv{})
			})
			if err != nil {
				t.Fatalf("WorkspaceInit: %v\n%s", err, out)
			}
			if got := readInitConfig(t, dir).Options["@putnami/nx"].Mode; got != c.want {
				t.Errorf("mode = %v, want %s\n%s", got, c.want, out)
			}
			if asked := strings.Contains(out, "1) import"); asked != c.asked {
				t.Errorf("asked = %v, want %v\n%s", asked, c.asked, out)
			}
			if c.asked && !strings.Contains(out, "(default)") {
				t.Errorf("import is preselected:\n%s", out)
			}
		})
	}

	// Pants delegates without a question, and records no mode.
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "pants.toml"), []byte("[GLOBAL]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubInitFrom(t, true, "")
	out, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", []string{"--from", "pants"}, LifecycleEnv{})
	})
	if err != nil || strings.Contains(out, "1) import") || readInitConfig(t, dir).Options != nil {
		t.Errorf("pants: err %v, options %v\n%s", err, readInitConfig(t, dir).Options, out)
	}

	for name, c := range map[string]struct {
		args   []string
		marker string
		want   string
	}{
		"an unknown mode":      {[]string{"--from", "nx", "--mode", "mirror"}, "nx.json", `--mode "mirror": --from nx adopts in import or delegate`},
		"pants imports":        {[]string{"--from", "pants", "--mode", "import"}, "pants.toml", `--mode "import": --from pants adopts in delegate`},
		"--mode without from":  {[]string{"--mode", "import"}, "nx.json", "give --from <tool>"},
		"--apply without from": {[]string{"--apply"}, "nx.json", "give --from <tool>"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if err := os.WriteFile(filepath.Join(dir, c.marker), []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := WorkspaceInit(context.Background(), "", c.args, LifecycleEnv{})
			if err == nil || !strings.Contains(err.Error(), c.want) || !errors.Is(err, protocolcli.ErrUsage) {
				t.Fatalf("err = %v, want a usage error with %q", err, c.want)
			}
		})
	}
}

func TestInitFromRerunPreviewsAndWritesOnlyWithApply(t *testing.T) {
	spectest.Proves(t, initFromFeature, "init-rerun-shows-a-diff", "init-from-rerun-previews-and-writes-only-with-apply")
	dir := nxRepository(t)
	workspace := `{
  "name": "shop",
  "extensions": [
    "@putnami/nx",
    "@putnami/typescript"
  ],
  "includes": [],
  "options": {
    "@putnami/nx": {
      "mode": "import"
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without --mode, the recorded mode is previewed; nothing is asked.
	calls := stubInitFrom(t, true, "")
	out, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), dir, []string{"--from", "nx"}, LifecycleEnv{})
	})
	if err != nil || !slices.Equal(calls.previewed, []string{"import"}) || calls.synced != nil || strings.Contains(out, "1) import") {
		t.Fatalf("err %v, previewed %v, synced %v\n%s", err, calls.previewed, calls.synced, out)
	}

	// Another mode is previewed with the change of the workspace option, and
	// nothing is written.
	calls = stubInitFrom(t, false, "")
	out, err = captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), dir, []string{"--from", "nx", "--mode", "delegate"}, LifecycleEnv{})
	})
	if err != nil || !slices.Equal(calls.previewed, []string{"delegate"}) || calls.synced != nil || len(calls.installed) != 0 {
		t.Fatalf("err %v, calls %+v\n%s", err, calls, out)
	}
	if !strings.Contains(out, `options["@putnami/nx"].mode import → delegate`) || !strings.Contains(out, "putnami init --from nx --mode delegate --apply") {
		t.Errorf("the preview names the mode change and the command that applies it:\n%s", out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename)); string(data) != workspace {
		t.Errorf("a preview wrote %s", data)
	}

	// --apply records the mode, keeps the rest, and synchronizes.
	calls = stubInitFrom(t, false, "")
	out, err = captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), dir, []string{"--from", "nx", "--mode", "delegate", "--apply"}, LifecycleEnv{})
	})
	if err != nil || calls.synced == nil || len(calls.previewed) != 0 {
		t.Fatalf("err %v, calls %+v\n%s", err, calls, out)
	}
	cfg := readInitConfig(t, dir)
	if cfg.Options["@putnami/nx"].Mode != "delegate" || !slices.Equal(cfg.Extensions, []string{"@putnami/nx", "@putnami/typescript"}) || cfg.Name != "shop" {
		t.Errorf("workspace config = %+v", cfg)
	}

	// Back to import from a delegate-only workspace: --apply declares and
	// installs the TypeScript extension.
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(`{"name": "shop", "extensions": ["@putnami/nx"], "options": {"@putnami/nx": {"mode": "delegate"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	calls = stubInitFrom(t, false, "")
	out, err = captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), dir, []string{"--from", "nx", "--mode", "import", "--apply"}, LifecycleEnv{})
	})
	if err != nil || !slices.Equal(calls.installed, []string{"@putnami/typescript"}) {
		t.Fatalf("err %v, installed %v\n%s", err, calls.installed, out)
	}
	if cfg := readInitConfig(t, dir); !slices.Equal(cfg.Extensions, []string{"@putnami/nx", "@putnami/typescript"}) || cfg.Options["@putnami/nx"].Mode != "import" {
		t.Errorf("workspace config = %+v", cfg)
	}
}

func TestInitFromApplyKeepsTheObjectFormOfExtensions(t *testing.T) {
	spectest.Proves(t, initFromFeature, "init-rerun-shows-a-diff", "init-from-rerun-previews-and-writes-only-with-apply")
	dir := nxRepository(t)
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(`{"name": "shop", "extensions": {"@putnami/nx": "^0.4.0", "@putnami/go": ""}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := stubInitFrom(t, false, "")
	out, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), dir, []string{"--from", "nx", "--mode", "import", "--apply"}, LifecycleEnv{})
	})
	if err != nil || !slices.Equal(calls.installed, []string{"@putnami/typescript"}) {
		t.Fatalf("err %v, installed %v\n%s", err, calls.installed, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
	var doc struct {
		Extensions map[string]string `json:"extensions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("extensions lost their object form: %v\n%s", err, data)
	}
	if want := map[string]string{"@putnami/nx": "^0.4.0", "@putnami/go": "", "@putnami/typescript": ""}; !maps.Equal(doc.Extensions, want) {
		t.Errorf("extensions = %v, want %v", doc.Extensions, want)
	}
}
