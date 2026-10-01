package versioncmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// ErrCLIActivation is exported so internal/commands can classify an upgrade's
// CLIActivationError with errors.Is without depending on this package's other
// version-update internals.
var ErrCLIActivation = errors.New("could not activate installed CLI")

// installCLIBinary and activateCLIUpdate are the upgrade's install and switch
// steps. Tests replace them to run another platform's strategy or to fail.
var (
	installCLIBinary  = installBinary
	activateCLIUpdate = activateCLI
)

// CLIPath is the active CLI in binDir: the putnami symlink on Unix, the
// putnami.exe copy on Windows.
func CLIPath(binDir string) string {
	return filepath.Join(binDir, pkgmeta.ExecutableName(runtime.GOOS, "putnami"))
}

// CLIActivationError is exported (including its fields) so internal/commands'
// upgrade orchestration — which lives outside this vertical — can inspect
// BinaryPath via errors.As and construct one in tests.
type CLIActivationError struct {
	BinaryPath string
	LinkPath   string
	Cause      error
}

func (e *CLIActivationError) Error() string {
	return fmt.Sprintf("could not activate installed CLI %s via %s: %v", e.BinaryPath, e.LinkPath, e.Cause)
}

func (e *CLIActivationError) Unwrap() error { return e.Cause }

func (e *CLIActivationError) Is(target error) bool { return target == ErrCLIActivation }

// VersionList prints all putnami-* binaries in .putnami/bin/ and marks the
// active one (the symlink target, on Windows the binary putnami.exe copies).
func VersionList(ctx context.Context, binDir string) error {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		if os.IsNotExist(err) {
			iox.Fprintln(os.Stdout, "  No CLI versions installed.")
			return nil
		}
		return fmt.Errorf("read bin directory: %w", err)
	}

	// Determine active target
	activeTarget := activeCLIName(binDir)

	iox.Fprintln(os.Stdout)
	found := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "putnami-") {
			continue
		}

		binary := filepath.Join(binDir, name)
		info, err := os.Stat(binary)
		if err != nil || info.IsDir() {
			continue
		}

		version := getBinaryVersion(ctx, binary)
		label := strings.TrimPrefix(name, "putnami-")
		if runtime.GOOS == "windows" {
			label = strings.TrimSuffix(label, ".exe")
		}

		active := " "
		if name == activeTarget {
			active = "*"
		}
		iox.Fprintf(os.Stdout, "  %s %-20s %s\n", active, label, version)
		found = true
	}

	if !found {
		iox.Fprintln(os.Stdout, "  No CLI versions installed.")
	}
	iox.Fprintln(os.Stdout)
	return nil
}

// VersionUse switches the .putnami/bin/putnami symlink to point to a specific
// putnami-<name> binary in the same directory.
func VersionUse(ctx context.Context, binDir string, name string, global bool) error {
	if name == "" {
		return fmt.Errorf("usage: putnami version use <name>  (e.g., go-dev, ts-dev)")
	}

	targetName := pkgmeta.ExecutableName(runtime.GOOS, "putnami-"+name)
	targetBinary := filepath.Join(binDir, targetName)

	info, err := os.Stat(targetBinary)
	if err != nil {
		if os.IsNotExist(err) {
			return cmderr.NotFoundf("version %q not found (expected %s)", name, targetBinary)
		}
		return fmt.Errorf("stat binary: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, expected a binary", targetBinary)
	}

	binLink := CLIPath(binDir)
	if err := activateCLI(targetName, binLink); err != nil {
		return fmt.Errorf("activate %s: %w", targetName, err)
	}

	version := getBinaryVersion(ctx, targetBinary)
	scope := "local"
	if global {
		scope = "global"
	}
	iox.Fprintf(os.Stdout, "  Active CLI (%s): %s (%s)\n", scope, name, version)
	iox.Fprintf(os.Stdout, "  %s -> %s\n", binLink, targetName)
	return nil
}

// unsafeUpdateEnv lets users opt in to installing an unverified binary
// during the transition period before the resolver advertises an integrity
// hash. It should never be used routinely — it disables the only defense
// against a tampered download.
const unsafeUpdateEnv = "PUTNAMI_UNSAFE_UPDATE"

type VersionUpdateOptions struct {
	Channel string
	DryRun  bool
	// Registries is the workspace `registries` section. Its `put` entry names the
	// projection the CLI archive is served from; without it the endpoint falls
	// back to the environment and then the default.
	Registries map[string]json.RawMessage
}

// VersionUpdateResult describes a successful CLI update. Updated is false when
// the selected release already matches the running CLI, or when the operation
// was a dry run. BinaryPath is the verified executable just installed, which a
// caller can use to continue a multi-phase operation under the new CLI.
type VersionUpdateResult struct {
	Updated    bool
	Version    string
	BinaryPath string
}

