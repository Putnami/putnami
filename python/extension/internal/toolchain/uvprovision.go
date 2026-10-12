package toolchain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/pinnedarchive"
)

// UVMode says whether selecting a uv may install one.
type UVMode int

const (
	// UVInstall selects a uv that reports the wanted release and installs the
	// release under the Putnami home when the machine holds none.
	UVInstall UVMode = iota
	// UVFind selects a uv the machine already holds and installs nothing: a
	// hosted run downloads no toolchain.
	UVFind
)

// UVRequest describes the uv a provisioning job needs.
type UVRequest struct {
	// WorkspaceRoot is the workspace whose lock and root pyproject.toml say
	// which release that is.
	WorkspaceRoot string
	// Mode says whether a release may be installed.
	Mode UVMode
	// UserAgent is sent with a download.
	UserAgent string
	// Log receives progress lines. Nil discards them.
	Log func(level, message string)
}

// uvArchiveLimits bound a uv archive: about 20 MB compressed and 60 MB
// extracted, in one directory of two or three programs.
var uvArchiveLimits = pinnedarchive.Limits{ArchiveBytes: 256 << 20, ExtractedBytes: 512 << 20, Entries: 64}

// ProvisionUV selects the uv the workspace-install job runs, and installs it
// when the request allows it and the machine holds none that reports the
// wanted release.
//
// The release is the one the workspace lock pins, else the one the root
// pyproject.toml declares as [tool.uv] required-version, else
// DefaultUVVersion. A required-version that names no single release, and a
// release older than uvtoolchain.MinimumVersion, fail before anything is
// installed. The candidates are those of ResolveUV: uv on PATH, then
// toolchains/uv/uv-<version>/uv under the Putnami home. Nothing is downloaded
// when one of them reports the release.
//
// Otherwise UVInstall downloads the release into that directory: the pinned
// one from the lock's source, checked against the lock's SHA-256 for the host
// platform, or the default one, checked against the digest the extension
// ships. An archive whose SHA-256 is missing or differs is refused before
// anything is extracted. A release that is declared and not pinned yet has no
// digest to check its download against, unless it is the default one, and is
// refused: an explicit `putnami install` pins the declared release before the
// installers run.
func ProvisionUV(ctx context.Context, req UVRequest) (UV, error) {
	return provisionUV(ctx, req, currentUVHost())
}

func provisionUV(ctx context.Context, req UVRequest, host uvHost) (UV, error) {
	want, err := wantedUV(req.WorkspaceRoot)
	if err != nil {
		return UV{}, err
	}
	search := newUVSearch(host, req.WorkspaceRoot, want)
	if uv, ok := search.find(); ok {
		return uv, nil
	}
	if want.release.Version == "" {
		return UV{}, fmt.Errorf("%s declares uv %s and %s does not pin it yet, so no SHA-256 checks its download: "+
			"run `putnami install`, which pins the declared release before it installs it%s",
			want.origin, want.version, WorkspaceLockFile, search.seen())
	}
	if req.Mode != UVInstall {
		return UV{}, fmt.Errorf("no uv %s on this machine, and a hosted run installs no toolchain: "+
			"put uv %s on the runner's PATH, or install it at %s with `putnami install`%s",
			want.version, want.version, search.dir, search.seen())
	}
	if err := search.install(ctx, req); err != nil {
		return UV{}, err
	}
	program := search.program()
	got, err := host.version(program)
	if err != nil {
		return UV{}, fmt.Errorf("the uv %s at %s does not run on this machine (%w): remove %s, then run `putnami install`",
			want.version, program, err, search.dir)
	}
	if got != want.version {
		return UV{}, fmt.Errorf("%s holds uv %s, not uv %s: remove it, then run `putnami install`", search.dir, got, want.version)
	}
	return UV{Path: program, Version: got}, nil
}

