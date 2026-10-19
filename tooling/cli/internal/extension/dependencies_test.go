package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

const dependenciesFeature = "cli/extension-dependencies"

// graphManifest returns the manifest of name stamped with contract (none when
// 0). It serves build and each of commands, and its extensionDependencies is
// deps, a JSON value, or absent when deps is "".
func graphManifest(name string, contract int, deps string, commands ...string) string {
	served := []string{`"build": {"run": [{"id": "b", "task": "t"}]}`}
	for _, command := range commands {
		served = append(served, fmt.Sprintf(`%q: {"visibility": "internal", "run": [{"id": "p", "task": "t"}]}`, command))
	}
	var fields []string
	if contract > 0 {
		fields = append(fields, fmt.Sprintf(`"cliContract": %d`, contract))
	}
	if deps != "" {
		fields = append(fields, `"extensionDependencies": `+deps)
	}
	fields = append(fields, `"commands": {`+strings.Join(served, ", ")+`}`, `"tasks": {"t": {"kind": "command", "command": "echo"}}`)
	return fmt.Sprintf(`{"name": %q, "version": "1.0.0", %s}`, name, strings.Join(fields, ", "))
}

// graphFixture is a workspace with its own artifact store and home
// directory, so no global configuration of the machine reaches it.
type graphFixture struct {
	ws, store, home string
}

func newGraphFixture(t *testing.T) graphFixture {
	t.Helper()
	f := graphFixture{ws: t.TempDir(), store: t.TempDir(), home: t.TempDir()}
	t.Setenv("PUTNAMI_ARTIFACT_DIR", f.store)
	t.Setenv("HOME", f.home)
	return f
}

// install writes the build of name into the artifact store and links it where
// the workspace installs it, as an install does.
func (f graphFixture) install(t *testing.T, name, manifest string) {
	t.Helper()
	stored := filepath.Join(f.store, "extensions", layout.EncodeName(name)+"@1.0.0")
	writeExtensionManifest(t, stored, manifest)
	link := layout.StableDir(f.ws, layout.Extensions, name)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stored, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// pin writes a lock that pins each of names at 1.0.0.
func (f graphFixture) pin(t *testing.T, names ...string) {
	t.Helper()
	lf := lockfile.NewLockFile()
	for _, name := range names {
		lf.SetExtension(name, lockfile.LockEntry{Version: "1.0.0"})
	}
	if err := lockfile.WriteLockFile(f.ws, lf); err != nil {
		t.Fatal(err)
	}
}

// config writes putnami.workspace.json declaring keys, and returns its config.
func (f graphFixture) config(t *testing.T, keys ...string) *wsproto.Config {
	t.Helper()
	list := make(map[string]string, len(keys))
	for _, key := range keys {
		list[key] = ""
	}
	writeJSON(t, filepath.Join(f.ws, wsproto.WorkspaceConfigFilename), map[string]map[string]string{"extensions": list})
	return &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: list}}
}

// devDependencies writes the root package.json declaring names.
func (f graphFixture) devDependencies(t *testing.T, names ...string) {
	t.Helper()
	deps := make(map[string]string, len(names))
	for _, name := range names {
		deps[name] = "1.0.0"
	}
	writeJSON(t, filepath.Join(f.ws, "package.json"), map[string]map[string]string{"devDependencies": deps})
}

