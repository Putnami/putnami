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

// WorkspaceLockFile is the workspace lock that pins the Bun release the tasks
// run with, with one SHA-256 per platform.
const WorkspaceLockFile = "putnami.lock.json"

// DefaultBunVersion is the Bun release the extension installs for a workspace
// that declares none. workspace-install then declares it in the root
// package.json, and the lock pins it.
const DefaultBunVersion = "1.4.0"

// defaultBunIntegrities are the SHA-256 digests of the DefaultBunVersion
// archives, per "<goos>/<goarch>", as the vendor publishes them for the
// release. They ship in the extension so that installing the default release
// reads no release metadata from the network.
var defaultBunIntegrities = map[string]string{
	"darwin/amd64":  "1d0211b8f1dc991182344687ad15e72ee86f154845a5f7fa477994cd341dd9b0",
	"darwin/arm64":  "c669e97f6164e1c96e0701748db98dfa77492908cbd8394c7557134a735de381",
	"linux/amd64":   "2d03fb5fb83ac8b567aca0a281b2ce1a1a19d488f56c2968d88c3f25e92fe452",
	"linux/arm64":   "4b1a332ee861983eb93bcfe6f770fff94e3e31b2c388bdaea3c8ed35e58eed0e",
	"windows/amd64": "e6f093d39da486b20262ca8cdd5ed6a9e8bc9c2f275b78e6d3a0c5b28cc95901",
	"windows/arm64": "f473bfe2df73ee770548c93dd5d380aea7120c218ec2aa1afdd0bbba7bf18c47",
}

// bunTargets maps "<goos>/<goarch>" to the target a Bun release names its
// archive after: the archive is bun-<target>.zip and holds the one directory
// bun-<target>.
var bunTargets = map[string]string{
	"darwin/amd64":  "darwin-x64",
	"darwin/arm64":  "darwin-aarch64",
	"linux/amd64":   "linux-x64",
	"linux/arm64":   "linux-aarch64",
	"windows/amd64": "windows-x64",
	"windows/arm64": "windows-aarch64",
}

// BunRelease is one Bun release and what verifies its archives.
type BunRelease struct {
	// Version is the exact release, such as "1.4.0".
	Version string `json:"version"`
	// Integrities maps "<goos>/<goarch>" to the SHA-256 of that platform's
	// archive, as 64 hexadecimal characters.
	Integrities map[string]string `json:"integrities"`
	// Source is the URL the archives are published under. Empty selects the
	// vendor's release downloads.
	Source string `json:"source"`
}

// DefaultBunRelease is the release the extension installs when the workspace
// declares none: DefaultBunVersion, from the vendor's release downloads, with
// the digests the extension ships.
func DefaultBunRelease() BunRelease {
	return BunRelease{
		Version:     DefaultBunVersion,
		Integrities: maps.Clone(defaultBunIntegrities),
		Source:      bunReleaseSource(DefaultBunVersion),
	}
}

// bunReleaseSource is where the vendor publishes the archives of version.
func bunReleaseSource(version string) string {
	return "https://github.com/oven-sh/bun/releases/download/bun-v" + version + "/"
}

// bunArchive names the archive of a release for one platform.
type bunArchive struct {
	// pin is the archive and the digest it must have. Its SHA256 is empty
	// when the release records none for the platform.
	pin pinnedarchive.Pin
	// dir is the one directory the archive holds, where the bun program is.
	dir string
}

// bunArchiveFor returns the archive of release for goos/goarch, and false when
// Bun publishes none for that platform. Every Bun archive is a zip named
// bun-<target>.zip.
func bunArchiveFor(release BunRelease, goos, goarch string) (bunArchive, bool) {
	platform := goos + "/" + goarch
	target, ok := bunTargets[platform]
	if !ok {
		return bunArchive{}, false
	}
	source := strings.TrimSpace(release.Source)
	if source == "" {
		source = bunReleaseSource(release.Version)
	}
	if !strings.HasSuffix(source, "/") {
		source += "/"
	}
	dir := "bun-" + target
	return bunArchive{
		pin: pinnedarchive.Pin{
			URL:    source + dir + ".zip",
			SHA256: release.Integrities[platform],
			Format: pinnedarchive.Zip,
		},
		dir: dir,
	}, true
}

// bunProgram is the file name of the bun program on goos.
func bunProgram(goos string) string {
	if goos == "windows" {
		return "bun.exe"
	}
	return "bun"
}

// lockedBunRelease returns the Bun release the workspace lock pins, and false
// when the workspace has no lock or its lock pins no Bun.
//
// Only toolchains.bun is decoded. The CLI validates the whole document when it
// writes it; this reader does not refuse a lock a newer CLI wrote because the
// document grew a field the extension does not know.
func lockedBunRelease(workspaceRoot string) (BunRelease, bool, error) {
	path := filepath.Join(workspaceRoot, WorkspaceLockFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return BunRelease{}, false, nil
	}
	if err != nil {
		return BunRelease{}, false, fmt.Errorf("read %s: %w", WorkspaceLockFile, err)
	}
	var doc struct {
		Toolchains map[string]json.RawMessage `json:"toolchains"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return BunRelease{}, false, fmt.Errorf("parse %s: %w", WorkspaceLockFile, err)
	}
	raw, ok := doc.Toolchains["bun"]
	if !ok || string(raw) == "null" {
		return BunRelease{}, false, nil
	}
	var release BunRelease
	if err := json.Unmarshal(raw, &release); err != nil {
		return BunRelease{}, false, fmt.Errorf("parse toolchains.bun in %s: %w", WorkspaceLockFile, err)
	}
	release.Version = strings.TrimSpace(release.Version)
	if release.Version == "" {
		return BunRelease{}, false, nil
	}
	return release, true, nil
}

// declaredBunVersion returns the Bun release the workspace root package.json
// declares in packageManager, as "bun@<version>", normalized the way the CLI
// normalizes it before it pins the lock: a leading "v" is dropped and a
// MAJOR.MINOR version gets a ".0" patch. It returns "" when the manifest is
// missing, unreadable, or declares no Bun.
func declaredBunVersion(workspaceRoot string) string {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	version, ok := strings.CutPrefix(strings.TrimSpace(pkg.PackageManager), "bun@")
	if !ok {
		return ""
	}
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if strings.Count(version, ".") == 1 {
		version += ".0"
	}
	return version
}

// IsPlainBunRelease reports whether version is MAJOR.MINOR.PATCH with decimal
// parts and no prerelease or build suffix. Only such a version names a
// published release, and only such a version is safe as a directory name and
// in a URL.
func IsPlainBunRelease(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
