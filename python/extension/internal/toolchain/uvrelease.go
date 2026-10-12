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
// release reads no release metadata from the network.
var defaultUVIntegrities = map[string]string{
	"darwin/amd64":  "77f5ca26c0de20e992a3677a174fe1121ee25c36f9b1434a863f75bf077a05eb",
	"darwin/arm64":  "ed336d0ba49db8ef89b2b41fffa372ce63bd032f22a56f001c265891aec32829",
	"linux/amd64":   "1fd052f196108d87e61fc3d98fe06b4ec758c9a1eb1466a6fd1a436fe45885f2",
	"linux/arm64":   "d70cdae687feb6aad9a09fe8d686df8c8efaf69a1007fa581379a2025adc10a5",
	"windows/amd64": "acfde570451cfdb8689fa159a138ee805ba4e241c466432750302c86254b0984",
	"windows/arm64": "a7427ea0440bb826b6716d1837ff3d173b8e7d496cb09ee8f456b4e023a2fdcd",
}

// uvTargets maps "<goos>/<goarch>" to the target a uv release names its
// archive after. Linux takes the statically linked musl build, which runs on
// glibc and musl hosts alike. The CLI pins the same archives
// (tooling/cli versioncmd uvArchivePlatforms).
var uvTargets = map[string]string{
	"darwin/amd64":  "x86_64-apple-darwin",
	"darwin/arm64":  "aarch64-apple-darwin",
	"linux/amd64":   "x86_64-unknown-linux-musl",
	"linux/arm64":   "aarch64-unknown-linux-musl",
	"windows/amd64": "x86_64-pc-windows-msvc",
	"windows/arm64": "aarch64-pc-windows-msvc",
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
		Source:      uvReleaseSource(DefaultUVVersion),
	}
}

// uvReleaseSource is where the archives of version are published.
func uvReleaseSource(version string) string {
	return "https://github.com/astral-sh/uv/releases/download/" + version + "/"
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

// uvArchiveFor returns the archive of release for goos/goarch, and false when
// uv publishes none for that platform: uv-<target>.zip on Windows, and
// uv-<target>.tar.gz, which holds the directory uv-<target>, elsewhere.
func uvArchiveFor(release UVRelease, goos, goarch string) (uvArchive, bool) {
	platform := goos + "/" + goarch
	target, ok := uvTargets[platform]
	if !ok {
		return uvArchive{}, false
	}
	source := strings.TrimSpace(release.Source)
	if source == "" {
		source = uvReleaseSource(release.Version)
	}
	if !strings.HasSuffix(source, "/") {
		source += "/"
	}
	name := "uv-" + target
	if goos == "windows" {
		return uvArchive{pin: pinnedarchive.Pin{
			URL:    source + name + ".zip",
			SHA256: release.Integrities[platform],
			Format: pinnedarchive.Zip,
		}}, true
	}
	return uvArchive{
		pin: pinnedarchive.Pin{
			URL:    source + name + ".tar.gz",
			SHA256: release.Integrities[platform],
			Format: pinnedarchive.TarGz,
		},
		dir: name,
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

// declaredUVVersion returns the uv release the workspace root pyproject.toml
// declares, or "" when the manifest is missing, unreadable, or declares none.
// See UVRequiredVersion for what counts as a declaration.
func declaredUVVersion(workspaceRoot string) string {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, RootPyproject))
	if err != nil {
		return ""
	}
	return UVRequiredVersion(string(data))
}

// UVRequiredVersion returns the exact uv release a pyproject.toml declares as
// required-version in its [tool.uv] table: "==X.Y.Z", or a bare "X.Y.Z",
// which uv reads the same way. A MAJOR.MINOR release gets a ".0" patch. Any
// other specifier, such as a range or a wildcard, names no single release and
// declares nothing, and so does the key in any other table or form.
//
// The CLI reads the declaration with the same rule when it pins the lock
// (tooling/cli versioncmd uvRequiredVersion), so the release it pins is the
// one this extension selects.
func UVRequiredVersion(pyproject string) string {
	table := ""
	for _, raw := range strings.Split(pyproject, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if strings.HasPrefix(line, "[") {
			table = tomlTableName(line)
			continue
		}
		if table != "tool.uv" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.Trim(strings.TrimSpace(key), `"'`) != "required-version" {
			continue
		}
		return exactUVRelease(tomlStringValue(value))
	}
	return ""
}

// tomlTableName returns the name of the table a header line opens, with the
// white space around its parts removed, or "" for an array-of-tables header
// or a line that is not a header.
func tomlTableName(line string) string {
	header, _, _ := strings.Cut(line, "#")
	header = strings.TrimSpace(header)
	if strings.HasPrefix(header, "[[") || !strings.HasPrefix(header, "[") || !strings.HasSuffix(header, "]") {
		return ""
	}
	return strings.Join(strings.Fields(strings.Trim(header, "[]")), "")
}

// tomlStringValue returns the content of the single-line TOML string that
// value starts with, or "" when it starts with none.
func tomlStringValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || (value[0] != '"' && value[0] != '\'') {
		return ""
	}
	content, _, closed := strings.Cut(value[1:], value[:1])
	if !closed {
		return ""
	}
	return content
}

// exactUVRelease returns the release spec names when it names exactly one:
// "==X.Y.Z" or "X.Y.Z", with a leading "v" dropped and a ".0" patch added to a
// MAJOR.MINOR version. It returns "" for any other specifier.
func exactUVRelease(spec string) string {
	spec = strings.TrimSpace(spec)
	if rest, ok := strings.CutPrefix(spec, "=="); ok && !strings.HasPrefix(rest, "=") {
		spec = strings.TrimSpace(rest)
	}
	version := strings.TrimPrefix(spec, "v")
	if strings.Count(version, ".") == 1 {
		version += ".0"
	}
	if !IsPlainUVRelease(version) {
		return ""
	}
	return version
}

// IsPlainUVRelease reports whether version is MAJOR.MINOR.PATCH with decimal
// parts and no prerelease or build suffix. Only such a version names a
// published release, and only such a version is safe as a directory name and
// in a URL.
func IsPlainUVRelease(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return false
		}
	}
	return true
}
