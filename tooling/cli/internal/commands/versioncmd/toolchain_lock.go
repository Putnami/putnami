package versioncmd

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

var (
	goDownloadsURL = "https://go.dev/dl/?mode=json&include=all"
	goSourceURL    = "https://go.dev/dl/"
	// toolchainHTTPClient overrides the client used for metadata fetches. It is
	// a test seam only: nil means "build the bounded registry client", which is
	// the sole production path.
	toolchainHTTPClient *http.Client
	// toolchainMaxMetadataBytes caps a single metadata document. Vendor indexes
	// are ~1 MiB at most, so 16 MiB is generous headroom; the point is that a
	// hostile or misrouted endpoint cannot stream unbounded bytes into memory.
	// A var (not a const) so tests can exercise the oversize path without
	// pushing 16 MiB over loopback.
	toolchainMaxMetadataBytes int64 = 16 << 20
	bunReleaseAPIURL                = func(version string) string {
		return "https://api.github.com/repos/oven-sh/bun/releases/tags/bun-v" + version
	}
	bunReleaseURL = func(version string) string {
		return "https://github.com/oven-sh/bun/releases/download/bun-v" + version + "/"
	}
)

// SetGoReleaseEndpoints points the Go release index the lock step reads at
// downloadsURL and the download source it records at sourceURL, and returns
// the function that restores the previous endpoints. It is the seam a test
// outside this package uses to pin Go without reaching go.dev; production
// never calls it.
func SetGoReleaseEndpoints(downloadsURL, sourceURL string) (restore func()) {
	previousDownloads, previousSource := goDownloadsURL, goSourceURL
	goDownloadsURL, goSourceURL = downloadsURL, sourceURL
	return func() { goDownloadsURL, goSourceURL = previousDownloads, previousSource }
}

// RefreshLockMetadata resolves declared toolchains into the lock's exact,
// integrity-pinned runtime dimension and, when the recorded CLI entry matches
// the running binary's version, stamps its protocol version. It moved here
// (from internal/commands' install lifecycle) because it is composed entirely
// of this vertical's own lock/toolchain machinery — refreshToolchainLock's own
// resolvers are unexported and specific to this package, so absorbing the
// caller here keeps them from needing a wider export surface. This is a
// deliberate v3 write; ordinary v2 lock updates remain v2 until install (or
// pin) derives the new vocabulary.
func RefreshLockMetadata(ctx context.Context, wsRoot, runningVersion string) error {
	_, err := RefreshLockMetadataWithResult(ctx, wsRoot, runningVersion)
	return err
}

// RefreshLockMetadataWithResult is RefreshLockMetadata with an exact changed
// bit for lifecycle reporting. It skips the atomic rewrite when the canonical
// bytes already match the file on disk.
func RefreshLockMetadataWithResult(ctx context.Context, wsRoot, runningVersion string) (bool, error) {
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return false, err
	}
	if lf == nil {
		lf = lockfile.NewLockFile()
	}
	if err := refreshToolchainLock(ctx, wsRoot, lf); err != nil {
		return false, err
	}
	// A source workspace pins no published CLI, so there is no artifact whose
	// output protocol could be recorded: stamping protocolVersion onto the
	// sentinel would invent a claim about bytes that do not exist, and rewriting
	// the entry would erase the sentinel a committed lock carries. Leave it
	// exactly as written. (sameCLIVersion already rejects the empty version a
	// sentinel carries; the explicit guard states the invariant instead of
	// relying on that coincidence.)
	if cli, ok := lf.GetCLI(); ok && !cli.IsWorkspaceSource() && sameCLIVersion(cli.Version, runningVersion) {
		cli.ProtocolVersion = protocolcli.ResultProtocolVersion
		lf.SetCLI(cli)
	}
	return lockfile.WriteLockFileIfChanged(wsRoot, lf)
}

// FillMissingToolchainPins pins each toolchain that go.work or
// package.json#packageManager declares and the existing lock does not pin
// yet, resolved the way RefreshLockMetadata resolves it. It changes no other
// entry: a pin that differs from its declaration, a pin a declared extension
// keeps, and the CLI entry stay as committed. Without a missing pin it reads no
// metadata and writes nothing, and without a lock it writes none. The implicit
// first-use install runs it in place of the refresh, so a checkout restores its
// committed lock and still reaches a pin the lock never had.
func FillMissingToolchainPins(ctx context.Context, wsRoot string) (bool, error) {
	return pinDeclaredToolchains(ctx, wsRoot, func(_ string, pinned lockfile.LockEntry) bool {
		return strings.TrimSpace(pinned.Version) == ""
	})
}

