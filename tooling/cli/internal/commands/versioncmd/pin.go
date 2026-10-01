package versioncmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/clibin"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/store"
)

// CLIPin implements `putnami pin`: pin, show, or remove the workspace's CLI
// version pin in putnami.lock.json.
//
//   - `putnami pin <version>` downloads that version's binary for the current
//     platform (and every platform in defaultPinPlatforms, plus any platform a
//     previous pin already carried) from the same endpoint the launcher uses,
//     records each one's SHA-256 as the pin (so the launcher resolves and
//     verifies the exact binary on any of those machines), and warms the
//     machine-global store for the current platform.
//   - `putnami pin` prints the current pin.
//   - `putnami pin --remove` clears it.
func CLIPin(ctx context.Context, wsRoot string, args []string) error {
	return CLIPinForVersion(ctx, wsRoot, args, "")
}

// defaultPinPlatforms are the platforms `putnami pin` records without being
// asked: the archive matrix the release pipeline builds for the CLI, which is
// the extension SDK's distribution matrix (pkgmeta.ArchivePlatforms). A
// platform outside it (e.g. windows/arm64, or a future arch) still needs one
// explicit `putnami pin <version>` run from that machine.
var defaultPinPlatforms = distributionPinPlatforms()

func distributionPinPlatforms() []string {
	platforms := pkgmeta.ArchivePlatforms()
	keys := make([]string, 0, len(platforms))
	for _, platform := range platforms {
		keys = append(keys, lockfile.PlatformKey(platform.GOOS, platform.GOARCH))
	}
	return keys
}

// CLIPinForVersion is CLIPin with the running binary version supplied by the
// CLI adapter. Keeping the value explicit prevents a lock from claiming that
// an arbitrary older target emits this binary's machine protocol.
func CLIPinForVersion(ctx context.Context, wsRoot string, args []string, runningVersion string) error {
	version, remove, err := parsePinArgs(args)
	if err != nil {
		return err
	}
	switch {
	case remove:
		return removeCLIPin(wsRoot)
	case version == "":
		return showCLIPin(wsRoot)
	default:
		return setCLIPin(ctx, wsRoot, version, runningVersion)
	}
}

func parsePinArgs(args []string) (version string, remove bool, err error) {
	for _, a := range args {
		switch {
		case a == "--remove":
			remove = true
		case strings.HasPrefix(a, "-"):
			return "", false, cmderr.Usagef("pin: unknown flag %q", a)
		case version != "":
			return "", false, cmderr.Usagef("pin: unexpected argument %q", a)
		default:
			version = a
		}
	}
	if remove && version != "" {
		return "", false, cmderr.Usagef("pin: --remove takes no version")
	}
	return version, remove, nil
}

func setCLIPin(ctx context.Context, wsRoot, version, runningVersion string) error {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		return cmderr.Usagef("pin: a version is required, e.g. `putnami pin 1.2.3`")
	}
	goos, goarch := runtime.GOOS, runtime.GOARCH
	localPlatform := lockfile.PlatformKey(goos, goarch)

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("pin: locate running putnami executable: %w", err)
	}
	resolver := clibin.NewWorkspace(artifactstore.New(store.ResolveArtifactStoreRoot(wsRoot)), wsRoot, executable)
	sha, err := resolver.Pin(ctx, version, goos, goarch)
	if err != nil {
		return fmt.Errorf("pin: %w", err)
	}

	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return fmt.Errorf("pin: read lock: %w", err)
	}
	if lf == nil {
		lf = lockfile.NewLockFile()
	}
	previous, hadPrevious := lf.GetCLI()
	// A source-workspace sentinel is not a previous PIN: it records no version
	// and no digests, so carrying it forward would seed the new entry with a
	// meaningless source and an empty version. Replacing it is exactly what
	// `putnami pin <version>` is for — the deliberate way out of self-hosting —
	// so drop it and start from a clean entry.
	if hadPrevious && previous.IsWorkspaceSource() {
		previous = lockfile.LockEntry{}
		hadPrevious = false
	}
	sameVersion := hadPrevious && previous.Version == version

	entry := lockfile.LockEntry{Version: version, Source: resolver.DownloadURL(version, goos, goarch)}
	if sameVersion {
		// The same version keeps every digest an earlier pin already verified —
		// archives are immutable per version, so nothing here needs re-fetching —
		// and this call only has to fill gaps (a new platform, or one dropped by
		// a prior interrupted run). A DIFFERENT version starts fresh: a
		// LockEntry's per-platform digests all describe one version's bytes, so
		// they cannot be carried over as-is (see the fetch loop below, which
		// re-verifies every platform the previous entry carried against the NEW
		// version instead of silently keeping stale digests).
		entry = previous
		entry.Version = version
		entry.Source = resolver.DownloadURL(version, goos, goarch)
	}
	entry.SetPlatformIntegrity(localPlatform, sha)

	// Record every platform putnami publishes by default, plus any platform the
	// previous pin carried (even outside that default set) — so re-pinning a new
	// version from one machine never silently drops coverage another platform's
	// launcher depends on. A platform the new version does not publish is
	// dropped, but only after trying, and only with a stderr note naming it.
	for _, platform := range pinTargetPlatforms(previous, hadPrevious, localPlatform) {
		if platform == localPlatform {
			continue // already fetched above; its failure aborts the whole pin
		}
		platformOS, platformArch, ok := lockfile.SplitPlatformKey(platform)
		if !ok {
			continue
		}
		if sameVersion && entry.IntegrityFor(platformOS, platformArch) != "" {
			continue // already verified for this exact version
		}
		platformSHA, err := resolver.Pin(ctx, version, platformOS, platformArch)
		if err != nil {
			if hadPrevious && previous.IntegrityFor(platformOS, platformArch) != "" {
				iox.Fprintf(os.Stderr,
					"pin: %s does not publish %s; dropping the digest the previous pin (%s) recorded for it: %v\n",
					version, platform, previous.Version, err)
			} else {
				slog.Debug("pin: platform not available", "platform", platform, "version", version, "error", err)
			}
			continue
		}
		entry.SetPlatformIntegrity(platform, platformSHA)
	}

	if sameCLIVersion(version, runningVersion) {
		entry.ProtocolVersion = protocolcli.ResultProtocolVersion
		if lf.Version < lockfile.FormatVersionV3 {
			lf.Version = lockfile.FormatVersionV3
		}
	}
	lf.SetCLI(entry)
	if err := refreshToolchainLock(ctx, wsRoot, lf); err != nil {
		// Pin is the recovery hatch for a broken workspace CLI and must not be
		// disabled by an unrelated vendor checksum endpoint. Keep the previous
		// toolchain dimension intact; install remains the fail-closed refresh.
		slog.Debug("pin: refresh toolchains", "error", err)
	}
	if err := lockfile.WriteLockFile(wsRoot, lf); err != nil {
		return fmt.Errorf("pin: write lock: %w", err)
	}

	iox.Fprintf(os.Stdout, "Pinned putnami CLI to %s\n", version)
	printPlatformIntegrities(entry)
	return nil
}

