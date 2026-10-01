package initchannel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const (
	// fixtureRuntimeEnv makes the test binary run the workspace-install job of
	// a fixture language extension; its value is the directory the job records
	// its runs in.
	fixtureRuntimeEnv = "PUTNAMI_INITCHANNEL_FIXTURE"

	// candidateChannel is the immutable channel a tagged publish creates for a
	// release candidate.
	candidateChannel = "tooling-v0.4.0"

	goExtensionPath = "/putnami/go/download"
	templatePath    = "/putnami/go-server/download"
	contentPath     = "/putnami/contributor/download"

	typeScriptExtensionPath = "/putnami/typescript/download"
	typeScriptTemplatePath  = "/putnami/typescript-web/download"

	// starterNPMRegistry is the registry the TypeScript starter declares for the
	// @putnami scope.
	starterNPMRegistry = "https://npm.putnami.dev"
)

// releaseSet is one release of every artifact a starter resolves: its language
// extension, its template, the agent-content extension and, for the Go
// starter, go.putnami.dev/app. The lines do not share a version, as the
// published lines do not.
type releaseSet struct {
	extension string
	template  string
	content   string
	// framework is the version of go.putnami.dev/app.
	framework string
}

var (
	// olderSet is the release set `latest` names in these scenarios.
	olderSet = releaseSet{extension: "0.3.0", template: "0.3.1", content: "0.1.0", framework: "v0.3.0"}
	// candidateSet is the release set candidateChannel names.
	candidateSet = releaseSet{extension: "0.4.0", template: "0.4.1", content: "0.2.0", framework: "v0.4.0"}
	// newerSet is a release set `latest` moves to after the candidate.
	newerSet = releaseSet{extension: "0.5.0", template: "0.5.1", content: "0.3.0", framework: "v0.5.0"}
)

// A smoke against a candidate channel, while `latest` names an older release
// set: init resolves the extension, the agent-content extension, the template
// and the Go framework version of the candidate set only. The channel reaches
// init the way the release smokes hand it: PUTNAMI_CHANNEL, with the public
// command line. The lock records exact versions and the workspace names no
// channel, so a later install reads the lock and `putnami upgrade` follows
// `latest`.
func TestInit_OnACandidateChannelUsesOnlyTheCandidateReleaseSet(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel",
		"a-candidate-channel-resolves-only-its-release-set-while-latest-is-older")
	spectest.Proves(t, "cli/init-channel", "the-workspace-pins-no-channel",
		"the-config-keeps-bare-names-and-the-lock-records-exact-versions")
	h := newHarness(t, map[string]releaseSet{"latest": olderSet, candidateChannel: candidateSet})
	t.Setenv("PUTNAMI_CHANNEL", candidateChannel)

	code, output := clitest.RunGateArgs(t, h.root, "init", "--project", "webapp", "--extension", "go")
	if code != 0 {
		t.Fatalf("putnami init on %s exited %d:\n%s", candidateChannel, code, output)
	}
	if want := "Channel: " + candidateChannel + " (PUTNAMI_CHANNEL)"; !strings.Contains(output, want) {
		t.Errorf("init does not state the channel it resolves on (%q):\n%s", want, output)
	}
	h.assertResolved(t, candidateSet, output)
	h.assertAskedOnly(t, candidateChannel, candidateSet)
	h.assertInstallerChannels(t, candidateChannel)
	h.assertPinsNoChannel(t, candidateSet)
}

