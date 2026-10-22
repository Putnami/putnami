package workspacejob

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.putnami.dev/sdk/extension/pinnedarchive"
)

// LockFileName is the workspace lock, which pins the Go release the jobs run
// and the archive of each platform.
const LockFileName = "putnami.lock.json"

// GoLock is the Go toolchain pin of the workspace lock.
type GoLock struct {
	// Version is the exact Go release, such as "1.26.1".
	Version string `json:"version"`
	// Integrities maps "<goos>/<goarch>" to the SHA-256 of that platform's
	// archive, as 64 hexadecimal characters.
	Integrities map[string]string `json:"integrities"`
	// Source is the URL the archives are published under, such as
	// "https://go.dev/dl/".
	Source string `json:"source"`
}

// ReadGoLock returns the Go toolchain pin of the workspace lock at path.
//
// Only toolchains.go is decoded. The CLI validates the whole document when it
// writes it; this reader must not refuse a lock a newer CLI wrote because the
// document grew a field the extension does not know. It refuses a version that
// is not a Go release name: the version names a directory every workspace of
// the machine shares, so it must not lead outside the toolchain root.
func ReadGoLock(path string) (GoLock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GoLock{}, err
	}
	var doc struct {
		Toolchains map[string]json.RawMessage `json:"toolchains"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return GoLock{}, fmt.Errorf("parse %s: %w", path, err)
	}
	raw, ok := doc.Toolchains["go"]
	if !ok || string(raw) == "null" {
		return GoLock{}, fmt.Errorf("%s pins no Go toolchain (toolchains.go)", path)
	}
	var entry GoLock
	if err := json.Unmarshal(raw, &entry); err != nil {
		return GoLock{}, fmt.Errorf("parse toolchains.go in %s: %w", path, err)
	}
	entry.Version = NormalizeGoVersion(strings.TrimSpace(entry.Version))
	if entry.Version == "" {
		return GoLock{}, fmt.Errorf("%s pins no Go version (toolchains.go.version)", path)
	}
	if !isPlainGoRelease(entry.Version) {
		return GoLock{}, fmt.Errorf("%s pins Go %q, which is not a Go release name such as 1.25.7 (toolchains.go.version)", path, entry.Version)
	}
	return entry, nil
}

// GoArchivePin is the archive of lock's Go release for goos/goarch: the Go
// distribution names it go<version>.<goos>-<goarch>, a zip on Windows and a
// gzip-compressed tar everywhere else.
func GoArchivePin(lock GoLock, goos, goarch string) pinnedarchive.Pin {
	format, extension := pinnedarchive.TarGz, ".tar.gz"
	if goos == "windows" {
		format, extension = pinnedarchive.Zip, ".zip"
	}
	source := lock.Source
	if source == "" {
		source = "https://go.dev/dl/"
	}
	if !strings.HasSuffix(source, "/") {
		source += "/"
	}
	return pinnedarchive.Pin{
		URL:    source + "go" + lock.Version + "." + goos + "-" + goarch + extension,
		SHA256: lock.Integrities[goos+"/"+goarch],
		Format: format,
	}
}

// goReleasePattern matches the name of a Go release as a lock pins it:
// MAJOR.MINOR or MAJOR.MINOR.PATCH, or MAJOR.MINOR followed by rc or beta and
// a number.
var goReleasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+|(rc|beta)[0-9]+)?$`)

// isPlainGoRelease reports whether version is the name of a Go release, so it
// can name a directory without leaving the toolchain root.
func isPlainGoRelease(version string) bool {
	return goReleasePattern.MatchString(version)
}
