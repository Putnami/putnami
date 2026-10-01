package extension

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/store"
)

const userScopeTestExtension = "@acme/audit"

// userScopeManifest is a loadable manifest whose group runs outside a
// workspace: `audit` defaults to the interactive, workspace-optional `run`.
func userScopeManifest(version string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "cliContract": %d,
  "commands": {"audit-run": {"description": "Audit", "run": [{"id": "run", "task": "audit-exec"}]}},
  "commandGroups": {"audit": {"default": "run", "subcommands": {
    "run": {"command": "audit-run", "interactive": true, "workspace": "optional"}
  }}},
  "tasks": {"audit-exec": {"kind": "command", "command": "echo"}}
}`, userScopeTestExtension, version, protocolcli.CurrentContract)
}

func writeUserScopeLock(t *testing.T, userRoot string, entry lockfile.LockEntry) {
	t.Helper()
	lf := lockfile.NewLockFile()
	lf.SetExtension(userScopeTestExtension, entry)
	if err := os.MkdirAll(userRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.WriteLockFile(userRoot, lf); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverUserScopeExtensions_LoadsLockPinnedLinks(t *testing.T) {
	userRoot := t.TempDir()
	writeExtensionManifest(t, layout.StableDir(userRoot, layout.Extensions, userScopeTestExtension), userScopeManifest("1.0.0"))
	writeUserScopeLock(t, userRoot, lockfile.LockEntry{Version: "1.0.0"})

	result, err := DiscoverUserScopeExtensions(context.Background(), userRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 0 || len(result.Extensions) != 1 {
		t.Fatalf("result = %d extensions, skipped %v; want the one pinned extension", len(result.Extensions), result.Skipped)
	}
	if got := GroupDefaultSubcommand(result.Extensions, "audit"); got != "run" {
		t.Fatalf("group default = %q, want run", got)
	}
}

// TestDiscoverUserScopeExtensions_RepairsAMissingLink pins that the user scope
// heals through the workspace ensure path: the link is gone, the lock pins the
// archive digest, and discovery downloads, verifies and relinks it.
func TestDiscoverUserScopeExtensions_RepairsAMissingLink(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	archive := buildArchiveBytes(t, map[string]string{"putnami.extension.json": userScopeManifest("1.0.0")})
	digest := sha256Bytes(archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resolved-Version", "1.0.0")
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	userRoot := t.TempDir()
	writeUserScopeLock(t, userRoot, lockfile.LockEntry{
		Version:     "1.0.0",
		Integrities: map[string]string{currentPlatform(): digest},
	})
	installer := &Installer{WorkspaceRoot: userRoot, ResolverURL: srv.URL, HTTPClient: srv.Client()}

	result, err := DiscoverUserScopeExtensions(context.Background(), userRoot, installer)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Extensions) != 1 {
		t.Fatalf("result = %d extensions, skipped %v; want the repaired extension", len(result.Extensions), result.Skipped)
	}
	if _, err := os.Stat(filepath.Join(layout.StableDir(userRoot, layout.Extensions, userScopeTestExtension), "putnami.extension.json")); err != nil {
		t.Fatalf("the user-scope link was not repaired: %v", err)
	}
}

// TestDiscoverUserScopeExtensions_UnrepairableNamesInstallCommand pins the
// remediation: a pin that cannot be repaired is skipped with a reason naming
// the exact command that reinstalls it.
func TestDiscoverUserScopeExtensions_UnrepairableNamesInstallCommand(t *testing.T) {
	t.Setenv(UnsafeInstallEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	userRoot := t.TempDir()
	writeUserScopeLock(t, userRoot, lockfile.LockEntry{
		Version:     "1.0.0",
		Integrities: map[string]string{currentPlatform(): strings.Repeat("a", 64)},
	})
	installer := &Installer{WorkspaceRoot: userRoot, ResolverURL: srv.URL, HTTPClient: srv.Client()}

	result, err := DiscoverUserScopeExtensions(context.Background(), userRoot, installer)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Extensions) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("result = %d extensions, skipped %v; want one skip", len(result.Extensions), result.Skipped)
	}
	if want := "putnami extensions install --user --latest " + userScopeTestExtension; !strings.Contains(result.Skipped[0].Reason.Error(), want) {
		t.Fatalf("skip reason %q does not name %q", result.Skipped[0].Reason, want)
	}
	if want := "putnami extensions install --user " + userScopeTestExtension; !strings.Contains(result.Skipped[0].Reason.Error(), want) {
		t.Fatalf("skip reason %q does not name %q", result.Skipped[0].Reason, want)
	}
}

func TestDiscoverUserScopeExtensions_WithoutLockIsEmpty(t *testing.T) {
	result, err := DiscoverUserScopeExtensions(context.Background(), filepath.Join(t.TempDir(), "absent"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Extensions) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("result = %+v, want empty", result)
	}
}

func TestDiscoverUserScopeExtensions_UnreadableLockIsAnError(t *testing.T) {
	userRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(userRoot, lockfile.LockFilename), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverUserScopeExtensions(context.Background(), userRoot, nil); err == nil {
		t.Fatal("an unreadable user-scope lock must be reported")
	}
}

// TestNewUserScopeInstaller_UsesTheEnvironmentRegistry pins that the user
// scope resolves its registry without a workspace: the environment override,
// then the default.
func TestNewUserScopeInstaller_UsesTheEnvironmentRegistry(t *testing.T) {
	t.Setenv(PrivatePutRegistryURLEnv, "")
	t.Setenv(PutRegistryURLEnv, "https://registry.example.test/")
	if got := NewUserScopeInstaller(t.TempDir()).ResolverURL; got != "https://registry.example.test" {
		t.Fatalf("ResolverURL = %q, want the environment registry", got)
	}
	t.Setenv(PutRegistryURLEnv, "")
	if got := NewUserScopeInstaller(t.TempDir()).ResolverURL; got != DefaultPutRegistryURL {
		t.Fatalf("ResolverURL = %q, want the default registry", got)
	}
}

func TestGroupDefaultSubcommand(t *testing.T) {
	withDefault := func(name, def string) *ExtensionDescription {
		ext := sampleExtensionWithGroup()
		ext.Name = name
		group := ext.CommandGroups["cloud"]
		group.Default = def
		ext.CommandGroups["cloud"] = group
		return ext
	}
	for _, tc := range []struct {
		name string
		exts []*ExtensionDescription
		want string
	}{
		{"no default", []*ExtensionDescription{sampleExtensionWithGroup()}, ""},
		{"declared default", []*ExtensionDescription{withDefault("@putnami/cloud", "status")}, "status"},
		{"unresolvable default falls back to help", []*ExtensionDescription{withDefault("@putnami/cloud", "missing")}, ""},
		{"first owner declaring a default wins", []*ExtensionDescription{
			sampleExtensionWithGroup(), withDefault("@acme/a", "login"), withDefault("@acme/b", "status"),
		}, "login"},
		{"unknown group", []*ExtensionDescription{withDefault("@putnami/cloud", "status")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := "cloud"
			if tc.name == "unknown group" {
				group = "nope"
			}
			if got := GroupDefaultSubcommand(tc.exts, group); got != tc.want {
				t.Fatalf("GroupDefaultSubcommand = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveUserScopeRoot_UnderHome pins the user scope beside the
// machine-wide artifact store: ~/.putnami/user, whatever directory the command
// runs in.
func TestResolveUserScopeRoot_UnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PUTNAMI_ARTIFACT_DIR", "")

	got, err := ResolveUserScopeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".putnami", "user"); got != want {
		t.Errorf("ResolveUserScopeRoot = %q, want %q", got, want)
	}
	if filepath.Dir(got) != filepath.Dir(store.ResolveArtifactStoreRoot("/unused")) {
		t.Errorf("user scope %q is not beside the artifact store", got)
	}
}

// TestResolveUserScopeRoot_HomeUnavailable pins the failure: the user scope has
// no workspace to fall back to, so an unset home is an error, never a path
// relative to the current directory.
func TestResolveUserScopeRoot_HomeUnavailable(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	if got, err := ResolveUserScopeRoot(); err == nil {
		t.Fatalf("ResolveUserScopeRoot = %q with no home directory, want an error", got)
	}
}