// The same smoke passes while `latest` is empty: no registry and no module
// proxy answers `latest`, and init never asks them for it. The channel is
// chosen by flag here.
func TestInit_OnACandidateChannelPassesWhileLatestIsEmpty(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel",
		"a-candidate-channel-resolves-while-latest-is-empty")
	h := newHarness(t, map[string]releaseSet{candidateChannel: candidateSet})
	t.Setenv("PUTNAMI_CHANNEL", "")

	code, output := clitest.RunGateArgs(t, h.root, "init", "--project", "webapp", "--extension", "go", "--channel", candidateChannel)
	if code != 0 {
		t.Fatalf("putnami init --channel %s with an empty latest exited %d:\n%s", candidateChannel, code, output)
	}
	if want := "Channel: " + candidateChannel + " (--channel)"; !strings.Contains(output, want) {
		t.Errorf("init does not state the channel it resolves on (%q):\n%s", want, output)
	}
	h.assertResolved(t, candidateSet, output)
	h.assertAskedOnly(t, candidateChannel, candidateSet)
	h.assertInstallerChannels(t, candidateChannel)
	h.assertPinsNoChannel(t, candidateSet)
}

// The control of the scenario above: without a channel, an empty `latest`
// stops init at its first resolution.
func TestInit_WithoutAChannelStopsWhileLatestIsEmpty(t *testing.T) {
	h := newHarness(t, map[string]releaseSet{candidateChannel: candidateSet})
	t.Setenv("PUTNAMI_CHANNEL", "")

	code, output := clitest.RunGateArgs(t, h.root, "init", "--project", "webapp", "--extension", "go")
	if code == 0 {
		t.Fatalf("putnami init with an empty latest and no channel succeeded:\n%s", output)
	}
	if got := h.registry.asked(); !slices.Equal(got, []string{goExtensionPath + " latest"}) {
		t.Fatalf("the registry was asked %v, want the Go extension on latest only", got)
	}
}

// With no channel choice, init resolves every artifact and the Go framework
// version on `latest`, and hands the installers no channel option. A CLI whose
// name records no channel, which is what this test binary is, has no install
// record.
func TestInit_WithoutAChannelChoiceResolvesOnLatest(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "no-choice-is-unchanged", "no-choice-asks-every-registry-for-latest")
	h := newHarness(t, map[string]releaseSet{"latest": olderSet, candidateChannel: candidateSet})
	t.Setenv("PUTNAMI_CHANNEL", "")

	code, output := clitest.RunGateArgs(t, h.root, "init", "--project", "webapp", "--extension", "go")
	if code != 0 {
		t.Fatalf("putnami init exited %d:\n%s", code, output)
	}
	if strings.Contains(output, "Channel:") {
		t.Errorf("init without a channel choice announces a channel:\n%s", output)
	}
	h.assertResolved(t, olderSet, output)
	h.assertAskedOnly(t, "latest", olderSet)
	if got, want := h.registry.channelRequests(), []string{
		goExtensionPath + " latest", contentPath + " latest", templatePath + " latest",
	}; !slices.Equal(got, want) {
		t.Errorf("channel requests = %v, want %v", got, want)
	}
	h.assertInstallerChannels(t, "none")
	h.assertPinsNoChannel(t, olderSet)
}

// `putnami upgrade` in the workspace an init on a candidate channel created
// follows `latest`: once `latest` moves to a newer release set, upgrade moves
// the lock to it and never asks for the candidate channel.
func TestInit_OnACandidateChannelLeavesUpgradeOnLatest(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "the-workspace-pins-no-channel", "upgrade-in-the-new-workspace-asks-for-latest")
	h := newHarness(t, map[string]releaseSet{"latest": olderSet, candidateChannel: candidateSet})
	t.Setenv("PUTNAMI_CHANNEL", candidateChannel)

	code, output := clitest.RunGateArgs(t, h.root, "init", "--project", "webapp", "--extension", "go")
	if code != 0 {
		t.Fatalf("putnami init on %s exited %d:\n%s", candidateChannel, code, output)
	}
	h.assertResolved(t, candidateSet, output)

	// The smoke's environment is gone, and `latest` has moved on.
	t.Setenv("PUTNAMI_CHANNEL", "")
	h.registry.publish("latest", h.releases(t, newerSet))
	before := len(h.registry.asked())

	code, output = clitest.RunGateArgs(t, h.root, "upgrade", "--extensions")
	if code != 0 {
		t.Fatalf("putnami upgrade --extensions exited %d:\n%s", code, output)
	}
	asked := h.registry.asked()[before:]
	for _, want := range []string{goExtensionPath + " latest", contentPath + " latest", templatePath + " latest"} {
		if !slices.Contains(asked, want) {
			t.Errorf("upgrade asked %v, want it to ask %q", asked, want)
		}
	}
	for _, request := range asked {
		if strings.HasSuffix(request, " "+candidateChannel) {
			t.Errorf("upgrade asked %q: it follows the channel init resolved on", request)
		}
	}
	lock := h.lock(t)
	if pin, _ := lock.GetExtension("@putnami/go"); pin.Version != newerSet.extension {
		t.Errorf("extensions.@putnami/go = %s after upgrade, want %s, the release latest names", pin.Version, newerSet.extension)
	}
	if pin, _ := lock.GetTemplate("go-server"); pin.Version != newerSet.template {
		t.Errorf("templates.go-server = %s after upgrade, want %s, the release latest names", pin.Version, newerSet.template)
	}
}

