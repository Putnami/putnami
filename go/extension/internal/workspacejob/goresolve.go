package workspacejob

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.putnami.dev/go/extension/internal/toolchain"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// GoLockIdentity is the workspace lock identity of the Go toolchain: the key
// of its pin under toolchains in the workspace lock.
const GoLockIdentity = "go"

// goReleaseSource is the URL go.dev publishes Go archives under. A pin record
// names it as its source.
const goReleaseSource = "https://go.dev/dl/"

// goReleaseIndexURL is the go.dev index of every Go release and the SHA-256 of
// each of its files.
var goReleaseIndexURL = "https://go.dev/dl/?mode=json&include=all"

const (
	// goReleaseIndexTimeout bounds the release index request, below the time
	// the CLI gives a control call.
	goReleaseIndexTimeout = 30 * time.Second
	// goReleaseIndexMaxBytes caps the release index document.
	goReleaseIndexMaxBytes = 16 << 20
	// goPinRecordMaxBytes caps the bytes read from a pin record. A record
	// holds one digest for each pinned platform, under 1 KiB in all.
	goPinRecordMaxBytes = 64 << 10
	// toolchainProblemMaxBytes is the toolchain-resolve bound on a problem's
	// message and remedy.
	toolchainProblemMaxBytes = 1024
)

// AnswerToolchainResolve answers a toolchain-resolve request for the workspace
// rooted at root. lookup reads one environment variable: it locates the
// Putnami home that holds the machine's Go pin records.
//
// The Go pin follows these rules, with the declaration read by
// GoWorkDeclaration and the committed pin taken from request.Locked:
//
//   - A committed pin at the declared release, or any committed pin when
//     go.work declares none, is kept: a lock version 5 entry as committed, a
//     version 3 or 4 entry converted without the network.
//   - Otherwise the declared release is resolved: from its pin record, else,
//     unless the request is offline, from the go.dev release index, whose
//     answer is then written as the record.
//   - With no declaration and no committed pin, the answer omits Go.
//   - A pin that has no archive for the request's platform is answered as a
//     problem.
//
// Every other asked identity is answered as a problem. It returns an error
// only when it cannot read go.work.
func AnswerToolchainResolve(root string, request wsproto.ToolchainResolveRequest, lookup func(string) string) (wsproto.ToolchainResolveResult, error) {
	result := wsproto.ToolchainResolveResult{Toolchains: make(map[string]wsproto.ToolchainResolution)}
	for _, identity := range request.Identities {
		if identity != GoLockIdentity {
			result.Toolchains[identity] = toolchainProblem(
				fmt.Sprintf("the Go extension resolves no toolchain %q", identity), "")
			continue
		}
		resolution, answered, err := resolveGoPin(root, request, lookup)
		if err != nil {
			return wsproto.ToolchainResolveResult{}, err
		}
		if answered {
			result.Toolchains[identity] = resolution
		}
	}
	return result, nil
}

// resolveGoPin answers for GoLockIdentity. It reports false when go.work
// declares no Go release and the lock pins none, so the lock keeps no Go pin.
func resolveGoPin(root string, request wsproto.ToolchainResolveRequest, lookup func(string) string) (wsproto.ToolchainResolution, bool, error) {
	declared, err := GoWorkDeclaration(root)
	if err != nil {
		return wsproto.ToolchainResolution{}, false, err
	}
	committed, pinned := request.Locked[GoLockIdentity]
	committedVersion := strings.TrimSpace(committed.Version)
	pinned = pinned && committedVersion != ""
	if pinned && (declared == "" || committedVersion == declared) {
		return committedGoPin(committed, request.Platform), true, nil
	}
	if declared == "" {
		return wsproto.ToolchainResolution{}, false, nil
	}
	lock, problem := resolveDeclaredGo(root, declared, request.Offline, lookup)
	if problem != nil {
		return wsproto.ToolchainResolution{Problem: problem}, true, nil
	}
	pin := goArchivesPin(lock)
	if !pinCoversPlatform(pin, request.Platform) {
		return toolchainProblem(
			fmt.Sprintf("Go %s publishes no archive Putnami pins for %s", lock.Version, request.Platform),
			"Putnami pins Go for darwin, linux and windows on amd64 and arm64. Declare in go.work a Go release that publishes an archive for "+
				request.Platform+", or install on a platform the release serves."), true, nil
	}
	return wsproto.ToolchainResolution{Pin: &pin}, true, nil
}

