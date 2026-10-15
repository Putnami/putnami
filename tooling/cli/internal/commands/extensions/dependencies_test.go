package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

const dependenciesFeature = "cli/extension-dependencies"

// graphRelease is one release of a registry double's extension.
type graphRelease struct {
	// deps is its extensionDependencies, a JSON value, or "" for none.
	deps string
	// contract is its stamp, 8 when 0.
	contract int
	// commands are reserved commands it serves besides build.
	commands []string
	// hooks is its hooks object, a JSON value, or "" for none.
	hooks string
}

// graphRegistry is a registry double for graph installs. Its install writes
// each build where the installer does, links it, and returns the outcome the
// real install closure returns, so the graph reads every manifest from disk.
type graphRegistry struct {
	t *testing.T
	// ws is the workspace, and store the artifact store the builds go to.
	ws, store string
	// releases maps a name to its releases by version.
	releases map[string]map[string]graphRelease
	// channels maps a name to the version each channel answers. A version
	// prefixed with "?" is an answer that does not name it.
	channels map[string]map[string]string
	// delay, when set, runs in each install before it answers.
	delay func(name string)

	mu sync.Mutex
	// requests records "name request" per install and resolve, in call order;
	// request is "lock:<version>" for an install from a lock entry and
	// "resolve:<constraint>" for a resolve-only lookup.
	requests []string
}

func newGraphRegistry(t *testing.T) *graphRegistry {
	t.Helper()
	store := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PUTNAMI_ARTIFACT_DIR", store)
	return &graphRegistry{t: t, ws: t.TempDir(), store: store, releases: map[string]map[string]graphRelease{}, channels: map[string]map[string]string{}}
}

func (r *graphRegistry) publish(name, version, deps string) {
	r.publishRelease(name, version, graphRelease{deps: deps})
}

func (r *graphRegistry) publishRelease(name, version string, release graphRelease) {
	if r.releases[name] == nil {
		r.releases[name] = map[string]graphRelease{}
	}
	r.releases[name][version] = release
}

// publishWithInstallHook publishes a release whose onInstall hook records
// each run in marker, a file of the workspace.
func (r *graphRegistry) publishWithInstallHook(name, version, deps, marker string) {
	r.t.Helper()
	dir := filepath.Join(r.store, "extensions", layout.EncodeName(name)+"@"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	command := writeInstallHookProgram(r.t, dir, filepath.Join(r.ws, marker), fixtureproc.Program{})
	r.publishRelease(name, version, graphRelease{deps: deps,
		hooks: fmt.Sprintf(`{"onInstall": {"kind": "command", "command": %q, "cwd": "{workspaceRoot}"}}`, command)})
}

func (r *graphRegistry) channel(name, channel, answer string) {
	if r.channels[name] == nil {
		r.channels[name] = map[string]string{}
	}
	r.channels[name][channel] = answer
}

// declare writes putnami.workspace.json declaring extensions, and returns its
// config and bytes.
func (r *graphRegistry) declare(extensions map[string]string) (*wsproto.Config, []byte) {
	r.t.Helper()
	data, err := json.Marshal(map[string]map[string]string{"extensions": extensions})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.ws, wsproto.WorkspaceConfigFilename), data, 0o644); err != nil {
		r.t.Fatal(err)
	}
	return &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: maps.Clone(extensions)}}, data
}

// resolve answers a request the way the registry does: a lock entry's
// version, a channel's answer, the highest release for latest, and the
// highest release a range matches.
func (r *graphRegistry) resolve(name, request string, lockEntry *lockfile.LockEntry) (string, bool, error) {
	releases := r.releases[name]
	if len(releases) == 0 {
		return "", false, fmt.Errorf("%s is not in the registry", name)
	}
	if lockEntry != nil {
		if _, ok := releases[lockEntry.Version]; !ok {
			return "", false, fmt.Errorf("%s@%s is not in the registry", name, lockEntry.Version)
		}
		return lockEntry.Version, false, nil
	}
	if answer, ok := r.channels[name][request]; ok {
		version, unnamed := strings.CutPrefix(answer, "?")
		return version, unnamed, nil
	}
	versions := make([]string, 0, len(releases))
	for version := range releases {
		versions = append(versions, version)
	}
	extension.SortVersions(versions)
	if request == "" || request == "latest" || request == "*" {
		return versions[len(versions)-1], false, nil
	}
	for i := len(versions) - 1; i >= 0; i-- {
		if extension.SatisfiesRange(versions[i], request) {
			return versions[i], false, nil
		}
	}
	return "", false, fmt.Errorf("no release of %s matches %s", name, request)
}

func (r *graphRegistry) install(_ context.Context, name, constraint string, lockEntry *lockfile.LockEntry) (*artifactInstallOutcome, error) {
	request := constraint
	if lockEntry != nil {
		request = "lock:" + lockEntry.Version
	}
	r.mu.Lock()
	r.requests = append(r.requests, name+" "+request)
	r.mu.Unlock()
	if r.delay != nil {
		r.delay(name)
	}
	version, unnamed, err := r.resolve(name, constraint, lockEntry)
	if err != nil {
		return nil, err
	}
	// A lock entry whose digest is not the release's fails, as the real
	// installer's archive check does, before the stable link moves.
	if digest := "sha256-" + name + "@" + version; lockEntry != nil && lockEntry.Integrity != "" && lockEntry.Integrity != digest {
		return nil, fmt.Errorf("integrity mismatch for %s@%s: lock %s, got %s; run `putnami extensions update` to refresh the lock", name, version, lockEntry.Integrity, digest)
	}
	release := r.releases[name][version]
	dir := filepath.Join(r.store, "extensions", layout.EncodeName(name)+"@"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, extension.ManifestFilename), []byte(registryManifest(name, version, release)), 0o644); err != nil {
		return nil, err
	}
	link := layout.StableDir(r.ws, layout.Extensions, name)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(link)
	if err := os.Symlink(dir, link); err != nil {
		return nil, err
	}
	outcome := &artifactInstallOutcome{
		Version: version, Integrity: "sha256-" + name + "@" + version, Source: "registry/" + name + "@" + version,
		ManifestHash: "manifest-" + name + "@" + version, VersionUnnamed: unnamed,
	}
	withGraphManifest(outcome, dir, name)
	return outcome, nil
}

func registryManifest(name, version string, release graphRelease) string {
	contract := release.contract
	if contract == 0 {
		contract = 8
	}
	served := []string{`"build": {"run": [{"id": "b", "task": "t"}]}`}
	for _, command := range release.commands {
		served = append(served, fmt.Sprintf(`%q: {"visibility": "internal", "run": [{"id": "p", "task": "t"}]}`, command))
	}
	deps := ""
	if release.deps != "" {
		deps = `"extensionDependencies": ` + release.deps + ", "
	}
	if release.hooks != "" {
		deps += `"hooks": ` + release.hooks + ", "
	}
	return fmt.Sprintf(`{"name": %q, "version": %q, "cliContract": %d, %s"commands": {%s}, "tasks": {"t": {"kind": "command", "command": "echo"}}}`,
		name, version, contract, deps, strings.Join(served, ", "))
}

// ops returns the workspace extension ops over the registry double, with the
// real graph reader.
func (r *graphRegistry) ops() artifactOps {
	ws := r.ws
	return artifactOps{
		label: "extension", plural: "extensions",
		parseArg: parseExtensionArg, configMap: shared.BuildExtensionMap,
		workspacePins: extensionWorkspacePins, pinLiteral: extensionPinLiteral,
		lockGet:    func(lf *lockfile.LockFile, name string) (lockfile.LockEntry, bool) { return lf.GetExtension(name) },
		lockSet:    func(lf *lockfile.LockFile, name string, entry lockfile.LockEntry) { lf.SetExtension(name, entry) },
		lockRemove: func(lf *lockfile.LockFile, name string) { lf.RemoveExtension(name) },
		install:    r.install,
		resolve: func(_ context.Context, name, constraint string) (*artifactInstallOutcome, error) {
			r.mu.Lock()
			r.requests = append(r.requests, name+" resolve:"+constraint)
			r.mu.Unlock()
			version, unnamed, err := r.resolve(name, constraint, nil)
			if err != nil {
				return nil, err
			}
			if unnamed {
				// A resolve-only lookup reads the version from the answer
				// alone, and an answer without one falls back to 0.0.0.
				version = "0.0.0"
			}
			return &artifactInstallOutcome{Version: version}, nil
		},
		remove:     func(name string) error { return os.RemoveAll(layout.StableDir(ws, layout.Extensions, name)) },
		stableLink: func(name string) string { return layout.StableDir(ws, layout.Extensions, name) },
		installLocal: func(name string, out io.Writer) (bool, error) {
			local, err := resolveLocalExtensionRef(ws, name)
			if err != nil || local == nil {
				return false, err
			}
			if out != nil {
				printLocalExtension(out, name, local)
			}
			return true, nil
		},
		graph: workspaceGraphOps(ws),
	}
}

// run installs with args, and returns the human stream, stderr and the error.
func (r *graphRegistry) run(cfg *wsproto.Config, args ...string) (string, string, error) {
	r.t.Helper()
	return r.runContext(context.Background(), cfg, args...)
}

func (r *graphRegistry) runContext(ctx context.Context, cfg *wsproto.Config, args ...string) (string, string, error) {
	r.t.Helper()
	var out bytes.Buffer
	var err error
	stderr := sharedtest.CaptureStderr(r.t, func() {
		err = installArtifacts(ctx, r.ws, cfg, args, r.ops(), "", &out)
	})
	return out.String(), stderr, err
}

// mustRun installs with args and fails the test on an error.
func (r *graphRegistry) mustRun(cfg *wsproto.Config, args ...string) string {
	r.t.Helper()
	out, stderr, err := r.run(cfg, args...)
	if err != nil {
		r.t.Fatalf("install %v: %v\nstderr: %s", args, err, stderr)
	}
	return out
}

