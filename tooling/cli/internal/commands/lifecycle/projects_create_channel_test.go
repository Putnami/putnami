package lifecycle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// channelGoProxy is a module proxy that answers a version per request path,
// 404 for every other path, and records the paths it is asked.
type channelGoProxy struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

// newChannelGoProxy starts a module proxy that answers answers[path] as the
// version at path.
func newChannelGoProxy(t *testing.T, answers map[string]string) *channelGoProxy {
	t.Helper()
	proxy := &channelGoProxy{}
	proxy.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.mu.Lock()
		proxy.paths = append(proxy.paths, r.URL.Path)
		proxy.mu.Unlock()
		version, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, frameworkLatest(version))
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

// asked returns the paths the proxy was asked so far.
func (p *channelGoProxy) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.paths)
}

const (
	// frameworkLatestPath and frameworkCandidatePath are the module proxy
	// paths that answer latest and candidateChannel for the Go framework.
	frameworkLatestPath    = "/go.putnami.dev/app/@latest"
	frameworkCandidatePath = "/go.putnami.dev/app/@v/" + candidateChannel + ".info"
	// olderFramework is the version latest names, candidateFramework the one
	// candidateChannel names.
	olderFramework     = "v0.3.0"
	candidateFramework = "v0.4.0"
)

// A channel other than latest is asked as the Go version query
// @v/<channel>.info, on the proxies @latest is asked on; latest, and no
// channel, ask @latest.
func TestResolveGoFrameworkVersionOnAChannelAsksTheChannelQuery(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel", "the-go-framework-version-is-the-one-the-channel-names")
	tests := []struct {
		channel  string
		want     string
		wantPath string
	}{
		{candidateChannel, candidateFramework, frameworkCandidatePath},
		{"canary", "v0.5.0-0123abcd", "/go.putnami.dev/app/@v/canary.info"},
		{"latest", olderFramework, frameworkLatestPath},
		{"", olderFramework, frameworkLatestPath},
	}
	for _, tt := range tests {
		t.Run("channel "+tt.channel, func(t *testing.T) {
			proxy := newChannelGoProxy(t, map[string]string{
				frameworkLatestPath:                  olderFramework,
				frameworkCandidatePath:               candidateFramework,
				"/go.putnami.dev/app/@v/canary.info": "v0.5.0-0123abcd",
			})
			version, source, err := resolveGoFrameworkVersionOn(context.Background(), http.DefaultClient,
				goLookupEnv(t, map[string]string{"GOPROXY": proxy.URL}), tt.channel)
			if err != nil || version != tt.want || source != proxy.URL {
				t.Fatalf("resolveGoFrameworkVersionOn(%q) = %q from %q, %v; want %s from %s", tt.channel, version, source, err, tt.want, proxy.URL)
			}
			if got := proxy.asked(); !slices.Equal(got, []string{tt.wantPath}) {
				t.Fatalf("the proxy was asked %v, want only %s", got, tt.wantPath)
			}
		})
	}
}

// A proxy that does not serve the channel passes the query on, as it does for
// latest. When no proxy serves it, the lookup fails naming the channel and
// every proxy it asked, and it never asks any of them for latest.
func TestResolveGoFrameworkVersionOnAChannelNeverAnswersWithLatest(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel", "a-missing-channel-fails-instead-of-resolving-latest")
	onlyLatest := newChannelGoProxy(t, map[string]string{frameworkLatestPath: olderFramework})
	serves := newChannelGoProxy(t, map[string]string{frameworkLatestPath: olderFramework, frameworkCandidatePath: candidateFramework})

	version, source, err := resolveGoFrameworkVersionOn(context.Background(), http.DefaultClient,
		goLookupEnv(t, map[string]string{"GOPROXY": onlyLatest.URL + "," + serves.URL}), candidateChannel)
	if err != nil || version != candidateFramework || source != serves.URL {
		t.Fatalf("resolveGoFrameworkVersionOn() = %q from %q, %v; want %s from the proxy that serves the channel", version, source, err, candidateFramework)
	}
	if got := onlyLatest.asked(); !slices.Equal(got, []string{frameworkCandidatePath}) {
		t.Fatalf("the proxy without the channel was asked %v, want only %s", got, frameworkCandidatePath)
	}

	other := newChannelGoProxy(t, map[string]string{frameworkLatestPath: olderFramework})
	version, _, err = resolveGoFrameworkVersionOn(context.Background(), http.DefaultClient,
		goLookupEnv(t, map[string]string{"GOPROXY": other.URL}), candidateChannel)
	if err == nil || version != "" {
		t.Fatalf("resolveGoFrameworkVersionOn() = %q, %v; want an error and no version", version, err)
	}
	for _, want := range []string{"go.putnami.dev/app", "on channel " + candidateChannel, other.URL, "HTTP 404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if got := other.asked(); !slices.Equal(got, []string{frameworkCandidatePath}) {
		t.Fatalf("the proxy was asked %v, want only %s: a channel is never answered with latest", got, frameworkCandidatePath)
	}
}

