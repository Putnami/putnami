package toolchain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/uvtoolchain"
)

// WorkspaceLockFile is the workspace lock that pins the uv release the tasks
// run with, with one SHA-256 per platform.
const WorkspaceLockFile = "putnami.lock.json"

// RootPyproject is the workspace root manifest whose [tool.uv] table declares
// the uv release, as required-version.
const RootPyproject = uvtoolchain.Pyproject

// DefaultUVVersion is the uv release the extension pins for a workspace that
// pins and declares none (uvtoolchain.DefaultVersion): the lock pins it, the
// CLI provisions it, and workspace-install declares it as the
// required-version of the root pyproject.toml.
const DefaultUVVersion = uvtoolchain.DefaultVersion

// UVRelease is one uv release and what verifies its archives.
type UVRelease struct {
	// Version is the exact release, such as "0.11.32".
	Version string `json:"version"`
	// Integrities maps "<goos>/<goarch>" to the SHA-256 of that platform's
	// archive, as 64 hexadecimal characters.
	Integrities map[string]string `json:"integrities"`
	// Source is the URL the archives are published under. Empty selects the
	// release downloads of the uv repository.
	Source string `json:"source"`
}

// DefaultUVRelease is the release the extension pins when the workspace pins
// and declares none: DefaultUVVersion, from the uv release downloads, with the
// digests it ships (uvtoolchain.DefaultIntegrities), so pinning it reads no
// release metadata from the network.
func DefaultUVRelease() UVRelease {
	return UVRelease{
		Version:     DefaultUVVersion,
		Integrities: uvtoolchain.DefaultIntegrities(),
		Source:      uvtoolchain.ReleaseURL(DefaultUVVersion),
	}
}

// uvArchive names the archive of a release for one platform.
type uvArchive struct {
	// pin is the archive and the digest it must have. Its SHA256 is empty
	// when the release records none for the platform.
	pin pinnedarchive.Pin
	// dir is the one directory a tar.gz archive holds its programs in, and ""
	// for a zip archive, which holds them at its top level.
	dir string
}

// uvArchiveFor returns the archive of release for goos/goarch, as
// uvtoolchain.ArchiveFor names it, and false when uv publishes none for that
// platform.
func uvArchiveFor(release UVRelease, goos, goarch string) (uvArchive, bool) {
	archive, ok := uvtoolchain.ArchiveFor(goos, goarch)
	if !ok {
		return uvArchive{}, false
	}
	source := strings.TrimSpace(release.Source)
	if source == "" {
		source = uvtoolchain.ReleaseURL(release.Version)
	}
	if !strings.HasSuffix(source, "/") {
		source += "/"
	}
	format := pinnedarchive.TarGz
	if archive.Zip {
		format = pinnedarchive.Zip
	}
	return uvArchive{
		pin: pinnedarchive.Pin{
			URL:    source + archive.Name,
			SHA256: release.Integrities[goos+"/"+goarch],
			Format: format,
		},
		dir: archive.Dir,
	}, true
}

// uvProgram is the file name of the uv program on goos.
func uvProgram(goos string) string {
	if goos == "windows" {
		return "uv.exe"
	}
	return "uv"
}

// lockedUVRelease returns the uv release the workspace lock pins, and false
// when the workspace has no lock or its lock pins no uv.
//
// Only toolchains.uv is decoded. The CLI validates the whole document when it
// writes it; this reader does not refuse a lock a newer CLI wrote because the
// document grew a field the extension does not know.
func lockedUVRelease(workspaceRoot string) (UVRelease, bool, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, WorkspaceLockFile))
	if errors.Is(err, fs.ErrNotExist) {
		return UVRelease{}, false, nil
	}
	if err != nil {
		return UVRelease{}, false, fmt.Errorf("read %s: %w", WorkspaceLockFile, err)
	}
	var doc struct {
		Toolchains map[string]json.RawMessage `json:"toolchains"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return UVRelease{}, false, fmt.Errorf("parse %s: %w", WorkspaceLockFile, err)
	}
	raw, ok := doc.Toolchains["uv"]
	if !ok || string(raw) == "null" {
		return UVRelease{}, false, nil
	}
	var release UVRelease
	if err := json.Unmarshal(raw, &release); err != nil {
		return UVRelease{}, false, fmt.Errorf("parse toolchains.uv in %s: %w", WorkspaceLockFile, err)
	}
	release.Version = strings.TrimSpace(release.Version)
	if release.Version == "" {
		return UVRelease{}, false, nil
	}
	return release, true, nil
}