// committedGoPin keeps the lock's Go pin. A lock version 5 entry, which has
// archives, is kept as committed. A lock version 3 or 4 entry is converted
// without the network: each platform's archive is the one its source
// publishes, named as GoArchivePin names it for the installer, and its
// integrities are kept.
func committedGoPin(committed wsproto.LockToolchainEntry, platform string) wsproto.ToolchainResolution {
	pin := committed
	if len(committed.Archives) == 0 {
		lock := GoLock{
			Version:     NormalizeGoVersion(strings.TrimSpace(committed.Version)),
			Integrities: committed.Integrities,
			Source:      committed.Source,
		}
		if !isPlainGoRelease(lock.Version) {
			return toolchainProblem(
				fmt.Sprintf("%s pins Go %q, which is not a Go release name such as 1.25.7", LockFileName, committed.Version),
				"Declare a Go release in go.work and run `putnami install` with network access to pin it.")
		}
		for key := range lock.Integrities {
			if _, _, ok := splitPlatform(key); !ok {
				return toolchainProblem(
					fmt.Sprintf("%s pins Go %s for %q, which is not an os/arch platform", LockFileName, lock.Version, key),
					"Remove toolchains.go from "+LockFileName+" and run `putnami install` with network access to pin Go again.")
			}
		}
		pin = goArchivesPin(lock)
	}
	if !pinCoversPlatform(pin, platform) {
		return toolchainProblem(
			fmt.Sprintf("%s pins Go %s with no archive for %s", LockFileName, pin.Version, platform),
			"Remove toolchains.go from "+LockFileName+" and run `putnami install` with network access to pin Go again.")
	}
	return wsproto.ToolchainResolution{Pin: &pin}
}

// resolveDeclaredGo returns the lock entry of the declared Go release: its
// pin record, else, unless offline, the release index's answer, which it then
// writes as the record. A record that cannot be written leaves the answer as
// it is. It returns a problem when neither yields the release.
func resolveDeclaredGo(root, version string, offline bool, lookup func(string) string) (GoLock, *wsproto.ToolchainProblem) {
	toolchainRoot := toolchain.ResolveGoToolchainRoot(lookup, root)
	if lock, ok := readGoPinRecord(toolchainRoot, version); ok {
		return lock, nil
	}
	if offline {
		return GoLock{}, toolchainProblem(
			fmt.Sprintf("go.work declares Go %s, which %s does not pin and no pin record on this machine holds, and the request is offline", version, LockFileName),
			"Run `putnami install` with network access to pin Go "+version+".").Problem
	}
	integrities, err := fetchGoReleaseIntegrities(version)
	if err != nil {
		return GoLock{}, toolchainProblem(
			fmt.Sprintf("cannot resolve Go %s, which go.work declares: %v", version, err),
			"Declare a published Go release in go.work, and run `putnami install` with network access to go.dev.").Problem
	}
	lock := GoLock{Version: version, Integrities: integrities, Source: goReleaseSource}
	_ = writeGoPinRecord(toolchainRoot, lock)
	return lock, nil
}

// goArchivesPin is the lock version 5 entry of lock's Go release: for each
// platform lock records a digest for, the archive GoArchivePin names, which
// unpacks to a go directory holding bin/go.
func goArchivesPin(lock GoLock) wsproto.LockToolchainEntry {
	pin := wsproto.LockToolchainEntry{
		Version:     lock.Version,
		Archives:    make(map[string]wsproto.LockToolchainArchive, len(lock.Integrities)),
		Integrities: maps.Clone(lock.Integrities),
	}
	for platform := range lock.Integrities {
		goos, goarch, _ := splitPlatform(platform)
		archive := GoArchivePin(lock, goos, goarch)
		pin.Archives[platform] = wsproto.LockToolchainArchive{
			URL:        archive.URL,
			Format:     string(archive.Format),
			Executable: "go/bin/" + pkgmeta.ExecutableName(goos, "go"),
		}
	}
	return pin
}