// A module read from its origin is asked for the channel on the module proxy
// its go-import tag names, as it is asked for latest there.
func TestResolveModuleChannelReadsTheOrigin(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel", "the-go-framework-version-is-the-one-the-channel-names")
	origin := &fakeOrigin{pages: map[string]string{}}
	origin.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin.mu.Lock()
		defer origin.mu.Unlock()
		origin.requests = append(origin.requests, goProxyRequest{path: r.URL.RequestURI(), userAgent: r.Header.Get("User-Agent")})
		switch {
		case strings.HasPrefix(r.URL.Path, "/proxy/") && strings.HasSuffix(r.URL.Path, "/@v/"+candidateChannel+".info"):
			_, _ = io.WriteString(w, frameworkLatest(candidateFramework))
		case strings.HasPrefix(r.URL.Path, "/proxy/") && strings.HasSuffix(r.URL.Path, "/@latest"):
			_, _ = io.WriteString(w, frameworkLatest(olderFramework))
		case r.URL.Query().Get("go-get") == "1" && origin.pages[r.URL.Path] != "":
			_, _ = io.WriteString(w, origin.pages[r.URL.Path])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	origin.serve("/app", origin.module()+" mod "+origin.URL+"/proxy")

	version, source, err := resolveModuleChannel(context.Background(), origin.Client(),
		goLookupEnv(t, map[string]string{"GOPROXY": "direct"}), origin.module(), candidateChannel)
	if err != nil || version != candidateFramework || source != "direct ("+origin.URL+"/proxy)" {
		t.Fatalf("resolveModuleChannel() = %q from %q, %v; want %s from direct (%s/proxy)", version, source, err, candidateFramework, origin.URL)
	}
	want := []goProxyRequest{
		{path: "/app?go-get=1", userAgent: "putnami-cli/dev"},
		{path: "/proxy/" + origin.module() + "/@v/" + candidateChannel + ".info", userAgent: "putnami-cli/dev"},
	}
	if got := origin.got(); !slices.Equal(got, want) {
		t.Fatalf("the origin got %+v, want %+v", got, want)
	}
}

// The create an init on a channel runs pins the framework version the channel
// names; `projects create` on its own still pins the one latest names.
func TestInitProjectCreatePinsTheFrameworkVersionOfTheChannel(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel", "the-go-framework-version-is-the-one-the-channel-names")
	answers := map[string]string{frameworkLatestPath: olderFramework, frameworkCandidatePath: candidateFramework}

	t.Run("the create of an init on a channel", func(t *testing.T) {
		root := frameworkTemplateWorkspace(t)
		stubModuleOriginCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
		proxy := newChannelGoProxy(t, answers)
		setGoProxyEnv(t, proxy.URL)

		stdout, err := captureStdout(t, func() error {
			return createInitProjectFiles(context.Background(), root, wsproto.Load(root),
				[]string{"api", "--template", "fw-app"}, true, LifecycleEnv{Out: io.Discard, channel: candidateChannel})
		})
		if err != nil {
			t.Fatalf("createInitProjectFiles: %v\n%s", err, stdout)
		}
		if data, err := os.ReadFile(filepath.Join(root, "api", "framework.txt")); err != nil || string(data) != "app "+candidateFramework+"\n" {
			t.Fatalf("framework.txt = %q, %v; want the version the channel names", data, err)
		}
		if got := proxy.asked(); !slices.Equal(got, []string{frameworkCandidatePath}) {
			t.Fatalf("the proxy was asked %v, want only %s", got, frameworkCandidatePath)
		}
	})

	t.Run("projects create on its own", func(t *testing.T) {
		root := frameworkTemplateWorkspace(t)
		stubModuleOriginCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
		proxy := newChannelGoProxy(t, answers)
		setGoProxyEnv(t, proxy.URL)
		// The variable and the install record choose the channel of init alone.
		installedAs(t, "putnami-go-"+candidateChannel)
		t.Setenv(initChannelEnv, candidateChannel)

		stdout, err := captureStdout(t, func() error {
			return ProjectsCreate(context.Background(), root, wsproto.Load(root),
				[]string{"api", "--template", "fw-app"}, true, LifecycleEnv{Out: io.Discard})
		})
		if err != nil {
			t.Fatalf("ProjectsCreate: %v\n%s", err, stdout)
		}
		if data, err := os.ReadFile(filepath.Join(root, "api", "framework.txt")); err != nil || string(data) != "app "+olderFramework+"\n" {
			t.Fatalf("framework.txt = %q, %v; want the version latest names", data, err)
		}
		if got := proxy.asked(); !slices.Equal(got, []string{frameworkLatestPath}) {
			t.Fatalf("the proxy was asked %v, want only %s", got, frameworkLatestPath)
		}
	})

	t.Run("a channel no proxy serves writes nothing", func(t *testing.T) {
		root := frameworkTemplateWorkspace(t)
		stubModuleOriginCredential(t, registrycred.Outcome{Kind: registrycred.KindMaterialized})
		proxy := newChannelGoProxy(t, map[string]string{frameworkLatestPath: olderFramework})
		setGoProxyEnv(t, proxy.URL)

		_, err := captureStdout(t, func() error {
			return createInitProjectFiles(context.Background(), root, wsproto.Load(root),
				[]string{"api", "--template", "fw-app"}, false, LifecycleEnv{Out: io.Discard, channel: candidateChannel})
		})
		if err == nil || !strings.Contains(err.Error(), "on channel "+candidateChannel) {
			t.Fatalf("createInitProjectFiles = %v, want an error naming the channel", err)
		}
		if _, statErr := os.Stat(filepath.Join(root, "api")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("a failed lookup left %s behind (stat: %v)", filepath.Join(root, "api"), statErr)
		}
		if got := proxy.asked(); !slices.Equal(got, []string{frameworkCandidatePath}) {
			t.Fatalf("the proxy was asked %v, want only %s", got, frameworkCandidatePath)
		}
	})
}

// templateRegistry is a put registry that serves one template per channel and
// records the channel of every download it is asked.
type templateRegistry struct {
	*httptest.Server
	mu       sync.Mutex
	channels []string
}

// newTemplateRegistry starts a registry that serves the template starter-app
// at versions[channel], points the installers at it, and isolates their home,
// store and credential lookup.
func newTemplateRegistry(t *testing.T, versions map[string]string) *templateRegistry {
	t.Helper()
	registry := &templateRegistry{}
	registry.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")
		registry.mu.Lock()
		registry.channels = append(registry.channels, r.URL.Path+" "+channel)
		registry.mu.Unlock()
		version, ok := versions[channel]
		if !ok || r.URL.Path != "/putnami/starter-app/download" {
			http.NotFound(w, r)
			return
		}
		archive := buildManifestArchive(t, "putnami.template.json",
			`{"name":"starter-app","description":"Starter `+version+`","extension":"@acme/app","version":"`+version+`"}`)
		w.Header().Set("X-Resolved-Version", version)
		w.Header().Set("X-Integrity", sha256Hex(archive))
		_, _ = w.Write(archive)
	}))
	t.Cleanup(registry.Close)
	hometest.Temp(t)
	t.Setenv(extension.PutRegistryURLEnv, registry.URL)
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	t.Setenv("PUTNAMI_STORE_DIR", t.TempDir())
	original := extension.ResolveRegistryToken
	extension.ResolveRegistryToken = func(string) (string, string) { return "", "" }
	t.Cleanup(func() { extension.ResolveRegistryToken = original })
	return registry
}