// PinDeclaredToolchains pins each toolchain that go.work or
// package.json#packageManager declares at the release it declares, when the
// existing lock pins none or pins another release, resolved the way
// RefreshLockMetadata resolves it. It changes no other entry. Without a pin to
// change it reads no metadata and writes nothing, and without a lock it writes
// none. An explicit `putnami install` runs it before the workspace installers,
// which install the toolchains the lock pins, so one install converges after a
// declaration changes.
func PinDeclaredToolchains(ctx context.Context, wsRoot string) (bool, error) {
	return pinDeclaredToolchains(ctx, wsRoot, func(declared string, pinned lockfile.LockEntry) bool {
		return strings.TrimSpace(pinned.Version) != declared
	})
}

// pinDeclaredToolchains resolves and writes the pin of each declared toolchain
// for which repin reports true, given the declared release and the current
// pin (empty when there is none).
func pinDeclaredToolchains(ctx context.Context, wsRoot string, repin func(declared string, pinned lockfile.LockEntry) bool) (bool, error) {
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil || lf == nil {
		return false, err
	}
	declared, err := readToolchainDeclarations(wsRoot)
	if err != nil {
		return false, err
	}
	filled := false
	for _, name := range []string{"go", "bun"} {
		constraint, ok := declared[name]
		if !ok {
			continue
		}
		current, _ := lf.GetToolchain(name)
		if !repin(constraint, current) {
			continue
		}
		var entry lockfile.LockEntry
		switch name {
		case "go":
			entry, err = resolveGoToolchain(ctx, wsRoot, constraint)
		case "bun":
			entry, err = resolveBunToolchain(ctx, constraint, lockfile.LockEntry{})
		}
		if err != nil {
			return false, fmt.Errorf("resolve %s toolchain: %w", name, err)
		}
		lf.SetToolchain(name, entry)
		filled = true
	}
	if !filled {
		return false, nil
	}
	if lf.Version < lockfile.FormatVersionV3 {
		lf.Version = lockfile.FormatVersionV3
	}
	return lockfile.WriteLockFileIfChanged(wsRoot, lf)
}

// sameCLIVersion reports whether a and b name the same CLI release, ignoring a
// leading "v" and surrounding whitespace either may carry.
func sameCLIVersion(a, b string) bool {
	a = strings.TrimPrefix(strings.TrimSpace(a), "v")
	b = strings.TrimPrefix(strings.TrimSpace(b), "v")
	return a != "" && a == b
}

// refreshToolchainLock derives exact, integrity-pinned runtime releases from
// the workspace's authored declarations. It replaces (rather than merges) the
// dimension so removing a declaration cannot leave a stale runtime pin behind.
//
// A pin no declaration derives is kept verbatim while a declared extension's
// runtime resolves it: the lock is then that extension's only source for the
// version, whether or not a project of the toolchain's language exists. Once
// no declared extension resolves it, the pin is dropped.
func refreshToolchainLock(ctx context.Context, wsRoot string, lf *lockfile.LockFile) error {
	declared, err := readToolchainDeclarations(wsRoot)
	if err != nil {
		return err
	}
	resolvedByExtensions, err := extensionToolchainLocks(wsRoot)
	if err != nil {
		return err
	}

	resolved := make(map[string]lockfile.LockEntry, len(declared))
	for _, name := range []string{"go", "bun"} {
		constraint, ok := declared[name]
		if !ok {
			continue
		}
		var entry lockfile.LockEntry
		switch name {
		case "go":
			entry, err = resolveGoToolchain(ctx, wsRoot, constraint)
		case "bun":
			current, _ := lf.GetToolchain(name)
			entry, err = resolveBunToolchain(ctx, constraint, current)
		}
		if err != nil {
			return fmt.Errorf("resolve %s toolchain: %w", name, err)
		}
		resolved[name] = entry
	}
	for _, name := range resolvedByExtensions {
		if _, derived := resolved[name]; derived {
			continue
		}
		if entry, pinned := lf.GetToolchain(name); pinned && strings.TrimSpace(entry.Version) != "" {
			resolved[name] = entry
		}
	}

	if lf.Version < lockfile.FormatVersionV3 {
		lf.Version = lockfile.FormatVersionV3
	}
	lf.Toolchains = resolved
	return nil
}