// The TypeScript starter on a candidate channel, the starter the release
// smokes initialize, while `latest` is empty: the real CLI hands the
// workspace-install job of @putnami/typescript one job context that carries
// the channel, as the putnami-channel option, and the npm registries the
// starter declares. The extension seeds the workspace catalog from those two
// members.
func TestInit_OnACandidateChannelHandsTheTypeScriptInstallerTheChannelAndItsRegistries(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel",
		"the-typescript-installer-receives-the-channel-and-its-registries")
	h := newHarness(t, nil)
	h.registry.publish(candidateChannel, map[string]release{
		typeScriptExtensionPath: {
			version: candidateSet.extension,
			archive: extensionArchive(t, h.records, "@putnami/typescript", candidateSet.extension),
		},
		typeScriptTemplatePath: {version: candidateSet.template, archive: typeScriptTemplateArchive(t, candidateSet.template)},
		contentPath:            contentRelease(t, candidateSet.content),
	})
	t.Setenv("PUTNAMI_CHANNEL", candidateChannel)

	code, output := clitest.RunGateArgs(t, h.root, "init", "--project", "webapp", "--extension", "ts")
	if code != 0 {
		t.Fatalf("putnami init --extension ts on %s exited %d:\n%s", candidateChannel, code, output)
	}
	lock := h.lock(t)
	if pin, ok := lock.GetExtension("@putnami/typescript"); !ok || pin.Version != candidateSet.extension {
		t.Errorf("extensions.@putnami/typescript = %+v (present %v), want %s\n%s", pin, ok, candidateSet.extension, output)
	}
	if pin, ok := lock.GetTemplate("typescript-web"); !ok || pin.Version != candidateSet.template {
		t.Errorf("templates.typescript-web = %+v (present %v), want %s\n%s", pin, ok, candidateSet.template, output)
	}
	if data, err := os.ReadFile(filepath.Join(h.root, "webapp", "template.txt")); err != nil || string(data) != "template "+candidateSet.template+"\n" {
		t.Errorf("webapp/template.txt = %q, %v; want the template release %s", data, err, candidateSet.template)
	}

	h.assertInstallerChannels(t, candidateChannel)
	h.assertInstallerRegistries(t, starterNPMRegistry)

	for _, request := range h.registry.asked() {
		if strings.HasSuffix(request, " latest") {
			t.Errorf("the registry was asked %q: init on %s reads latest", request, candidateChannel)
		}
	}
	if got := h.proxy.asked(); len(got) != 0 {
		t.Errorf("the module proxy was asked %v by a TypeScript starter", got)
	}
}