// install makes the search's directory hold the wanted uv release, with its
// programs at the top of the directory, from the archive the release names
// for the host platform. Each error names the release, the archive without
// the credentials of its URL, and the command to run next.
func (s *uvSearch) install(ctx context.Context, req UVRequest) error {
	version, release := s.want.version, s.want.release
	platform := s.host.goos + "/" + s.host.goarch
	// The lock is the authority for a release it pins; the extension is for
	// its default release.
	pinnedBy, check := "the @putnami/python extension", "run `putnami install` again from a network that serves the published archive"
	if s.want.origin == WorkspaceLockFile {
		pinnedBy, check = WorkspaceLockFile, "check toolchains.uv in "+WorkspaceLockFile+", then run `putnami install`"
	}
	archive, ok := uvArchiveFor(release, s.host.goos, s.host.goarch)
	if !ok {
		return fmt.Errorf("cannot install uv %s: uv publishes no archive for %s: "+
			"put a uv that reports %s on PATH, then run `putnami install`%s", version, platform, version, s.seen())
	}
	if strings.TrimSpace(archive.pin.SHA256) == "" {
		return fmt.Errorf("refusing to download uv %s: %s records no SHA-256 for %s: %s%s",
			version, pinnedBy, platform, check, s.seen())
	}
	pin, client, err := pinnedarchive.WithoutCredentials(archive.pin)
	if err != nil {
		return fmt.Errorf("refusing to download uv %s: the source %s records for it is not a valid URL: %s", version, pinnedBy, check)
	}
	if client == nil {
		client = s.host.client
	}

	programs := uvPrograms(s.host.goos)
	s.log(req, "info", "Installing uv "+version+" from "+pin.URL+" under "+s.dir+"...")
	installed, err := pinnedarchive.Install(ctx, pin, s.dir, pinnedarchive.Options{
		Client:    client,
		UserAgent: req.UserAgent,
		Limits:    uvArchiveLimits,
		Complete:  func(dir string) bool { return isRegularFile(filepath.Join(dir, programs[0])) },
		Prepare:   func(stage string) error { return shapeUVInstall(stage, archive.dir, programs) },
	})
	switch {
	case err == nil && installed:
		s.log(req, "info", "uv "+version+" installed")
		return nil
	case err == nil:
		// The directory was complete already, or another process published
		// the release first. The caller checks the release it reports.
		s.log(req, "info", "Kept the existing uv install under "+s.dir)
		return nil
	case errors.Is(err, pinnedarchive.ErrNoDigest):
		return fmt.Errorf("refusing to download uv %s from %s: the %s SHA-256 that %s records is not valid (%w): %s",
			version, pin.URL, platform, pinnedBy, err, check)
	case errors.Is(err, pinnedarchive.ErrDigestMismatch):
		return fmt.Errorf("refusing uv %s: %w, the %s digest that %s records: %s", version, err, platform, pinnedBy, check)
	case errors.Is(err, pinnedarchive.ErrIncomplete), errors.Is(err, pinnedarchive.ErrUnsafeEntry), errors.Is(err, pinnedarchive.ErrTooLarge):
		return fmt.Errorf("refusing uv %s from %s: %w: %s", version, pin.URL, err, check)
	default:
		// The cause names the archive URL.
		return fmt.Errorf("no uv %s on this machine, and its download failed (%w): "+
			"run `putnami install` again when that address is reachable", version, err)
	}
}

func (s *uvSearch) log(req UVRequest, level, message string) {
	if req.Log != nil {
		req.Log(level, message)
	}
}

// shapeUVInstall turns an extracted uv archive into an install that holds the
// programs at its top level. A tar.gz archive holds them in the directory
// archiveDir, which is moved up and then removed; a zip archive, whose
// archiveDir is "", already holds them at its top level. The uv program must
// be there. Every program is left executable.
func shapeUVInstall(stage, archiveDir string, programs []string) error {
	if archiveDir != "" {
		nested := filepath.Join(stage, archiveDir)
		entries, err := os.ReadDir(nested)
		if err != nil {
			return fmt.Errorf("the archive holds no %s directory", archiveDir)
		}
		for _, entry := range entries {
			if err := os.Rename(filepath.Join(nested, entry.Name()), filepath.Join(stage, entry.Name())); err != nil {
				return err
			}
		}
		if err := os.Remove(nested); err != nil {
			return err
		}
	}
	for i, program := range programs {
		path := filepath.Join(stage, program)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			if i == 0 {
				return fmt.Errorf("the archive holds no %s program", program)
			}
			continue
		}
		if err := os.Chmod(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}