func writeJSON(t *testing.T, path string, value map[string]map[string]string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func discover(t *testing.T, f graphFixture, cfg *wsproto.Config, projects ...string) *DiscoveryResult {
	t.Helper()
	result, err := DiscoverExtensionsDetailed(f.ws, cfg, projects)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func skipFor(result *DiscoveryResult, name string) error {
	for _, skip := range result.Skipped {
		if skip.Name == name {
			return skip.Reason
		}
	}
	return nil
}

// Discovery loads each locked dependency of a declared extension from the
// install layout, transitively, after the declared extensions. Removing the
// only dependent from the workspace stops loading the dependency.
func TestDiscoveryLoadsLockedDependenciesAfterTheDeclaredExtensions(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-graph-loads", "discovery-loads-a-locked-dependency")
	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 8, `{"@acme/dev": "^1.0.0"}`))
	f.install(t, "@acme/dev", graphManifest("@acme/dev", 8, `["@acme/base"]`))
	f.install(t, "@acme/base", graphManifest("@acme/base", 4, ""))
	f.pin(t, "@acme/app", "@acme/dev", "@acme/base")

	result := discover(t, f, f.config(t, "@acme/app"))
	if got, want := extensionNames(result.Extensions), []string{"@acme/app", "@acme/dev", "@acme/base"}; !slices.Equal(got, want) {
		t.Fatalf("discovered %v, want %v in that order", got, want)
	}
	want := []GraphDependency{
		{Name: "@acme/dev", RequiredBy: []string{"@acme/app"}, Loaded: true},
		{Name: "@acme/base", RequiredBy: []string{"@acme/dev"}, Loaded: true},
	}
	if fmt.Sprint(result.Dependencies) != fmt.Sprint(want) {
		t.Fatalf("dependencies = %+v, want %+v", result.Dependencies, want)
	}
	dev := FindExtensionByName(result.Extensions, "@acme/dev")
	if dev.Path != layout.StableDir(f.ws, layout.Extensions, "@acme/dev") || dev.LocalSource {
		t.Errorf("@acme/dev loaded from %s, want its stable link", dev.Path)
	}
	if len(result.Skipped) != 0 || len(result.MissingDependencies()) != 0 {
		t.Errorf("skipped %+v, missing %v, want none", result.Skipped, result.MissingDependencies())
	}

	alone := discover(t, f, f.config(t))
	if len(alone.Extensions) != 0 || len(alone.Dependencies) != 0 {
		t.Fatalf("without its dependent, discovery loaded %v and %+v", extensionNames(alone.Extensions), alone.Dependencies)
	}
}

// A root package.json development dependency never loads in place of a
// dependency: the locked build loads from the store, and an unpinned one loads
// from nowhere.
func TestDevDependenciesSkipADependencyName(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-graph-loads", "root-package-dependencies-skip-a-dependency-name")
	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 8, `{"@acme/dev": "", "@acme/lint": ""}`))
	f.install(t, "@acme/dev", graphManifest("@acme/dev", 8, ""))
	f.pin(t, "@acme/app", "@acme/dev")
	writeExtensionManifest(t, installedPackageDir(f.ws, "@acme/dev"), graphManifest("@acme/dev", 4, ""))
	writeExtensionManifest(t, installedPackageDir(f.ws, "@acme/lint"), graphManifest("@acme/lint", 4, ""))
	writeExtensionManifest(t, installedPackageDir(f.ws, "@acme/other"), graphManifest("@acme/other", 4, ""))
	f.devDependencies(t, "@acme/dev", "@acme/lint", "@acme/other")

	result := discover(t, f, f.config(t, "@acme/app"))
	if dev := FindExtensionByName(result.Extensions, "@acme/dev"); dev == nil || dev.Path != layout.StableDir(f.ws, layout.Extensions, "@acme/dev") {
		t.Fatalf("@acme/dev = %+v, want the locked build, not the development dependency", dev)
	}
	if lint := FindExtensionByName(result.Extensions, "@acme/lint"); lint != nil {
		t.Fatalf("the development dependency @acme/lint loaded from %s in place of an unpinned dependency", lint.Path)
	}
	if !IsDependencyNotPinned(skipFor(result, "@acme/lint")) {
		t.Errorf("@acme/lint skip = %v, want the unpinned dependency", skipFor(result, "@acme/lint"))
	}
	if FindExtensionByName(result.Extensions, "@acme/other") == nil {
		t.Error("a development dependency that is no dependency name stopped loading")
	}
}