// A channel init cannot resolve on stops the real command with a usage error
// before it writes anything or asks any registry.
func TestInit_RefusesAnUnsafeChannelBeforeAnyRequest(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "one-channel-choice", "an-unsafe-name-or-a-version-is-refused-before-anything-is-written")
	tests := []struct {
		name     string
		args     []string
		variable string
		want     string
	}{
		{name: "a version by flag", args: []string{"--channel", "0.4.0"}, want: "not on an exact version"},
		{name: "a path by flag", args: []string{"--channel=../" + candidateChannel}, want: "invalid channel"},
		{name: "an uppercase name by flag", args: []string{"--channel", "Tooling-v0.4.0"}, want: "invalid channel"},
		{name: "a query by variable", variable: candidateChannel + "&channel=latest", want: "invalid channel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, map[string]releaseSet{"latest": olderSet, candidateChannel: candidateSet})
			t.Setenv("PUTNAMI_CHANNEL", tt.variable)

			args := append([]string{"init", "--project", "webapp", "--extension", "go"}, tt.args...)
			code, output := clitest.RunGateArgs(t, h.root, args...)
			if code != protocolcli.ExitUsage || !strings.Contains(output, tt.want) {
				t.Fatalf("putnami %v exited %d, want the usage exit %d and %q:\n%s", args, code, protocolcli.ExitUsage, tt.want, output)
			}
			if entries, err := os.ReadDir(h.root); err != nil || len(entries) != 0 {
				t.Errorf("a refused init left %d entries behind (%v), want none", len(entries), err)
			}
			if got := h.registry.asked(); len(got) != 0 {
				t.Errorf("a refused init asked the registry %v", got)
			}
			if got := h.proxy.asked(); len(got) != 0 {
				t.Errorf("a refused init asked the module proxy %v", got)
			}
		})
	}
}

// harness is one empty directory to run init in, the registry and the module
// proxy init reaches, and the directory the fixture runtime records in.
type harness struct {
	root     string
	records  string
	registry *releaseRegistry
	proxy    *moduleProxy
	// archives are the release archives by set, built once per harness.
	archives map[releaseSet]map[string]release
}

// newHarness starts a registry and a module proxy that serve channels, and
// points a fresh, isolated CLI environment at them.
func newHarness(t *testing.T, channels map[string]releaseSet) *harness {
	t.Helper()
	clitest.RequireShell(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hometest.Temp(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(t.TempDir(), "artifacts"))
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_ARTIFACTS_ENSURED", "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
	t.Setenv("PUTNAMI_HTTP_RETRY_BACKOFF", "0")
	t.Setenv("DO_NOT_TRACK", "1")

	h := &harness{root: root, records: t.TempDir(), archives: map[releaseSet]map[string]release{}}
	h.registry = newReleaseRegistry(t)
	frameworks := map[string]string{}
	for channel, set := range channels {
		h.registry.publish(channel, h.releases(t, set))
		frameworks[channel] = set.framework
	}
	t.Setenv("PUTNAMI_REGISTRY_URL", h.registry.URL)

	h.proxy = newModuleProxy(t, frameworks)
	// The Go settings of the host must not reach the lookup: the fixture proxy
	// is the only one, and no go env file or private pattern applies.
	t.Setenv("GOPROXY", h.proxy.URL)
	t.Setenv("GOENV", "off")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GOPRIVATE", "")
	return h
}

// releases returns the archives of set by download path.
func (h *harness) releases(t *testing.T, set releaseSet) map[string]release {
	t.Helper()
	if built, ok := h.archives[set]; ok {
		return built
	}
	built := map[string]release{
		goExtensionPath: {version: set.extension, archive: extensionArchive(t, h.records, "@putnami/go", set.extension)},
		templatePath:    {version: set.template, archive: templateArchive(t, set.template)},
		contentPath:     contentRelease(t, set.content),
	}
	h.archives[set] = built
	return built
}

// lock reads the lock init left.
func (h *harness) lock(t *testing.T) *lockfile.LockFile {
	t.Helper()
	lock, err := lockfile.ReadLockFile(h.root)
	if err != nil || lock == nil {
		t.Fatalf("read the lock init left: %v", err)
	}
	return lock
}

// assertResolved fails unless the lock pins set's exact versions with the
// digests of set's archives, the project renders set's framework version, and
// the agent content comes from set's release.
func (h *harness) assertResolved(t *testing.T, set releaseSet, transcript string) {
	t.Helper()
	lock := h.lock(t)
	archives := h.releases(t, set)
	platform := lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)

	extensionPin, ok := lock.GetExtension("@putnami/go")
	if !ok || extensionPin.Version != set.extension || extensionPin.Integrities[platform] != sha256Hex(archives[goExtensionPath].archive) {
		t.Errorf("extensions.@putnami/go = %+v (present %v), want %s with the digest of that release\n%s",
			extensionPin, ok, set.extension, transcript)
	}
	contentPin, ok := lock.GetExtension("@putnami/contributor")
	if !ok || contentPin.Version != set.content || contentPin.Integrities[platform] != sha256Hex(archives[contentPath].archive) {
		t.Errorf("extensions.@putnami/contributor = %+v (present %v), want %s with the digest of that release\n%s",
			contentPin, ok, set.content, transcript)
	}
	templatePin, ok := lock.GetTemplate("go-server")
	// A template archive serves every platform, so its digest is not keyed by
	// this host's.
	if !ok || templatePin.Version != set.template || templatePin.Integrity != sha256Hex(archives[templatePath].archive) {
		t.Errorf("templates.go-server = %+v (present %v), want %s with the digest of that release\n%s",
			templatePin, ok, set.template, transcript)
	}
	if data, err := os.ReadFile(filepath.Join(h.root, "webapp", "framework.txt")); err != nil || string(data) != "app "+set.framework+"\n" {
		t.Errorf("webapp/framework.txt = %q, %v; want the framework version %s", data, err, set.framework)
	}
	if data, err := os.ReadFile(filepath.Join(h.root, "webapp", "template.txt")); err != nil || string(data) != "template "+set.template+"\n" {
		t.Errorf("webapp/template.txt = %q, %v; want the template release %s", data, err, set.template)
	}
	skill := filepath.Join(h.root, ".claude", "skills", "fixture-review", "SKILL.md")
	if data, err := os.ReadFile(skill); err != nil || !strings.Contains(string(data), "Review release "+set.content+".") {
		t.Errorf("%s = %q, %v; want the agent content of release %s", skill, data, err, set.content)
	}
}