// asked returns "<path> <channel>" for every download asked so far.
func (r *templateRegistry) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.channels)
}

// emptyWorkspace writes a workspace that declares nothing and has an empty
// lock.
func emptyWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeRawFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"name":"channel-ws"}`)
	if err := lockfile.WriteLockFile(root, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	return root
}

// The install create falls back to for a template the workspace does not hold
// resolves on the channel of the init that runs it, and never on latest: it
// succeeds with the channel's release while latest is empty, and it fails
// instead of installing the release latest names when the channel is empty.
func TestInitProjectCreateInstallsAMissingTemplateOnTheChannel(t *testing.T) {
	spectest.Proves(t, "cli/init-channel", "every-resolution-reads-the-channel", "the-fallback-template-install-resolves-on-the-channel")

	t.Run("the channel serves the template and latest does not", func(t *testing.T) {
		registry := newTemplateRegistry(t, map[string]string{candidateChannel: "0.4.0"})
		root := emptyWorkspace(t)
		out, err := captureStdout(t, func() error {
			return createInitProjectFiles(context.Background(), root, wsproto.Load(root),
				[]string{"web", "--template", "starter-app"}, false, LifecycleEnv{Out: io.Discard, channel: candidateChannel})
		})
		if err != nil {
			t.Fatalf("createInitProjectFiles: %v\n%s", err, out)
		}
		for _, asked := range registry.asked() {
			if strings.HasSuffix(asked, " latest") {
				t.Fatalf("the registry was asked %v: the fallback install resolved on latest", registry.asked())
			}
		}
		if got := registry.asked(); len(got) == 0 || got[0] != "/putnami/starter-app/download "+candidateChannel {
			t.Fatalf("the registry was asked %v, want the template on %s first", got, candidateChannel)
		}
		lock, err := lockfile.ReadLockFile(root)
		if err != nil || lock == nil {
			t.Fatalf("read the lock: %v", err)
		}
		pin, ok := lock.GetTemplate("starter-app")
		if !ok || pin.Version != "0.4.0" || strings.Contains(pin.Source, candidateChannel) {
			t.Fatalf("templates.starter-app = %+v (present %v), want the exact version 0.4.0 and a source that names no channel", pin, ok)
		}
		if data, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename)); strings.Contains(string(data), candidateChannel) {
			t.Fatalf("the workspace config names the channel:\n%s", data)
		}
	})

	t.Run("only latest serves the template", func(t *testing.T) {
		registry := newTemplateRegistry(t, map[string]string{"latest": "0.3.0"})
		root := emptyWorkspace(t)
		_, err := captureStdout(t, func() error {
			return createInitProjectFiles(context.Background(), root, wsproto.Load(root),
				[]string{"web", "--template", "starter-app"}, false, LifecycleEnv{Out: io.Discard, channel: candidateChannel})
		})
		if err == nil {
			t.Fatal("createInitProjectFiles succeeded with the release latest names, want the template not found on the channel")
		}
		if got := registry.asked(); !slices.Equal(got, []string{"/putnami/starter-app/download " + candidateChannel}) {
			t.Fatalf("the registry was asked %v, want the template on %s only", got, candidateChannel)
		}
		if _, statErr := os.Stat(filepath.Join(root, "web")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("a failed install left %s behind (stat: %v)", filepath.Join(root, "web"), statErr)
		}
	})

	t.Run("a create without a channel installs on latest", func(t *testing.T) {
		registry := newTemplateRegistry(t, map[string]string{"latest": "0.3.0", candidateChannel: "0.4.0"})
		root := emptyWorkspace(t)
		out, err := captureStdout(t, func() error {
			return ProjectsCreate(context.Background(), root, wsproto.Load(root),
				[]string{"web", "--template", "starter-app"}, false, LifecycleEnv{Out: io.Discard})
		})
		if err != nil {
			t.Fatalf("ProjectsCreate: %v\n%s", err, out)
		}
		if got := registry.asked(); len(got) == 0 || got[0] != "/putnami/starter-app/download latest" {
			t.Fatalf("the registry was asked %v, want the template on latest first", got)
		}
		lock, err := lockfile.ReadLockFile(root)
		if err != nil || lock == nil {
			t.Fatalf("read the lock: %v", err)
		}
		if pin, ok := lock.GetTemplate("starter-app"); !ok || pin.Version != "0.3.0" {
			t.Fatalf("templates.starter-app = %+v (present %v), want the version latest names", pin, ok)
		}
	})
}