// extensionToolchainLocks returns the sorted lock identities the runtimes of
// the workspace's declared extensions resolve. Extensions are discovered the
// way a job run discovers them: from the workspace projects, the workspace
// `extensions` list and the root package declarations.
func extensionToolchainLocks(wsRoot string) ([]string, error) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return nil, fmt.Errorf("load workspace for extension toolchains: %w", err)
	}
	projectPaths := make([]string, len(ws.Projects))
	for i, project := range ws.Projects {
		projectPaths[i] = project.Path
	}
	discovered, err := extension.DiscoverExtensions(wsRoot, wsproto.Load(wsRoot), projectPaths)
	if err != nil {
		return nil, fmt.Errorf("discover extensions for toolchains: %w", err)
	}
	seen := make(map[string]bool)
	var locks []string
	for _, ext := range discovered {
		for _, lock := range extension.RuntimeToolchainLocks(ext) {
			if !seen[lock] {
				seen[lock] = true
				locks = append(locks, lock)
			}
		}
	}
	sort.Strings(locks)
	return locks, nil
}

func readToolchainDeclarations(wsRoot string) (map[string]string, error) {
	declared := make(map[string]string)
	if f, err := os.Open(filepath.Join(wsRoot, "go.work")); err == nil {
		s := bufio.NewScanner(f)
		goVersion, toolchainVersion := "", ""
		for s.Scan() {
			line, _, _ := strings.Cut(s.Text(), "//")
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "go" {
				goVersion = normalizeExactVersion(fields[1])
			}
			if len(fields) >= 2 && fields[0] == "toolchain" {
				toolchainVersion = normalizeExactVersion(fields[1])
			}
		}
		closeErr := f.Close()
		if err := s.Err(); err != nil {
			return nil, fmt.Errorf("read go.work: %w", err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close go.work: %w", closeErr)
		}
		declared["go"] = higherExactVersion(goVersion, toolchainVersion)
		if declared["go"] == "" {
			delete(declared, "go")
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read go.work: %w", err)
	}

	data, err := os.ReadFile(filepath.Join(wsRoot, "package.json"))
	if os.IsNotExist(err) {
		return declared, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}
	var pkg struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse package.json toolchains: %w", err)
	}
	if version, ok := strings.CutPrefix(strings.TrimSpace(pkg.PackageManager), "bun@"); ok {
		if version = normalizeExactVersion(version); version != "" {
			declared["bun"] = version
		}
	}
	return declared, nil
}

func higherExactVersion(a, b string) string {
	av, aerr := extension.ParseVersion(a)
	bv, berr := extension.ParseVersion(b)
	switch {
	case aerr != nil:
		return b
	case berr != nil:
		return a
	case bv.GreaterThan(av):
		return b
	default:
		return a
	}
}

func normalizeExactVersion(version string) string {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "go")
	version = strings.TrimPrefix(version, "v")
	parts := strings.Split(version, ".")
	if len(parts) == 2 {
		return version + ".0"
	}
	return version
}

type goRelease struct {
	Version string   `json:"version"`
	Files   []goFile `json:"files"`
}