// A dependency that does not load is reported with the extension that
// requires it: the lock does not pin it, its pinned build is not installed, or
// its manifest claims the dependency rung below 8. The missing-extension check
// names each one.
func TestADependencyThatDoesNotLoadNamesItsDependent(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-graph-loads", "a-dependency-that-does-not-load-names-its-dependent")
	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 8, `{"@acme/unpinned": "", "@acme/absent": "", "@acme/old": ""}`))
	f.install(t, "@acme/old", graphManifest("@acme/old", 7, `{"@acme/below": ""}`))
	f.pin(t, "@acme/app", "@acme/absent", "@acme/old")

	result := discover(t, f, f.config(t, "@acme/app"))
	for name, want := range map[string]string{
		"@acme/unpinned": "does not pin it; run `putnami install`",
		"@acme/absent":   "the build the lock pins is not installed",
		"@acme/old":      "requires 8 for its extensionDependencies",
	} {
		reason := skipFor(result, name)
		if reason == nil || !strings.HasPrefix(reason.Error(), "@acme/app requires "+name+": ") || !strings.Contains(reason.Error(), want) {
			t.Errorf("%s skip = %v, want @acme/app named with %q", name, reason, want)
		}
	}
	if got, want := result.MissingDependencies(), []string{"@acme/absent", "@acme/old", "@acme/unpinned"}; !slices.Equal(got, want) {
		t.Errorf("missing dependencies = %v, want %v", got, want)
	}
	for _, dep := range result.Dependencies {
		if dep.Name == "@acme/below" {
			t.Errorf("discovery followed the edges of a manifest it refused: %+v", dep)
		}
	}
}

// The edges of an extension loaded from node_modules are not followed unless
// the workspace declares its name and the lock pins it. Such a manifest loads
// below rung 8, and a hook-only one loads with no stamp at all.
func TestNodeModulesEdgesAreNotFollowed(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "node-modules-edges", "node-modules-edges-are-not-followed")
	spectest.Proves(t, dependenciesFeature, "an-older-cli-refuses-a-dependent", "a-node-modules-manifest-loads-unstamped")
	f := newGraphFixture(t)
	writeExtensionManifest(t, installedPackageDir(f.ws, "@acme/web"), graphManifest("@acme/web", 4, `{"@acme/runtime": "^1.0.0"}`))
	writeExtensionManifest(t, installedPackageDir(f.ws, "@acme/analytics"), `{"name": "@acme/analytics", "extensionDependencies": ["@acme/runtime"],
		"hooks": {"preBuild": {"kind": "command", "command": "echo"}}, "commands": {}}`)
	f.devDependencies(t, "@acme/analytics")
	f.pin(t, "@acme/analytics")
	cfg := f.config(t, "@acme/web")

	result := discover(t, f, cfg)
	if got, want := extensionNames(result.Extensions), []string{"@acme/web", "@acme/analytics"}; !slices.Equal(got, want) {
		t.Fatalf("discovered %v, want %v: a node_modules manifest loads below rung 8", got, want)
	}
	if len(result.Dependencies) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("discovery followed node_modules edges: dependencies %+v, skipped %+v", result.Dependencies, result.Skipped)
	}
	lf, _ := lockfile.ReadLockFile(f.ws)
	if walk := WalkWorkspaceGraph(f.ws, cfg, lf, WalkOptions{}); len(walk.Dependencies) != 0 || len(walk.Issues) != 0 {
		t.Fatalf("the graph walk followed node_modules edges: %+v", walk)
	}
}

// A declared name the lock pins has its edges followed wherever discovery
// finds it, node_modules included, and loads there without the rung.
func TestAPinnedDeclaredNameIsFollowedFromNodeModules(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "node-modules-edges", "a-pinned-declared-name-is-followed-from-node-modules")
	f := newGraphFixture(t)
	writeExtensionManifest(t, installedPackageDir(f.ws, "@acme/web"), graphManifest("@acme/web", 4, `{"@acme/runtime": "^1.0.0"}`))
	f.install(t, "@acme/runtime", graphManifest("@acme/runtime", 8, ""))
	f.pin(t, "@acme/web", "@acme/runtime")
	cfg := f.config(t, "@acme/web")

	result := discover(t, f, cfg)
	if web := FindExtensionByName(result.Extensions, "@acme/web"); web == nil || web.Path != installedPackageDir(f.ws, "@acme/web") {
		t.Fatalf("@acme/web = %+v, want the node_modules copy", web)
	}
	want := []GraphDependency{{Name: "@acme/runtime", RequiredBy: []string{"@acme/web"}, Loaded: true}}
	if fmt.Sprint(result.Dependencies) != fmt.Sprint(want) {
		t.Fatalf("dependencies = %+v, want %+v", result.Dependencies, want)
	}
	if FindExtensionByName(result.Extensions, "@acme/runtime") == nil {
		t.Fatal("the locked dependency of a pinned node_modules root did not load")
	}
	lf, _ := lockfile.ReadLockFile(f.ws)
	if walk := WalkWorkspaceGraph(f.ws, cfg, lf, WalkOptions{}); fmt.Sprint(walk.Dependencies) != fmt.Sprint(want) {
		t.Fatalf("the graph walk = %+v, want %+v", walk.Dependencies, want)
	}
}

