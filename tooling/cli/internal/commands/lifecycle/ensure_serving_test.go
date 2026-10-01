package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// EnsureArtifactsServing makes a credential source the first one of the
// lock-pinned extension downloads and of nothing else: the source is in
// effect for the extension pass only, the template download carries no
// credential, and the source is removed before it returns. A call that
// EnsureArtifacts would skip never installs it.
func TestEnsureArtifactsServingServesOnlyTheExtensionDownloads(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "bootstrap-provider-serves-only-the-locked-downloads")
	const (
		name     = "@acme/tool"
		version  = "1.0.0"
		template = "alpha"
	)
	manifest := sharedtest.ContextTestExtensionManifest(name, version)
	extensionArchive := readPreparationArchive(t, map[string]string{"putnami.extension.json": manifest})
	templateManifest := `{"name":"alpha","version":"1.0.0","description":"Alpha template","extension":"@putnami/go"}`
	templateArchive := buildManifestArchive(t, "putnami.template.json", templateManifest)

	seen := map[string]string{}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := "extension"
		if strings.Contains(r.URL.Path, template) {
			kind = "template"
		}
		seen[kind] = r.Header.Get("Authorization")
		w.Header().Set("X-Resolved-Version", version)
		if kind == "template" {
			_, _ = w.Write(templateArchive)
			return
		}
		_, _ = w.Write(extensionArchive)
	}))
	defer registry.Close()
	origin, err := url.Parse(registry.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(extension.PutRegistryURLEnv, registry.URL)
	t.Setenv("PUTNAMI_REGISTRY_URL", registry.URL)
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	t.Setenv(artifactsEnsuredEnv, "")
	previous := extension.ResolveRegistryToken
	extension.ResolveRegistryToken = func(string) (string, string) { return "", "" }
	t.Cleanup(func() { extension.ResolveRegistryToken = previous })

	ws := t.TempDir()
	lf := lockfile.NewLockFile()
	extensionSum, manifestSum := sha256.Sum256(extensionArchive), sha256.Sum256([]byte(manifest))
	lf.SetExtension(name, lockfile.LockEntry{
		Version: version, ManifestHash: hex.EncodeToString(manifestSum[:]),
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): hex.EncodeToString(extensionSum[:])},
	})
	templateSum, templateManifestSum := sha256.Sum256(templateArchive), sha256.Sum256([]byte(templateManifest))
	lf.SetTemplate(template, lockfile.LockEntry{
		Version: version, ManifestHash: hex.EncodeToString(templateManifestSum[:]),
		Integrities: map[string]string{lockfile.PlatformKey("linux", "x64"): hex.EncodeToString(templateSum[:])},
	})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Templates: []string{template}, Extensions: wsproto.ExtensionsConfig{List: map[string]string{name: version}}}

	var events []string
	reads := 0
	serve := func() func() {
		events = append(events, "serve")
		restore := extension.InstallRegistryReadCredential(func(_ context.Context, target *url.URL) (string, bool, error) {
			reads++
			return "pat_bootstrap", target.Host == origin.Host, nil
		})
		return func() {
			events = append(events, "restore")
			restore()
		}
	}
	if err := EnsureArtifactsServing(context.Background(), ws, cfg, serve); err != nil {
		t.Fatalf("ensure artifacts: %v", err)
	}
	if got := seen["extension"]; got != "Bearer pat_bootstrap" {
		t.Errorf("the extension download carried %q, want the served bearer", got)
	}
	if got, ok := seen["template"]; !ok || got != "" {
		t.Errorf("the template download carried %q (requested %v), want no credential", got, ok)
	}
	if want := []string{"serve", "restore"}; !slices.Equal(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}

	// The source is gone, and the pass does not run twice.
	servedReads := reads
	req, err := http.NewRequest(http.MethodGet, registry.URL+"/after", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extension.AuthorizeRegistryRequest(req); err != nil || reads != servedReads || req.Header.Get("Authorization") != "" {
		t.Errorf("after the pass a download asked the source: reads %d, want %d; header %q; err %v",
			reads, servedReads, req.Header.Get("Authorization"), err)
	}
	if err := EnsureArtifactsServing(context.Background(), ws, cfg, serve); err != nil || len(events) != 2 {
		t.Errorf("a second pass for the same workspace = %v with events %v, want a no-op", err, events)
	}
}