// locked returns the version the lock pins for each extension.
func (r *graphRegistry) locked() map[string]string {
	r.t.Helper()
	lf, err := lockfile.ReadLockFile(r.ws)
	if err != nil {
		r.t.Fatal(err)
	}
	pins := map[string]string{}
	if lf == nil {
		return pins
	}
	for name, entry := range lf.Extensions {
		pins[name] = entry.Version
	}
	return pins
}

func (r *graphRegistry) lockBytes() []byte {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.ws, lockfile.LockFilename))
	if err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	return data
}

// takeRequests returns the requests since the last call, and forgets them.
func (r *graphRegistry) takeRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	requests := r.requests
	r.requests = nil
	return requests
}

func requestsFor(requests []string, name string) []string {
	var matched []string
	for _, request := range requests {
		if strings.HasPrefix(request, name+" ") {
			matched = append(matched, strings.TrimPrefix(request, name+" "))
		}
	}
	return matched
}

func wantPins(t *testing.T, got, want map[string]string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("lock pins %v, want %v", got, want)
	}
}

// Installing a declared extension installs every registry extension its
// manifest names, transitively, and pins each one with an ordinary lock
// entry. The workspace file never receives a dependency.
func TestInstallingADependentInstallsAndPinsItsDependencies(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "install-resolves-the-graph", "installing-a-dependent-installs-and-pins-its-dependencies")
	spectest.Proves(t, dependenciesFeature, "install-resolves-the-graph", "the-workspace-file-never-receives-a-dependency")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0"}`)
	r.publish("@acme/dev", "1.0.0", "")
	r.publish("@acme/dev", "1.2.0", `["@acme/base"]`)
	r.publish("@acme/dev", "2.0.0", "")
	r.publish("@acme/base", "1.0.0", "")
	cfg, workspaceFile := r.declare(map[string]string{"@acme/app": ""})

	out := r.mustRun(cfg)
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/base": "1.0.0", "@acme/dev": "1.2.0"})
	for _, line := range []string{"✓ @acme/app@1.0.0\n", "✓ @acme/dev@1.2.0 (required by @acme/app)\n", "✓ @acme/base@1.0.0 (required by @acme/dev)\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}
	lf, err := lockfile.ReadLockFile(r.ws)
	if err != nil {
		t.Fatal(err)
	}
	if lf.Version != lockfile.NewLockFile().Version {
		t.Errorf("lock format %d, want the ordinary %d", lf.Version, lockfile.NewLockFile().Version)
	}
	if entry, _ := lf.GetExtension("@acme/dev"); entry.Integrity != "sha256-@acme/dev@1.2.0" || entry.Source != "registry/@acme/dev@1.2.0" {
		t.Errorf("the dependency's lock entry = %+v, want an ordinary entry", entry)
	}

	r.publish("@acme/dev", "1.3.0", "")
	if _, err := sharedtest.CaptureStdout(t, func() error {
		return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{}, "")
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(r.ws, wsproto.WorkspaceConfigFilename)); err != nil || !bytes.Equal(got, workspaceFile) {
		t.Fatalf("the workspace file changed to %s (%v), want it as written", got, err)
	}

	specific := newGraphRegistry(t)
	specific.releases = r.releases
	empty, _ := specific.declare(map[string]string{})
	specific.mustRun(empty, "@acme/app")
	wantPins(t, specific.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/dev": "1.3.0"})
}

// Only the workspace file satisfies a dependency: a dependency it declares by
// name installs once, as a root, and one it declares by a path whose manifest
// has that name installs nothing.
func TestASatisfiedDependencyInstallsNothing(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "what-satisfies-a-dependency", "a-satisfied-dependency-installs-nothing")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0", "@acme/local": ""}`)
	r.publish("@acme/dev", "1.1.0", "")
	r.publish("@acme/dev", "1.4.0", "")
	r.publish("@acme/local", "1.0.0", "")
	local := filepath.Join(r.ws, "tools", "local")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, extension.ManifestFilename), []byte(registryManifest("@acme/local", "0.0.1", graphRelease{})), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.declare(map[string]string{"@acme/app": "", "@acme/dev": "1.1.0", "/tools/local": ""})

	r.mustRun(cfg)
	requests := r.takeRequests()
	if got := requestsFor(requests, "@acme/dev"); !slices.Equal(got, []string{"1.1.0"}) {
		t.Errorf("@acme/dev requests = %v, want the declared root once", got)
	}
	if got := requestsFor(requests, "@acme/local"); len(got) != 0 {
		t.Errorf("a dependency the workspace declares by path was requested: %v", got)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/dev": "1.1.0"})
}

// An install follows no edge of an extension that loads from node_modules,
// unless the workspace declares its name and the lock pins it.
func TestAnInstallFollowsNodeModulesEdgesOnlyForAPinnedDeclaredName(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "node-modules-edges", "node-modules-edges-are-not-followed")
	spectest.Proves(t, dependenciesFeature, "node-modules-edges", "a-pinned-declared-name-is-followed-from-node-modules")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", "")
	r.publish("@acme/runtime", "1.0.0", "")
	r.publish("@acme/analytics-runtime", "1.0.0", "")
	for name, deps := range map[string]string{"@acme/web": `{"@acme/runtime": "^1.0.0"}`, "@acme/analytics": `["@acme/analytics-runtime"]`} {
		dir := filepath.Join(r.ws, "node_modules", filepath.FromSlash(name))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, extension.ManifestFilename), []byte(registryManifest(name, "1.0.0", graphRelease{deps: deps, contract: 4})), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.ws, "package.json"), []byte(`{"devDependencies": {"@acme/analytics": "1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.declare(map[string]string{"@acme/app": "", "@acme/web": ""})

	r.mustRun(cfg, "@acme/app")
	requests := r.takeRequests()
	if got := append(requestsFor(requests, "@acme/runtime"), requestsFor(requests, "@acme/analytics-runtime")...); len(got) != 0 {
		t.Fatalf("an install followed node_modules edges: %v", requests)
	}

	lf := lockfile.NewLockFile()
	for _, name := range []string{"@acme/app", "@acme/web"} {
		lf.SetExtension(name, lockfile.LockEntry{Version: "1.0.0"})
	}
	if err := lockfile.WriteLockFile(r.ws, lf); err != nil {
		t.Fatal(err)
	}
	r.mustRun(cfg, "@acme/app")
	requests = r.takeRequests()
	if got := requestsFor(requests, "@acme/runtime"); !slices.Equal(got, []string{"^1.0.0"}) {
		t.Fatalf("@acme/runtime requests = %v, want the pinned node_modules root's edge followed", got)
	}
	if got := requestsFor(requests, "@acme/analytics-runtime"); len(got) != 0 {
		t.Fatalf("an install followed the edge of an undeclared devDependency: %v", got)
	}
	if r.locked()["@acme/runtime"] != "1.0.0" {
		t.Errorf("lock pins %v, want @acme/runtime", r.locked())
	}
}

// A global-configuration key that names a graph member is ignored, with one
// warning that names the global configuration; the graph resolves the
// extension. A global key that names no member still installs.
func TestAGlobalKeyForADependencyIsIgnoredByAnInstall(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "what-satisfies-a-dependency", "a-global-key-for-a-dependency-is-ignored")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0"}`)
	r.publish("@acme/dev", "1.0.0", "")
	r.publish("@acme/dev", "2.0.0", "")
	r.publish("@acme/tool", "1.0.0", "")
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, wsproto.GlobalConfigDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, wsproto.GlobalConfigDir, wsproto.GlobalConfigFilename),
		[]byte(`{"extensions": {"@acme/dev": "2.0.0", "@acme/tool": ""}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.declare(map[string]string{"@acme/app": ""})
	cfg.Extensions.List["@acme/dev"] = "2.0.0"
	cfg.Extensions.List["@acme/tool"] = ""

	_, stderr, err := r.run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestsFor(r.takeRequests(), "@acme/dev"); !slices.Equal(got, []string{"^1.0.0"}) {
		t.Errorf("@acme/dev requests = %v, want the graph's constraint only", got)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/dev": "1.0.0", "@acme/tool": "1.0.0"})
	if strings.Count(stderr, extension.GlobalConfigDisplay+" declares @acme/dev") != 1 || strings.Contains(stderr, "@acme/tool") {
		t.Errorf("stderr = %q, want one warning that names %s and @acme/dev", stderr, extension.GlobalConfigDisplay)
	}

	stderr = sharedtest.CaptureStderr(t, func() {
		if _, err := sharedtest.CaptureStdout(t, func() error {
			return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{}, "")
		}); err != nil {
			t.Error(err)
		}
	})
	if got := requestsFor(r.takeRequests(), "@acme/dev"); !slices.Equal(got, []string{"^1.0.0"}) {
		t.Errorf("an update requested @acme/dev with %v, want the graph's constraint only", got)
	}
	if strings.Count(stderr, extension.GlobalConfigDisplay+" declares @acme/dev") != 1 {
		t.Errorf("update stderr = %q, want the warning once", stderr)
	}
}

// A specific install installs that extension and its graph only. An unpinned
// key that only the global configuration declares is neither requested nor
// pinned, and a global key for a dependency still warns once.
func TestASpecificInstallLeavesGlobalOnlyKeysAlone(t *testing.T) {
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0"}`)
	r.publish("@acme/dev", "1.0.0", "")
	r.publish("@acme/tool", "1.0.0", "")
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, wsproto.GlobalConfigDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, wsproto.GlobalConfigDir, wsproto.GlobalConfigFilename),
		[]byte(`{"extensions": {"@acme/dev": "1.0.0", "@acme/tool": ""}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.declare(map[string]string{})
	cfg.Extensions.List["@acme/dev"] = "1.0.0"
	cfg.Extensions.List["@acme/tool"] = ""

	_, stderr, err := r.run(cfg, "@acme/app")
	if err != nil {
		t.Fatalf("install @acme/app: %v\nstderr: %s", err, stderr)
	}
	if got := requestsFor(r.takeRequests(), "@acme/tool"); len(got) != 0 {
		t.Errorf("a specific install requested the global-only key @acme/tool: %v", got)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/dev": "1.0.0"})
	if strings.Count(stderr, extension.GlobalConfigDisplay+" declares @acme/dev") != 1 {
		t.Errorf("stderr = %q, want one warning that names %s and @acme/dev", stderr, extension.GlobalConfigDisplay)
	}
}

// A locked dependency resolves from its lock entry while that version
// satisfies every constraint.
func TestALockedDependencyResolvesFromTheLock(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-lock-wins-while-it-satisfies-the-graph", "a-locked-dependency-resolves-from-the-lock")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0"}`)
	r.publish("@acme/dev", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": ""})
	r.mustRun(cfg)
	r.takeRequests()
	r.publish("@acme/dev", "1.5.0", "")

	r.mustRun(cfg)
	if got := requestsFor(r.takeRequests(), "@acme/dev"); !slices.Equal(got, []string{"lock:1.0.0"}) {
		t.Fatalf("@acme/dev requests = %v, want its lock entry", got)
	}
	if r.locked()["@acme/dev"] != "1.0.0" {
		t.Fatalf("lock pins %v, want @acme/dev kept at 1.0.0", r.locked())
	}
}

