package extensions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/store"
)

// foreignPlatformArg is a platform guaranteed not to be the host's, so the
// materialization assertions mean the same thing on every CI runner.
func foreignPlatformArg() string {
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		return "darwin/arm64"
	}
	return "linux/amd64"
}

// hookedLocalExtensionWorkspace builds a workspace with one workspace-local
// extension whose onInstall hook drops a marker file, so a test can tell
// whether install hooks ran. It needs no registry.
func hookedLocalExtensionWorkspace(t *testing.T) (dir string, cfg *wsproto.Config, marker string) {
	t.Helper()
	dir = t.TempDir()
	extDir := filepath.Join(dir, "extensions", "local")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	marker = filepath.Join(dir, "hook-ran.txt")
	hookCommand := writeInstallHookProgram(t, extDir, marker, fixtureproc.Program{})
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(`{
  "name": "@local/ext",
  "hooks": {
    "onInstall": {
      "kind": "command",
      "command": "`+hookCommand+`",
      "args": ["{workspaceRoot}/hook-ran.txt"],
      "cwd": "{workspaceRoot}"
    }
  },
  "cliContract": 4,
  "commands": {},
  "tasks": {}
}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// Written as a literal rather than through a map fixture: the CLI's
	// structural baseline pins how many test lines mention untyped maps.
	if err := os.WriteFile(
		filepath.Join(dir, wsproto.WorkspaceConfigFilename),
		[]byte(`{"name":"materialize-ws","extensions":["/extensions/local"]}`),
		0o644,
	); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}
	cfg = &wsproto.Config{
		Name:       "materialize-ws",
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{"/extensions/local": ""}},
	}
	return dir, cfg, marker
}

// TestExtensionsInstall_HostInstallStillRunsHooksAndWritesTheLock is the control
// for the two materialization tests below: without the new flags nothing moves.
func TestExtensionsInstall_HostInstallStillRunsHooksAndWritesTheLock(t *testing.T) {
	dir, cfg, marker := hookedLocalExtensionWorkspace(t)

	if _, err := sharedtest.CaptureStdout(t, func() error {
		return ExtensionsInstall(context.Background(), dir, cfg, nil, "")
	}); err != nil {
		t.Fatalf("ExtensionsInstall: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("a plain install must still run onInstall hooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockfile.LockFilename)); err != nil {
		t.Errorf("a plain install must still write %s: %v", lockfile.LockFilename, err)
	}
}

// TestExtensionsInstall_ForeignPlatformIsSideEffectFree pins the three workspace
// side effects a materialization must not have: running THIS machine's onInstall
// hooks against a foreign tree, and writing a lock whose digests it only read.
func TestExtensionsInstall_ForeignPlatformIsSideEffectFree(t *testing.T) {
	dir, cfg, marker := hookedLocalExtensionWorkspace(t)

	out, err := sharedtest.CaptureStdout(t, func() error {
		return ExtensionsInstall(context.Background(), dir, cfg, []string{"--platform", foreignPlatformArg()}, "")
	})
	if err != nil {
		t.Fatalf("ExtensionsInstall --platform: %v", err)
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("install hooks ran for a foreign-platform materialization (err=%v)", err)
	}
	if strings.Contains(out, "Running extension install hooks") {
		t.Errorf("output announced the hook phase for a materialization:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, lockfile.LockFilename)); !os.IsNotExist(err) {
		t.Errorf("a materialization wrote %s (err=%v)", lockfile.LockFilename, err)
	}
	// The skip must be stated, never inferred.
	if !strings.Contains(out, foreignPlatformArg()) || !strings.Contains(out, "skipped") {
		t.Errorf("output does not state what the materialization skipped:\n%s", out)
	}
}

// TestExtensionsInstall_DestAloneAlsoMaterializes: --dest without --platform is
// still packaging output, so it gets the same containment.
func TestExtensionsInstall_DestAloneAlsoMaterializes(t *testing.T) {
	dir, cfg, marker := hookedLocalExtensionWorkspace(t)
	dest := filepath.Join(t.TempDir(), "warm")

	if _, err := sharedtest.CaptureStdout(t, func() error {
		return ExtensionsInstall(context.Background(), dir, cfg, []string{"--dest", dest}, "")
	}); err != nil {
		t.Fatalf("ExtensionsInstall --dest: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("install hooks ran for a --dest materialization (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockfile.LockFilename)); !os.IsNotExist(err) {
		t.Errorf("a --dest materialization wrote %s (err=%v)", lockfile.LockFilename, err)
	}
}

func TestParseArtifactTarget_RejectsMalformedPlatform(t *testing.T) {
	for _, value := range []string{"linux", "", "linux/", "/amd64", "linux/amd64/v3", "linux amd64"} {
		t.Run("value="+value, func(t *testing.T) {
			_, _, err := parseArtifactTarget(t.TempDir(), []string{"--platform", value})
			if err == nil {
				t.Fatalf("--platform %q was accepted", value)
			}
			if !errors.Is(err, cmderr.ErrUsage) {
				t.Errorf("error %v is not classified as a usage error", err)
			}
			if !strings.Contains(err.Error(), "<os>/<arch>") {
				t.Errorf("error = %v, want it to name the expected form", err)
			}
		})
	}
}

func TestParseArtifactTarget_RequiresValues(t *testing.T) {
	for _, args := range [][]string{{"--platform"}, {"--dest"}, {"--dest", "  "}} {
		_, _, err := parseArtifactTarget(t.TempDir(), args)
		if err == nil {
			t.Fatalf("%v was accepted", args)
		}
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("%v: error %v is not classified as a usage error", args, err)
		}
	}
}

// TestParseArtifactTarget_StripsFlagsAndTheirValues is the trap this parse
// exists for: the remaining argument list is re-scanned for the artifact name,
// so a leftover "linux/amd64" would be read as an extension to install.
func TestParseArtifactTarget_StripsFlagsAndTheirValues(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "warm")
	rest, target, err := parseArtifactTarget(t.TempDir(), []string{
		"@putnami/go", "--platform", "linux/amd64", "--latest", "--dest=" + dest,
	})
	if err != nil {
		t.Fatalf("parseArtifactTarget: %v", err)
	}
	if got, want := strings.Join(rest, " "), "@putnami/go --latest"; got != want {
		t.Errorf("rest = %q, want %q", got, want)
	}
	if target.OS != "linux" || target.Arch != "amd64" {
		t.Errorf("target platform = %s/%s, want linux/amd64", target.OS, target.Arch)
	}
	if target.StoreRoot != dest {
		t.Errorf("target store root = %q, want %q", target.StoreRoot, dest)
	}
}

func TestParseArtifactTarget_RelativeDestBecomesAbsolute(t *testing.T) {
	_, target, err := parseArtifactTarget(t.TempDir(), []string{"--dest", filepath.Join(".", "warm")})
	if err != nil {
		t.Fatalf("parseArtifactTarget: %v", err)
	}
	if !filepath.IsAbs(target.StoreRoot) {
		t.Errorf("store root = %q, want an absolute path", target.StoreRoot)
	}
}

// TestParseArtifactTarget_RefusesTheMachineGlobalStore guards the finalize step:
// it deletes the store's advisory lock, staging and per-digest locks, which on a
// live store would break the cross-process lock identity GC depends on.
func TestParseArtifactTarget_RefusesTheMachineGlobalStore(t *testing.T) {
	global := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", global)
	ws := t.TempDir()

	if got := store.ResolveArtifactStoreRoot(ws); got != global {
		t.Fatalf("test setup: artifact root = %q, want %q", got, global)
	}
	_, _, err := parseArtifactTarget(ws, []string{"--dest", global})
	if err == nil {
		t.Fatal("--dest pointing at the machine-global store was accepted")
	}
	if !errors.Is(err, cmderr.ErrUsage) {
		t.Errorf("error %v is not classified as a usage error", err)
	}
}

// TestExtensionsInstallMaterializes covers the dispatcher's gate on regenerating
// the shared AI context — that file describes what THIS workspace runs.
func TestExtensionsInstallMaterializes(t *testing.T) {
	hostPlatform := lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--latest"}, false},
		{[]string{"@putnami/go"}, false},
		{[]string{"--platform", hostPlatform}, false},
		{[]string{"--platform", foreignPlatformArg()}, true},
		{[]string{"--platform=" + foreignPlatformArg()}, true},
		{[]string{"--dest", "/tmp/warm"}, true},
		// A malformed value reports false and lets the command itself explain.
		{[]string{"--platform", "nonsense"}, false},
	}
	for _, c := range cases {
		if got := ExtensionsInstallMaterializes(c.args); got != c.want {
			t.Errorf("ExtensionsInstallMaterializes(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}