// An undeclared workspace project never replaces the build of a dependency.
// The locked build loads and records the project in PinnedOver, and when the
// build is not installed the project does not load in its place, locally or
// on a hosted run.
func TestAProjectNeverReplacesADependencyBuild(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "what-satisfies-a-dependency", "a-project-never-replaces-a-dependency-build")
	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 8, `{"@acme/dev": ""}`))
	f.install(t, "@acme/dev", graphManifest("@acme/dev", 8, ""))
	writeExtensionManifest(t, filepath.Join(f.ws, "tools", "dev"), graphManifest("@acme/dev", 8, ""))
	f.pin(t, "@acme/app", "@acme/dev")
	cfg := f.config(t, "@acme/app")

	result := discover(t, f, cfg, "tools/dev")
	dev := FindExtensionByName(result.Extensions, "@acme/dev")
	if dev == nil || dev.LocalSource || dev.PinnedOver != "tools/dev" {
		t.Fatalf("@acme/dev = %+v, want the locked build over the project tools/dev", dev)
	}
	if job := dev.Jobs["build"]; job == nil || job.ExtensionPath != "tools/dev" {
		t.Errorf("the build job of @acme/dev = %+v, want the project path tools/dev", job)
	}
	if n := len(slices.DeleteFunc(extensionNames(result.Extensions), func(name string) bool { return name != "@acme/dev" })); n != 1 {
		t.Errorf("@acme/dev loaded %d times, want once", n)
	}

	if err := os.Remove(layout.StableDir(f.ws, layout.Extensions, "@acme/dev")); err != nil {
		t.Fatal(err)
	}
	for _, hosted := range []bool{false, true} {
		if hosted {
			t.Cleanup(runcredential.SetForTest("run-bearer"))
		}
		result := discover(t, f, cfg, "tools/dev")
		if dev := FindExtensionByName(result.Extensions, "@acme/dev"); dev != nil {
			t.Fatalf("hosted=%v: the project loaded from %s in place of the dependency build", hosted, dev.Path)
		}
		if reason := skipFor(result, "@acme/dev"); reason == nil || !strings.HasPrefix(reason.Error(), "@acme/app requires @acme/dev: ") {
			t.Errorf("hosted=%v: @acme/dev skip = %v, want its dependent named", hosted, reason)
		}
	}
}

// Discovery sets aside a project of a dependency name even when the lock does
// not pin the dependency: the project does not load, and the dependency is
// reported with its dependent. A path declaration of that project satisfies
// the dependency instead.
func TestThePrePassSetsAsideAnUnpinnedDependencyName(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "what-satisfies-a-dependency", "the-pre-pass-sets-aside-an-unpinned-dependency-name")
	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 8, `{"@acme/dev": ""}`))
	writeExtensionManifest(t, filepath.Join(f.ws, "tools", "dev"), graphManifest("@acme/dev", 8, ""))
	f.pin(t, "@acme/app")

	result := discover(t, f, f.config(t, "@acme/app"), "tools/dev")
	if dev := FindExtensionByName(result.Extensions, "@acme/dev"); dev != nil {
		t.Fatalf("the project of an unpinned dependency name loaded from %s", dev.Path)
	}
	if reason := skipFor(result, "@acme/dev"); !IsDependencyNotPinned(reason) || !strings.HasPrefix(reason.Error(), "@acme/app requires @acme/dev: ") {
		t.Errorf("@acme/dev skip = %v, want the unpinned dependency of @acme/app", reason)
	}
	if got := result.MissingDependencies(); !slices.Equal(got, []string{"@acme/dev"}) {
		t.Errorf("missing dependencies = %v, want [@acme/dev]", got)
	}

	byPath := discover(t, f, f.config(t, "@acme/app", "/tools/dev"), "tools/dev")
	if dev := FindExtensionByName(byPath.Extensions, "@acme/dev"); dev == nil || !dev.LocalSource || dev.RelPath != "tools/dev" {
		t.Fatalf("@acme/dev = %+v, want the project the workspace declares by path", dev)
	}
	if len(byPath.Dependencies) != 0 {
		t.Errorf("a path declaration did not satisfy the dependency: %+v", byPath.Dependencies)
	}
}