// Under an explicit channel, every dependency below the requested extension
// resolves on that channel, at every hop, and no hop reads latest.
func TestAChannelFlowsDownEveryHop(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "an-explicit-channel-flows-down", "a-channel-flows-down-every-hop")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0"}`)
	r.publish("@acme/app", "2.0.0", `{"@acme/dev": "^2.0.0"}`)
	r.publish("@acme/dev", "1.0.0", "")
	r.publish("@acme/dev", "2.1.0", `{"@acme/base": ""}`)
	r.publish("@acme/base", "3.0.0", "")
	r.publish("@acme/base", "4.0.0", "")
	r.channel("@acme/app", "canary", "2.0.0")
	r.channel("@acme/dev", "canary", "2.1.0")
	r.channel("@acme/base", "canary", "3.0.0")
	cfg, _ := r.declare(map[string]string{})

	r.mustRun(cfg, "@acme/app@canary")
	if got, want := r.takeRequests(), []string{"@acme/app canary", "@acme/dev canary", "@acme/base canary"}; !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "2.0.0", "@acme/base": "3.0.0", "@acme/dev": "2.1.0"})
}

// A version or a range on the command line is not a channel: the
// dependencies resolve on their own constraints.
func TestAVersionRequestIsNotAChannel(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "an-explicit-channel-flows-down", "a-version-request-is-not-a-channel")
	for _, request := range []string{"1.0.0", "^1.0.0"} {
		r := newGraphRegistry(t)
		r.publish("@acme/app", "1.0.0", `{"@acme/dev": "~1.1.0"}`)
		r.publish("@acme/dev", "1.1.4", "")
		r.publish("@acme/dev", "1.2.0", "")
		r.channel("@acme/dev", request, "1.2.0")
		cfg, _ := r.declare(map[string]string{})

		r.mustRun(cfg, "@acme/app@"+request)
		if got := requestsFor(r.takeRequests(), "@acme/dev"); !slices.Equal(got, []string{"~1.1.0"}) {
			t.Errorf("after @acme/app@%s, @acme/dev requests = %v, want its own constraint", request, got)
		}
	}
}

// Outside an explicit channel, a non-empty constraint resolves as written and
// the registry picks the highest match; an empty one, *, and latest resolve on
// latest.
func TestOnLatestARangeResolvesAsWritten(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "an-explicit-channel-flows-down", "on-latest-a-range-resolves-as-written")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/ranged": "^1.0.0", "@acme/bare": "", "@acme/star": "*"}`)
	for _, name := range []string{"@acme/ranged", "@acme/bare", "@acme/star"} {
		r.publish(name, "1.0.0", "")
		r.publish(name, "1.5.0", "")
		r.publish(name, "2.0.0", "")
	}
	cfg, _ := r.declare(map[string]string{})

	r.mustRun(cfg, "@acme/app@latest")
	requests := r.takeRequests()
	for name, want := range map[string]string{"@acme/ranged": "^1.0.0", "@acme/bare": "latest", "@acme/star": "latest"} {
		if got := requestsFor(requests, name); !slices.Equal(got, []string{want}) {
			t.Errorf("%s requests = %v, want %s", name, got, want)
		}
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/bare": "2.0.0", "@acme/ranged": "1.5.0", "@acme/star": "2.0.0"})
}

// An explicit channel on the command line re-resolves the requested
// extension's subgraph on that channel, whatever the lock holds, and leaves
// the rest of the graph on the lock. --latest re-resolves the whole graph.
func TestACommandLineChannelReResolvesTheSubgraph(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-lock-wins-while-it-satisfies-the-graph", "a-command-line-channel-re-resolves-the-subgraph")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": ""}`)
	r.publish("@acme/other", "1.0.0", `{"@acme/lib": ""}`)
	r.publish("@acme/dev", "1.0.0", "")
	r.publish("@acme/lib", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": "", "@acme/other": ""})
	r.mustRun(cfg)
	r.takeRequests()
	r.publish("@acme/dev", "1.9.0", "")
	r.publish("@acme/lib", "1.5.0", "")
	r.channel("@acme/app", "canary", "1.0.0")
	r.channel("@acme/dev", "canary", "1.9.0")

	out := r.mustRun(cfg, "@acme/app@canary")
	requests := r.takeRequests()
	if got := requestsFor(requests, "@acme/dev"); !slices.Equal(got, []string{"resolve:canary", "canary"}) {
		t.Errorf("@acme/dev requests = %v, want the channel over the lock", got)
	}
	if got := requestsFor(requests, "@acme/lib"); !slices.Equal(got, []string{"lock:1.0.0"}) {
		t.Errorf("@acme/lib requests = %v, want its lock entry outside the channel's subgraph", got)
	}
	if !strings.Contains(out, "↑ @acme/dev: 1.0.0 → 1.9.0 (required by @acme/app)") {
		t.Errorf("output lacks the move:\n%s", out)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/dev": "1.9.0", "@acme/lib": "1.0.0", "@acme/other": "1.0.0"})

	r.mustRun(cfg, "--latest")
	if got := requestsFor(r.takeRequests(), "@acme/lib"); !slices.Equal(got, []string{"resolve:latest", "latest"}) {
		t.Errorf("--latest requested @acme/lib with %v, want latest", got)
	}
	if r.locked()["@acme/lib"] != "1.5.0" {
		t.Errorf("--latest pins %v, want @acme/lib 1.5.0", r.locked())
	}
}

// A locked version that misses a constraint resolves again, and the install
// reports the move with the dependent.
func TestAStaleLockEntryMovesToSatisfyTheGraph(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-lock-wins-while-it-satisfies-the-graph", "a-stale-lock-entry-moves-to-satisfy-the-graph")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0"}`)
	r.publish("@acme/dev", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": ""})
	r.mustRun(cfg)
	r.takeRequests()
	r.publish("@acme/app", "2.0.0", `{"@acme/dev": "^2.0.0"}`)
	r.publish("@acme/dev", "2.3.0", "")

	out := r.mustRun(cfg, "@acme/app@2.0.0")
	if got := requestsFor(r.takeRequests(), "@acme/dev"); !slices.Equal(got, []string{"^2.0.0"}) {
		t.Errorf("@acme/dev requests = %v, want the constraint the lock misses", got)
	}
	if !strings.Contains(out, "↑ @acme/dev: 1.0.0 → 2.3.0 (required by @acme/app)") {
		t.Errorf("output lacks the move:\n%s", out)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "2.0.0", "@acme/dev": "2.3.0"})

	var actions []InstallAction
	r.publish("@acme/app", "3.0.0", `{"@acme/dev": "^3.0.0"}`)
	r.publish("@acme/dev", "3.0.1", "")
	if err := installArtifactsWithOptions(context.Background(), r.ws, cfg, []string{"@acme/app@3.0.0"}, r.ops(), InstallOptions{
		OnAction: func(action InstallAction) { actions = append(actions, action) },
	}); err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(actions, func(a InstallAction) bool { return a.Name == "@acme/dev" }); i < 0 ||
		actions[i].OldVersion != "2.3.0" || actions[i].Version != "3.0.1" || actions[i].RequiredBy != "@acme/app" {
		t.Errorf("actions = %+v, want the move of @acme/dev with its dependent", actions)
	}
}

// A resolved version that misses a range fails the install, naming the
// dependency, its version and each dependent with its constraint. The lock is
// not written.
func TestAnUnmatchedRangeFailsNamingEachDependent(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "an-unmet-range-fails", "an-unmatched-range-fails-naming-each-dependent")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^2.0.0"}`)
	r.publish("@acme/lint", "1.0.0", `{"@acme/dev": "^2.1.0"}`)
	r.publish("@acme/tool", "1.0.0", `{"@acme/dev": ""}`)
	r.publish("@acme/dev", "1.0.0", "")
	r.publish("@acme/dev", "2.0.5", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": "", "@acme/lint": "", "@acme/tool": ""})
	before := r.lockBytes()

	_, stderr, err := r.run(cfg)
	want := "@acme/dev@2.0.5 does not satisfy @acme/app, which requires ^2.0.0; @acme/lint, which requires ^2.1.0; @acme/tool, which requires any version"
	if !errors.Is(err, errGraphUnresolved) || !strings.Contains(err.Error(), want) {
		t.Fatalf("install = %v, want %q", err, want)
	}
	if !strings.Contains(stderr, "✗ @acme/dev: "+want) {
		t.Errorf("stderr = %q, want the failure line", stderr)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Errorf("a failed graph wrote the lock")
	}
}