// pinCoversPlatform reports whether pin records both an archive and an
// integrity for platform.
func pinCoversPlatform(pin wsproto.LockToolchainEntry, platform string) bool {
	_, archive := pin.Archives[platform]
	_, integrity := pin.Integrities[platform]
	return archive && integrity
}

// splitPlatform splits an "os/arch" key into exactly one non-empty os and one
// non-empty arch.
func splitPlatform(key string) (goos, goarch string, ok bool) {
	goos, goarch, found := strings.Cut(key, "/")
	if !found || goos == "" || goarch == "" || strings.Contains(goarch, "/") {
		return "", "", false
	}
	return goos, goarch, true
}

// toolchainProblem is the resolution that reports message and remedy, each
// cut to the protocol's bound on a rune boundary.
func toolchainProblem(message, remedy string) wsproto.ToolchainResolution {
	return wsproto.ToolchainResolution{Problem: &wsproto.ToolchainProblem{
		Message: boundedProblemText(message),
		Remedy:  boundedProblemText(remedy),
	}}
}

func boundedProblemText(text string) string {
	if len(text) <= toolchainProblemMaxBytes {
		return text
	}
	cut := toolchainProblemMaxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// GoWorkDeclaration returns the Go release the go.work at root declares: the
// higher of its go and toolchain directives, each normalized by
// NormalizeGoVersion, or "" when go.work is missing or names neither. The last
// line of each directive counts. A directive that is not three numeric fields
// once normalized, such as `toolchain default`, declares nothing.
func GoWorkDeclaration(root string) (string, error) {
	file, err := os.Open(filepath.Join(root, "go.work"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read go.work: %w", err)
	}
	defer func() { _ = file.Close() }()
	goVersion, toolchainVersion := "", ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line, _, _ := strings.Cut(scanner.Text(), "//")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "go":
			goVersion = NormalizeGoVersion(fields[1])
		case "toolchain":
			toolchainVersion = NormalizeGoVersion(fields[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read go.work: %w", err)
	}
	return higherGoVersion(goVersion, toolchainVersion), nil
}

// higherGoVersion returns the higher of two Go versions of three numeric
// fields, the one of them that has three numeric fields, or "".
func higherGoVersion(a, b string) string {
	aParts, aOK := numericVersion(a)
	bParts, bOK := numericVersion(b)
	switch {
	case !aOK && !bOK:
		return ""
	case !aOK:
		return b
	case !bOK:
		return a
	case slices.Compare(bParts[:], aParts[:]) > 0:
		return b
	default:
		return a
	}
}

// goPinRecord is the content of a pin record: the lock version 4 entry of one
// Go release as the go.dev release index publishes it. The CLI reads and
// writes the same records at the same path, so a machine that pinned a release
// once pins it in every workspace with no request to go.dev.
type goPinRecord struct {
	Version     string            `json:"version"`
	Integrities map[string]string `json:"integrities"`
	Source      string            `json:"source"`
}

// goPinRecordPath returns the pin record of a Go release:
// go-<version>.pin.json in toolchainRoot, beside the directory the release
// installs in. It reports false for a version that is not letters, digits and
// dots, which has no record: go.work names the version, and a path separator
// in it must not name a file outside toolchainRoot.
func goPinRecordPath(toolchainRoot, version string) (string, bool) {
	if version == "" {
		return "", false
	}
	for _, r := range version {
		plain := r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !plain {
			return "", false
		}
	}
	return filepath.Join(toolchainRoot, "go-"+version+".pin.json"), true
}

// readGoPinRecord returns the lock entry the pin record of version holds. It
// reports false unless the record is a regular file that parses, names version
// and goReleaseSource, and maps at least one platform Putnami pins Go for, and
// no other key, to a lowercase 64-hex SHA-256: exactly the entry
// fetchGoReleaseIntegrities builds from the release index.
func readGoPinRecord(toolchainRoot, version string) (GoLock, bool) {
	path, ok := goPinRecordPath(toolchainRoot, version)
	if !ok {
		return GoLock{}, false
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return GoLock{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return GoLock{}, false
	}
	data, err := io.ReadAll(io.LimitReader(file, goPinRecordMaxBytes+1))
	_ = file.Close()
	if err != nil || len(data) > goPinRecordMaxBytes {
		return GoLock{}, false
	}
	var record goPinRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return GoLock{}, false
	}
	if record.Version != version || record.Source != goReleaseSource || len(record.Integrities) == 0 {
		return GoLock{}, false
	}
	for platform, digest := range record.Integrities {
		goos, goarch, ok := splitPlatform(platform)
		if !ok || !goPinPlatform(goos, goarch) {
			return GoLock{}, false
		}
		if canonical, ok := normalizedSHA256(digest); !ok || canonical != digest {
			return GoLock{}, false
		}
	}
	return GoLock(record), true
}

// writeGoPinRecord writes lock as the pin record of its release, through a
// temporary file renamed over the record, so a reader finds the previous
// record or the complete new one.
func writeGoPinRecord(toolchainRoot string, lock GoLock) error {
	path, ok := goPinRecordPath(toolchainRoot, lock.Version)
	if !ok {
		return fmt.Errorf("go %s names no pin record", lock.Version)
	}
	data, err := json.MarshalIndent(goPinRecord(lock), "", "  ")
	if err != nil {
		return fmt.Errorf("encode the go %s pin record: %w", lock.Version, err)
	}
	if err := os.MkdirAll(toolchainRoot, 0o755); err != nil {
		return err
	}
	return WriteFileAtomically(path, append(data, '\n'), 0o644)
}

// goRelease is one release of the go.dev release index.
type goRelease struct {
	Version string   `json:"version"`
	Files   []goFile `json:"files"`
}

// goFile is one file of a goRelease.
type goFile struct {
	Filename string `json:"filename"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	SHA256   string `json:"sha256"`
	Kind     string `json:"kind"`
}

// fetchGoReleaseIntegrities returns the SHA-256 of the archive of each
// platform Putnami pins Go for that the release index publishes for version,
// keyed by "<goos>/<goarch>". It fails when the index cannot be read or
// publishes no such archive.
func fetchGoReleaseIntegrities(version string) (map[string]string, error) {
	request, err := http.NewRequest(http.MethodGet, goReleaseIndexURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %s: %w", goReleaseIndexURL, err)
	}
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: goReleaseIndexTimeout}
	response, err := client.Do(request) //nolint:gosec // G704: goReleaseIndexURL is the go.dev release index, a package endpoint
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", goReleaseIndexURL, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch %s: HTTP %s", goReleaseIndexURL, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, goReleaseIndexMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", goReleaseIndexURL, err)
	}
	if len(body) > goReleaseIndexMaxBytes {
		return nil, fmt.Errorf("read %s: the release index exceeds %d bytes", goReleaseIndexURL, goReleaseIndexMaxBytes)
	}
	var releases []goRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("decode %s: %w", goReleaseIndexURL, err)
	}
	want := "go" + version
	integrities := make(map[string]string)
	for _, release := range releases {
		if release.Version != want {
			continue
		}
		for _, file := range release.Files {
			if file.Kind != "archive" || !goPinPlatform(file.OS, file.Arch) {
				continue
			}
			if digest, ok := normalizedSHA256(file.SHA256); ok {
				integrities[file.OS+"/"+file.Arch] = digest
			}
		}
	}
	if len(integrities) == 0 {
		return nil, fmt.Errorf("go %s has no supported archives in %s", version, goReleaseIndexURL)
	}
	return integrities, nil
}

// goPinPlatform reports whether Putnami pins Go for goos/goarch.
func goPinPlatform(goos, goarch string) bool {
	return (goos == "darwin" || goos == "linux" || goos == "windows") &&
		(goarch == "amd64" || goarch == "arm64")
}

// normalizedSHA256 returns value as a lowercase 64-hex SHA-256, with a
// "sha256:" or "sha-256:" prefix removed, and reports whether it is one.
func normalizedSHA256(value string) (string, bool) {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"sha256:", "sha-256:"} {
		value = strings.TrimPrefix(value, prefix)
	}
	if len(value) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", false
	}
	return strings.ToLower(value), true
}
