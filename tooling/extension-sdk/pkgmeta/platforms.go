package pkgmeta

import "strings"

// ArchivePlatform is one registry platform key a release archive is published
// under: the Go os/arch pair the CLI's installer asks the registry for, and
// the <os>-<arch> suffix the archive filename carries.
type ArchivePlatform struct {
	GOOS   string
	GOARCH string
	Suffix string
}

// archivePlatforms is the distribution matrix, in publication order.
var archivePlatforms = []ArchivePlatform{
	{GOOS: "linux", GOARCH: "amd64", Suffix: "linux-x64"},
	{GOOS: "linux", GOARCH: "arm64", Suffix: "linux-arm64"},
	{GOOS: "darwin", GOARCH: "amd64", Suffix: "darwin-x64"},
	{GOOS: "darwin", GOARCH: "arm64", Suffix: "darwin-arm64"},
	{GOOS: "windows", GOARCH: "amd64", Suffix: "windows-x64"},
}

// windowsExecutableSuffix is the file name suffix Windows requires of a
// program it starts.
const windowsExecutableSuffix = ".exe"

// ExecutableName is the file name of the program called name in an archive
// built for goos: name with ".exe" appended on windows, and name unchanged on
// every other OS. A name that already ends in ".exe" is returned as it is.
//
// Every manifest and project option declares a program without the suffix
// (runtime.executable "compiled/putnami-go"), so this is the one spelling the
// packagers write into a Windows archive (compiled/putnami-go.exe) and the
// CLI reads back out of an installed one. name may be a slash-separated path;
// only its last element gains the suffix.
func ExecutableName(goos, name string) string {
	if goos != "windows" || strings.HasSuffix(strings.ToLower(name), windowsExecutableSuffix) {
		return name
	}
	return name + windowsExecutableSuffix
}

// ArchivePlatforms returns the distribution matrix: every registry platform key
// a release archive is published under, in publication order. It is the one
// list every archive packager reads, so the Go packager's per-platform
// extension archives and a content-only extension's identical copies cover
// the same keys. The result is a copy the caller may modify.
func ArchivePlatforms() []ArchivePlatform {
	return append([]ArchivePlatform(nil), archivePlatforms...)
}

// ArchivePlatformSuffixes returns the archive filename suffix of every entry of
// ArchivePlatforms, in the same order.
func ArchivePlatformSuffixes() []string {
	suffixes := make([]string, 0, len(archivePlatforms))
	for _, platform := range archivePlatforms {
		suffixes = append(suffixes, platform.Suffix)
	}
	return suffixes
}