// A registry answer to a channel that does not name the version it resolved
// fails a dependency whose dependent states a range; without a range it
// installs.
func TestAnUnversionedChannelAnswerFailsARange(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "an-unmet-range-fails", "an-unversioned-channel-answer-fails-a-range")
	for _, tc := range []struct {
		constraint string
		fails      bool
	}{{"^1.0.0", true}, {"", false}} {
		r := newGraphRegistry(t)
		r.publish("@acme/app", "1.0.0", fmt.Sprintf(`{"@acme/dev": %q}`, tc.constraint))
		r.publish("@acme/dev", "1.4.0", "")
		r.channel("@acme/app", "canary", "1.0.0")
		r.channel("@acme/dev", "canary", "?1.4.0")
		cfg, _ := r.declare(map[string]string{})

		_, _, err := r.run(cfg, "@acme/app@canary")
		if !tc.fails {
			if err != nil {
				t.Errorf("without a range, an unnamed channel answer failed: %v", err)
			}
			continue
		}
		want := "@acme/dev: the registry answered channel canary without naming the version it resolved, so it cannot be checked against @acme/app, which requires ^1.0.0"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("install = %v, want %q", err, want)
		}
		if len(r.lockBytes()) != 0 {
			t.Errorf("a failed graph wrote the lock")
		}
	}
}

// A dependency the workspace declares by name satisfies the edge, and its
// version is still checked against every dependent's constraint.
func TestADeclaredDependencyIsCheckedAgainstItsDependents(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "an-unmet-range-fails", "a-declared-dependency-is-checked-against-its-dependents")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^2.0.0"}`)
	r.publish("@acme/dev", "1.0.0", "")
	r.publish("@acme/dev", "2.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": "", "@acme/dev": "1.0.0"})

	_, _, err := r.run(cfg)
	if err == nil || !strings.Contains(err.Error(), "@acme/dev@1.0.0 does not satisfy @acme/app, which requires ^2.0.0") {
		t.Fatalf("install = %v, want the declared dependency checked", err)
	}
	if len(r.lockBytes()) != 0 {
		t.Error("a failed graph wrote the lock")
	}

	cfg, _ = r.declare(map[string]string{"@acme/app": "", "@acme/dev": "2.0.0"})
	r.mustRun(cfg)
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/dev": "2.0.0"})
}

// The graph resolves level by level: every node of a level resolves before
// any node of the next one, whatever order the workers finish in. The members
// of a level sort by name, so the first dependent by name picks the request,
// and two runs give the same lock and the same report.
func TestTheResolutionOrderIsLevelByLevel(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-order-is-deterministic", "the-resolution-order-is-level-by-level")
	setup := func() (*graphRegistry, *wsproto.Config) {
		r := newGraphRegistry(t)
		r.publish("@acme/a", "1.0.0", `{"@acme/x": "", "@acme/y": "^1.0.0"}`)
		r.publish("@acme/b", "1.0.0", `{"@acme/y": "", "@acme/z": ""}`)
		r.publish("@acme/x", "1.0.0", `{"@acme/deep": ""}`)
		r.publish("@acme/z", "1.0.0", `{"@acme/deeper": ""}`)
		r.publish("@acme/y", "1.0.0", "")
		r.publish("@acme/y", "2.0.0", "")
		r.publish("@acme/deep", "1.0.0", "")
		r.publish("@acme/deeper", "1.0.0", "")
		delays := map[string]time.Duration{"@acme/x": 30 * time.Millisecond, "@acme/y": 15 * time.Millisecond}
		r.delay = func(name string) { time.Sleep(delays[name]) }
		cfg, _ := r.declare(map[string]string{"@acme/b": "", "@acme/a": ""})
		return r, cfg
	}
	level := map[string]int{"@acme/a": 0, "@acme/b": 0, "@acme/x": 1, "@acme/y": 1, "@acme/z": 1, "@acme/deep": 2, "@acme/deeper": 2}

	var outputs, locks []string
	for range 2 {
		r, cfg := setup()
		out := r.mustRun(cfg)
		requests := r.takeRequests()
		if len(requests) != len(level) {
			t.Fatalf("requests = %v, want each node once", requests)
		}
		for i := 1; i < len(requests); i++ {
			prev, cur := strings.Fields(requests[i-1])[0], strings.Fields(requests[i])[0]
			if level[cur] < level[prev] {
				t.Fatalf("requests = %v: %s resolved after the next level started", requests, cur)
			}
		}
		if got := requestsFor(requests, "@acme/y"); !slices.Equal(got, []string{"^1.0.0"}) {
			t.Errorf("@acme/y requests = %v, want the first dependent by name, @acme/a", got)
		}
		if !strings.Contains(out, "✓ @acme/y@1.0.0 (required by @acme/a)") {
			t.Errorf("output lacks @acme/y with its first dependent:\n%s", out)
		}
		outputs = append(outputs, out)
		locks = append(locks, string(r.lockBytes()))
	}
	if outputs[0] != outputs[1] || locks[0] != locks[1] {
		t.Errorf("two runs differ:\n%s\n---\n%s", outputs[0], outputs[1])
	}
}

// Within a level, a dependent resolved under an explicit channel comes first,
// so it picks the request of a shared dependency. A dependency that a
// dependent outside the channel's subgraph reaches at an earlier level keeps
// its request, and the channel only checks it.
func TestAnExplicitChannelDependentPicksFirst(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-order-is-deterministic", "an-explicit-channel-dependent-picks-first")
	r := newGraphRegistry(t)
	r.publish("@acme/a", "1.0.0", `{"@acme/c": "^1.0.0"}`)
	r.publish("@acme/z", "1.0.0", `{"@acme/c": ""}`)
	r.publish("@acme/c", "1.2.0", "")
	r.publish("@acme/c", "2.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/a": "", "@acme/z": ""})
	r.mustRun(cfg)
	if got := requestsFor(r.takeRequests(), "@acme/c"); !slices.Equal(got, []string{"^1.0.0"}) {
		t.Fatalf("without a channel, @acme/c requests = %v, want @acme/a's constraint", got)
	}
	r.publish("@acme/c", "1.3.0", "")
	r.channel("@acme/z", "canary", "1.0.0")
	r.channel("@acme/c", "canary", "1.3.0")

	out := r.mustRun(cfg, "@acme/z@canary")
	if got := requestsFor(r.takeRequests(), "@acme/c"); !slices.Equal(got, []string{"resolve:canary", "canary"}) {
		t.Errorf("@acme/c requests = %v, want the channel of @acme/z first", got)
	}
	if !strings.Contains(out, "↑ @acme/c: 1.2.0 → 1.3.0 (required by @acme/z)") {
		t.Errorf("output lacks @acme/c moved for @acme/z:\n%s", out)
	}

	earlier := newGraphRegistry(t)
	earlier.publish("@acme/a", "1.0.0", `{"@acme/c": "^1.0.0"}`)
	earlier.publish("@acme/z", "1.0.0", `{"@acme/m": ""}`)
	earlier.publish("@acme/m", "1.0.0", `{"@acme/c": ""}`)
	earlier.publish("@acme/c", "1.2.0", "")
	earlier.publish("@acme/c", "2.0.0", "")
	cfg, _ = earlier.declare(map[string]string{"@acme/a": "", "@acme/z": ""})
	earlier.mustRun(cfg)
	earlier.takeRequests()
	earlier.publish("@acme/c", "1.3.0", "")
	earlier.channel("@acme/z", "canary", "1.0.0")
	earlier.channel("@acme/m", "canary", "1.0.0")
	earlier.channel("@acme/c", "canary", "1.3.0")
	earlier.mustRun(cfg, "@acme/z@canary")
	requests := earlier.takeRequests()
	if got := requestsFor(requests, "@acme/m"); !slices.Equal(got, []string{"resolve:canary", "canary"}) {
		t.Errorf("@acme/m requests = %v, want the channel", got)
	}
	if got := requestsFor(requests, "@acme/c"); !slices.Equal(got, []string{"lock:1.2.0"}) {
		t.Errorf("@acme/c requests = %v, want the request @acme/a reached it with", got)
	}
}

// A constraint found at a later level than its target is only checked: a
// match installs nothing more, and a miss fails naming every dependent.
func TestALaterLevelConstraintIsOnlyChecked(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-order-is-deterministic", "a-later-level-constraint-is-only-checked")
	for _, tc := range []struct {
		later string
		fails bool
	}{{"^1.1.0", false}, {"^2.0.0", true}} {
		r := newGraphRegistry(t)
		r.publish("@acme/app", "1.0.0", `{"@acme/x": "", "@acme/c": "^1.0.0"}`)
		r.publish("@acme/x", "1.0.0", fmt.Sprintf(`{"@acme/c": %q}`, tc.later))
		r.publish("@acme/c", "1.2.0", "")
		r.publish("@acme/c", "2.0.0", "")
		cfg, _ := r.declare(map[string]string{"@acme/app": ""})

		_, _, err := r.run(cfg)
		if got := requestsFor(r.takeRequests(), "@acme/c"); !slices.Equal(got, []string{"^1.0.0"}) {
			t.Errorf("later %s: @acme/c requests = %v, want one resolution at level 1", tc.later, got)
		}
		if !tc.fails {
			if err != nil {
				t.Errorf("later %s: install = %v", tc.later, err)
			}
			continue
		}
		want := "@acme/c@1.2.0 does not satisfy @acme/app, which requires ^1.0.0; @acme/x, which requires ^2.0.0"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("later %s: install = %v, want %q", tc.later, err, want)
		}
	}
}