// A global-configuration key that names a graph member is ignored with one
// warning that names the global configuration: the dependency loads from its
// lock pin, not from where the key would load it.
func TestAGlobalKeyForADependencyIsIgnoredByDiscovery(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "what-satisfies-a-dependency", "a-global-key-for-a-dependency-is-ignored")
	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 8, `{"@acme/dev": ""}`))
	f.install(t, "@acme/dev", graphManifest("@acme/dev", 8, ""))
	f.pin(t, "@acme/app", "@acme/dev")
	writeExtensionManifest(t, installedPackageDir(f.ws, "@acme/dev"), graphManifest("@acme/dev", 4, ""))
	writeJSON(t, filepath.Join(f.home, wsproto.GlobalConfigDir, wsproto.GlobalConfigFilename), map[string]map[string]string{"extensions": {"@acme/dev": "2.0.0"}})
	cfg := f.config(t, "@acme/app")
	cfg.Extensions.List["@acme/dev"] = "2.0.0"

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	result := discover(t, f, cfg)
	if dev := FindExtensionByName(result.Extensions, "@acme/dev"); dev == nil || dev.Path != layout.StableDir(f.ws, layout.Extensions, "@acme/dev") {
		t.Fatalf("@acme/dev = %+v, want the locked build, not the global key's node_modules copy", dev)
	}
	if _, ok := (GraphWalk{Dependencies: result.Dependencies}).Dependency("@acme/dev"); !ok {
		t.Errorf("@acme/dev is not a dependency of the graph: %+v", result.Dependencies)
	}
	if got := strings.Count(logs.String(), "the global key is ignored"); got != 1 || !strings.Contains(logs.String(), GlobalConfigDisplay) {
		t.Errorf("warnings = %q, want one that names %s", logs.String(), GlobalConfigDisplay)
	}
	if only := GlobalOnlyExtensions(f.ws); !only["@acme/dev"] || only["@acme/app"] {
		t.Errorf("global-only keys = %v, want only @acme/dev", only)
	}
}

// A dependency the workspace does not declare loses every reserved provider
// command, and discovery records each removal with its dependent. The
// declared extension keeps its providers, and declaring the dependency by name
// gives its providers back.
func TestADependencyLosesReservedProviderCommands(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-dependency-serves-no-reserved-provider", "a-dependency-loses-reserved-provider-commands")
	providers := []string{"credential-provider", "cache-provider", "runner-provider", "session-reporter", "log-reporter", "cloud-release-set"}
	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 8, `{"@acme/cloud": ""}`))
	f.install(t, "@acme/cloud", graphManifest("@acme/cloud", 8, "", providers...))
	f.pin(t, "@acme/app", "@acme/cloud")

	result := discover(t, f, f.config(t, "@acme/app"))
	cloud := FindExtensionByName(result.Extensions, "@acme/cloud")
	if cloud == nil || cloud.Jobs["build"] == nil {
		t.Fatalf("@acme/cloud = %+v, want it loaded with its build command", cloud)
	}
	removed := map[string]RemovedCapability{}
	for _, capability := range result.RemovedCapabilities {
		removed[capability.Command] = capability
	}
	for _, command := range providers {
		if _, ok := cloud.Commands[command]; ok || cloud.Jobs[command] != nil {
			t.Errorf("the dependency kept the %s provider", command)
		}
		capability, ok := removed[command]
		if !ok || capability.Extension != "@acme/cloud" || capability.RequiredBy != "@acme/app" {
			t.Errorf("no removal names %s of @acme/cloud and its dependent: %+v", command, result.RemovedCapabilities)
			continue
		}
		reason := capability.Reason()
		if !errors.Is(reason, errDependencyProvider) || !strings.Contains(reason.Error(), "a dependency of @acme/app, does not serve the "+command) {
			t.Errorf("%s removal reason = %v", command, reason)
		}
		if cause := result.ProviderCause(command); !strings.Contains(cause, "@acme/cloud") {
			t.Errorf("the cause of an absent %s provider is %q, want the removal", command, cause)
		}
	}

	declared := discover(t, f, f.config(t, "@acme/app", "@acme/cloud"))
	cloud = FindExtensionByName(declared.Extensions, "@acme/cloud")
	for _, command := range providers {
		if cloud.Jobs[command] == nil {
			t.Errorf("a declared @acme/cloud lost the %s provider", command)
		}
	}
	if len(declared.RemovedCapabilities) != 0 {
		t.Errorf("a declared extension lost providers: %+v", declared.RemovedCapabilities)
	}
}

