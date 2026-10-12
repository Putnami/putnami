package toolchain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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
const RootPyproject = "pyproject.toml"

// DefaultUVVersion is the uv release the extension installs for a workspace
// that pins and declares none. workspace-install then declares it as the
// required-version of the root pyproject.toml, and the lock pins it.
const DefaultUVVersion = "0.11.32"

// defaultUVIntegrities are the SHA-256 digests of the DefaultUVVersion
// archives, per "<goos>/<goarch>", as the release publishes them in its
// sha256.sum. They ship in the extension so that installing the default
// release reads no release metadata from the network. While the repository's
// own putnami.lock.json pins the same release, it pins the same digests:
// TestDefaultUVReleaseMatchesTheRepositoryLock compares them.
var defaultUVIntegrities = map[string]string{
	"darwin/amd64":  "77f5ca26c0de20e992a3677a174fe1121ee25c36f9b1434a863f75bf077a05eb",
	"darwin/arm64":  "ed336d0ba49db8ef89b2b41fffa372ce63bd032f22a56f001c265891aec32829",
	"linux/amd64":   "1fd052f196108d87e61fc3d98fe06b4ec758c9a1eb1466a6fd1a436fe45885f2",
	"linux/arm64":   "d70cdae687feb6aad9a09fe8d686df8c8efaf69a1007fa581379a2025adc10a5",
	"windows/amd64": "acfde570451cfdb8689fa159a138ee805ba4e241c466432750302c86254b0984",
	"windows/arm64": "a7427ea0440bb826b6716d1837ff3d173b8e7d496cb09ee8f456b4e023a2fdcd",
}

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

// DefaultUVRelease is the release the extension installs when the workspace
// pins and declares none: DefaultUVVersion, from the uv release downloads,
// with the digests the extension ships.
func DefaultUVRelease() UVRelease {
	return UVRelease{
		Version:     DefaultUVVersion,
		Integrities: maps.Clone(defaultUVIntegrities),
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

// uvPrograms are the programs of a uv install, by their file name on goos:
// uv, and uvx, which runs a tool the way `uv tool run` does.
func uvPrograms(goos string) []string {
	return []string{uvProgram(goos), executableName("uvx", goos)}
}

// uvProgram is the file name of the uv program on goos.
func uvProgram(goos string) string {
	return executableName("uv", goos)
}

func executableName(name, goos string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
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

// readUVDeclaration reads what the workspace root pyproject.toml declares
// about the uv release, with the rule the CLI pins the lock with
// (uvtoolchain.ReadDeclaration). A missing manifest declares nothing.
func readUVDeclaration(workspaceRoot string) (uvtoolchain.Declaration, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, RootPyproject))
	if errors.Is(err, fs.ErrNotExist) {
		return uvtoolchain.Declaration{}, nil
	}
	if err != nil {
		return uvtoolchain.Declaration{}, fmt.Errorf("read %s: %w", RootPyproject, err)
	}
	return uvtoolchain.ReadDeclaration(string(data)), nil
}