// A cycle installs each extension once, and the declared root of a cycle is
// not installed again as a dependency.
func TestACycleInstallsEachExtensionOnce(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-order-is-deterministic", "a-cycle-installs-each-extension-once")
	r := newGraphRegistry(t)
	r.publish("@acme/a", "1.0.0", `{"@acme/b": ""}`)
	r.publish("@acme/b", "1.0.0", `{"@acme/c": ""}`)
	r.publish("@acme/c", "1.0.0", `{"@acme/b": "", "@acme/a": ""}`)
	cfg, _ := r.declare(map[string]string{"@acme/a": ""})

	r.mustRun(cfg)
	requests := r.takeRequests()
	for _, name := range []string{"@acme/a", "@acme/b", "@acme/c"} {
		if got := requestsFor(requests, name); len(got) != 1 {
			t.Errorf("%s installed %d times (%v), want once", name, len(got), requests)
		}
	}
	wantPins(t, r.locked(), map[string]string{"@acme/a": "1.0.0", "@acme/b": "1.0.0", "@acme/c": "1.0.0"})
}

// An update re-resolves each dependency, writes its lock entry and reports it
// with its dependent, and never writes it into the workspace file's pins.
func TestUpdateReResolvesDependenciesWithoutPinningThem(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "update-moves-the-graph", "update-re-resolves-dependencies-without-pinning-them")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0"}`)
	r.publish("@acme/dev", "1.0.0", "")
	cfg, workspaceFile := r.declare(map[string]string{"@acme/app": "1.0.0"})
	r.mustRun(cfg)
	r.takeRequests()
	r.publish("@acme/dev", "1.4.0", "")
	r.publish("@acme/dev", "2.0.0", "")

	out, err := sharedtest.CaptureStdout(t, func() error {
		return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := requestsFor(r.takeRequests(), "@acme/dev"); !slices.Equal(got, []string{"^1.0.0"}) {
		t.Errorf("@acme/dev requests = %v, want a fresh resolution", got)
	}
	if !strings.Contains(out, "↑ @acme/dev: 1.0.0 → 1.4.0 (required by @acme/app)") {
		t.Errorf("output lacks the move:\n%s", out)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@acme/dev": "1.4.0"})
	if got, _ := os.ReadFile(filepath.Join(r.ws, wsproto.WorkspaceConfigFilename)); !bytes.Equal(got, workspaceFile) {
		t.Errorf("the update wrote the workspace file: %s", got)
	}

	jsonl, err := sharedtest.CaptureStdout(t, func() error {
		return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{}, "jsonl")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonl, `"name":"@acme/dev","version":"1.4.0","oldVersion":"1.4.0"`) || !strings.Contains(jsonl, `"requiredBy":"@acme/app"`) {
		t.Errorf("jsonl lacks the dependency with its dependent:\n%s", jsonl)
	}
}

// An update that fails on several extensions reports them by name, so its
// error and its failure lines are the same on every run, and it writes no
// lock.
func TestAnUpdateReportsItsFailuresByName(t *testing.T) {
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/a": "^1.0.0", "@acme/b": "^1.0.0"}`)
	r.publish("@acme/a", "1.0.0", "")
	r.publish("@acme/b", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": "", "@acme/a": "1.0.0", "@acme/b": "1.0.0"})
	r.mustRun(cfg)
	r.publish("@acme/app", "2.0.0", `{"@acme/a": "^2.0.0", "@acme/b": "^2.0.0"}`)
	before := r.lockBytes()

	var first string
	for run := range 20 {
		var err error
		stderr := sharedtest.CaptureStderr(t, func() {
			_, err = sharedtest.CaptureStdout(t, func() error {
				return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{}, "")
			})
		})
		want := "@acme/a: @acme/a@1.0.0 does not satisfy @acme/app, which requires ^2.0.0"
		if !errors.Is(err, errGraphUnresolved) || !strings.Contains(err.Error(), want) {
			t.Fatalf("update %d = %v, want the first failure by name, %q", run, err, want)
		}
		var lines []string
		for _, line := range strings.Split(stderr, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "✗") {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "✗ @acme/a:") || !strings.HasPrefix(lines[1], "✗ @acme/b:") {
			t.Fatalf("update %d failure lines = %q, want @acme/a then @acme/b", run, lines)
		}
		report := err.Error() + "\n" + strings.Join(lines, "\n")
		if first == "" {
			first = report
		} else if report != first {
			t.Fatalf("update %d reported\n%s\nwant the same report as the first update\n%s", run, report, first)
		}
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Error("a failed update wrote the lock")
	}
}

// A dry run previews the declared extensions only, says so, and resolves no
// dependency.
func TestDryRunPreviewsDeclaredExtensionsOnly(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "update-moves-the-graph", "dry-run-previews-declared-extensions-only")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": ""}`)
	r.publish("@acme/dev", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": ""})
	r.mustRun(cfg)
	r.takeRequests()
	before := r.lockBytes()

	out, err := sharedtest.CaptureStdout(t, func() error {
		return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{DryRun: true}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.takeRequests(); !slices.Equal(got, []string{"@acme/app resolve:latest"}) {
		t.Errorf("a dry run requested %v, want the declared extension only", got)
	}
	if !strings.Contains(out, "This preview covers the declared extensions only; `putnami extensions update` also resolves their dependencies again: @acme/dev") {
		t.Errorf("output lacks the note:\n%s", out)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Error("a dry run wrote the lock")
	}
}

// The builds of a canary line: a canary pinned on 2026-10-13, the older build
// latest serves, and a newer one.
const (
	olderBuild   = "0.0.0-20260929233842-35e20e821"
	lockedCanary = "0.0.0-20261013204708-711d2a7"
	newerBuild   = "0.0.0-20261014154651-d465c1b"
)

// update runs an update with args and returns its stdout.
func (r *graphRegistry) update(cfg *wsproto.Config, opts ArtifactUpdateOptions, outputFormat string, args ...string) string {
	r.t.Helper()
	out, err := sharedtest.CaptureStdout(r.t, func() error {
		return updateArtifacts(context.Background(), r.ws, cfg, args, r.ops(), opts, outputFormat)
	})
	if err != nil {
		r.t.Fatalf("update %v: %v", args, err)
	}
	return out
}

// linkedVersion returns the version the stable link of name points at.
func (r *graphRegistry) linkedVersion(name string) string {
	r.t.Helper()
	target, err := os.Readlink(layout.StableDir(r.ws, layout.Extensions, name))
	if err != nil {
		r.t.Fatal(err)
	}
	_, version, _ := strings.Cut(filepath.Base(target), "@")
	return version
}

// A channel that serves a build older than the locked one moves nothing: an
// update, with or without the name, an upgrade over an exact workspace pin,
// its dry run and install --latest keep the pin, write nothing, and name the
// channel and the build it serves. A newer build moves the pin with ↑, and
// the same build is up to date.
func TestAnUpdateNeverMovesAPinToAnOlderBuild(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "a-locked-canary-newer-than-the-channel-stays")
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "the-up-arrow-marks-only-a-newer-build")
	r := newGraphRegistry(t)
	for _, version := range []string{olderBuild, lockedCanary, newerBuild} {
		r.publish("@putnami/cloud", version, "")
	}
	r.channel("@putnami/cloud", "latest", olderBuild)
	cfg, _ := r.declare(map[string]string{"@putnami/cloud": ""})
	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)
	r.takeRequests()
	before := r.lockBytes()
	kept := "✓ @putnami/cloud@" + lockedCanary + " (kept: channel latest serves the older build " + olderBuild + ")"

	for _, args := range [][]string{nil, {"@putnami/cloud"}} {
		out := r.update(cfg, ArtifactUpdateOptions{}, "", args...)
		if !strings.Contains(out, kept) || strings.Contains(out, "↑") {
			t.Errorf("update %v output lacks %q, or shows ↑:\n%s", args, kept, out)
		}
		if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:latest", "lock:" + lockedCanary}) {
			t.Errorf("update %v requests = %v, want the channel's answer, then the pin, and never the older build", args, got)
		}
		if !bytes.Equal(r.lockBytes(), before) {
			t.Fatalf("update %v moved the lock:\n%s", args, r.lockBytes())
		}
		if got := r.linkedVersion("@putnami/cloud"); got != lockedCanary {
			t.Errorf("update %v links %s, want the pinned %s", args, got, lockedCanary)
		}
	}

	jsonl := r.update(cfg, ArtifactUpdateOptions{}, "jsonl")
	for _, want := range []string{`"version":"` + lockedCanary + `"`, `"status":"kept"`, `"channel":"latest","channelVersion":"` + olderBuild + `"`} {
		if !strings.Contains(jsonl, want) {
			t.Errorf("jsonl lacks %s:\n%s", want, jsonl)
		}
	}
	if preview := r.update(cfg, ArtifactUpdateOptions{DryRun: true}, ""); !strings.Contains(preview,
		"Would keep @putnami/cloud@"+lockedCanary+": channel latest serves the older build "+olderBuild) {
		t.Errorf("dry run lacks the kept pin:\n%s", preview)
	}
	r.takeRequests()
	if out := r.mustRun(cfg, "--latest"); !strings.Contains(out, kept) {
		t.Errorf("install --latest output lacks %q:\n%s", kept, out)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:latest", "lock:" + lockedCanary}) {
		t.Errorf("install --latest requests = %v, want the channel's answer, then the pin, and never the older build", got)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Fatalf("install --latest moved the lock:\n%s", r.lockBytes())
	}

	pinned, workspaceFile := r.declare(map[string]string{"@putnami/cloud": lockedCanary})
	if out := r.update(pinned, ArtifactUpdateOptions{ConstraintOverride: "latest"}, ""); !strings.Contains(out, kept) {
		t.Errorf("upgrade output lacks %q:\n%s", kept, out)
	}
	if got, _ := os.ReadFile(filepath.Join(r.ws, wsproto.WorkspaceConfigFilename)); !bytes.Equal(got, workspaceFile) || !bytes.Equal(r.lockBytes(), before) {
		t.Fatalf("upgrade moved the workspace pin or the lock:\n%s\n%s", got, r.lockBytes())
	}

	r.channel("@putnami/cloud", "latest", newerBuild)
	if out := r.update(cfg, ArtifactUpdateOptions{}, ""); !strings.Contains(out, "↑ @putnami/cloud: "+lockedCanary+" → "+newerBuild) {
		t.Errorf("a newer build lacks ↑:\n%s", out)
	}
	wantPins(t, r.locked(), map[string]string{"@putnami/cloud": newerBuild})
	r.takeRequests()
	if out := r.update(cfg, ArtifactUpdateOptions{}, ""); !strings.Contains(out, "✓ @putnami/cloud@"+newerBuild+" (up to date)") || strings.Contains(out, "↑") {
		t.Errorf("the same build is not up to date:\n%s", out)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:latest", "latest"}) {
		t.Errorf("the same build requests = %v, want the channel's answer, then its download", got)
	}
	wantPins(t, r.locked(), map[string]string{"@putnami/cloud": newerBuild})

	r.channel("@putnami/cloud", "canary", lockedCanary)
	out := r.mustRun(cfg, "@putnami/cloud@canary")
	if want := "✓ @putnami/cloud@" + newerBuild + " (kept: channel canary serves the older build " + lockedCanary + ")"; !strings.Contains(out, want) {
		t.Errorf("install @putnami/cloud@canary output lacks %q:\n%s", want, out)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:canary", "lock:" + newerBuild}) {
		t.Errorf("install @putnami/cloud@canary requests = %v, want the channel's answer, then the pin, and never the older build", got)
	}
	wantPins(t, r.locked(), map[string]string{"@putnami/cloud": newerBuild})
}

