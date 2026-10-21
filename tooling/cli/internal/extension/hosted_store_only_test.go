package extension

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

func storeOnlyManifest(name string) string {
	return fmt.Sprintf(`{
	"name": %q,
	"version": "1.0.0",
	"cliContract": 4,
	"commands": {"build": {"run": [{"id": "b", "task": "t"}]}},
	"tasks": {"t": {"kind": "command", "command": "echo"}}
}`, name)
}

// nodeModulesDir is the directory a package manager installs name into. No
// source of extensions reads it, so a test writes an extension there to prove
// that it does not load, or that a hosted run refuses a path into it.
func nodeModulesDir(root, name string) string {
	return filepath.Join(root, installedPackagesDirName, name)
}

// storeOnlyFixture writes a workspace with three extensions that can load: a
// workspace project (@acme/local), a registry extension present both in
// node_modules and, through its stable link, in the artifact store
// (@acme/tool), and a directory committed where the stable link belongs
// (@acme/planted). A devDependency in node_modules (@acme/dev) never loads.
func storeOnlyFixture(t *testing.T) (ws, storeRoot string, cfg *wsproto.Config) {
	t.Helper()
	ws, storeRoot = t.TempDir(), t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)

	writeExtensionManifest(t, filepath.Join(ws, "tools", "local"), storeOnlyManifest("@acme/local"))
	writeExtensionManifest(t, nodeModulesDir(ws, "@acme/tool"), storeOnlyManifest("@acme/tool"))
	stored := filepath.Join(storeRoot, "extensions", "acme-tool@1.0.0")
	writeExtensionManifest(t, stored, storeOnlyManifest("@acme/tool"))
	link := layout.StableDir(ws, layout.Extensions, "@acme/tool")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stored, link); err != nil {
		t.Fatal(err)
	}
	writeExtensionManifest(t, layout.StableDir(ws, layout.Extensions, "@acme/planted"), storeOnlyManifest("@acme/planted"))
	writeExtensionManifest(t, nodeModulesDir(ws, "@acme/dev"), storeOnlyManifest("@acme/dev"))
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte(`{"devDependencies": {"@acme/dev": "1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@acme/tool": "1.0.0", "@acme/planted": "1.0.0",
	}}}
	return ws, storeRoot, cfg
}

// A hosted run keeps the extensions installed from the artifact store and the
// workspace's own path extensions: a registry extension loads through its
// stable link even when node_modules holds a copy, and the workspace project
// loads from its path. A directory committed where a link belongs is skipped
// with the reason. A run without the run credential discovers the same three.
// A devDependency in node_modules loads in neither run.
func TestHostedDiscoveryKeepsStoreAndPathExtensions(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "hosted-run-runs-only-store-and-path-extensions")
	ws, storeRoot, cfg := storeOnlyFixture(t)

	local, err := DiscoverExtensionsDetailed(ws, cfg, []string{"tools/local"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(local.Extensions); got != 3 {
		t.Fatalf("a local run discovered %v, want 3 extensions", extensionNames(local.Extensions))
	}
	if tool := FindExtensionByName(local.Extensions, "@acme/tool"); tool == nil || tool.Path != layout.StableDir(ws, layout.Extensions, "@acme/tool") {
		t.Fatalf("a local run loads @acme/tool from its stable link, got %+v", tool)
	}
	if FindExtensionByName(local.Extensions, "@acme/dev") != nil {
		t.Error("a local run loaded the devDependency from node_modules")
	}

	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	hosted, err := DiscoverExtensionsDetailed(ws, cfg, []string{"tools/local"})
	if err != nil {
		t.Fatal(err)
	}
	if got := extensionNames(hosted.Extensions); !slices.Equal(got, []string{"@acme/local", "@acme/tool"}) {
		t.Fatalf("a hosted run discovered %v, want [@acme/local @acme/tool]", got)
	}
	tool := FindExtensionByName(hosted.Extensions, "@acme/tool")
	resolved, _ := filepath.EvalSymlinks(tool.Path)
	if root, _ := filepath.EvalSymlinks(storeRoot); !within(root, resolved) {
		t.Errorf("@acme/tool loaded from %s, want the artifact store %s", resolved, root)
	}
	if local := FindExtensionByName(hosted.Extensions, "@acme/local"); local.Jobs["build"] == nil || local.Jobs["build"].ExtensionPath != "tools/local" {
		t.Errorf("a hosted run lost the build job of the path extension @acme/local: %+v", local.Jobs)
	}
	skipped := map[string]bool{}
	for _, skip := range hosted.Skipped {
		if errors.Is(skip.Reason, errNotFromTheStore) {
			skipped[skip.Name] = true
		}
	}
	if !skipped["@acme/planted"] || skipped["@acme/local"] {
		t.Errorf("a hosted run skipped %v as not from the store, want only @acme/planted: %+v", skipped, hosted.Skipped)
	}
	if FindExtensionByName(hosted.Extensions, "@acme/dev") != nil {
		t.Error("a hosted run loaded the devDependency from node_modules")
	}
}

// storeInstalled writes the build of name into the artifact store at
// storeRoot and links it where the workspace ws installs it, as an install
// does.
func storeInstalled(t *testing.T, ws, storeRoot, name string) {
	t.Helper()
	stored := filepath.Join(storeRoot, "extensions", strings.ReplaceAll(strings.TrimPrefix(name, "@"), "/", "-")+"@1.0.0")
	writeExtensionManifest(t, stored, storeOnlyManifest(name))
	link := layout.StableDir(ws, layout.Extensions, name)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stored, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// A path extension is one the workspace declares by a path inside it: a
// workspace project, or a path-shaped `extensions` key. A hosted run skips,
// with the reason, an extension loaded from an absolute path, inside the
// workspace or outside it, a declared path that links out of the workspace,
// and a declared path inside a node_modules directory, at the root or deeper.
// A key that names an extension, even one equal to a workspace directory or
// project path, loads its build from the artifact store; when that build is
// not installed, the project of that name is skipped with a reason that names
// the pin. A run without the run credential loads every one of them from the
// workspace.
func TestHostedDiscoverySkipsEveryOtherLocalSource(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "hosted-run-runs-only-store-and-path-extensions")
	ws, outside, storeRoot := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)
	writeExtensionManifest(t, filepath.Join(ws, "tools", "declared"), storeOnlyManifest("@acme/declared"))
	writeExtensionManifest(t, filepath.Join(ws, "tools", "absolute"), storeOnlyManifest("@acme/absolute-inside"))
	writeExtensionManifest(t, filepath.Join(outside, "ext"), storeOnlyManifest("@acme/absolute-outside"))
	writeExtensionManifest(t, nodeModulesDir(ws, "nm"), storeOnlyManifest("@acme/node-modules"))
	writeExtensionManifest(t, nodeModulesDir(filepath.Join(ws, "packages", "web"), "nested"), storeOnlyManifest("@acme/nested"))
	writeExtensionManifest(t, filepath.Join(outside, "escaped"), storeOnlyManifest("@acme/escaped"))
	if err := os.Symlink(filepath.Join(outside, "escaped"), filepath.Join(ws, "tools", "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A directory and a project whose paths equal the extension names that
	// their keys pin; the store holds both builds.
	writeExtensionManifest(t, filepath.Join(ws, "@acme", "named"), storeOnlyManifest("@acme/named"))
	writeExtensionManifest(t, filepath.Join(ws, "@acme", "project"), storeOnlyManifest("@acme/project"))
	// A project whose name a key pins, with no build installed.
	writeExtensionManifest(t, filepath.Join(ws, "tools", "unpinned"), storeOnlyManifest("@acme/unpinned"))
	storeInstalled(t, ws, storeRoot, "@acme/named")
	storeInstalled(t, ws, storeRoot, "@acme/project")
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"/tools/declared":                      "",
		filepath.Join(ws, "tools", "absolute"): "",
		filepath.Join(outside, "ext"):          "",
		"/node_modules/nm":                     "",
		"./packages/web/node_modules/nested":   "",
		"/tools/escape":                        "",
		"@acme/named":                          "1.0.0",
		"@acme/project":                        "1.0.0",
		"@acme/unpinned":                       "1.0.0",
	}}}
	projects := []string{filepath.Join("@acme", "project"), filepath.Join("tools", "unpinned")}
	fromWorkspace := func(result *DiscoveryResult, name string) bool {
		ext := FindExtensionByName(result.Extensions, name)
		return ext != nil && ext.LocalSource
	}

	local, err := DiscoverExtensionsDetailed(ws, cfg, projects)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(local.Extensions); got != 9 {
		t.Fatalf("a local run discovered %v, want nine extensions", extensionNames(local.Extensions))
	}
	for _, name := range []string{"@acme/named", "@acme/project", "@acme/unpinned"} {
		if !fromWorkspace(local, name) {
			t.Errorf("a local run loaded %s from the store, want its workspace directory", name)
		}
	}

	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	hosted, err := DiscoverExtensionsDetailed(ws, cfg, projects)
	if err != nil {
		t.Fatal(err)
	}
	if got := extensionNames(hosted.Extensions); !slices.Equal(got, []string{"@acme/declared", "@acme/named", "@acme/project"}) {
		t.Fatalf("a hosted run discovered %v, want [@acme/declared @acme/named @acme/project]", got)
	}
	for _, name := range []string{"@acme/named", "@acme/project"} {
		if ext := FindExtensionByName(hosted.Extensions, name); ext == nil || !InArtifactStore(ws, ext) {
			t.Errorf("a hosted run loaded %s as %+v, want its build from the artifact store", name, ext)
		}
	}
	reasons := map[error][]string{}
	for _, skip := range hosted.Skipped {
		for _, reason := range []error{errNotFromTheStore, errInstalledPackage, errPinnedBuildMissing} {
			if errors.Is(skip.Reason, reason) {
				reasons[reason] = append(reasons[reason], skip.Name)
			}
		}
	}
	for reason, want := range map[error][]string{
		errNotFromTheStore:    {"@acme/absolute-inside", "@acme/absolute-outside", "@acme/escaped"},
		errInstalledPackage:   {"@acme/nested", "@acme/node-modules"},
		errPinnedBuildMissing: {"@acme/unpinned"},
	} {
		got := reasons[reason]
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("a hosted run skipped %v with %q, want %v", got, reason, want)
		}
	}
	for _, skip := range hosted.Skipped {
		if errors.Is(skip.Reason, errPinnedBuildMissing) && !strings.Contains(skip.Reason.Error(), "pins @acme/unpinned") {
			t.Errorf("the skip of an uninstalled pin does not name the pin: %v", skip.Reason)
		}
	}
	if strings.Contains(errInstalledPackage.Error(), "declare it by its path") {
		t.Errorf("the node_modules skip tells the user to declare a path: %v", errInstalledPackage)
	}
}

// A path extension serves no provider capability on a hosted run: each
// reserved provider command it declares is removed, with a record that names
// the capability and the extension, and its other commands and jobs stay. The
// extension loaded, so no skip record calls it unreadable; the cause of an
// absent provider names the removal. A store extension keeps its providers,
// and a run without the run credential keeps every command.
func TestHostedDiscoveryRemovesTheProviderCapabilitiesOfAPathExtension(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "path-extension-serves-no-provider")
	ws := t.TempDir()
	storeRoot := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)
	providers := []string{"credential-provider", "cache-provider", "runner-provider", "session-reporter", "log-reporter", "cloud-release-set"}
	manifest := func(name string) string {
		commands := []string{`"build": {"run": [{"id": "b", "task": "t"}]}`, `"publish": {"run": [{"id": "p", "task": "t"}]}`}
		for _, command := range providers {
			commands = append(commands, fmt.Sprintf(`%q: {"visibility": "internal", "run": [{"id": "p", "task": "t"}]}`, command))
		}
		return fmt.Sprintf(`{
	"name": %q,
	"version": "1.0.0",
	"cliContract": 4,
	"commands": {%s},
	"tasks": {"t": {"kind": "command", "command": "echo"}}
}`, name, strings.Join(commands, ", "))
	}
	writeExtensionManifest(t, filepath.Join(ws, "tools", "providers"), manifest("@acme/path-providers"))
	stored := filepath.Join(storeRoot, "extensions", "acme-cloud@1.0.0")
	writeExtensionManifest(t, stored, manifest("@acme/cloud"))
	link := layout.StableDir(ws, layout.Extensions, "@acme/cloud")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stored, link); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@acme/cloud": "1.0.0"}}}

	local, err := DiscoverExtensionsDetailed(ws, cfg, []string{"tools/providers"})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range providers {
		if FindExtensionByName(local.Extensions, "@acme/path-providers").Jobs[command] == nil {
			t.Errorf("a local run removed the %s job of the path extension", command)
		}
	}

	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	hosted, err := DiscoverExtensionsDetailed(ws, cfg, []string{"tools/providers"})
	if err != nil {
		t.Fatal(err)
	}
	path := FindExtensionByName(hosted.Extensions, "@acme/path-providers")
	cloud := FindExtensionByName(hosted.Extensions, "@acme/cloud")
	if path == nil || cloud == nil {
		t.Fatalf("a hosted run discovered %v, want both extensions", extensionNames(hosted.Extensions))
	}
	for _, kept := range []string{"build", "publish"} {
		if _, ok := path.Commands[kept]; !ok || path.Jobs[kept] == nil {
			t.Errorf("a hosted run removed the %s command of the path extension", kept)
		}
	}
	for _, skip := range hosted.Skipped {
		if skip.Name == "@acme/path-providers" {
			t.Errorf("a hosted run recorded the loaded path extension as skipped: %v", skip.Reason)
		}
	}
	removed := map[string]error{}
	for _, removal := range hosted.RemovedCapabilities {
		if removal.Extension == "@acme/path-providers" && filepath.Base(removal.Path) == "providers" {
			removed[removal.Command] = removal.Reason()
		}
	}
	for _, command := range providers {
		_, declared := path.Commands[command]
		_, visible := path.CommandVisibility[command]
		if declared || visible || path.Jobs[command] != nil {
			t.Errorf("a hosted run kept the %s capability of the path extension", command)
		}
		if reason := removed[command]; !errors.Is(reason, errPathExtensionProvider) ||
			!strings.Contains(reason.Error(), "path extension @acme/path-providers does not serve the "+command+" capability") {
			t.Errorf("no removal record names the %s capability of @acme/path-providers: %+v", command, hosted.RemovedCapabilities)
		}
		if cause := hosted.ProviderCause(command); cause != removed[command].Error() {
			t.Errorf("the cause of an absent %s provider is %q, want the removal %q", command, cause, removed[command])
		}
		if provider, err := ResolveReservedProvider(hosted.Extensions, command); err != nil || provider == nil || provider.ExtensionName != "@acme/cloud" {
			t.Errorf("%s resolves to %+v (err %v), want the store extension @acme/cloud", command, provider, err)
		}
	}
	if len(hosted.RemovedCapabilities) != len(providers) {
		t.Errorf("a hosted run removed %+v, want only the %d providers of the path extension", hosted.RemovedCapabilities, len(providers))
	}
	if len(local.RemovedCapabilities) != 0 {
		t.Errorf("a local run removed %+v, want nothing", local.RemovedCapabilities)
	}
}

// In a source workspace, whose lock records the workspace as the source of its
// CLI, a hosted run keeps the provider capabilities of a path extension whose
// runtime.prepare builds its runtime: `./putnamiw --print-engine` prepared it
// from the tested tree before the run (PreparedBySourceBuild). It still removes
// them from a path extension that declares no runtime.prepare, and from every
// path extension of a workspace whose lock pins a published CLI or that has no
// lock.
func TestHostedDiscoveryKeepsTheProvidersOfASourceBuiltPathExtension(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "source-built-providers", "a-source-workspace-path-extension-keeps-its-providers-on-a-hosted-run")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	providers := []string{"credential-provider", "cache-provider", "runner-provider", "session-reporter", "log-reporter", "cloud-release-set"}
	manifest := func(name, runtime string) string {
		commands := []string{`"build": {"run": [{"id": "b", "task": "t"}]}`}
		for _, command := range providers {
			commands = append(commands, fmt.Sprintf(`%q: {"visibility": "internal", "run": [{"id": "p", "task": "t"}]}`, command))
		}
		return fmt.Sprintf(`{
	"name": %q,
	"version": "1.0.0",
	"cliContract": 4,
	%s
	"commands": {%s},
	"tasks": {"t": {"kind": "command", "command": "echo"}}
}`, name, runtime, strings.Join(commands, ", "))
	}
	ws := t.TempDir()
	writeExtensionManifest(t, filepath.Join(ws, "tools", "built"), manifest("@acme/built",
		`"runtime": {"executable": "compiled/built", "prepare": {"command": "{extensionRoot}/bin/prepare", "args": ["--output", "{runtimeOutput}"], "inputs": ["cmd/**"]}},`))
	writeExtensionManifest(t, filepath.Join(ws, "tools", "plain"), manifest("@acme/plain", ""))
	projects := []string{"tools/built", "tools/plain"}

	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	for _, c := range []struct {
		name string
		cli  *lockfile.LockEntry
		kept []string
	}{
		{name: "a source workspace", cli: &lockfile.LockEntry{Source: lockfile.SourceWorkspace}, kept: []string{"@acme/built"}},
		{name: "a published CLI", cli: &lockfile.LockEntry{Version: "1.0.0"}},
		{name: "no lock"},
	} {
		if err := os.Remove(filepath.Join(ws, lockfile.LockFilename)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if c.cli != nil {
			locked := lockfile.NewLockFile()
			locked.SetCLI(*c.cli)
			if err := lockfile.WriteLockFile(ws, locked); err != nil {
				t.Fatal(err)
			}
		}
		hosted, err := DiscoverExtensionsDetailed(ws, &wsproto.Config{}, projects)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, name := range []string{"@acme/built", "@acme/plain"} {
			ext := FindExtensionByName(hosted.Extensions, name)
			if ext == nil {
				t.Fatalf("%s: a hosted run discovered %v, want %s", c.name, extensionNames(hosted.Extensions), name)
			}
			keeps := slices.Contains(c.kept, name)
			if PreparedBySourceBuild(ws, ext) != keeps {
				t.Errorf("%s: PreparedBySourceBuild(%s) = %v, want %v", c.name, name, !keeps, keeps)
			}
			for _, command := range providers {
				if kept := ext.Jobs[command] != nil; kept != keeps {
					t.Errorf("%s: a hosted run kept the %s capability of %s = %v, want %v", c.name, command, name, kept, keeps)
				}
			}
			if _, ok := ext.Commands["build"]; !ok || ext.Jobs["build"] == nil {
				t.Errorf("%s: a hosted run removed the build command of %s", c.name, name)
			}
		}
		removed := map[string]int{}
		for _, removal := range hosted.RemovedCapabilities {
			removed[removal.Extension]++
		}
		wantBuilt := len(providers)
		if slices.Contains(c.kept, "@acme/built") {
			wantBuilt = 0
		}
		if removed["@acme/plain"] != len(providers) || removed["@acme/built"] != wantBuilt {
			t.Errorf("%s: a hosted run removed %+v, want the %d providers of @acme/plain and %d of @acme/built",
				c.name, hosted.RemovedCapabilities, len(providers), wantBuilt)
		}
	}
}

// ProviderCause names the removed capabilities of the requested command and
// the load failures, and nothing for a result without either.
func TestProviderCause(t *testing.T) {
	var none *DiscoveryResult
	if cause := none.ProviderCause("cache-provider"); cause != "" {
		t.Errorf("a nil result has the cause %q, want none", cause)
	}
	result := &DiscoveryResult{
		RemovedCapabilities: []RemovedCapability{
			{Extension: "@acme/b", Path: "/ws/b", Command: "cache-provider"},
			{Extension: "@acme/a", Path: "/ws/a", Command: "cache-provider"},
			{Extension: "@acme/c", Path: "/ws/c", Command: "runner-provider"},
		},
	}
	cause := result.ProviderCause("cache-provider")
	a, b := strings.Index(cause, "@acme/a"), strings.Index(cause, "@acme/b")
	if a < 0 || b < a || strings.Contains(cause, "@acme/c") || strings.Contains(cause, "could not be loaded") {
		t.Errorf("the cache-provider cause is %q, want @acme/a then @acme/b and no load failure", cause)
	}
	result.Skipped = []SkippedExtension{{Name: "@acme/broken", Path: "/ws/broken", Reason: errors.New("bad manifest")}}
	if cause := result.ProviderCause("runner-provider"); !strings.HasPrefix(cause, result.RemovedCapabilities[2].Reason().Error()+"; ") ||
		!strings.Contains(cause, "@acme/broken") {
		t.Errorf("the runner-provider cause is %q, want the removal then the load failure", cause)
	}
	if cause := result.ProviderCause("session-reporter"); cause != SkippedProviderCause(result.Skipped) {
		t.Errorf("the session-reporter cause is %q, want only the load failure", cause)
	}
}

// OnlyFromPathExtensions returns the path extensions a capability was removed
// from, sorted, while no extension that stays declares it.
func TestOnlyFromPathExtensions(t *testing.T) {
	var none *DiscoveryResult
	if removed := none.OnlyFromPathExtensions("cache-provider"); removed != nil {
		t.Errorf("a nil result returns %v, want nil", removed)
	}
	result := &DiscoveryResult{
		Extensions: []*ExtensionDescription{{Name: "@acme/store", Commands: map[string]string{"runner-provider": "Run jobs."}}},
		RemovedCapabilities: []RemovedCapability{
			{Extension: "@acme/b", Path: "/ws/b", Command: "cache-provider"},
			{Extension: "@acme/a", Path: "/ws/a", Command: "cache-provider"},
			{Extension: "@acme/dep", Path: "/ws/dep", Command: "cloud-release-set", RequiredBy: "@acme/a"},
			{Extension: "@acme/c", Path: "/ws/c", Command: "runner-provider"},
		},
	}
	removed := result.OnlyFromPathExtensions("cache-provider")
	if len(removed) != 2 || removed[0].Extension != "@acme/a" || removed[1].Extension != "@acme/b" {
		t.Errorf("cache-provider returns %v, want @acme/a then @acme/b", removed)
	}
	if removed := result.OnlyFromPathExtensions("cloud-release-set"); removed != nil {
		t.Errorf("cloud-release-set, removed only from a dependency, returns %v, want nil", removed)
	}
	if removed := result.OnlyFromPathExtensions("runner-provider"); removed != nil {
		t.Errorf("runner-provider, which @acme/store serves, returns %v, want nil", removed)
	}
	if removed := result.OnlyFromPathExtensions("session-reporter"); removed != nil {
		t.Errorf("session-reporter, removed from no extension, returns %v, want nil", removed)
	}
}

// The `extensions` entry or argument that a hosted install loads as a local
// extension names one of the workspace's own path extensions; any other local
// path loads nothing.
func TestLoadWorkspacePathExtension(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	writeExtensionManifest(t, filepath.Join(ws, "tools", "local"), storeOnlyManifest("@acme/local"))
	writeExtensionManifest(t, nodeModulesDir(ws, "nm"), storeOnlyManifest("@acme/nm"))
	writeExtensionManifest(t, nodeModulesDir(filepath.Join(ws, "tools", "web"), "nested"), storeOnlyManifest("@acme/nested"))
	writeExtensionManifest(t, filepath.Join(outside, "ext"), storeOnlyManifest("@acme/outside"))
	for ref, want := range map[string]string{
		"/tools/local":                    "@acme/local",
		"./tools/local":                   "@acme/local",
		"tools/local":                     "",
		"/node_modules/nm":                "",
		"/tools/web/node_modules/nested":  "",
		"./tools/web/Node_Modules/nested": "",
		"../" + filepath.Base(outside):    "",
		filepath.Join(outside, "ext"):     "",
		"/tools/missing":                  "",
	} {
		got := LoadWorkspacePathExtension(ws, ref)
		switch {
		case want == "" && got != nil:
			t.Errorf("%s: loaded %s, want nothing", ref, got.Name)
		case want != "" && (got == nil || got.Name != want || got.RelPath != filepath.Join("tools", "local") || !got.LocalSource):
			t.Errorf("%s: loaded %+v, want the path extension %s", ref, got, want)
		}
	}

	// The installed-package directory is a link to another directory of the
	// workspace: a path into that directory loads nothing either.
	linked := t.TempDir()
	writeExtensionManifest(t, filepath.Join(linked, "pkgs", "sub", "nm"), storeOnlyManifest("@acme/nm"))
	if err := os.Symlink(filepath.Join(linked, "pkgs", "sub"), nodeModulesDir(linked, "")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := LoadWorkspacePathExtension(linked, "/pkgs/sub/nm"); got != nil {
		t.Errorf("/pkgs/sub/nm behind the installed-package link: loaded %s, want nothing", got.Name)
	}
}

// InStoreRoot accepts an extension whose directory resolves inside the store,
// through a link included, unless it is a local source; InArtifactStore also
// refuses a store placed inside the workspace.
func TestInStoreRootAndInArtifactStore(t *testing.T) {
	ws, storeRoot := t.TempDir(), t.TempDir()
	stored := filepath.Join(storeRoot, "extensions", "acme-tool@1.0.0")
	if err := os.MkdirAll(stored, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "link")
	if err := os.Symlink(stored, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	outside := t.TempDir()
	for name, c := range map[string]struct {
		ext  *ExtensionDescription
		want bool
	}{
		"in the store":           {&ExtensionDescription{Path: stored}, true},
		"linked into the store":  {&ExtensionDescription{Path: link}, true},
		"a local source":         {&ExtensionDescription{Path: stored, LocalSource: true}, false},
		"outside the store":      {&ExtensionDescription{Path: outside}, false},
		"a path that is missing": {&ExtensionDescription{Path: filepath.Join(storeRoot, "missing")}, false},
		"no path":                {&ExtensionDescription{}, false},
		"no extension":           {nil, false},
	} {
		if got := InStoreRoot(storeRoot, c.ext); got != c.want {
			t.Errorf("%s: InStoreRoot = %v, want %v", name, got, c.want)
		}
		t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)
		if got := InArtifactStore(ws, c.ext); got != c.want {
			t.Errorf("%s: InArtifactStore = %v, want %v", name, got, c.want)
		}
	}

	// A store inside the workspace holds repository files.
	inside := filepath.Join(ws, "store")
	extDir := filepath.Join(inside, "extensions", "acme-tool@1.0.0")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_ARTIFACT_DIR", inside)
	ext := &ExtensionDescription{Path: extDir}
	if !InStoreRoot(inside, ext) || InArtifactStore(ws, ext) {
		t.Errorf("a store inside the workspace: InStoreRoot = %v, InArtifactStore = %v; want true, false",
			InStoreRoot(inside, ext), InArtifactStore(ws, ext))
	}
}

func extensionNames(extensions []*ExtensionDescription) []string {
	names := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		names = append(names, ext.Name)
	}
	return names
}