// A manifest that names a registry dependency requires contract 8: the stamp
// is rung 8, so a CLI below it refuses the manifest as written for a newer
// putnami, and this CLI refuses such a manifest stamped below rung 8 from the
// artifact store or a declared path, without following its dependencies.
func TestADependentManifestRequiresContract8(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "an-older-cli-refuses-a-dependent", "a-dependent-manifest-requires-contract-8")
	dependent := &Manifest{
		Commands:      map[string]CommandDefinition{"build": {}},
		ExtensionDeps: Dependencies{List: map[string]string{"@acme/dev": ""}},
	}
	if got := extproto.RequiredCLIContract(dependent); got != 8 {
		t.Fatalf("a dependent requires contract %d, want 8", got)
	}
	newer := filepath.Join(t.TempDir(), "newer")
	writeExtensionManifest(t, newer, graphManifest("@acme/newer", protocolcli.LatestContract+1, `{"@acme/dev": ""}`))
	if _, err := LoadManifest(filepath.Join(newer, ManifestFilename)); err == nil || !strings.Contains(err.Error(), "requires a newer putnami") {
		t.Fatalf("a stamp above this CLI's latest rung = %v, want requires a newer putnami", err)
	}

	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 7, `{"@acme/dev": ""}`))
	f.install(t, "@acme/dev", graphManifest("@acme/dev", 8, ""))
	writeExtensionManifest(t, filepath.Join(f.ws, "tools", "local"), graphManifest("@acme/local", 7, `{"@acme/dev": ""}`))
	f.pin(t, "@acme/app", "@acme/dev")

	result := discover(t, f, f.config(t, "@acme/app", "/tools/local"))
	for _, name := range []string{"@acme/app", "@acme/local"} {
		if ext := FindExtensionByName(result.Extensions, name); ext != nil {
			t.Errorf("%s loaded under a stamp below rung 8", name)
		}
		if reason := skipFor(result, name); reason == nil || !strings.Contains(reason.Error(), "requires 8 for its extensionDependencies") {
			t.Errorf("%s skip = %v, want the rung-8 refusal", name, reason)
		}
	}
	if FindExtensionByName(result.Extensions, "@acme/dev") != nil || len(result.Dependencies) != 0 {
		t.Errorf("discovery followed the edges of a refused manifest: %+v", result.Dependencies)
	}
}