// assertAskedOnly fails when the registry or the module proxy was asked for
// anything but channel and the exact versions of set.
func (h *harness) assertAskedOnly(t *testing.T, channel string, set releaseSet) {
	t.Helper()
	allowed := map[string][]string{
		goExtensionPath: {channel, set.extension},
		templatePath:    {channel, set.template},
		contentPath:     {channel, set.content},
	}
	asked := h.registry.asked()
	if len(asked) == 0 {
		t.Fatal("the registry was never asked, so this test proves nothing")
	}
	for _, request := range asked {
		path, selector, _ := strings.Cut(request, " ")
		if !slices.Contains(allowed[path], selector) {
			t.Errorf("the registry was asked %q, want only %s and the exact versions it names", request, channel)
		}
	}
	for _, path := range []string{goExtensionPath, templatePath, contentPath} {
		if !slices.Contains(asked, path+" "+channel) {
			t.Errorf("the registry was asked %v, want %s on %s", asked, path, channel)
		}
	}
	want := "/go.putnami.dev/app/@v/" + channel + ".info"
	if channel == "latest" {
		want = "/go.putnami.dev/app/@latest"
	}
	if got := h.proxy.asked(); !slices.Equal(got, []string{want}) {
		t.Errorf("the module proxy was asked %v, want only %s", got, want)
	}
}

// assertInstallerChannels fails unless every workspace-install run received
// want as its putnami-channel option ("none" for no option), and one ran.
func (h *harness) assertInstallerChannels(t *testing.T, want string) {
	t.Helper()
	runs := readLines(t, filepath.Join(h.records, "install-runs.txt"))
	if len(runs) == 0 {
		t.Fatal("no workspace-install ran, so the installers' option is unproven")
	}
	for _, run := range runs {
		if run != "putnami-channel="+want {
			t.Errorf("workspace-install runs = %v, want every run with putnami-channel=%s", runs, want)
			return
		}
	}
}