// A dependency the lock pins newer than its channel's build keeps its pin
// through an update, and the line names the channel and its dependent.
func TestAnUpdateKeepsANewerDependencyPin(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "a-dependency-keeps-a-newer-pin")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@putnami/cloud": ""}`)
	r.publish("@putnami/cloud", olderBuild, "")
	r.publish("@putnami/cloud", lockedCanary, "")
	r.channel("@putnami/cloud", "latest", lockedCanary)
	cfg, _ := r.declare(map[string]string{"@acme/app": ""})
	r.mustRun(cfg)
	r.channel("@putnami/cloud", "latest", olderBuild)
	before := r.lockBytes()

	out := r.update(cfg, ArtifactUpdateOptions{}, "")
	want := "✓ @putnami/cloud@" + lockedCanary + " (kept: channel latest serves the older build " + olderBuild + ", required by @acme/app)"
	if !strings.Contains(out, want) || strings.Contains(out, "↑") {
		t.Errorf("output lacks %q, or shows ↑:\n%s", want, out)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Fatalf("the update moved the lock:\n%s", r.lockBytes())
	}
}

// A channel build whose version does not compare with the pin keeps the pin,
// and the line says so.
func TestAVersionThatDoesNotCompareKeepsThePin(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "a-version-that-does-not-compare-keeps-the-pin")
	r := newGraphRegistry(t)
	r.publish("@acme/tool", "1.0.0", "")
	r.publish("@acme/tool", "nightly", "")
	r.channel("@acme/tool", "latest", "1.0.0")
	cfg, _ := r.declare(map[string]string{"@acme/tool": ""})
	r.mustRun(cfg)
	r.channel("@acme/tool", "latest", "nightly")

	out := r.update(cfg, ArtifactUpdateOptions{}, "")
	if want := "✓ @acme/tool@1.0.0 (kept: channel latest serves nightly, which does not compare with the pin)"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/tool": "1.0.0"})
}

// A channel answer that names no build installs, then compares the build's
// own version: an older one installs the pin again from the lock. Its dry run
// does not claim to keep the pin, because only the download tells.
func TestAnUnnamedAnswerComparesAfterTheDownload(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "an-unnamed-answer-compares-after-the-download")
	r := newGraphRegistry(t)
	r.publish("@putnami/cloud", olderBuild, "")
	r.publish("@putnami/cloud", lockedCanary, "")
	cfg, _ := r.declare(map[string]string{"@putnami/cloud": ""})
	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)
	r.channel("@putnami/cloud", "latest", "?"+olderBuild)
	r.takeRequests()
	before := r.lockBytes()

	preview := r.update(cfg, ArtifactUpdateOptions{DryRun: true}, "")
	if want := "Would resolve @putnami/cloud@latest: the channel names no build, so the update downloads it and keeps " + lockedCanary + " unless the build is newer"; !strings.Contains(preview, want) || strings.Contains(preview, "Would keep") {
		t.Errorf("dry run lacks %q, or claims to keep the pin:\n%s", want, preview)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:latest"}) {
		t.Errorf("dry run requests = %v, want the resolve-only lookup", got)
	}

	out := r.update(cfg, ArtifactUpdateOptions{}, "")
	if want := "✓ @putnami/cloud@" + lockedCanary + " (kept: channel latest serves the older build " + olderBuild + ")"; !strings.Contains(out, want) {
		t.Errorf("update output lacks %q:\n%s", want, out)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:latest", "latest", "lock:" + lockedCanary}) {
		t.Errorf("update requests = %v, want the lookup, the download, then the pin again", got)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Fatalf("the update moved the lock:\n%s", r.lockBytes())
	}
	if got := r.linkedVersion("@putnami/cloud"); got != lockedCanary {
		t.Errorf("the update links %s, want the pinned %s", got, lockedCanary)
	}
}

// When a channel answer that names no build is older than the pin and the pin
// cannot be installed again, the update fails that root: the stable link
// points at the pinned build again, the lock stays as it was, and no
// dependency only the older build declares is pinned.
func TestAFailedPinReinstallRestoresTheStableLink(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "a-failed-pin-reinstall-restores-the-stable-link")
	r := newGraphRegistry(t)
	r.publish("@putnami/cloud", olderBuild, `{"@acme/extra": ""}`)
	r.publish("@putnami/cloud", lockedCanary, "")
	r.publish("@acme/extra", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@putnami/cloud": ""})
	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)
	link := layout.StableDir(r.ws, layout.Extensions, "@putnami/cloud")
	pinnedTarget, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	r.channel("@putnami/cloud", "latest", "?"+olderBuild)
	delete(r.releases["@putnami/cloud"], lockedCanary)
	r.takeRequests()
	before := r.lockBytes()

	var runErr error
	stderr := sharedtest.CaptureStderr(t, func() {
		_, runErr = sharedtest.CaptureStdout(t, func() error {
			return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{}, "")
		})
	})
	// A failed root reports ✗ and does not fail the update.
	if runErr != nil {
		t.Fatalf("update = %v, want the failed root reported, not returned", runErr)
	}
	if want := "✗ @putnami/cloud: channel latest serves the older build " + olderBuild + ", and installing the pinned " + lockedCanary + " failed"; !strings.Contains(stderr, want) {
		t.Errorf("stderr lacks %q:\n%s", want, stderr)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:latest", "latest", "lock:" + lockedCanary}) {
		t.Errorf("update requests = %v, want the lookup, the download, then the pin again", got)
	}
	if got, err := os.Readlink(link); err != nil || got != pinnedTarget {
		t.Errorf("stable link = %q (%v), want the pinned build %q again", got, err, pinnedTarget)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Fatalf("the failed update moved the lock:\n%s", r.lockBytes())
	}
}

// A lookup that fails while the command runs does not fail the update: the
// update downloads the channel's build and compares its version, so an older
// build keeps the pin and a newer one moves it.
func TestAFailedLookupComparesAfterTheDownload(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "a-failed-lookup-compares-after-the-download")
	r := newGraphRegistry(t)
	for _, version := range []string{olderBuild, lockedCanary, newerBuild} {
		r.publish("@putnami/cloud", version, "")
	}
	cfg, _ := r.declare(map[string]string{"@putnami/cloud": ""})
	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)
	ops := r.ops()
	ops.resolve = func(context.Context, string, string) (*artifactInstallOutcome, error) {
		return nil, errors.New("HTTP 501 to HEAD")
	}
	update := func() string {
		t.Helper()
		out, err := sharedtest.CaptureStdout(t, func() error {
			return updateArtifacts(context.Background(), r.ws, cfg, nil, ops, ArtifactUpdateOptions{}, "")
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	r.channel("@putnami/cloud", "latest", olderBuild)
	r.takeRequests()
	if want := "✓ @putnami/cloud@" + lockedCanary + " (kept: channel latest serves the older build " + olderBuild + ")"; !strings.Contains(update(), want) {
		t.Errorf("update lacks %q", want)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"latest", "lock:" + lockedCanary}) {
		t.Errorf("update requests = %v, want the download, then the pin again", got)
	}
	wantPins(t, r.locked(), map[string]string{"@putnami/cloud": lockedCanary})

	r.channel("@putnami/cloud", "latest", newerBuild)
	if want := "↑ @putnami/cloud: " + lockedCanary + " → " + newerBuild; !strings.Contains(update(), want) {
		t.Errorf("update lacks %q", want)
	}
	wantPins(t, r.locked(), map[string]string{"@putnami/cloud": newerBuild})
}

// A channel that moves to an older build between the lookup and the download
// keeps the pin: the downloaded build's own version is compared again.
func TestAChannelThatMovesAfterTheLookupKeepsThePin(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "a-channel-that-moves-after-the-lookup-keeps-the-pin")
	r := newGraphRegistry(t)
	for _, version := range []string{olderBuild, lockedCanary, newerBuild} {
		r.publish("@putnami/cloud", version, "")
	}
	cfg, _ := r.declare(map[string]string{"@putnami/cloud": ""})
	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)
	r.channel("@putnami/cloud", "latest", newerBuild)
	r.delay = func(string) { r.channel("@putnami/cloud", "latest", olderBuild) }
	r.takeRequests()
	before := r.lockBytes()

	out := r.update(cfg, ArtifactUpdateOptions{}, "")
	if want := "✓ @putnami/cloud@" + lockedCanary + " (kept: channel latest serves the older build " + olderBuild + ")"; !strings.Contains(out, want) || strings.Contains(out, "↑") {
		t.Errorf("output lacks %q, or shows ↑:\n%s", want, out)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:latest", "latest", "lock:" + lockedCanary}) {
		t.Errorf("requests = %v, want the lookup, the download, then the pin again", got)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Fatalf("the update moved the lock:\n%s", r.lockBytes())
	}
	if got := r.linkedVersion("@putnami/cloud"); got != lockedCanary {
		t.Errorf("the update links %s, want the pinned %s", got, lockedCanary)
	}
}

