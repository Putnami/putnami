package lifecycle

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const (
	pinnedBefore = "0.0.0-20260926154701-255b7e227"
	pinnedAfter  = "0.0.0-20260928052023-c2cd30187"
)

// servePinnedExtensions starts a registry that serves every extension at the
// release its download channel names, advertising the archive's digest, and
// points the installer at it.
func servePinnedExtensions(t *testing.T) {
	t.Helper()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	t.Setenv(artifactsEnsuredEnv, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /<scope>/<name>/download?channel=<version>
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 3 || parts[2] != "download" {
			http.NotFound(w, r)
			return
		}
		name := "@" + parts[0] + "/" + parts[1]
		version := r.URL.Query().Get("channel")
		archive := buildManifestArchive(t, "putnami.extension.json", sharedtest.ContextTestExtensionManifest(name, version))
		w.Header().Set("X-Resolved-Version", version)
		w.Header().Set("X-Integrity", "sha256:"+sha256Hex(archive))
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PUTNAMI_REGISTRY_URL", srv.URL)
}

// writePinnedWorkspace writes a workspace whose `extensions` map pins two
// published extensions and declares one local path, with a lock that agrees
// with the pins.
func writePinnedWorkspace(t *testing.T) string {
	t.Helper()
	// An empty user home: wsproto.Load must not merge a global putnami config.
	hometest.Temp(t)
	dir := t.TempDir()
	local := filepath.Join(dir, "local-ext")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "putnami.extension.json"), []byte(sharedtest.ContextTestExtensionManifest("local-ext", "0.1.0")), 0o644); err != nil {
		t.Fatal(err)
	}
	config := `{
  "name": "upgrade-ws",
  "extensions": {
    "@fake/one": "` + pinnedBefore + `",
    "@fake/two": "` + pinnedBefore + `",
    "./local-ext": ""
  }
}
`
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	lf := lockfile.NewLockFile()
	lf.SetExtension("@fake/one", lockfile.LockEntry{Version: pinnedBefore})
	lf.SetExtension("@fake/two", lockfile.LockEntry{Version: pinnedBefore})
	if err := lockfile.WriteLockFile(dir, lf); err != nil {
		t.Fatal(err)
	}
	return dir
}

func workspaceExtensionPins(t *testing.T, dir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Extensions map[string]string `json:"extensions"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse %s: %v\n%s", wsproto.WorkspaceConfigFilename, err, data)
	}
	return config.Extensions
}

// An extension pinned to one release that upgrade moves carries its new
// release in both putnami.workspace.json and putnami.lock.json, and a local
// path entry is left as it was.
func TestUpgrade_ExtensionsMovesTheWorkspacePins(t *testing.T) {
	servePinnedExtensions(t)
	dir := writePinnedWorkspace(t)

	if _, err := sharedtest.CaptureStdout(t, func() error {
		return Upgrade(context.Background(), dir, wsproto.Load(dir), "1.0.0", t.TempDir(), UpgradeFlags{
			Extensions: true, Version: pinnedAfter,
		}, upgradeTestEnv(WorkspaceJobOK))
	}); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	want := map[string]string{"@fake/one": pinnedAfter, "@fake/two": pinnedAfter, "./local-ext": ""}
	if got := workspaceExtensionPins(t, dir); !maps.Equal(got, want) {
		t.Errorf("%s extensions = %v, want %v", wsproto.WorkspaceConfigFilename, got, want)
	}
	lf, err := lockfile.ReadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"@fake/one", "@fake/two"} {
		if entry, _ := lf.GetExtension(name); entry.Version != pinnedAfter {
			t.Errorf("lock %s = %q, want %q", name, entry.Version, pinnedAfter)
		}
	}
	if _, locked := lf.GetExtension("./local-ext"); locked {
		t.Error("the local path entry entered the lock")
	}
}

// --dry-run names each pin it would move and writes neither file.
func TestUpgrade_ExtensionsDryRunListsTheWorkspacePins(t *testing.T) {
	servePinnedExtensions(t)
	dir := writePinnedWorkspace(t)
	configPath := filepath.Join(dir, wsproto.WorkspaceConfigFilename)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error {
		return Upgrade(context.Background(), dir, wsproto.Load(dir), "1.0.0", t.TempDir(), UpgradeFlags{
			Extensions: true, Version: pinnedAfter, DryRun: true,
		}, upgradeTestEnv(WorkspaceJobOK))
	})
	if err != nil {
		t.Fatalf("Upgrade --dry-run: %v", err)
	}

	for _, name := range []string{"@fake/one", "@fake/two"} {
		line := "Would update putnami.workspace.json pin " + name + ": " + pinnedBefore + " → " + pinnedAfter
		if !strings.Contains(out, line) {
			t.Errorf("dry run does not list %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "pin ./local-ext") {
		t.Errorf("dry run lists the local path entry as a pin:\n%s", out)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("dry run rewrote %s:\n%s", wsproto.WorkspaceConfigFilename, after)
	}
	lf, err := lockfile.ReadLockFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if entry, _ := lf.GetExtension("@fake/one"); entry.Version != pinnedBefore {
		t.Errorf("dry run moved the lock to %q", entry.Version)
	}
}
