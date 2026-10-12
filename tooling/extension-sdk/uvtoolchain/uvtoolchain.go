// Package uvtoolchain holds what the Putnami CLI and the @putnami/python
// extension both know about uv releases: where a release is published, which
// archive each platform installs, the oldest release Putnami runs, the
// release it installs by default, and the release the root pyproject.toml of
// a workspace declares.
//
// The CLI pins the lock's uv toolchain with it, and the extension installs and
// selects the uv a task runs with it, so both read the same declaration and
// name the same archives.
package uvtoolchain

import (
	"cmp"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// MinimumVersion is the oldest uv release Putnami runs. It is the first
// release whose `uv self version --short` prints the release alone: an older
// one prints its commit and date too, so no exact probe can tell which
// release a uv program is. Every release from it on also lists its archives
// in ChecksumsFile.
const MinimumVersion = "0.11.8"

// ChecksumsFile is the file a release publishes beside its archives: one
// "<sha256> *<archive>" line each.
const ChecksumsFile = "sha256.sum"

// DefaultVersion is the uv release Putnami installs for a workspace that pins
// and declares none.
const DefaultVersion = "0.11.32"

// defaultIntegrities are the SHA-256 digests of the DefaultVersion archives,
// per "<goos>/<goarch>", as the release lists them in its ChecksumsFile. The
// test compares them with a copy of that file, committed under testdata.
var defaultIntegrities = map[string]string{
	"darwin/amd64":  "77f5ca26c0de20e992a3677a174fe1121ee25c36f9b1434a863f75bf077a05eb",
	"darwin/arm64":  "ed336d0ba49db8ef89b2b41fffa372ce63bd032f22a56f001c265891aec32829",
	"linux/amd64":   "1fd052f196108d87e61fc3d98fe06b4ec758c9a1eb1466a6fd1a436fe45885f2",
	"linux/arm64":   "d70cdae687feb6aad9a09fe8d686df8c8efaf69a1007fa581379a2025adc10a5",
	"windows/amd64": "acfde570451cfdb8689fa159a138ee805ba4e241c466432750302c86254b0984",
	"windows/arm64": "a7427ea0440bb826b6716d1837ff3d173b8e7d496cb09ee8f456b4e023a2fdcd",
}

// DefaultIntegrities returns the SHA-256 of each DefaultVersion archive, per
// "<goos>/<goarch>". They ship with Putnami, so installing the default release
// reads no release metadata from the network. The map is a copy.
func DefaultIntegrities() map[string]string {
	return maps.Clone(defaultIntegrities)
}

// releasesURL is where the uv repository publishes its release downloads.
const releasesURL = "https://github.com/astral-sh/uv/releases/download/"

// ReleaseURL is the URL the archives of version are published under, with a
// trailing slash.
func ReleaseURL(version string) string {
	return releasesURL + version + "/"
}

// ChecksumsURL is the URL of the ChecksumsFile of version.
func ChecksumsURL(version string) string {
	return ReleaseURL(version) + ChecksumsFile
}

// targets maps "<goos>/<goarch>" to the target a uv release names its archive
// after. Linux takes the statically linked musl build, which runs on glibc
// and musl hosts alike.
var targets = map[string]string{
	"darwin/amd64":  "x86_64-apple-darwin",
	"darwin/arm64":  "aarch64-apple-darwin",
	"linux/amd64":   "x86_64-unknown-linux-musl",
	"linux/arm64":   "aarch64-unknown-linux-musl",
	"windows/amd64": "x86_64-pc-windows-msvc",
	"windows/arm64": "aarch64-pc-windows-msvc",
}

// Archive is the archive a uv release publishes for one platform.
type Archive struct {
	// Name is the file name, such as uv-aarch64-apple-darwin.tar.gz.
	Name string
	// Dir is the one directory a .tar.gz archive holds the programs in, such
	// as uv-aarch64-apple-darwin, and "" for a .zip archive, which holds them
	// at its top level.
	Dir string
	// Zip reports a .zip archive. Every other archive is a .tar.gz.
	Zip bool
}

// ArchiveFor returns the archive a release publishes for goos/goarch:
// uv-<target>.zip on Windows and uv-<target>.tar.gz elsewhere. It returns
// false for a platform Putnami installs no uv on.
func ArchiveFor(goos, goarch string) (Archive, bool) {
	target, ok := targets[goos+"/"+goarch]
	if !ok {
		return Archive{}, false
	}
	name := "uv-" + target
	if goos == "windows" {
		return Archive{Name: name + ".zip", Zip: true}, true
	}
	return Archive{Name: name + ".tar.gz", Dir: name}, true
}

// PlatformDigests reads the ChecksumsFile of a release and returns the
// SHA-256 it lists for the archive of each platform ArchiveFor names, keyed by
// "<goos>/<goarch>", as 64 lower-case hexadecimal characters. A line that is
// not "<sha256> *<archive>", and an archive no platform installs, are skipped.
func PlatformDigests(checksums string) map[string]string {
	platforms := make(map[string]string, len(targets))
	for platform := range targets {
		goos, goarch, _ := strings.Cut(platform, "/")
		if archive, ok := ArchiveFor(goos, goarch); ok {
			platforms[archive.Name] = platform
		}
	}
	digests := make(map[string]string)
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		platform, ok := platforms[strings.TrimPrefix(fields[1], "*")]
		if !ok {
			continue
		}
		if digest := strings.ToLower(fields[0]); len(digest) == 64 && isHex(digest) {
			digests[platform] = digest
		}
	}
	return digests
}

func isHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

// Platforms lists, sorted, the "<goos>/<goarch>" platforms ArchiveFor names
// an archive for.
func Platforms() []string {
	platforms := make([]string, 0, len(targets))
	for platform := range targets {
		platforms = append(platforms, platform)
	}
	slices.Sort(platforms)
	return platforms
}

// IsRelease reports whether version is MAJOR.MINOR.PATCH with decimal parts
// and no prerelease or build suffix. Only such a version names a published
// release, and only such a version is safe as a directory name and in a URL.
func IsRelease(version string) bool {
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

// Supported returns nil when version is a release Putnami runs: IsRelease,
// and no older than MinimumVersion. Otherwise its error says why, and names
// MinimumVersion; the caller adds what to change.
func Supported(version string) error {
	if !IsRelease(version) {
		return fmt.Errorf("%q names no uv release, which is MAJOR.MINOR.PATCH", version)
	}
	if compareReleases(version, MinimumVersion) < 0 {
		return fmt.Errorf("uv %s is older than uv %s, the oldest release Putnami runs: "+
			"an older uv prints more than its release with `uv self version --short`, so Putnami cannot tell which release it is",
			version, MinimumVersion)
	}
	return nil
}

// compareReleases orders two versions IsRelease accepts, part by part, as
// numbers of any length.
func compareReleases(a, b string) int {
	aParts, bParts := strings.Split(a, "."), strings.Split(b, ".")
	for i := range aParts {
		x, y := strings.TrimLeft(aParts[i], "0"), strings.TrimLeft(bParts[i], "0")
		if order := cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y)); order != 0 {
			return order
		}
	}
	return 0
}
