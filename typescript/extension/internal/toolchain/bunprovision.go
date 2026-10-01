package toolchain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
)

// BunMode says whether selecting a bun may install one.
type BunMode int

const (
	// BunInstall selects a bun that fits and installs the release under the
	// Putnami home when the host holds none.
	BunInstall BunMode = iota
	// BunFind selects a bun the machine already holds and installs nothing: a
	// hosted run downloads no toolchain.
	BunFind
)

// BunRequest describes the bun a provisioning job needs.
type BunRequest struct {
	// WorkspaceRoot is the workspace whose lock and root package.json say
	// which release that is.
	WorkspaceRoot string
	// Mode says whether a release may be installed.
	Mode BunMode
	// Version returns the version the bun at path reports with --version.
	Version func(path string) (string, error)
	// Refuse, when set, returns why the job must not run the bun at path, or
	// "" when it may. A refused bun is never started, not even to read its
	// version.
	Refuse func(path string) string
	// UserAgent is sent with a download.
	UserAgent string
	// Log receives progress lines. Nil discards them.
	Log func(level, message string)
}

// Bun is the bun a job runs.
type Bun struct {
	// Path is the bun program.
	Path string
	// Version is the release it runs. It is "" for a bun of the host that is
	// used as it is, because the workspace pins and declares no release.
	Version string
	// Managed reports whether Putnami installed it under the Putnami home.
	Managed bool
}

// bunArchiveLimits bound a Bun archive: about 40 MB compressed and 100 MB
// extracted, in one directory of one or two files.
var bunArchiveLimits = pinnedarchive.Limits{ArchiveBytes: 512 << 20, ExtractedBytes: 1 << 30, Entries: 64}

// ProvisionBun selects the bun a provisioning job runs, and installs it when
// the request allows it and the machine holds none that fits.
//
// The release is the one the workspace lock pins, else the one the root
// package.json declares in packageManager, else none. A pinned or declared
// release is exact, as it is for the tasks the CLI runs with the extension's
// task runtime: only a bun that reports that version qualifies.
//
// A bun of the host comes first (see hostBuns): the first one that reports the
// release, or the first one when no release is named. Then comes the install of
// the release under the Putnami home, toolchains/bun/bun-<version>/bin/bun,
// where the release is the extension's default one when none is named. Nothing
// is downloaded when either is found.
//
// Otherwise BunInstall downloads the release into that directory: the pinned
// one from the lock's source, checked against the lock's SHA-256 for the host
// platform, or the default one, checked against the digest the extension
// ships. An archive whose SHA-256 is missing or differs is refused before
// anything is extracted.
//
// A release that is declared and not pinned yet has no digest to check its
// download against, unless it is the default one. The job then runs with a bun
// of the host of another release, else with the default release, and logs a
// warning: the CLI pins the declared release when the install ends, and the
// next install installs it.
//
// A bun under the Putnami home runs with its own files under its install
// directory (see useManagedBun), so nothing is written to .bun in the user's
// home directory. A bun of the host keeps its own locations.
func ProvisionBun(ctx context.Context, req BunRequest) (Bun, error) {
	return provisionBun(ctx, req, currentBunHost())
}

func provisionBun(ctx context.Context, req BunRequest, host bunHost) (Bun, error) {
	want, err := wantedBun(req.WorkspaceRoot)
	if err != nil {
		return Bun{}, err
	}
	search := bunSearch{req: req, host: host, want: want}
	search.root = putnamihome.ToolchainRoot(host.lookup, req.WorkspaceRoot, bunToolchainName)

	if bun, ok := search.onHost(); ok {
		return search.selected(bun)
	}
	switch {
	case want.version == "":
		return search.managed(ctx, DefaultBunVersion, DefaultBunRelease())
	case want.release.Version != "":
		return search.managed(ctx, want.version, want.release)
	case IsPlainBunRelease(want.version) && isRegularFile(managedBunPath(managedBunDir(search.root, want.version), host.platform.goos)):
		return search.managed(ctx, want.version, BunRelease{})
	}

	// The release is declared and the lock does not pin it yet.
	unpinned := fmt.Sprintf("%s declares Bun %s and %s does not pin it yet, so no SHA-256 checks its download: ",
		want.origin, want.version, WorkspaceLockFile)
	again := ". Run `putnami install` again once the lock pins it: that install puts Bun " + want.version + " under the Putnami home"
	if search.fallback != "" {
		search.log("warn", unpinned+"this install runs with "+search.fallbackName+again)
		return search.selected(Bun{Path: search.fallback, Managed: managedBunDirOf(search.fallback) != ""})
	}
	search.log("warn", unpinned+"this install runs with Bun "+DefaultBunVersion+", the default release of the extension"+again)
	return search.managed(ctx, DefaultBunVersion, DefaultBunRelease())
}