// assertInstallerRegistries fails unless every workspace-install run received
// a `registries` member whose npm entry maps the @putnami scope to registry,
// and one ran.
func (h *harness) assertInstallerRegistries(t *testing.T, registry string) {
	t.Helper()
	runs := readLines(t, filepath.Join(h.records, "install-registries.txt"))
	if len(runs) == 0 {
		t.Fatal("no workspace-install ran, so the installers' registries are unproven")
	}
	for _, run := range runs {
		var registries map[string]struct {
			Scopes map[string]string `json:"scopes"`
		}
		if err := json.Unmarshal([]byte(run), &registries); err != nil || registries["npm"].Scopes["@putnami"] != registry {
			t.Errorf("workspace-install received registries %s (%v), want npm.scopes.@putnami = %s", run, err, registry)
		}
	}
}

// assertPinsNoChannel fails when the workspace config or the lock names the
// candidate channel or constrains an artifact, or when a lock source is not
// the download of an exact version.
func (h *harness) assertPinsNoChannel(t *testing.T, set releaseSet) {
	t.Helper()
	for _, file := range []string{wsproto.WorkspaceConfigFilename, lockfile.LockFilename} {
		data, err := os.ReadFile(filepath.Join(h.root, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(data), candidateChannel) {
			t.Errorf("%s names the channel %s:\n%s", file, candidateChannel, data)
		}
	}
	data, err := os.ReadFile(filepath.Join(h.root, wsproto.WorkspaceConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Extensions []string `json:"extensions"`
		Templates  []string `json:"templates"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("the workspace config does not declare its artifacts as bare names: %v\n%s", err, data)
	}
	if !slices.Equal(config.Extensions, []string{"@putnami/go", "@putnami/contributor"}) {
		t.Errorf("workspace extensions = %v, want the two bare names", config.Extensions)
	}
	for _, name := range config.Templates {
		if strings.Contains(name, "@") {
			t.Errorf("workspace template %q carries a constraint", name)
		}
	}

	lock := h.lock(t)
	sources := map[string]string{}
	if pin, ok := lock.GetExtension("@putnami/go"); ok {
		sources[pin.Source] = h.registry.URL + goExtensionPath + "?channel=" + set.extension
	}
	if pin, ok := lock.GetExtension("@putnami/contributor"); ok {
		sources[pin.Source] = h.registry.URL + contentPath + "?channel=" + set.content
	}
	if pin, ok := lock.GetTemplate("go-server"); ok {
		sources[pin.Source] = h.registry.URL + templatePath + "?channel=" + set.template
	}
	if len(sources) != 3 {
		t.Errorf("the lock pins %d of the three artifacts", len(sources))
	}
	for got, want := range sources {
		if got != want {
			t.Errorf("lock source = %s, want the download of the exact version: %s", got, want)
		}
	}
}

// release is one published archive.
type release struct {
	version string
	archive []byte
}

// releaseRegistry serves archives the way the registry protocol does: the
// channel query names a channel or an exact version, and the response carries
// the resolved version and the archive digest. It records every download it
// is asked as "<path> <channel>".
type releaseRegistry struct {
	*httptest.Server
	mu       sync.Mutex
	channels map[string]map[string]release
	requests []string
}

func newReleaseRegistry(t *testing.T) *releaseRegistry {
	t.Helper()
	registry := &releaseRegistry{channels: map[string]map[string]release{}}
	registry.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selector := r.URL.Query().Get("channel")
		registry.mu.Lock()
		registry.requests = append(registry.requests, r.URL.Path+" "+selector)
		found, ok := registry.channels[selector][r.URL.Path]
		if !ok {
			for _, releases := range registry.channels {
				if candidate, has := releases[r.URL.Path]; has && candidate.version == selector {
					found, ok = candidate, true
					break
				}
			}
		}
		registry.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Resolved-Version", found.version)
		w.Header().Set("X-Integrity", sha256Hex(found.archive))
		_, _ = w.Write(found.archive)
	}))
	t.Cleanup(registry.Close)
	return registry
}

// publish makes channel name releases, by download path.
func (r *releaseRegistry) publish(channel string, releases map[string]release) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[channel] = releases
}

// asked returns every download asked so far, as "<path> <channel>".
func (r *releaseRegistry) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// channelRequests returns the downloads that named a channel rather than an
// exact version, each once, in the order they were first asked.
func (r *releaseRegistry) channelRequests() []string {
	var out []string
	for _, request := range r.asked() {
		_, selector, _ := strings.Cut(request, " ")
		if selector == "" || (selector[0] >= '0' && selector[0] <= '9') || slices.Contains(out, request) {
			continue
		}
		out = append(out, request)
	}
	return out
}

// moduleProxy is a Go module proxy that answers the version of
// go.putnami.dev/app per channel, and records the paths it is asked.
type moduleProxy struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

// newModuleProxy starts a module proxy that answers frameworks[channel]:
// `latest` on /@latest, any other channel on /@v/<channel>.info.
func newModuleProxy(t *testing.T, frameworks map[string]string) *moduleProxy {
	t.Helper()
	answers := map[string]string{}
	for channel, version := range frameworks {
		if channel == "latest" {
			answers["/go.putnami.dev/app/@latest"] = version
			continue
		}
		answers["/go.putnami.dev/app/@v/"+channel+".info"] = version
	}
	proxy := &moduleProxy{}
	proxy.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.mu.Lock()
		proxy.paths = append(proxy.paths, r.URL.Path)
		proxy.mu.Unlock()
		version, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"Version":"`+version+`","Time":"2026-09-20T10:00:00Z"}`)
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

// asked returns the paths the proxy was asked so far.
func (p *moduleProxy) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.paths)
}

// extensionArchive is the fixture language extension name at version. Its
// runtime is a shell script: it prints the runtime-info document of name at
// version itself, execs the test binary for workspace-install
// (runFixtureRuntime), which records the channel option and the registries it
// received, and exits 0 for any other invocation.
func extensionArchive(t *testing.T, records, name, version string) []byte {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := json.Marshal(runtimeproto.Info{
		Extension:       name,
		Version:         version,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
		CLIContract:     protocolcli.CurrentContract,
		RuntimeProtocol: runtimeproto.MaxKnownProtocolVersion,
		RuntimeABI:      runtimeproto.RuntimeABIVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "name": "` + name + `",
  "version": "` + version + `",
  "cliContract": ` + fmt.Sprint(protocolcli.CurrentContract) + `,
  "runtime": { "executable": "bin/runtime" },
  "commands": {
    "workspace-install": {
      "description": "Record the options the installers receive.",
      "run": [{ "id": "workspace-install", "task": "workspace-install-exec" }]
    }
  },
  "tasks": {
    "workspace-install-exec": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["workspace-install"],
      "cwd": "{workspaceRoot}",
      "cache": false,
      "timeoutMs": 60000
    }
  }
}`
	runtimeScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n" +
		"  printf '%s\\n' " + shellQuote(string(info)) + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" != \"workspace-install\" ]; then\n" +
		"  exit 0\n" +
		"fi\n" +
		fixtureRuntimeEnv + "=" + shellQuote(records) + "\n" +
		"export " + fixtureRuntimeEnv + "\n" +
		"exec " + shellQuote(testBinary) + " \"$@\"\n"
	return tarGz(t, []tarEntry{
		{name: "bin/runtime", mode: 0o755, content: runtimeScript},
		{name: "putnami.extension.json", mode: 0o644, content: manifest},
	})
}

// shellQuote quotes value as one POSIX shell word.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// templateArchive is the fixture go-server template at version. It renders the
// Go framework version and its own release into text files and declares no Go
// module, so create resolves the framework version and runs no go command:
// the toolchainlock package covers the Go setup of a real module.
func templateArchive(t *testing.T, version string) []byte {
	t.Helper()
	return tarGz(t, []tarEntry{
		{name: "framework.txt.template", mode: 0o644, content: "app <%= goFrameworkVersion %>\n"},
		{name: "template.txt", mode: 0o644, content: "template " + version + "\n"},
		{name: "putnami.json.template", mode: 0o644, content: `{"name":"<%= projectName %>","extensions":["@putnami/go"]}`},
		{name: "putnami.template.json", mode: 0o644,
			content: `{"name":"go-server","description":"Go server","extension":"@putnami/go","version":"` + version + `"}`},
	})
}

// typeScriptTemplateArchive is the fixture typescript-web template at version.
// It renders its own release into a text file and declares no package, so no
// package manager runs.
func typeScriptTemplateArchive(t *testing.T, version string) []byte {
	t.Helper()
	return tarGz(t, []tarEntry{
		{name: "template.txt", mode: 0o644, content: "template " + version + "\n"},
		{name: "putnami.json.template", mode: 0o644, content: `{"name":"<%= projectName %>","extensions":["@putnami/typescript"]}`},
		{name: "putnami.template.json", mode: 0o644,
			content: `{"name":"typescript-web","description":"TypeScript web","extension":"@putnami/typescript","version":"` + version + `"}`},
	})
}

// contentRelease packages the fixture @putnami/contributor at version with the
// SDK's package step: one skill that names the release it ships in.
func contentRelease(t *testing.T, version string) release {
	t.Helper()
	source := t.TempDir()
	files := map[string]string{
		"putnami.extension.json": `{"name":"@putnami/contributor","agentContent":{"path":"agent-content","source":"agent-src"}}`,
		"putnami.json":           `{"name":"@putnami/contributor","options":{"agent-artifact":{"forbiddenContent":[],"requiredSkills":["fixture-review"]}}}`,
		"agent-src/skills/fixture-review/SKILL.md": "---\nname: fixture-review\ndescription: Review a change\n---\n\n" +
			"# fixture-review\n\nReview release " + version + ".\n",
	}
	for name, content := range files {
		clitest.WriteFile(t, filepath.Join(source, filepath.FromSlash(name)), content)
	}
	pkg, err := sdkagentartifact.PackageExtension(source, version)
	if err != nil {
		t.Fatalf("package @putnami/contributor@%s: %v", version, err)
	}
	return release{version: version, archive: pkg.Archive}
}

type tarEntry struct {
	name    string
	mode    int64
	content string
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readLines(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// runFixtureRuntime is the workspace-install job of a fixture language
// extension, run by the test binary under fixtureRuntimeEnv. It records two
// members of the job context it was handed: the putnami-channel option, and
// the registries as JSON. It records "none" for a member the context does not
// carry.
func runFixtureRuntime(args []string) int {
	if len(args) == 0 || args[0] != "workspace-install" {
		fmt.Fprintf(os.Stderr, "the fixture runtime runs workspace-install only, got %q\n", args)
		return 1
	}
	contextFile := ""
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--putnamiContext" {
			contextFile = args[i+1]
		}
	}
	data, err := os.ReadFile(contextFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read the job context:", err)
		return 1
	}
	var jobContext struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &jobContext); err != nil {
		fmt.Fprintln(os.Stderr, "decode the job context:", err)
		return 1
	}
	channel := "none"
	if value, ok := jobContext.Params["putnami-channel"]; ok {
		channel = fmt.Sprint(value)
	}
	registries := "none"
	if value, ok := jobContext.Params["registries"]; ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			fmt.Fprintln(os.Stderr, "encode the registries of the job context:", err)
			return 1
		}
		registries = string(encoded)
	}
	records := os.Getenv(fixtureRuntimeEnv)
	for name, line := range map[string]string{
		"install-runs.txt":       "putnami-channel=" + channel,
		"install-registries.txt": registries,
	} {
		if err := appendLine(filepath.Join(records, name), line); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

// appendLine appends line to file, creating it when it does not exist.
func appendLine(file, line string) error {
	out, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, line); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