// VersionUpdate downloads the latest CLI binary from the resolver, verifies
// integrity, and installs it to .putnami/bin/putnami-go-<version> (with .exe
// on Windows).
//
// The update is fail-closed: if the resolver does not advertise an integrity
// hash (via the X-Integrity header or RFC 9530 Digest header), the install
// is refused. Set PUTNAMI_UNSAFE_UPDATE=1 to bypass — but doing so removes
// the only protection against a MITM or tampered mirror.
func VersionUpdate(ctx context.Context, currentVersion string, binDir string) error {
	_, err := VersionUpdateWithOptions(ctx, currentVersion, binDir, VersionUpdateOptions{Channel: "latest"})
	return err
}

// VersionUpdateWithOptions resolves a CLI release from a registry channel or
// exact version. In dry-run mode it only reports the resolved version.
func VersionUpdateWithOptions(ctx context.Context, currentVersion string, binDir string, opts VersionUpdateOptions) (VersionUpdateResult, error) {
	resolverURL := extension.ResolvePutRegistryURL(opts.Registries)
	if err := extension.ValidateRegistryURL(resolverURL); err != nil {
		return VersionUpdateResult{}, err
	}

	client := extension.NewRegistryHTTPClient()

	iox.Fprintln(os.Stdout, "  Checking for updates...")

	channel := strings.TrimSpace(opts.Channel)
	if channel == "" {
		channel = "latest"
	}
	latestVersion, downloadURL, integrity, err := fetchRelease(ctx, client, resolverURL, channel)
	if err != nil {
		return VersionUpdateResult{}, fmt.Errorf("check for updates: %w", err)
	}

	if latestVersion == currentVersion {
		iox.Fprintf(os.Stdout, "  Already up to date (%s)\n", currentVersion)
		return VersionUpdateResult{}, nil
	}

	if opts.DryRun {
		iox.Fprintf(os.Stdout, "  Would update: %s → %s\n", currentVersion, latestVersion)
		if integrity == "" {
			iox.Fprintln(os.Stdout, "  Integrity: not advertised by resolver")
		} else {
			iox.Fprintln(os.Stdout, "  Integrity: advertised by resolver")
		}
		return VersionUpdateResult{}, nil
	}

	if integrity == "" && os.Getenv(unsafeUpdateEnv) != "1" {
		return VersionUpdateResult{}, fmt.Errorf(
			"resolver did not advertise an integrity hash for %s; refusing to install unverified binary "+
				"(set %s=1 to override)",
			latestVersion, unsafeUpdateEnv,
		)
	}

	iox.Fprintf(os.Stdout, "  Update available: %s → %s\n", currentVersion, latestVersion)

	iox.Fprintln(os.Stdout, "  Downloading...")
	tmpPath, err := downloadBinary(ctx, client, downloadURL)
	if err != nil {
		return VersionUpdateResult{}, fmt.Errorf("download: %w", err)
	}
	defer os.Remove(tmpPath)

	if integrity == "" {
		iox.Fprintf(os.Stderr, "  warning: installing %s without integrity verification (%s=1)\n", latestVersion, unsafeUpdateEnv)
	} else {
		expected, err := normalizeIntegrity(integrity)
		if err != nil {
			return VersionUpdateResult{}, cmderr.InvalidConfigf("invalid integrity %q: %w", integrity, err)
		}
		hash, err := hashFileSHA256(tmpPath)
		if err != nil {
			return VersionUpdateResult{}, fmt.Errorf("hash binary: %w", err)
		}
		if !strings.EqualFold(hash, expected) {
			return VersionUpdateResult{}, fmt.Errorf("integrity check failed: expected %s, got %s", expected, hash)
		}
		iox.Fprintln(os.Stdout, "  Integrity verified")
	}

	binaryPath, cleanup, err := extractDownloadedBinary(tmpPath)
	if err != nil {
		return VersionUpdateResult{}, fmt.Errorf("extract: %w", err)
	}
	defer cleanup()

	if err := os.Chmod(binaryPath, 0o755); err != nil {
		return VersionUpdateResult{}, fmt.Errorf("chmod: %w", err)
	}

	if err := verifyDownloadedBinaryStamp(ctx, binaryPath, latestVersion); err != nil {
		return VersionUpdateResult{}, err
	}

	// Install as putnami-go-<version> in binDir
	targetName := pkgmeta.ExecutableName(runtime.GOOS, fmt.Sprintf("putnami-go-%s", latestVersion))
	targetPath := filepath.Join(binDir, targetName)

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return VersionUpdateResult{}, fmt.Errorf("create bin directory: %w", err)
	}

	if err := installCLIBinary(binaryPath, targetPath); err != nil {
		return VersionUpdateResult{}, fmt.Errorf("install binary: %w", err)
	}

	iox.Fprintf(os.Stdout, "  Installed %s\n", targetPath)

	// Update symlink to the new version
	binLink := CLIPath(binDir)
	if err := activateCLIUpdate(targetName, binLink); err != nil {
		return VersionUpdateResult{}, &CLIActivationError{
			BinaryPath: targetPath,
			LinkPath:   binLink,
			Cause:      err,
		}
	}
	iox.Fprintf(os.Stdout, "  Active: %s -> %s\n", binLink, targetName)

	return VersionUpdateResult{Updated: true, Version: latestVersion, BinaryPath: targetPath}, nil
}

