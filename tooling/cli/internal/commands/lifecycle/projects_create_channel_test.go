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
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

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
				[]string{"web", "--template", "starter-app"}, LifecycleEnv{Out: io.Discard, RunJob: noProjectJob, channel: candidateChannel})
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
				[]string{"web", "--template", "starter-app"}, LifecycleEnv{Out: io.Discard, RunJob: noProjectJob, channel: candidateChannel})
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
				[]string{"web", "--template", "starter-app"}, LifecycleEnv{Out: io.Discard, RunJob: noProjectJob})
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