// A channel answer equal to the pin downloads the build, so an update
// refreshes a lock entry whose digest or manifest hash is wrong at the pinned
// version.
func TestAnEqualAnswerRefreshesTheLockEntry(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "an-equal-answer-refreshes-the-lock-entry")
	r := newGraphRegistry(t)
	r.publish("@acme/tool", "1.2.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/tool": ""})
	r.mustRun(cfg)
	lf := mustLock(t, r.ws)
	stale, _ := lf.GetExtension("@acme/tool")
	stale.Integrity, stale.ManifestHash = "sha256-wrong", "manifest-wrong"
	lf.SetExtension("@acme/tool", stale)
	if err := lockfile.WriteLockFile(r.ws, lf); err != nil {
		t.Fatal(err)
	}
	r.takeRequests()

	r.update(cfg, ArtifactUpdateOptions{}, "")
	if got := requestsFor(r.takeRequests(), "@acme/tool"); !slices.Equal(got, []string{"resolve:latest", "latest"}) {
		t.Errorf("requests = %v, want the lookup, then the download", got)
	}
	entry, _ := mustLock(t, r.ws).GetExtension("@acme/tool")
	if entry.Version != "1.2.0" || entry.Integrity != "sha256-@acme/tool@1.2.0" || entry.ManifestHash != "manifest-@acme/tool@1.2.0" {
		t.Errorf("lock entry = %+v, want 1.2.0 with the registry's digest and manifest hash", entry)
	}
}

// A held pin whose lock entry fails to install ends its error with the
// exact-version install, which refreshes the entry and keeps the pin, never
// with the update that holds the pin again.
func TestAHeldPinWithAWrongEntryNamesTheExactInstall(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "a-held-pin-with-a-wrong-entry-names-the-exact-install")
	r := newGraphRegistry(t)
	r.publish("@putnami/cloud", olderBuild, "")
	r.publish("@putnami/cloud", lockedCanary, "")
	cfg, _ := r.declare(map[string]string{"@putnami/cloud": ""})
	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)
	lf := mustLock(t, r.ws)
	stale, _ := lf.GetExtension("@putnami/cloud")
	stale.Integrity = "sha256-wrong"
	lf.SetExtension("@putnami/cloud", stale)
	if err := lockfile.WriteLockFile(r.ws, lf); err != nil {
		t.Fatal(err)
	}
	r.channel("@putnami/cloud", "latest", olderBuild)
	r.takeRequests()

	var runErr error
	stderr := sharedtest.CaptureStderr(t, func() {
		_, runErr = sharedtest.CaptureStdout(t, func() error {
			return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{}, "")
		})
	})
	if runErr != nil {
		t.Fatalf("update = %v, want the failed root reported, not returned", runErr)
	}
	var line string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.Contains(l, "✗ @putnami/cloud") {
			line = strings.TrimSpace(l)
		}
	}
	remedy := "; `update` keeps this pin, so run `putnami extensions install @putnami/cloud@" + lockedCanary + "` to refresh its lock entry"
	if !strings.Contains(line, "✗ @putnami/cloud: channel latest serves the older build "+olderBuild+", and installing the pinned "+lockedCanary+" failed: integrity mismatch") ||
		!strings.HasSuffix(line, remedy) {
		t.Errorf("stderr = %q, want the integrity mismatch ending with %q", line, remedy)
	}
	if got := requestsFor(r.takeRequests(), "@putnami/cloud"); !slices.Equal(got, []string{"resolve:latest", "lock:" + lockedCanary}) {
		t.Errorf("update requests = %v, want the lookup, then the pin", got)
	}

	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)
	if entry, _ := mustLock(t, r.ws).GetExtension("@putnami/cloud"); entry.Version != lockedCanary || entry.Integrity != "sha256-@putnami/cloud@"+lockedCanary {
		t.Errorf("after the named install the lock entry = %+v, want the pin with the registry's digest", entry)
	}
}

// The exact-version install a held dependency's failure names works for a
// dependency the workspace does not declare: it refreshes the dependency's
// lock entry at the pin and writes nothing to putnami.workspace.json.
func TestTheExactInstallHintRefreshesADependency(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "the-exact-install-hint-refreshes-a-dependency")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@putnami/cloud": ""}`)
	r.publish("@putnami/cloud", olderBuild, "")
	r.publish("@putnami/cloud", lockedCanary, "")
	r.channel("@putnami/cloud", "latest", lockedCanary)
	cfg, workspaceFile := r.declare(map[string]string{"@acme/app": ""})
	r.mustRun(cfg)
	lf := mustLock(t, r.ws)
	stale, _ := lf.GetExtension("@putnami/cloud")
	stale.Integrity = "sha256-wrong"
	lf.SetExtension("@putnami/cloud", stale)
	if err := lockfile.WriteLockFile(r.ws, lf); err != nil {
		t.Fatal(err)
	}
	r.channel("@putnami/cloud", "latest", olderBuild)

	var runErr error
	stderr := sharedtest.CaptureStderr(t, func() {
		_, runErr = sharedtest.CaptureStdout(t, func() error {
			return updateArtifacts(context.Background(), r.ws, cfg, nil, r.ops(), ArtifactUpdateOptions{}, "")
		})
	})
	var line string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.Contains(l, "✗ @putnami/cloud") {
			line = strings.TrimSpace(l)
		}
	}
	remedy := "; `update` keeps this pin, so run `putnami extensions install @putnami/cloud@" + lockedCanary + "` to refresh its lock entry"
	if runErr == nil || !strings.HasSuffix(line, remedy) {
		t.Fatalf("update = %v with stderr %q, want the dependency's failure ending with %q", runErr, line, remedy)
	}

	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)
	if entry, _ := mustLock(t, r.ws).GetExtension("@putnami/cloud"); entry.Version != lockedCanary || entry.Integrity != "sha256-@putnami/cloud@"+lockedCanary {
		t.Errorf("after the named install the lock entry = %+v, want the pin with the registry's digest", entry)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/app": "1.0.0", "@putnami/cloud": lockedCanary})
	if got, _ := os.ReadFile(filepath.Join(r.ws, wsproto.WorkspaceConfigFilename)); !bytes.Equal(got, workspaceFile) {
		t.Errorf("the named install wrote %s:\n%s", wsproto.WorkspaceConfigFilename, got)
	}
}

// An exact version, such as upgrade --version, moves a pin to an older build,
// and the line marks the move ↓.
func TestAnExactVersionMovesAPinBackwards(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "a-channel-never-moves-a-pin-backwards", "an-exact-version-moves-a-pin-backwards")
	r := newGraphRegistry(t)
	r.publish("@putnami/cloud", olderBuild, "")
	r.publish("@putnami/cloud", lockedCanary, "")
	cfg, _ := r.declare(map[string]string{"@putnami/cloud": ""})
	r.mustRun(cfg, "@putnami/cloud@"+lockedCanary)

	out := r.update(cfg, ArtifactUpdateOptions{ConstraintOverride: olderBuild}, "")
	if want := "↓ @putnami/cloud: " + lockedCanary + " → " + olderBuild; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	wantPins(t, r.locked(), map[string]string{"@putnami/cloud": olderBuild})
}

func runRemove(t *testing.T, r *graphRegistry, cfg *wsproto.Config, name string) (string, error) {
	t.Helper()
	return sharedtest.CaptureStdout(t, func() error { return removeExtension(r.ws, cfg, []string{name}, r.ops()) })
}