// getBinaryVersion runs the binary with --version and returns the output. It
// runs under ctx so a canceled command (e.g. a hung or wedged binary) aborts
// instead of blocking the listing. A listed binary may be a build of the
// workspace's source, so running it counts as repository code
// (runcredential.MarkRepositoryCodeStarted).
func getBinaryVersion(ctx context.Context, binaryPath string) string {
	runcredential.MarkRepositoryCodeStarted(filepath.Base(binaryPath) + " --version")
	//nolint:gosec // G702: binaryPath is a putnami CLI binary this command lists/installs and runs by design; on the upgrade path its sha256 is verified before here. Args go straight to exec — no shell.
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	cmd.Env = append(os.Environ(), launch.NoRelaunchEnv+"=1", launch.LaunchedEnv+"=")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// verifyDownloadedBinaryStamp refuses a downloaded binary whose embedded
// --version disagrees with the channel-resolved version. The producer refuses
// to publish such a binary, but a registry can still resolve a tag to a
// prior commit's artifact when release archives were not uploaded.
// Best-effort: a binary that cannot report a version is not blocked, so older
// archives and future --version format changes can still install.
func verifyDownloadedBinaryStamp(ctx context.Context, binaryPath, resolvedVersion string) error {
	reported := getBinaryVersion(ctx, binaryPath)
	if reported == "" || reported == "unknown" {
		return nil
	}
	fields := strings.Fields(reported)
	got := strings.TrimPrefix(fields[len(fields)-1], "v")
	if got == strings.TrimPrefix(resolvedVersion, "v") {
		return nil
	}
	return fmt.Errorf(
		"resolved %s but the downloaded binary reports %q; refusing to install stale build (the release-archives channel may not have been uploaded)",
		resolvedVersion, reported,
	)
}

// ─── HTTP helpers ────────────────────────────────────────────────────────────

func fetchLatestRelease(ctx context.Context, client *http.Client, baseURL string) (version, downloadURL, integrity string, err error) {
	return fetchRelease(ctx, client, baseURL, "latest")
}

func fetchRelease(ctx context.Context, client *http.Client, baseURL string, channel string) (version, downloadURL, integrity string, err error) {
	params := url.Values{}
	params.Set("channel", channel)
	params.Set("os", runtime.GOOS)
	params.Set("arch", runtime.GOARCH)
	dlURL := fmt.Sprintf("%s/putnami/cli/download?%s", strings.TrimRight(baseURL, "/"), params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return "", "", "", err
	}
	// The CLI is an archive member of the put projection, and that projection can
	// be private. Send the user's credential when the cloud resolves one.
	missing, err := extension.AuthorizeRegistryRequest(req)
	if err != nil {
		return "", "", "", err
	}

	resp, err := extension.DoWithRetry(ctx, client, req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if authErr := extension.RegistryRefusalError(baseURL, resp.StatusCode, missing); authErr != nil {
			return "", "", "", authErr
		}
		return "", "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	resolvedVersion := resp.Header.Get("X-Resolved-Version")
	if resolvedVersion == "" {
		return "", "", "", fmt.Errorf("no version resolved")
	}

	// The resolver may advertise the binary's SHA-256 either via X-Integrity
	// or the standard RFC 9530 Digest header. We accept either; downstream
	// verification fails closed if neither is present.
	integrity = extension.ReadAdvertisedIntegrity(resp.Header)

	// The download endpoint streams the binary directly — return the URL
	// so downloadBinary can re-fetch it.
	return resolvedVersion, dlURL, integrity, nil
}

// normalizeIntegrity converts the resolver-supplied integrity string into a
// plain lowercase hex SHA-256 digest. It accepts:
//   - "sha256:<hex>" (Docker / OCI convention)
//   - "sha-256:<hex>" (some registries)
//   - "<hex>" (raw)
//
// Other forms (base64 SRI, signatures) are rejected — we want a single
// canonical comparison path against hashFileSHA256.
func normalizeIntegrity(integrity string) (string, error) {
	v := strings.TrimSpace(integrity)
	for _, prefix := range []string{"sha256:", "sha-256:", "sha256-", "sha-256-"} {
		if rest, ok := strings.CutPrefix(strings.ToLower(v), prefix); ok {
			v = rest
			break
		}
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) != 64 {
		return "", fmt.Errorf("expected 64-character hex SHA-256, got %d characters", len(v))
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", fmt.Errorf("not valid hex: %w", err)
	}
	return v, nil
}

// extractDownloadedBinary returns a path to a runnable putnami binary from a
// downloaded asset. The registry serves either a raw binary or a .tar.gz
// containing compiled/putnami plus auxiliary assets. Format is detected by
// sniffing the gzip magic bytes.
//
// The returned cleanup removes any temp directory created for extraction; it
// is a no-op when the asset is a raw binary.
func extractDownloadedBinary(assetPath string) (string, func(), error) {
	noopCleanup := func() {}

	f, err := os.Open(assetPath)
	if err != nil {
		return "", noopCleanup, fmt.Errorf("open asset: %w", err)
	}
	var magic [2]byte
	n, _ := io.ReadFull(f, magic[:])
	f.Close()
	if n < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return assetPath, noopCleanup, nil
	}

	extractDir, err := os.MkdirTemp("", "putnami-update-extract-*")
	if err != nil {
		return "", noopCleanup, fmt.Errorf("create extract dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(extractDir) }

	if err := extension.ExtractTarGz(assetPath, extractDir); err != nil {
		cleanup()
		return "", noopCleanup, fmt.Errorf("extract archive: %w", err)
	}

	binName := "putnami"
	if runtime.GOOS == "windows" {
		binName = "putnami.exe"
	}

	var found string
	walkErr := filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if filepath.Base(path) == binName {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		cleanup()
		return "", noopCleanup, fmt.Errorf("locate %s in archive: %w", binName, walkErr)
	}
	if found == "" {
		cleanup()
		return "", noopCleanup, fmt.Errorf("archive does not contain %s", binName)
	}
	return found, cleanup, nil
}

// maxBinaryDownloadSize caps the self-update payload at 500 MiB. The
// release archive is well under 100 MiB today; the limit exists to keep
// a hostile or malfunctioning resolver from filling the temp filesystem.
// It matches the extension downloader's cap for consistency.
// A var so the oversize test can exercise the cap without streaming 500 MiB.
var maxBinaryDownloadSize int64 = 500 * 1024 * 1024

func downloadBinary(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// Same endpoint as the resolve above, so the same credential: a projection
	// that authenticates the resolve authenticates the transfer too.
	missing, err := extension.AuthorizeRegistryRequest(req)
	if err != nil {
		return "", err
	}

	resp, err := extension.DoWithRetry(ctx, client, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if authErr := extension.RegistryRefusalError(url, resp.StatusCode, missing); authErr != nil {
			return "", authErr
		}
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp("", "putnami-update-*")
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	// Read one byte past the cap so we can detect "claimed bigger than max"
	// even when Content-Length is absent or lies.
	limited := io.LimitReader(resp.Body, maxBinaryDownloadSize+1)
	n, err := io.Copy(tmpFile, limited)
	if err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	if n > maxBinaryDownloadSize {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("download exceeds %d MB limit", maxBinaryDownloadSize/(1024*1024))
	}

	return tmpFile.Name(), nil
}

// stageBinary copies src to an executable staging file next to dst and
// returns its path, for the caller to rename into place.
func stageBinary(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()

	staging, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".new-*")
	if err != nil {
		return "", err
	}
	stagingPath := staging.Name()

	if _, err := io.Copy(staging, in); err != nil {
		staging.Close()
		os.Remove(stagingPath)
		return "", err
	}
	if err := staging.Close(); err != nil {
		os.Remove(stagingPath)
		return "", err
	}
	if err := os.Chmod(stagingPath, 0o755); err != nil {
		os.Remove(stagingPath)
		return "", err
	}
	return stagingPath, nil
}

// replaceSymlink points link at target by renaming a staged symlink over it,
// so there is no window where the link is missing and concurrent invocations
// never see a half-updated path.
func replaceSymlink(target, link string) error {
	staging := fmt.Sprintf("%s.new-%d", link, os.Getpid())
	os.Remove(staging)
	if err := os.Symlink(target, staging); err != nil {
		return err
	}
	if err := os.Rename(staging, link); err != nil {
		os.Remove(staging)
		return err
	}
	return nil
}

func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