// An agent-content opt-in may name a dependency of the workspace graph: it
// resolves through the lock pin, and a node_modules package of that name does
// not shadow it. Without the dependent, the name is not declared.
func TestAgentContentMayNameADependency(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "agent-content-may-name-a-dependency", "agent-content-may-name-a-dependency")
	f := newGraphFixture(t)
	f.install(t, "@acme/app", graphManifest("@acme/app", 8, fmt.Sprintf(`{%q: ""}`, contentExtension)))
	manifest := packagedContentManifest("1.2.0")
	writeExtensionManifest(t, layout.StableDir(f.ws, layout.Extensions, contentExtension), manifest)
	lf := lockfile.NewLockFile()
	lf.SetExtension("@acme/app", lockfile.LockEntry{Version: "1.0.0"})
	lf.SetExtension(contentExtension, lockfile.LockEntry{Version: "1.2.0", ManifestHash: lockfile.HashBytes([]byte(manifest))})
	if err := lockfile.WriteLockFile(f.ws, lf); err != nil {
		t.Fatal(err)
	}

	if _, err := LocateAgentContent(f.ws, f.config(t), contentExtension); err == nil || !strings.Contains(err.Error(), "declares no extension") {
		t.Fatalf("without its dependent, locate = %v, want an undeclared extension", err)
	}
	cfg := f.config(t, "@acme/app")
	source, err := LocateAgentContent(f.ws, cfg, contentExtension)
	if err != nil {
		t.Fatalf("locate a dependency's agent content: %v", err)
	}
	if source.Name != contentExtension || source.Version != "1.2.0" || source.Local {
		t.Fatalf("source = %+v, want the locked dependency", source)
	}
	writeExtensionManifest(t, installedPackageDir(f.ws, contentExtension), packagedContentManifest("9.9.9"))
	f.devDependencies(t, contentExtension)
	if source, err := LocateAgentContent(f.ws, cfg, contentExtension); err != nil || source.Version != "1.2.0" {
		t.Fatalf("with a node_modules package of its name, locate = %+v, %v; want the locked dependency", source, err)
	}
}

// A request is a channel when it is in the portable alphabet and is neither a
// semantic version, with or without a leading v, nor a range. Only a channel
// other than latest is explicit.
func TestChannelRequests(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "an-explicit-channel-flows-down", "a-version-request-is-not-a-channel")
	for request, want := range map[string][2]bool{
		"canary":       {true, true},
		"beta-2":       {true, true},
		"latest":       {true, false},
		"1.2.3":        {false, false},
		"v1.2.3":       {false, false},
		"1.2.3-rc.1":   {false, false},
		"v2.0.0+build": {false, false},
		"^1.0.0":       {false, false},
		"~1.2":         {false, false},
		">=1.0.0":      {false, false},
		"*":            {false, false},
		"Canary":       {false, false},
		"":             {false, false},
	} {
		if got := IsChannelRequest(request); got != want[0] {
			t.Errorf("IsChannelRequest(%q) = %v, want %v", request, got, want[0])
		}
		if got := IsExplicitChannel(request); got != want[1] {
			t.Errorf("IsExplicitChannel(%q) = %v, want %v", request, got, want[1])
		}
	}
	for constraint, want := range map[string]bool{"^1.0.0": true, "1.2.3": true, "": false, "*": false, "latest": false} {
		if got := IsRangeConstraint(constraint); got != want {
			t.Errorf("IsRangeConstraint(%q) = %v, want %v", constraint, got, want)
		}
	}
	if SatisfiesRange("0.0.0", "*") || !SatisfiesRange("1.4.0", "^1.0.0") || SatisfiesRange("2.0.0", "^1.0.0") {
		t.Error("SatisfiesRange disagrees with the range")
	}
}

// DependencyPath walks the first dependents back to a root.
func TestDependencyPath(t *testing.T) {
	deps := []GraphDependency{
		{Name: "@acme/b", RequiredBy: []string{"@acme/a"}},
		{Name: "@acme/c", RequiredBy: []string{"@acme/b", "@acme/x"}},
		{Name: "@acme/d", RequiredBy: []string{"@acme/e"}},
		{Name: "@acme/e", RequiredBy: []string{"@acme/d"}},
	}
	if got := DependencyPath(deps, "@acme/c"); !slices.Equal(got, []string{"@acme/a", "@acme/b", "@acme/c"}) {
		t.Errorf("path = %v", got)
	}
	if got := DependencyPath(deps, "@acme/d"); !slices.Equal(got, []string{"@acme/e", "@acme/d"}) {
		t.Errorf("a cycle path = %v", got)
	}
	if got := DependencyPath(deps, "@acme/z"); got != nil {
		t.Errorf("an absent name has path %v", got)
	}
}

