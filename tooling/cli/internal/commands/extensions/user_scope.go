package extensions

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// UserScopeFlag selects the user scope on `putnami extensions install|list|remove`.
const UserScopeFlag = "--user"

// userScopeLockFilename serializes every writer of one user scope. The lock
// file and the stable links are shared by every shell of the person, so two
// concurrent `install --user` runs must not interleave their read-modify-write
// of putnami.lock.json.
const userScopeLockFilename = "user-scope.lock"

// HasUserScopeFlag reports whether an `extensions` argument list selects the
// user scope. Arguments after a `--` separator are not flags.
func HasUserScopeFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == UserScopeFlag {
			return true
		}
	}
	return false
}

// ExtensionsInstallUser pins one registry extension in the user scope rooted
// at userRoot (extension.ResolveUserScopeRoot), so its commands run outside any
// workspace. args is the `extensions install` argument list, --user included.
//
// It shares the workspace install path: the same download, the same archive
// verification (a digest the registry advertises or the lock records, and the
// PUTNAMI_UNSAFE_INSTALL rule), the same lock entry and the same stable link,
// written under userRoot instead of a workspace root. It reads and writes
// nothing in the current directory, runs no extension install hook, and a
// re-run with the same argument keeps the recorded pin.
func ExtensionsInstallUser(ctx context.Context, userRoot string, args []string, outputFormat string, out io.Writer) error {
	rest, name, err := userScopeArgs("install", args, true)
	if err != nil {
		return err
	}
	ref, _ := parseExtensionArg(name)
	if !shared.IsRegistryArtifactRef(ref) {
		return cmderr.Usagef("the user scope pins registry extensions: expected @scope/name[@version], got %q", name)
	}
	release, err := lockUserScope(userRoot)
	if err != nil {
		return err
	}
	defer release()
	// A credential helper this install spawns is a nested putnami outside any
	// workspace. It must load the user scope without repairing it, or a pin it
	// cannot download would fork one CLI per nesting level.
	extension.ClaimUserScopeRepair(userRoot)

	installer := extension.NewUserScopeInstaller(userRoot)
	ops := extensionOpsFor(userRoot, installer)
	// A local path belongs to a workspace; the user scope has none to resolve it
	// against, and resolving it from the current directory would read it.
	ops.installLocal = nil
	return installArtifactsWithOptions(ctx, userRoot, &wsproto.Config{}, rest, ops, InstallOptions{
		OutputFormat: outputFormat,
		Out:          out,
	})
}

// ExtensionsRemoveUser drops one extension from the user scope: its lock entry,
// then its stable link. The extension tree stays in the artifact store, whose
// garbage collection reclaims it.
func ExtensionsRemoveUser(userRoot string, args []string, out io.Writer) error {
	_, name, err := userScopeArgs("remove", args, true)
	if err != nil {
		return err
	}
	release, err := lockUserScope(userRoot)
	if err != nil {
		return err
	}
	defer release()

	lf, err := lockfile.ReadLockFile(userRoot)
	if err != nil {
		return fmt.Errorf("read the user-scope lock: %w", err)
	}
	if lf == nil {
		return fmt.Errorf("extension %s is not installed in the user scope", name)
	}
	if _, ok := lf.GetExtension(name); !ok {
		return fmt.Errorf("extension %s is not installed in the user scope", name)
	}
	lf.RemoveExtension(name)
	if _, err := lockfile.WriteLockFileIfChanged(userRoot, lf); err != nil {
		return fmt.Errorf("write the user-scope lock: %w", err)
	}
	if err := extension.NewUserScopeInstaller(userRoot).Remove(name, ""); err != nil {
		return fmt.Errorf("remove extension: %w", err)
	}
	iox.Fprintf(out, "  ✓ Removed %s from the user scope\n", name)
	return nil
}

// ExtensionsListUser lists the extensions the user scope pins, from its lock.
func ExtensionsListUser(userRoot string, args []string, outputFormat string, out io.Writer) error {
	if _, _, err := userScopeArgs("list", args, false); err != nil {
		return err
	}
	lf, err := lockfile.ReadLockFile(userRoot)
	if err != nil {
		return fmt.Errorf("read the user-scope lock: %w", err)
	}
	var entries []artifactListEntry
	if lf != nil {
		for _, name := range sortedLockNames(lf.Extensions) {
			entries = append(entries, artifactListEntry{Name: name, Installed: lf.Extensions[name].Version, Source: "user"})
		}
	}
	if outputFormat == "jsonl" {
		return printArtifactListJSONL(entries)
	}
	iox.Fprintln(out)
	iox.Fprintf(out, "  %-35s %-15s %s\n", "EXTENSION", "INSTALLED", "SOURCE")
	iox.Fprintf(out, "  %-35s %-15s %s\n", "---------", "---------", "------")
	for _, entry := range entries {
		iox.Fprintf(out, "  %-35s %-15s %s\n", entry.Name, entry.Installed, entry.Source)
	}
	iox.Fprintln(out)
	return nil
}

// userScopeArgs removes --user from args and returns the rest together with
// the one positional argument. Materialization flags are rejected: they
// produce a tree for another machine, and the user scope installs for this
// one.
func userScopeArgs(verb string, args []string, needName bool) ([]string, string, error) {
	rest := make([]string, 0, len(args))
	var name string
	for _, arg := range args {
		flag, _, _ := strings.Cut(arg, "=")
		switch {
		case arg == UserScopeFlag:
			continue
		case flag == platformFlag || flag == destFlag:
			return nil, "", cmderr.Usagef("%s does not apply to the user scope: it materializes for another machine, and %s installs for this one",
				flag, UserScopeFlag)
		case !strings.HasPrefix(arg, "-") && name == "":
			name = arg
		case !strings.HasPrefix(arg, "-"):
			return nil, "", cmderr.Usagef("extensions %s %s takes one extension, got %q and %q", verb, UserScopeFlag, name, arg)
		}
		rest = append(rest, arg)
	}
	if needName && name == "" {
		return nil, "", cmderr.Usagef("extensions %s %s requires an extension: putnami extensions %s %s <@scope/name>",
			verb, UserScopeFlag, verb, UserScopeFlag)
	}
	if !needName && name != "" {
		return nil, "", cmderr.Usagef("extensions %s %s takes no extension, got %q", verb, UserScopeFlag, name)
	}
	return rest, name, nil
}

// lockUserScope takes the exclusive writer lock of the user scope, creating
// the scope directory on first use. The returned function releases it.
func lockUserScope(userRoot string) (func(), error) {
	dir := filepath.Join(userRoot, ".putnami")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create the user scope: %w", err)
	}
	lock, err := flock.Acquire(filepath.Join(dir, userScopeLockFilename), true, false)
	if err != nil {
		return nil, fmt.Errorf("lock the user scope: %w", err)
	}
	return func() { _ = lock.Release() }, nil
}

func sortedLockNames(entries map[string]lockfile.LockEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
