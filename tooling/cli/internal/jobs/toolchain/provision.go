package toolchain

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/cli/internal/jobs/machineprobe"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// provisionRemedy ends every error of a toolchain the core provisions.
const provisionRemedy = "run `putnami install`"

// ProvisionTarget is one pinned toolchain the core installs.
type ProvisionTarget struct {
	// Identity is the lock identity, such as "go".
	Identity string
	// Entry is the workspace lock's format 5 entry of Identity.
	Entry lockfile.LockEntry
	// Toolchain is the runtime toolchain, locked on Identity, of the
	// extension that answered the pin, or nil when it declares none.
	Toolchain *extensionproto.RuntimeToolchain
}

// ProvisionOptions configures Provision.
type ProvisionOptions struct {
	// Platform is the "os/arch" key of the archive to install. Empty means
	// the host.
	Platform string
	// UserAgent, when set, is sent with the download.
	UserAgent string
	// Client performs the download. Nil uses pinnedarchive's default client.
	Client *http.Client
}

// ProvisionedFolder is the directory the archive of version of the toolchain
// identity is unpacked into: <PutnamiHome>/toolchains/<identity>/<identity>-<version>.
// identity and version are ones ProvisionedExecutable accepts.
func ProvisionedFolder(workspaceRoot, identity, version string) string {
	return provisionedFolder(PutnamiHome(workspaceRoot, os.Environ()), identity, version)
}

func provisionedFolder(home, identity, version string) string {
	return filepath.Join(home, "toolchains", identity, identity+"-"+version)
}

// ProvisionedExecutable returns the path of the executable of entry's archive
// for platform ("os/arch"; empty means the host) in the provisioned folder of
// identity: the path a provisioned candidate of a toolchain locked on identity
// resolves to. It reads no file. An entry below lock format 5 records no
// archives, so it has no such path.
func ProvisionedExecutable(workspaceRoot, identity string, entry lockfile.LockEntry, platform string) (string, error) {
	if platform == "" {
		platform = machineprobe.HostPlatform()
	}
	archive, err := provisionedArchive(identity, entry, platform)
	if err != nil {
		return "", provisionError(identity, entry.Version, platform, err)
	}
	return filepath.Join(ProvisionedFolder(workspaceRoot, identity, entry.Version), filepath.FromSlash(archive.Executable)), nil
}

// Provision makes the pinned release of target available for the platform,
// and returns the path of its executable and whether this call installed it.
//
// On the host platform, a candidate of target.Toolchain other than the
// provisioned one that answers the pinned version, found and probed as a job
// finds and probes it, is the release: Provision returns it and installs
// nothing. Otherwise the provisioned folder holds the release. A complete
// folder is returned with no network access. Otherwise the archive the entry
// pins for the platform is downloaded, refused unless its SHA-256 is the
// entry's integrity for the platform, and unpacked with pinnedarchive.Install
// into a staging directory that replaces the folder. Each top-level entry of
// the folder the archive does not hold, such as the files of another layout
// of the same release, moves into the staging directory once it is complete,
// so the folder keeps it; on a name the archive holds, the archive's entry
// wins.
//
// A folder is complete when the archive's executable in it is a regular
// executable file and, on the host platform, the probe of target.Toolchain,
// when there is one, answers the pinned version from it. The executable of
// another platform is never run.
func Provision(ctx context.Context, workspaceRoot string, target ProvisionTarget, opts ProvisionOptions) (executable string, installed bool, err error) {
	host := machineprobe.HostPlatform()
	platform := opts.Platform
	if platform == "" {
		platform = host
	}
	version := target.Entry.Version
	if target.Toolchain != nil && target.Toolchain.Lock != target.Identity {
		return "", false, provisionError(target.Identity, version, platform,
			fmt.Errorf("the runtime toolchain locks %q", target.Toolchain.Lock))
	}
	archive, err := provisionedArchive(target.Identity, target.Entry, platform)
	if err != nil {
		return "", false, provisionError(target.Identity, version, platform, err)
	}
	digest := target.Entry.Integrities[platform]
	if strings.TrimSpace(digest) == "" {
		return "", false, provisionError(target.Identity, version, platform,
			fmt.Errorf("the lock pin records no integrity for %s", platform))
	}
	base := os.Environ()
	var probe *extensionproto.RuntimeToolchainProbe
	if platform == host && target.Toolchain != nil {
		if held, found := heldRelease(ctx, workspaceRoot, *target.Toolchain, target.Entry, base); found {
			return held, false, nil
		}
		probe = &target.Toolchain.Probe
	}
	folder := ProvisionedFolder(workspaceRoot, target.Identity, version)
	rel := filepath.FromSlash(archive.Executable)
	complete := func(dir string) bool {
		return provisionedInstallComplete(ctx, filepath.Join(dir, rel), version, probe, base)
	}
	if complete(folder) {
		return filepath.Join(folder, rel), false, nil
	}
	pin := pinnedarchive.Pin{URL: archive.URL, SHA256: digest, Format: pinnedarchive.Format(archive.Format)}
	installed, err = pinnedarchive.Install(ctx, pin, folder, pinnedarchive.Options{
		Client:    opts.Client,
		UserAgent: opts.UserAgent,
		Limits:    pinnedarchive.DefaultLimits(),
		Complete:  complete,
		Prepare: func(stage string) error {
			if !complete(stage) {
				return errors.New("its executable is not the pinned release")
			}
			return keepFolderEntries(folder, stage)
		},
	})
	if err != nil {
		return "", false, provisionError(target.Identity, version, platform, err)
	}
	return filepath.Join(folder, rel), installed, nil
}