type goFile struct {
	Filename string `json:"filename"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	SHA256   string `json:"sha256"`
	Kind     string `json:"kind"`
}

// resolveGoToolchain returns the lock entry of one exact Go release: its
// version, the go.dev download source, and the SHA-256 of the archive of each
// supported platform.
//
// The entry the release index publishes for a release is kept in a pin record
// under the Putnami home, and a valid record is returned without a request:
// go.dev never changes the archives of a published release, so each recorded
// digest stays the one a download of that release is checked against. A
// machine that pinned the release once therefore pins it in every other
// workspace with no request to go.dev. resolveBunToolchain applies the same
// rule to a lock entry that already pins the declared version.
//
// A missing record, or one readGoPinRecord rejects, is ignored and the index
// is fetched. The fetched entry is then written as the record. A write that
// fails leaves the entry and the pin as they are: the next pin fetches again.
func resolveGoToolchain(ctx context.Context, wsRoot, version string) (lockfile.LockEntry, error) {
	if entry, ok := readGoPinRecord(wsRoot, version); ok {
		return entry, nil
	}
	var releases []goRelease
	if err := getJSON(ctx, goDownloadsURL, &releases); err != nil {
		return lockfile.LockEntry{}, err
	}
	want := "go" + version
	integrities := make(map[string]string)
	for _, release := range releases {
		if release.Version != want {
			continue
		}
		for _, file := range release.Files {
			if file.Kind != "archive" || !supportedPlatform(file.OS, file.Arch) {
				continue
			}
			if digest, ok := normalizedSHA256(file.SHA256); ok {
				integrities[lockfile.PlatformKey(file.OS, file.Arch)] = digest
			}
		}
	}
	if len(integrities) == 0 {
		return lockfile.LockEntry{}, fmt.Errorf("go %s has no supported archives in %s", version, goDownloadsURL)
	}
	entry := lockfile.LockEntry{Version: version, Integrities: integrities, Source: goSourceURL}
	if err := writeGoPinRecord(wsRoot, entry); err != nil {
		slog.Debug("toolchain lock: go pin record not written", "version", version, "error", err)
	}
	return entry, nil
}

// goPinRecord is the content of a pin record: the lock entry of one Go release
// as the release index published it.
type goPinRecord struct {
	Version     string            `json:"version"`
	Integrities map[string]string `json:"integrities"`
	Source      string            `json:"source"`
}

// goPinRecordMaxBytes caps the bytes read from a pin record. A record holds
// one digest for each supported platform, under 1 KiB in all.
const goPinRecordMaxBytes = 64 << 10

// goPinRecordPath returns the pin record of a Go release:
// toolchains/go/go-<version>.pin.json under the Putnami home of a command that
// runs in wsRoot, beside the directory the release installs in.
//
// It reports false for a version that is not letters, digits and dots, which
// has no record. The version comes from go.work, a file of the repository, and
// one that holds a path separator must not name a file outside that directory.
func goPinRecordPath(wsRoot, version string) (string, bool) {
	if version == "" {
		return "", false
	}
	for _, r := range version {
		plain := r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !plain {
			return "", false
		}
	}
	home := jobs.PutnamiHome(wsRoot, os.Environ())
	return filepath.Join(home, "toolchains", "go", "go-"+version+".pin.json"), true
}

// readGoPinRecord returns the lock entry the pin record of version holds.
//
// It reports false unless the record is a regular file that parses, names
// version and goSourceURL, and maps at least one supported platform, and no
// other key, to a lowercase 64-hex SHA-256. resolveGoToolchain builds exactly
// such an entry from the release index, so the entry of an accepted record is
// the one a fetch returns, and the lock written from either holds the same
// bytes.
func readGoPinRecord(wsRoot, version string) (lockfile.LockEntry, bool) {
	path, ok := goPinRecordPath(wsRoot, version)
	if !ok {
		return lockfile.LockEntry{}, false
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return lockfile.LockEntry{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return lockfile.LockEntry{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, goPinRecordMaxBytes+1))
	_ = f.Close()
	if err != nil || len(data) > goPinRecordMaxBytes {
		return lockfile.LockEntry{}, false
	}
	var record goPinRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return lockfile.LockEntry{}, false
	}
	if record.Version != version || record.Source != goSourceURL || len(record.Integrities) == 0 {
		return lockfile.LockEntry{}, false
	}
	for platform, digest := range record.Integrities {
		goos, goarch, ok := lockfile.SplitPlatformKey(platform)
		if !ok || !supportedPlatform(goos, goarch) {
			return lockfile.LockEntry{}, false
		}
		if canonical, ok := normalizedSHA256(digest); !ok || canonical != digest {
			return lockfile.LockEntry{}, false
		}
	}
	return lockfile.LockEntry{Version: record.Version, Integrities: record.Integrities, Source: record.Source}, true
}

// writeGoPinRecord writes entry as the pin record of its version. The bytes go
// to a temporary file in the record's directory that is then renamed over the
// record, so a reader finds the previous record or the complete new one, and
// processes that write the same record at once leave one complete file.
func writeGoPinRecord(wsRoot string, entry lockfile.LockEntry) error {
	path, ok := goPinRecordPath(wsRoot, entry.Version)
	if !ok {
		return fmt.Errorf("go %s names no pin record", entry.Version)
	}
	data, err := json.MarshalIndent(goPinRecord{
		Version:     entry.Version,
		Integrities: entry.Integrities,
		Source:      entry.Source,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the go %s pin record: %w", entry.Version, err)
	}
	return shared.AtomicWriteFile(path, append(data, '\n'))
}

type bunRelease struct {
	Assets []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func resolveBunToolchain(ctx context.Context, version string, current lockfile.LockEntry) (lockfile.LockEntry, error) {
	// A lock entry already pinning the declared exact version with vendor
	// digests is reused verbatim: release assets are immutable, so re-fetching
	// them buys nothing and forces an api.github.com round-trip that
	// credential-free environments (the contributor CI gate, offline installs)
	// cannot rely on.
	if current.Version == version && len(current.Integrities) > 0 && current.Source != "" {
		return current, nil
	}
	var release bunRelease
	if err := getJSON(ctx, bunReleaseAPIURL(version), &release); err != nil {
		return lockfile.LockEntry{}, err
	}
	assets := map[string]string{
		"bun-darwin-x64.zip":      "darwin/amd64",
		"bun-darwin-aarch64.zip":  "darwin/arm64",
		"bun-linux-x64.zip":       "linux/amd64",
		"bun-linux-aarch64.zip":   "linux/arm64",
		"bun-windows-x64.zip":     "windows/amd64",
		"bun-windows-aarch64.zip": "windows/arm64",
	}
	integrities := make(map[string]string)
	for _, asset := range release.Assets {
		platform, ok := assets[asset.Name]
		if !ok {
			continue
		}
		if digest, ok := normalizedSHA256(asset.Digest); ok {
			integrities[platform] = digest
		}
	}
	if len(integrities) == 0 {
		return lockfile.LockEntry{}, fmt.Errorf("bun %s release has no supported assets with SHA-256 digests", version)
	}
	return lockfile.LockEntry{Version: version, Integrities: integrities, Source: bunReleaseURL(version)}, nil
}

func supportedPlatform(goos, goarch string) bool {
	return (goos == "darwin" || goos == "linux" || goos == "windows") &&
		(goarch == "amd64" || goarch == "arm64")
}

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

func getJSON(ctx context.Context, url string, dst any) error {
	body, err := get(ctx, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

// toolchainClient returns the client used for toolchain metadata reads. Every
// outbound fetch must carry its own bound: the install context is cancelable on
// SIGINT but never carries a deadline, so a peer that accepts the connection and
// then stalls would otherwise wedge `putnami install` forever. This reuses the
// registry client rather than inventing a parallel mechanism, so toolchain reads
// inherit the same PUTNAMI_HTTP_TIMEOUT knob and user-agent transport as
// extension/template installs and self-update.
//
// http.Client.Timeout is the single owning bound: it covers connect, headers,
// and body reads (the deadline keeps running while the caller drains
// resp.Body), and Go applies it as a deadline *derived from* the request
// context, so it composes with cancellation instead of replacing it — Ctrl-C
// still aborts a fetch immediately.
func toolchainClient() *http.Client {
	if toolchainHTTPClient != nil {
		return toolchainHTTPClient
	}
	return extension.NewRegistryHTTPClient()
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json, text/plain")
	// Callers pass only package-owned Go and Bun release endpoints. The
	// client and endpoint vars are replaceable solely as in-process test seams.
	resp, err := toolchainClient().Do(req) //nolint:gosec // G704: no user-controlled destination
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response read error is reported below
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: HTTP %s", url, resp.Status)
	}
	// Read one byte past the cap so an oversized document is rejected as such
	// instead of being silently truncated into a confusing JSON/checksum parse
	// error at the call site.
	body, err := io.ReadAll(io.LimitReader(resp.Body, toolchainMaxMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if int64(len(body)) > toolchainMaxMetadataBytes {
		return nil, fmt.Errorf("read %s: response exceeds the %d byte metadata limit", url, toolchainMaxMetadataBytes)
	}
	return body, nil
}