// pinTargetPlatforms returns every platform `putnami pin` should try to
// verify for this invocation: the CLI's default published matrix
// (defaultPinPlatforms), plus any platform a previous pin already carried,
// with localPlatform first (it is fetched separately and must never be
// skipped). Order beyond that is deterministic so runs and their stderr
// warnings are reproducible.
func pinTargetPlatforms(previous lockfile.LockEntry, hadPrevious bool, localPlatform string) []string {
	seen := map[string]bool{localPlatform: true}
	platforms := []string{localPlatform}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			platforms = append(platforms, p)
		}
	}
	for _, p := range defaultPinPlatforms {
		add(p)
	}
	if hadPrevious {
		carried := make([]string, 0, len(previous.Integrities))
		for p := range previous.Integrities {
			carried = append(carried, p)
		}
		sort.Strings(carried)
		for _, p := range carried {
			add(p)
		}
	}
	return platforms
}

// printPlatformIntegrities prints one line per recorded platform digest,
// sorted for deterministic output. Shared by setCLIPin and showCLIPin so the
// two report the same lock state the same way.
func printPlatformIntegrities(entry lockfile.LockEntry) {
	plats := make([]string, 0, len(entry.Integrities))
	for p := range entry.Integrities {
		plats = append(plats, p)
	}
	sort.Strings(plats)
	for _, p := range plats {
		iox.Fprintf(os.Stdout, "  %s  %s\n", p, entry.Integrities[p])
	}
}

func showCLIPin(wsRoot string) error {
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return fmt.Errorf("pin: read lock: %w", err)
	}
	entry, ok := cliPin(lf)
	if !ok {
		iox.Fprintf(os.Stdout, "No CLI version pinned. Pin one with `putnami pin <version>`.\n")
		return nil
	}
	if entry.IsWorkspaceSource() {
		// Print the sentinel rather than an empty version. "pinned to " with
		// nothing after it would read as a broken lock; this workspace is not
		// unpinned, it is self-hosted.
		iox.Fprintf(os.Stdout,
			"putnami CLI is built from this workspace (%s: cli.source = %q).\n"+
				"Run commands through `./putnamiw`. Pin a published version with `putnami pin <version>`.\n",
			lockfile.LockFilename, lockfile.SourceWorkspace)
		return nil
	}
	iox.Fprintf(os.Stdout, "putnami CLI pinned to %s\n", entry.Version)
	printPlatformIntegrities(entry)
	return nil
}

func removeCLIPin(wsRoot string) error {
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return fmt.Errorf("pin: read lock: %w", err)
	}
	entry, ok := cliPin(lf)
	if !ok {
		iox.Fprintf(os.Stdout, "No CLI version pinned.\n")
		return nil
	}
	lf.RemoveCLI()
	if err := lockfile.WriteLockFile(wsRoot, lf); err != nil {
		return fmt.Errorf("pin: write lock: %w", err)
	}
	if entry.IsWorkspaceSource() {
		iox.Fprintf(os.Stdout,
			"Removed the source-workspace declaration (cli.source = %q). "+
				"The launcher no longer requires a CLI built from this tree.\n",
			lockfile.SourceWorkspace)
		return nil
	}
	iox.Fprintf(os.Stdout, "Removed the CLI version pin (was %s).\n", entry.Version)
	return nil
}

// cliPin returns the pinned CLI entry from a (possibly nil) lock file.
func cliPin(lf *lockfile.LockFile) (lockfile.LockEntry, bool) {
	if lf == nil {
		return lockfile.LockEntry{}, false
	}
	return lf.GetCLI()
}