// copyFirstPartyProject copies the extension manifest of the repository
// project rel, and its putnami.json when it has one, to the same path in ws.
func copyFirstPartyProject(t *testing.T, repoRoot, ws, rel string) {
	t.Helper()
	for _, name := range []string{ManifestFilename, "putnami.json"} {
		data, err := os.ReadFile(filepath.Join(repoRoot, rel, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(ws, rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, rel, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The Go extension names @putnami/dev as a dependency, and the shipped
// @putnami/dev declared by path satisfies it: discovery loads it once, from
// that path, and its vocabulary merges as the eleven verbs in order, with the
// core's descriptions, aliases, related commands and default traits.
func TestTheShippedDevExtensionSatisfiesALanguageDependencyByPath(t *testing.T) {
	f := newGraphFixture(t)
	repoRoot := findRepoRoot(t)
	goExtension, dev := filepath.Join("go", "extension"), filepath.Join("tooling", "dev")
	copyFirstPartyProject(t, repoRoot, f.ws, goExtension)
	copyFirstPartyProject(t, repoRoot, f.ws, dev)

	// Control: without the path declaration, the dependency is missing.
	without := discover(t, f, f.config(t, "/go/extension"))
	if got := without.MissingDependencies(); !slices.Equal(got, []string{"@putnami/dev"}) {
		t.Fatalf("missing dependencies without /tooling/dev = %v, want [@putnami/dev]", got)
	}

	result := discover(t, f, f.config(t, "/go/extension", "/tooling/dev"))
	if FindExtensionByName(result.Extensions, "@putnami/go") == nil {
		t.Fatalf("@putnami/go did not load: skipped %v", result.Skipped)
	}
	loaded := FindExtensionByName(result.Extensions, "@putnami/dev")
	if loaded == nil || !loaded.LocalSource || loaded.RelPath != filepath.ToSlash(dev) {
		t.Fatalf("@putnami/dev = %+v, want the project the workspace declares by path", loaded)
	}
	if len(result.Dependencies) != 0 || len(result.MissingDependencies()) != 0 || skipFor(result, "@putnami/dev") != nil {
		t.Fatalf("the path declaration did not satisfy the dependency: dependencies %+v, skipped %v", result.Dependencies, result.Skipped)
	}
	if len(loaded.Jobs) != 0 {
		t.Errorf("@putnami/dev declares jobs %v; a vocabulary runs nothing", slices.Sorted(maps.Keys(loaded.Jobs)))
	}

	// The core's help lists its job commands in catalog order, and the
	// vocabulary order is the help order: the two must be the same list.
	var developmentVerbs []string
	descriptions := map[string]string{}
	for _, command := range commandmeta.PublicJobCommands() {
		developmentVerbs = append(developmentVerbs, command.Name)
		descriptions[command.Name] = command.Description
	}
	vocabulary := MergeVocabulary(result.Extensions, nil)
	if !slices.Equal(vocabulary.VerbNames(), developmentVerbs) || len(vocabulary.Warnings) != 0 {
		t.Fatalf("merged verbs %v with warnings %v, want the core's help order %v", vocabulary.VerbNames(), vocabulary.Warnings, developmentVerbs)
	}
	aliases := map[string][]string{}
	for _, alias := range commandmeta.BuiltinAliasList() {
		aliases[alias.Command] = append(aliases[alias.Command], alias.Name)
	}
	for _, name := range developmentVerbs {
		verb, _ := vocabulary.Verb(name)
		core, ok := commandmeta.Lookup(name)
		if !ok {
			t.Fatalf("the core catalog has no %s", name)
		}
		if verb.Description != descriptions[name] || !slices.Equal(verb.Related, core.Related) || !slices.Equal(verb.Aliases, aliases[name]) {
			t.Errorf("%s = %q aliases %v related %v; the core shows %q aliases %v related %v",
				name, verb.Description, verb.Aliases, verb.Related, descriptions[name], aliases[name], core.Related)
		}
		if got, want := vocabulary.DefaultTraits(name), extproto.EffectiveTraits(name, nil); got != want {
			t.Errorf("%s default traits = %+v, want the core defaults %+v", name, got, want)
		}
	}
	wantAliases := map[string]string{}
	for verb, names := range aliases {
		for _, name := range names {
			wantAliases[name] = verb
		}
	}
	if got := vocabulary.AliasMap(); !maps.Equal(got, wantAliases) {
		t.Errorf("merged aliases = %v, want the core's %v", got, wantAliases)
	}
}