// bunWant is the release a workspace asks for.
type bunWant struct {
	// version is the exact release, or "" when the workspace names none.
	version string
	// origin names the file that names version.
	origin string
	// release is what an install of the wanted release is verified with. Its
	// Version is "" when no digest is known for that release.
	release BunRelease
}

// wantedBun reads the release workspaceRoot asks for: the one its lock pins,
// else the one its root package.json declares, else none, which an install
// answers with the extension's default release.
func wantedBun(workspaceRoot string) (bunWant, error) {
	locked, pinned, err := lockedBunRelease(workspaceRoot)
	if err != nil {
		return bunWant{}, err
	}
	if pinned {
		return bunWant{version: locked.Version, origin: WorkspaceLockFile, release: locked}, nil
	}
	if declared := declaredBunVersion(workspaceRoot); declared != "" {
		want := bunWant{version: declared, origin: "package.json#packageManager"}
		if declared == DefaultBunVersion {
			want.release = DefaultBunRelease()
		}
		return want, nil
	}
	return bunWant{release: DefaultBunRelease()}, nil
}

// bunSearch is one selection of a bun, with what it saw on the way.
type bunSearch struct {
	req  BunRequest
	host bunHost
	want bunWant
	// root is the toolchains/bun directory of the Putnami home.
	root string
	// other lists the buns of the host that report another release.
	other []string
	// refused lists the buns the request refuses to run.
	refused []string
	// fallback is the first bun of the host that runs and reports another
	// release than the wanted one, and fallbackName describes it.
	fallback, fallbackName string
}

// onHost returns the first bun of the host that fits the wanted release.
func (s *bunSearch) onHost() (Bun, bool) {
	for _, candidate := range s.host.hostBuns() {
		if reason := s.refusal(candidate); reason != "" {
			s.refused = append(s.refused, candidate+" ("+reason+")")
			continue
		}
		managed := managedBunDirOf(candidate) != ""
		if s.want.version == "" {
			return Bun{Path: candidate, Managed: managed}, true
		}
		got, err := s.req.Version(candidate)
		if err != nil {
			s.other = append(s.other, candidate+" (reports no version: "+err.Error()+")")
			continue
		}
		if got == s.want.version {
			return Bun{Path: candidate, Version: got, Managed: managed}, true
		}
		s.other = append(s.other, candidate+" (Bun "+got+")")
		if s.fallback == "" {
			s.fallback, s.fallbackName = candidate, "Bun "+got+" at "+candidate
		}
	}
	return Bun{}, false
}

// managed returns the install of version under the Putnami home, which it
// downloads first when the directory holds none, the request allows it, and
// release verifies the archive.
func (s *bunSearch) managed(ctx context.Context, version string, release BunRelease) (Bun, error) {
	if !IsPlainBunRelease(version) {
		return Bun{}, fmt.Errorf("no bun on this machine reports %q, the version %s names, and no published Bun release has that version: "+
			"declare a release as bun@MAJOR.MINOR.PATCH in package.json#packageManager, then run `putnami install`%s",
			version, s.want.origin, s.seen())
	}
	dir := managedBunDir(s.root, version)
	program := managedBunPath(dir, s.host.platform.goos)
	if reason := s.refusal(program); reason != "" {
		return Bun{}, fmt.Errorf("no Bun %s that this job may run: it does not run %s, %s%s", version, program, reason, s.seen())
	}
	if !isRegularFile(program) {
		if s.req.Mode != BunInstall {
			return Bun{}, fmt.Errorf("no Bun %s on this machine, and a hosted run installs no toolchain: "+
				"put Bun %s on the runner's PATH, or install it at %s with `putnami install`%s", version, version, dir, s.seen())
		}
		if err := s.install(ctx, version, dir, release); err != nil {
			return Bun{}, err
		}
	}
	got, err := s.req.Version(program)
	if err != nil {
		return Bun{}, fmt.Errorf("the Bun %s at %s does not run on this machine (%w): remove %s, then run `putnami install`",
			version, program, err, dir)
	}
	if got != version {
		return Bun{}, fmt.Errorf("%s holds Bun %s, not Bun %s: remove it, then run `putnami install`", dir, got, version)
	}
	return s.selected(Bun{Path: program, Version: version, Managed: true})
}

// refusal returns why the request refuses to run the bun at path, or "".
func (s *bunSearch) refusal(path string) string {
	if s.req.Refuse == nil {
		return ""
	}
	return s.req.Refuse(path)
}

