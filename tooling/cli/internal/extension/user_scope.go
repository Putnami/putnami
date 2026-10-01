package extension

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// The user scope is the set of extensions a person pins with `putnami
// extensions install --user` so their commands run outside any workspace. Its
// root (ResolveUserScopeRoot, ~/.putnami/user) has the layout of a
// workspace root without a workspace manifest: a putnami.lock.json in the
// workspace format and the stable links under layout.StableDir. The extension
// trees stay in the machine-global artifact store, so a tree a workspace
// already installed is linked, not downloaded again.
//
// Nothing here reads a workspace, and inside a workspace the workspace's own
// pins are the only ones. The one root the user scope can take over is a
// package.json with "workspaces" and no putnami.workspace.json, for a command
// group that root does not provide itself.

// ResolveUserScopeRoot returns ~/.putnami/user, the root of the user scope,
// beside the machine-wide artifact store under ~/.putnami. It fails when the
// home directory is unavailable, because a user scope has no workspace to fall
// back to and must never resolve relative to the current directory.
func ResolveUserScopeRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve the user scope: %w", err)
	}
	if home == "" {
		return "", errors.New("resolve the user scope: the home directory is not set")
	}
	return filepath.Join(home, ".putnami", "user"), nil
}

// NewUserScopeInstaller returns the installer for the user scope rooted at
// userRoot. It downloads and verifies archives through the same code path as a
// workspace installer. The registry comes from the environment or the default,
// never from a workspace manifest, because the user scope has none.
func NewUserScopeInstaller(userRoot string) *Installer {
	return &Installer{
		WorkspaceRoot: userRoot,
		ResolverURL:   ResolvePutRegistryURL(nil),
		HTTPClient:    NewRegistryHTTPClient(),
	}
}

// UserScopeRepairEnv names the user scope whose pins a putnami process is
// already repairing or installing. A nested putnami inherits it: the registry
// credential helper a download spawns, or an extension job that calls the CLI.
// Such a process loads the user scope without repairing it, so a pin that
// cannot be downloaded fails once instead of forking one CLI per nesting level.
const UserScopeRepairEnv = "PUTNAMI_USER_SCOPE_REPAIRED"

// ClaimUserScopeRepair reports whether this process owns the repair of the
// user scope at userRoot, and marks it owned for every process it spawns. It
// returns false when a parent process already owns it.
func ClaimUserScopeRepair(userRoot string) bool {
	if os.Getenv(UserScopeRepairEnv) == userRoot {
		return false
	}
	_ = os.Setenv(UserScopeRepairEnv, userRoot)
	return true
}

// UserScopeMayPin reports whether the user-scope lock at userRoot pins an
// extension, or cannot be read, so a caller that needs the user scope only when
// it pins something decides with one file read.
func UserScopeMayPin(userRoot string) bool {
	lf, err := lockfile.ReadLockFile(userRoot)
	return err != nil || (lf != nil && len(lf.Extensions) > 0)
}

// UserScopeInstallCommand is the command that pins, or repairs the pin of, a
// user-scope extension.
func UserScopeInstallCommand(name string) string {
	return "putnami extensions install --user " + name
}

// UserScopeLatestCommand is the command that moves the pin of a user-scope
// extension to its newest release, for a pinned version that no longer loads.
func UserScopeLatestCommand(name string) string {
	return "putnami extensions install --user --latest " + name
}

// DiscoverUserScopeExtensions loads every extension the user-scope lock pins,
// in name order. When installer is non-nil each pin is first repaired through
// the same ensure path a workspace uses (a missing or dangling link is
// relinked from the artifact store, or downloaded and verified again).
//
// An extension that cannot be repaired or loaded is returned in Skipped with a
// reason naming UserScopeInstallCommand. A lock that cannot be read is an
// error; an absent lock is an empty result.
func DiscoverUserScopeExtensions(ctx context.Context, userRoot string, installer *Installer) (*DiscoveryResult, error) {
	lf, err := lockfile.ReadLockFile(userRoot)
	if err != nil {
		return nil, fmt.Errorf("read the user-scope lock: %w", err)
	}
	result := &DiscoveryResult{}
	if lf == nil {
		return result, nil
	}
	names := make([]string, 0, len(lf.Extensions))
	for name := range lf.Extensions {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entry := lf.Extensions[name]
		if installer != nil {
			if err := installer.EnsureExtension(ctx, name, entry.Version, &entry); err != nil {
				result.Skipped = append(result.Skipped, userScopeSkip(name, entry.Version, err))
				continue
			}
		}
		ext, skip := tryLoadInstalledExtension(userRoot, name, lf)
		switch {
		case skip != nil:
			result.Skipped = append(result.Skipped, userScopeSkip(name, entry.Version, skip.Reason))
		case ext == nil:
			result.Skipped = append(result.Skipped, userScopeSkip(name, entry.Version,
				fmt.Errorf("no installed manifest at the user-scope link")))
		case ext.Name != name:
			result.Skipped = append(result.Skipped, userScopeSkip(name, entry.Version,
				fmt.Errorf("the installed manifest names %q", ext.Name)))
		default:
			result.Extensions = append(result.Extensions, ext)
		}
	}
	return result, nil
}

func userScopeSkip(name, version string, reason error) SkippedExtension {
	return SkippedExtension{
		Ref:     name,
		Name:    name,
		Version: version,
		Reason:  fmt.Errorf("user-scope extension %s cannot be loaded: %w; run `%s`, or `%s` to move the pin", name, reason, UserScopeInstallCommand(name), UserScopeLatestCommand(name)),
	}
}