// keepFolderEntries moves into stage each top-level entry of folder that stage
// does not hold. A missing folder, or one that is not a directory, holds
// nothing to keep. When a move fails, the entries already moved go back to
// folder.
func keepFolderEntries(folder, stage string) error {
	info, err := os.Lstat(folder)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.IsDir() {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		return err
	}
	var moved []string
	for _, entry := range entries {
		name := entry.Name()
		if _, err := os.Lstat(filepath.Join(stage, name)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, restoreFolderEntries(folder, stage, moved))
		}
		if err := robustio.Rename(filepath.Join(folder, name), filepath.Join(stage, name)); err != nil {
			return errors.Join(fmt.Errorf("keep %s: %w", name, err), restoreFolderEntries(folder, stage, moved))
		}
		moved = append(moved, name)
	}
	return nil
}

// restoreFolderEntries moves the entries names back from stage to folder.
func restoreFolderEntries(folder, stage string, names []string) error {
	var errs []error
	for _, name := range names {
		if err := robustio.Rename(filepath.Join(stage, name), filepath.Join(folder, name)); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// heldRelease returns the executable of the first candidate of toolchain
// other than a provisioned one that answers the version entry pins, resolved
// against base as a job resolves it, and whether one does.
func heldRelease(ctx context.Context, workspaceRoot string, toolchain extensionproto.RuntimeToolchain, entry lockfile.LockEntry, base []string) (string, bool) {
	toolchain.Candidates = slices.DeleteFunc(slices.Clone(toolchain.Candidates), func(candidate extensionproto.RuntimeToolchainCandidate) bool {
		return candidate.From == extensionproto.RuntimeToolchainCandidateProvisioned
	})
	if len(toolchain.Candidates) == 0 {
		return "", false
	}
	resolution, _, found := resolveRuntimeToolchain(ctx, workspaceRoot, toolchain, entry, base)
	return resolution.Executable, found
}

// provisionedInstallComplete reports whether executable is a regular
// executable file that, when probe is set, answers version.
func provisionedInstallComplete(ctx context.Context, executable, version string, probe *extensionproto.RuntimeToolchainProbe, base []string) bool {
	if !runtimeExecutable(executable) {
		return false
	}
	if probe == nil {
		return true
	}
	got, want, err := probeRuntimeToolchainVersion(ctx, executable, *probe, version, base)
	return err == nil && got == want
}

// provisionedArchive returns entry's archive for platform, the archive a
// provisioned candidate of a toolchain locked on identity names. It fails
// when identity and version do not name one directory, when entry records no
// archives, as an entry below lock format 5 does, when it records none for
// platform, and when the archive's executable is not a path inside the
// archive.
func provisionedArchive(identity string, entry lockfile.LockEntry, platform string) (lockfile.LockArchive, error) {
	switch {
	case !provisionedPathElement(identity):
		return lockfile.LockArchive{}, fmt.Errorf("lock identity %q does not name a directory", identity)
	case strings.TrimSpace(entry.Version) == "":
		return lockfile.LockArchive{}, errors.New("the lock pin records no version")
	case !provisionedPathElement(identity + "-" + entry.Version):
		return lockfile.LockArchive{}, fmt.Errorf("version %q does not name a directory", entry.Version)
	case len(entry.Archives) == 0:
		return lockfile.LockArchive{}, errors.New("the lock pin records no archives (lock format below 5)")
	}
	archive, ok := entry.Archives[platform]
	if !ok {
		return lockfile.LockArchive{}, fmt.Errorf("the lock pin records no archive for %s", platform)
	}
	if strings.Contains(archive.Executable, `\`) || !filepath.IsLocal(filepath.FromSlash(archive.Executable)) {
		return lockfile.LockArchive{}, fmt.Errorf("archive executable %q is not a path inside the archive", archive.Executable)
	}
	return archive, nil
}

// provisionedPathElement reports whether name is one directory name that
// stays below its parent.
func provisionedPathElement(name string) bool {
	return !strings.ContainsAny(name, `/\`) && filepath.IsLocal(name)
}

// provisionError names the toolchain, its version and the platform beside
// cause, and the remedy.
func provisionError(identity, version, platform string, cause error) error {
	return fmt.Errorf("toolchain %q version %q for %s: %w; %s", identity, version, platform, cause, provisionRemedy)
}