// seen describes the buns the search passed over, for the end of an error.
func (s *bunSearch) seen() string {
	var parts []string
	if len(s.other) > 0 {
		parts = append(parts, "found "+strings.Join(s.other, ", "))
	}
	if len(s.refused) > 0 {
		parts = append(parts, "not run: "+strings.Join(s.refused, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// selected returns bun as the job's bun, after pointing the environment of
// this process at it when Putnami installed it.
func (s *bunSearch) selected(bun Bun) (Bun, error) {
	if !bun.Managed {
		return bun, nil
	}
	if err := s.host.useManagedBun(managedBunDirOf(bun.Path)); err != nil {
		return Bun{}, err
	}
	return bun, nil
}

func (s *bunSearch) log(level, message string) {
	if s.req.Log != nil {
		s.req.Log(level, message)
	}
}

// install makes dir hold Bun version, with its program at bin/bun, from the
// archive release names for the host platform. Each error names the release,
// the archive without the credentials of its URL, and the command to run next.
func (s *bunSearch) install(ctx context.Context, version, dir string, release BunRelease) error {
	platform := s.host.platform.goos + "/" + s.host.platform.goarch
	// The lock is the authority for a release it pins; the extension is for
	// its default release.
	pinnedBy, check := "the extension", "run `putnami install` again from a network that serves the published archive"
	if s.want.origin == WorkspaceLockFile && release.Version == s.want.version {
		pinnedBy, check = WorkspaceLockFile, "check toolchains.bun in "+WorkspaceLockFile+", then run `putnami install`"
	}
	if s.host.platform.musl {
		return fmt.Errorf("cannot install Bun %s: this host uses the musl C library, and %s holds the digest of the glibc archive only: "+
			"put the musl build of Bun %s on PATH, then run `putnami install`%s", version, pinnedBy, version, s.seen())
	}
	archive, ok := bunArchiveFor(release, s.host.platform.goos, s.host.platform.goarch)
	if !ok {
		return fmt.Errorf("cannot install Bun %s: Bun publishes no archive for %s: "+
			"put a bun that reports %s on PATH, then run `putnami install`%s", version, platform, version, s.seen())
	}
	if strings.TrimSpace(archive.pin.SHA256) == "" {
		return fmt.Errorf("refusing to download Bun %s: %s records no SHA-256 for %s: "+
			"declare a Bun release that publishes this platform in package.json#packageManager, then run `putnami install`%s",
			version, pinnedBy, platform, s.seen())
	}
	pin, client, err := pinnedarchive.WithoutCredentials(archive.pin)
	if err != nil {
		return fmt.Errorf("refusing to download Bun %s: the source %s records for it is not a valid URL: %s", version, pinnedBy, check)
	}
	if client == nil {
		client = s.host.client
	}

	program := bunProgram(s.host.platform.goos)
	s.log("info", "Installing Bun "+version+" from "+pin.URL+" under "+dir+"...")
	_, err = pinnedarchive.Install(ctx, pin, dir, pinnedarchive.Options{
		Client:    client,
		UserAgent: s.req.UserAgent,
		Limits:    bunArchiveLimits,
		Complete:  func(dir string) bool { return isRegularFile(filepath.Join(dir, "bin", program)) },
		Prepare:   func(stage string) error { return shapeBunInstall(stage, archive.dir, program) },
	})
	switch {
	case err == nil:
		s.log("info", "Bun "+version+" installed")
		return nil
	case errors.Is(err, pinnedarchive.ErrNoDigest):
		return fmt.Errorf("refusing to download Bun %s from %s: the %s SHA-256 that %s records is not valid (%w): %s",
			version, pin.URL, platform, pinnedBy, err, check)
	case errors.Is(err, pinnedarchive.ErrDigestMismatch):
		return fmt.Errorf("refusing Bun %s: %w, the %s digest that %s records: %s", version, err, platform, pinnedBy, check)
	case errors.Is(err, pinnedarchive.ErrIncomplete), errors.Is(err, pinnedarchive.ErrUnsafeEntry), errors.Is(err, pinnedarchive.ErrTooLarge):
		return fmt.Errorf("refusing Bun %s from %s: %w: %s", version, pin.URL, err, check)
	default:
		// The cause names the archive URL.
		return fmt.Errorf("no Bun %s on this machine, and its download failed (%w): "+
			"run `putnami install` again when that address is reachable", version, err)
	}
}

// shapeBunInstall turns an extracted Bun archive into an install: the archive
// holds the program as <archiveDir>/<program>, and the install holds it as
// bin/<program>. Nothing else of the archive is kept.
func shapeBunInstall(stage, archiveDir, program string) error {
	extracted := filepath.Join(stage, archiveDir, program)
	info, err := os.Lstat(extracted)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("the archive holds no %s/%s", archiveDir, program)
	}
	bin := filepath.Join(stage, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		return err
	}
	installed := filepath.Join(bin, program)
	if err := os.Rename(extracted, installed); err != nil {
		return err
	}
	//nolint:gosec // G302: the installed file is a program, and every user of the machine may run it.
	if err := os.Chmod(installed, 0o755); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(stage, archiveDir))
}