// Remove refuses a name the remaining declarations still reach, naming the
// members on that path. Otherwise it removes the lock entry of the name and of
// each dependency the remaining declarations no longer reach.
func TestRemoveRefusesARequiredDependencyAndPrunesUnreachableOnes(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "remove-keeps-the-graph-whole", "remove-refuses-a-required-dependency-and-prunes-unreachable-ones")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "", "@acme/only": "", "@acme/shared": ""}`)
	r.publish("@acme/tool", "1.0.0", `{"@acme/shared": ""}`)
	r.publish("@acme/dev", "1.0.0", `{"@acme/base": ""}`)
	r.publish("@acme/only", "1.0.0", `{"@acme/deeper": ""}`)
	for _, name := range []string{"@acme/shared", "@acme/base", "@acme/deeper"} {
		r.publish(name, "1.0.0", "")
	}
	cfg, _ := r.declare(map[string]string{"@acme/app": "", "@acme/tool": "", "@acme/dev": ""})
	r.mustRun(cfg)
	all := r.locked()
	if len(all) != 7 {
		t.Fatalf("lock pins %v, want the whole graph", all)
	}

	for name, path := range map[string]string{"@acme/dev": "@acme/app → @acme/dev", "@acme/shared": "@acme/app → @acme/shared"} {
		before := r.lockBytes()
		_, err := runRemove(t, r, cfg, name)
		if err == nil || !strings.Contains(err.Error(), "extension "+name+" is still required: "+path) {
			t.Errorf("remove %s = %v, want the path %s", name, err, path)
		}
		if !bytes.Equal(r.lockBytes(), before) {
			t.Errorf("a refused remove of %s wrote the lock", name)
		}
	}

	out, err := runRemove(t, r, cfg, "@acme/app")
	if err != nil {
		t.Fatal(err)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/base": "1.0.0", "@acme/dev": "1.0.0", "@acme/shared": "1.0.0", "@acme/tool": "1.0.0"})
	for _, name := range []string{"@acme/only", "@acme/deeper"} {
		if !strings.Contains(out, "✓ Removed "+name+", which no declared extension requires any more") {
			t.Errorf("output lacks the pruned %s:\n%s", name, out)
		}
		if _, err := os.Lstat(layout.StableDir(r.ws, layout.Extensions, name)); !os.IsNotExist(err) {
			t.Errorf("the stable link of the pruned %s remains", name)
		}
	}
	if !strings.Contains(out, "Note: update putnami.workspace.json extensions field manually") {
		t.Errorf("output lacks the workspace file note:\n%s", out)
	}
}

// The remaining roots of a remove are the declarations without the name, so
// the declared root of a cycle can be removed, and the cycle with it.
func TestTheRootOfACycleCanBeRemoved(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "remove-keeps-the-graph-whole", "the-root-of-a-cycle-can-be-removed")
	r := newGraphRegistry(t)
	r.publish("@acme/a", "1.0.0", `{"@acme/b": ""}`)
	r.publish("@acme/b", "1.0.0", `{"@acme/a": ""}`)
	r.publish("@acme/other", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/a": "", "@acme/other": ""})
	r.mustRun(cfg)

	if _, err := runRemove(t, r, cfg, "@acme/a"); err != nil {
		t.Fatalf("remove the root of a cycle: %v", err)
	}
	wantPins(t, r.locked(), map[string]string{"@acme/other": "1.0.0"})
}

// putnami extensions list shows each dependency with the extensions that
// require it, and the reserved provider commands it does not serve.
func TestListShowsTheRequiringExtension(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "update-moves-the-graph", "list-shows-the-requiring-extension")
	spectest.Proves(t, dependenciesFeature, "a-dependency-serves-no-reserved-provider", "a-dependency-loses-reserved-provider-commands")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": ""}`)
	r.publish("@acme/tool", "1.0.0", `{"@acme/dev": ""}`)
	r.publishRelease("@acme/dev", "1.2.0", graphRelease{commands: []string{"cache-provider"}})
	cfg, _ := r.declare(map[string]string{"@acme/app": "", "@acme/tool": ""})
	r.mustRun(cfg)

	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsList(r.ws, cfg, "") })
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "@acme/dev") && !strings.Contains(line, "!") {
			row = line
		}
	}
	if !strings.Contains(row, "1.2.0") || !strings.HasSuffix(strings.TrimSpace(row), "required by @acme/app, @acme/tool") {
		t.Errorf("the @acme/dev row = %q, want its version and its dependents:\n%s", row, out)
	}
	if !strings.Contains(out, "! extension @acme/dev, a dependency of @acme/app, does not serve the cache-provider capability") {
		t.Errorf("output lacks the removed provider:\n%s", out)
	}

	jsonl, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsList(r.ws, cfg, "jsonl") })
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"@acme/dev","installed":"1.2.0","source":"dependency","requiredBy":["@acme/app","@acme/tool"],"removedCapabilities":["cache-provider"]}`
	if !strings.Contains(jsonl, want) {
		t.Errorf("jsonl lacks %s:\n%s", want, jsonl)
	}
}

// A hosted run installs the graph from the committed lock and writes no
// lock. A dependency the lock does not pin fails the install and is never
// requested.
func TestAHostedInstallRefusesAnUnpinnedDependency(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "hosted-installs-the-locked-graph", "a-hosted-install-refuses-an-unpinned-dependency")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": ""}`)
	r.publish("@acme/dev", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": ""})
	r.mustRun(cfg)
	r.takeRequests()
	pinned := r.lockBytes()

	restore := runcredential.SetForTest("run-bearer")
	t.Cleanup(restore)
	r.mustRun(cfg)
	if got := r.takeRequests(); !slices.Equal(got, []string{"@acme/app lock:1.0.0", "@acme/dev lock:1.0.0"}) {
		t.Errorf("a hosted install requested %v, want the locked graph", got)
	}
	if !bytes.Equal(r.lockBytes(), pinned) {
		t.Error("a hosted install wrote the lock")
	}

	lf := lockfile.NewLockFile()
	entry, _ := mustLock(t, r.ws).GetExtension("@acme/app")
	lf.SetExtension("@acme/app", entry)
	if err := lockfile.WriteLockFile(r.ws, lf); err != nil {
		t.Fatal(err)
	}
	before := r.lockBytes()
	_, _, err := r.run(cfg)
	want := "@acme/app requires @acme/dev, which the committed putnami.lock.json does not pin; a hosted run writes no lock"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), runcredential.Flag) {
		t.Fatalf("hosted install = %v, want %q", err, want)
	}
	if got := requestsFor(r.takeRequests(), "@acme/dev"); len(got) != 0 {
		t.Errorf("a hosted install requested the unpinned @acme/dev: %v", got)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Error("a hosted install wrote the lock")
	}
}

func mustLock(t *testing.T, ws string) *lockfile.LockFile {
	t.Helper()
	lf, err := lockfile.ReadLockFile(ws)
	if err != nil || lf == nil {
		t.Fatalf("read lock: %v", err)
	}
	return lf
}

// An implicit install writes no lock, so it never resolves a dependency the
// lock does not pin: it reports the dependency with its dependent and
// `putnami install`.
func TestAnImplicitInstallNeverResolvesAnUnpinnedDependency(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "restore-follows-the-lock", "an-unpinned-dependency-is-never-resolved-implicitly")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": ""}`)
	r.publish("@acme/dev", "1.0.0", "")
	cfg, _ := r.declare(map[string]string{"@acme/app": ""})
	lf := lockfile.NewLockFile()
	lf.SetExtension("@acme/app", lockfile.LockEntry{Version: "1.0.0"})
	if err := lockfile.WriteLockFile(r.ws, lf); err != nil {
		t.Fatal(err)
	}
	before := r.lockBytes()

	_, _, err := r.runContext(shared.WithImplicitInstall(context.Background()), cfg)
	if err == nil || !strings.Contains(err.Error(), "@acme/app requires @acme/dev, which putnami.lock.json does not pin; run `putnami install`") {
		t.Fatalf("implicit install = %v, want the unpinned dependency named with its dependent", err)
	}
	if got := requestsFor(r.takeRequests(), "@acme/dev"); len(got) != 0 {
		t.Errorf("an implicit install requested @acme/dev: %v", got)
	}
	if !bytes.Equal(r.lockBytes(), before) {
		t.Error("an implicit install wrote the lock")
	}
}

// Installing an extension runs the onInstall hooks of the dependencies it
// reaches, transitively, and no hook of an extension it does not reach.
func TestInstallingADependentRunsItsDependenciesInstallHooks(t *testing.T) {
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": ""}`)
	r.publishWithInstallHook("@acme/dev", "1.0.0", `{"@acme/base": ""}`, "dev-ran")
	r.publishWithInstallHook("@acme/base", "1.0.0", "", "base-ran")
	r.publishWithInstallHook("@acme/other", "1.0.0", "", "other-ran")
	cfg, _ := r.declare(map[string]string{"@acme/other": ""})
	r.mustRun(cfg)
	r.mustRun(cfg, "@acme/app")

	var out bytes.Buffer
	if err := runExtensionInstallHooks(context.Background(), r.ws, cfg, []string{"@acme/app"}, "", &out); err != nil {
		t.Fatalf("install hooks: %v\n%s", err, out.String())
	}
	for marker, want := range map[string]int{"dev-ran": 1, "base-ran": 1, "other-ran": 0} {
		if got := len(fixtureproc.Runs(t, filepath.Join(r.ws, marker))); got != want {
			t.Errorf("%s: the hook ran %d times, want %d\n%s", marker, got, want, out.String())
		}
	}
	if !strings.Contains(out.String(), "✓ @acme/dev onInstall") || !strings.Contains(out.String(), "✓ @acme/base onInstall") {
		t.Errorf("output lacks the dependencies' hooks:\n%s", out.String())
	}
}

// putnami extensions install --user refuses an extension whose manifest
// names a registry dependency, names the limit, and keeps no pin of it.
func TestAUserInstallRefusesADependent(t *testing.T) {
	spectest.Proves(t, dependenciesFeature, "the-user-scope-takes-no-dependency", "a-user-install-refuses-a-dependent")
	r := newGraphRegistry(t)
	r.publish("@acme/app", "1.0.0", `{"@acme/dev": "^1.0.0"}`)
	r.publish("@acme/solo", "1.0.0", "")
	install := refuseUserScopeDependents(r.install, extension.NewInstaller(r.ws))

	_, err := install(context.Background(), "@acme/app", "latest", nil)
	want := "@acme/app@1.0.0 names extension dependencies (@acme/dev); the user scope takes no dependency"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "Declare @acme/app in a workspace's putnami.workspace.json") {
		t.Fatalf("user install = %v, want %q", err, want)
	}
	if _, err := os.Lstat(layout.StableDir(r.ws, layout.Extensions, "@acme/app")); !os.IsNotExist(err) {
		t.Error("the refused extension stays linked")
	}
	if got := requestsFor(r.takeRequests(), "@acme/dev"); len(got) != 0 {
		t.Errorf("a user install requested the dependency: %v", got)
	}
	if outcome, err := install(context.Background(), "@acme/solo", "latest", nil); err != nil || outcome.Version != "1.0.0" {
		t.Errorf("a user install of an extension without dependencies = %+v, %v", outcome, err)
	}
}
