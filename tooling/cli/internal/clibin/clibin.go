// Package clibin resolves the pinned putnami CLI binary into the machine-global
// artifact store. Native Put registry downloads and compatible putnamiw /dl
// mirrors key the same content-addressed cache by SHA-256 under
// <root>/cli/<sha>/. Given the CLI pin from putnami.lock.json, the launcher
// receives a verified, ready-to-exec binary path.
package clibin

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/privatebroker"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const (
	// defaultResolverURL is the historical putnamiw-compatible /dl resolver.
	// It is used only when PUTNAMI_REGISTRY_URL explicitly names that contract.
	defaultResolverURL = "https://putnami.dev/dl"
	// registryURLEnv overrides the resolver endpoint (the same var putnamiw reads).
	registryURLEnv = "PUTNAMI_REGISTRY_URL"
	// archiveBinaryPath is the CLI executable's path inside legacy download
	// archives. The current putnami/cli registry path streams raw binaries, but
	// older releases may still be gzip+tar archives with this path.
	archiveBinaryPath = "compiled/putnami"
	// maxBinarySize bounds the extracted binary so a hostile or corrupt archive
	// can neither exhaust memory nor smuggle an oversized payload.
	maxBinarySize = 256 << 20 // 256 MiB
)

// Failure modes of a resolve, as sentinels a caller can errors.Is against.
// Resolution is fail-closed for the launcher (a pinned workspace must not run
// some other CLI), so the launcher has to tell a user WHICH wall it hit —
// "the registry is unreachable", "the bytes do not match the pin", and "the
// machine-global store would not take them" have three different recoveries.
// They are attached with protocolcli.Classify, which leaves each error's own
// message untouched, so the specific text a user reads is unchanged.
var (
	// ErrNoPlatformDigest: the CLI pin records no integrity digest for the
	// requested platform, so a download could not be verified against anything.
	// Nothing is fetched — unverified bytes are never admitted.
	ErrNoPlatformDigest = errors.New("clibin: CLI pin has no integrity digest for this platform")

	// ErrDownload: the pinned binary could not be fetched from the registry —
	// offline, DNS failure, a proxy, or a non-200 response.
	ErrDownload = errors.New("clibin: could not download the pinned CLI")

	// ErrIntegrity: bytes arrived but are not the pinned CLI — the payload did
	// not hash to the pinned digest, or it was empty/truncated/not a readable
	// archive. Nothing is admitted.
	ErrIntegrity = errors.New("clibin: pinned CLI failed integrity verification")

	// ErrStore: the verified binary could not be admitted into the
	// machine-global artifact store — a read-only or full disk, a permission
	// problem, or a corrupt store tree.
	ErrStore = errors.New("clibin: could not admit the pinned CLI into the artifact store")
)

// Fetcher streams the bytes at url. Injectable so tests drive the full
// extract/verify/admit path without a network.
type Fetcher func(ctx context.Context, url string) (io.ReadCloser, error)

// Resolver resolves pinned CLI binaries into a Store.
type Resolver struct {
	store   *artifactstore.Store
	baseURL string
	fetch   Fetcher
	native  bool
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithBaseURL overrides a legacy /dl resolver endpoint.
func WithBaseURL(u string) Option {
	return func(r *Resolver) {
		if u != "" {
			r.baseURL = u
		}
	}
}

// WithFetcher overrides the byte source (used in tests).
func WithFetcher(f Fetcher) Option {
	return func(r *Resolver) {
		if f != nil {
			r.fetch = f
		}
	}
}

// newLegacyResolver resolves the historical /dl archive endpoint. It remains
// reachable only through NewWorkspace when PUTNAMI_REGISTRY_URL explicitly
// names a /dl mirror, preserving putnamiw compatibility for existing mirrors.
func newLegacyResolver(store *artifactstore.Store, opts ...Option) *Resolver {
	r := &Resolver{
		store:   store,
		baseURL: resolverURL(),
		fetch:   httpFetch,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// NewWorkspace resolves CLI pins from the workspace's native Put registry,
// using the same package and credential boundary as CLI upgrades. The caller
// supplies its current executable so a cold private pin can obtain a credential
// without recursively trying to launch that pin. An existing
// PUTNAMI_REGISTRY_URL ending in /dl retains the wrapper's legacy archive
// contract; an authored registries.put.registry is always native.
func NewWorkspace(store *artifactstore.Store, workspaceRoot, executable string) *Resolver {
	broker, brokerErr := privatebroker.FromEnv(extension.PrivatePutRegistryURLEnv, "/put")
	if _, declared := extension.WorkspaceDeclaredPutRegistryURL(workspaceRoot); !declared && broker == nil && brokerErr == nil && legacyDownloadMirrorConfigured() {
		return newLegacyResolver(store)
	}
	return &Resolver{
		store:   store,
		baseURL: extension.WorkspacePutRegistryURL(workspaceRoot),
		native:  true,
		fetch: func(ctx context.Context, rawURL string) (io.ReadCloser, error) {
			return fetchHTTP(ctx, rawURL, func(req *http.Request) (string, error) {
				return extension.AuthorizeRegistryRequestWithCLI(req, executable)
			})
		},
	}
}

// legacyDownloadMirrorConfigured identifies only the historical resolver base
// used by putnamiw. PUTNAMI_REGISTRY_URL otherwise remains the native Put
// registry fallback used by archive consumers.
func legacyDownloadMirrorConfigured() bool {
	raw := strings.TrimSpace(os.Getenv(registryURLEnv))
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && strings.TrimRight(u.Path, "/") == "/dl"
}

func resolverURL() string {
	if u := os.Getenv(registryURLEnv); u != "" {
		return u
	}
	return defaultResolverURL
}

// Resolve returns an absolute path to a verified, ready-to-exec putnami CLI for
// entry on the given platform, fetching and admitting it into the store on a
// cache miss. It returns ("", nil) when entry is nil (no CLI pinned → the caller
// uses the running binary). Verification is fail-closed: a binary whose SHA-256
// does not match the pin is never admitted, and a pin with no digest for this
// platform returns ErrNoPlatformDigest.
func (r *Resolver) Resolve(ctx context.Context, entry *lockfile.LockEntry, goos, goarch string) (string, error) {
	if entry == nil {
		return "", nil
	}
	sha := strings.ToLower(entry.IntegrityFor(goos, goarch))
	if sha == "" {
		return "", ErrNoPlatformDigest
	}

	// Fast path: already in the shared store (possibly published by putnamiw).
	if r.store.HasCLI(sha) {
		r.store.TouchCLI(sha)
		return r.store.CLIBinary(sha), nil
	}

	// Slow path: download, extract, verify == sha (fail-closed), admit.
	if err := r.validateURL(); err != nil {
		return "", err
	}
	dlURL := r.downloadURL(entry.Version, goos, goarch)
	path, err := r.store.AdmitCLI(sha, func(stageDir string) error {
		return r.stage(ctx, dlURL, r.DownloadURL(entry.Version, goos, goarch), sha, stageDir, goos)
	})
	if err != nil {
		return "", classifyAdmit(err)
	}
	return path, nil
}

// classifyAdmit tags an AdmitCLI failure as ErrStore unless the staging
// function already named the failure mode: admit returns the stage error
// verbatim, so a download or integrity failure must keep its own class rather
// than be relabelled as a store problem.
func classifyAdmit(err error) error {
	if errors.Is(err, ErrDownload) || errors.Is(err, ErrIntegrity) {
		return err
	}
	return protocolcli.Classify(err, ErrStore)
}

// DownloadURL returns the registry URL this resolver fetches the CLI binary from
// for version on the given platform, without any credential the registry URL
// carries. Exposed so `putnami pin` records the source the launcher will later
// fetch from in the committed lock.
func (r *Resolver) DownloadURL(version, goos, goarch string) string {
	return urlFor(extension.RedactRegistryURL(r.baseURL), r.native, version, goos, goarch)
}

// Pin downloads the CLI binary for version on the given platform from the same
// endpoint Resolve uses, computes its SHA-256 — the digest to record in the
// lock's CLI pin — and admits the verified binary to the machine-global store so
// the first launch is a cache hit. It returns the digest.
//
// Unlike Resolve, Pin ESTABLISHES trust (it has no prior digest to check
// against), so it must be driven only by an explicit, trusted user action
// (`putnami pin <version>`). Because the digest is taken from the SAME endpoint
// the launcher later fetches from, a launch is guaranteed to verify against the
// exact bytes pinned here.
func (r *Resolver) Pin(ctx context.Context, version, goos, goarch string) (string, error) {
	if err := r.validateURL(); err != nil {
		return "", err
	}
	dlURL := r.downloadURL(version, goos, goarch)
	body, err := r.fetch(ctx, dlURL)
	if err != nil {
		return "", protocolcli.Classify(fmt.Errorf("clibin: download %s: %w", r.DownloadURL(version, goos, goarch), err), ErrDownload)
	}
	defer body.Close()

	bin, err := extractBinary(body, goos)
	if err != nil {
		return "", protocolcli.Classify(err, ErrIntegrity)
	}
	sum := sha256.Sum256(bin)
	sha := hex.EncodeToString(sum[:])

	if _, err := r.store.AdmitCLI(sha, func(stageDir string) error {
		return os.WriteFile(filepath.Join(stageDir, artifactstore.CLIBinaryName()), bin, 0o755)
	}); err != nil {
		return "", classifyAdmit(err)
	}
	return sha, nil
}

func (r *Resolver) downloadURL(version, goos, goarch string) string {
	return urlFor(r.baseURL, r.native, version, goos, goarch)
}

func urlFor(baseURL string, native bool, version, goos, goarch string) string {
	if version == "" {
		version = "latest"
	}
	q := url.Values{}
	if native {
		q.Set("channel", version)
		q.Set("os", goos)
		q.Set("arch", goarch)
		return strings.TrimRight(baseURL, "/") + "/putnami/cli/download?" + q.Encode()
	}
	q.Set("version", version)
	q.Set("target", goarch)
	q.Set("platform", goos)
	return strings.TrimRight(baseURL, "/") + "/putnami?" + q.Encode()
}

// stage downloads the binary payload from dlURL (shownURL is its redacted
// form, for errors), extracts compiled/putnami
// (compiled/putnami.exe in a Windows archive) from legacy archives when needed,
// verifies it hashes to wantSHA, and writes it as
// <stageDir>/<artifactstore.CLIBinaryName()> (mode 0o755). Returning an error
// aborts the admit, so unverified bytes never land in the machine-global store.
func (r *Resolver) stage(ctx context.Context, dlURL, shownURL, wantSHA, stageDir, goos string) error {
	body, err := r.fetch(ctx, dlURL)
	if err != nil {
		return protocolcli.Classify(fmt.Errorf("clibin: download %s: %w", shownURL, err), ErrDownload)
	}
	defer body.Close()

	bin, err := extractBinary(body, goos)
	if err != nil {
		return protocolcli.Classify(err, ErrIntegrity)
	}

	sum := sha256.Sum256(bin)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, wantSHA) {
		return protocolcli.Classify(
			fmt.Errorf("clibin: integrity check failed: pinned %s, got %s", wantSHA, got), ErrIntegrity)
	}

	if err := os.WriteFile(filepath.Join(stageDir, artifactstore.CLIBinaryName()), bin, 0o755); err != nil {
		return protocolcli.Classify(fmt.Errorf("clibin: write staged binary: %w", err), ErrStore)
	}
	return nil
}

// extractBinary returns the putnami executable bytes from either the current raw
// registry payload or a legacy gzip+tar archive. It errors if an archive entry is
// missing, not a regular file, or if the executable exceeds maxBinarySize.
func extractBinary(r io.Reader, goos string) ([]byte, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("clibin: read binary header: %w", err)
	}
	if len(magic) < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return readBoundedBinary(br)
	}
	return extractArchiveBinary(br, goos)
}

func readBoundedBinary(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, maxBinarySize+1))
	if err != nil {
		return nil, fmt.Errorf("clibin: read binary: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("clibin: binary is empty")
	}
	if n > maxBinarySize {
		return nil, fmt.Errorf("clibin: binary exceeds %d bytes", maxBinarySize)
	}
	return buf.Bytes(), nil
}

func extractArchiveBinary(r io.Reader, goos string) (bin []byte, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("clibin: open gzip: %w", err)
	}
	defer func() {
		if closeErr := gz.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("clibin: close gzip: %w", closeErr)
			bin = nil
		}
	}()

	paths := archiveBinaryPaths(goos)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("clibin: read archive: %w", err)
		}
		clean := path.Clean(hdr.Name)
		if !containsArchivePath(paths, clean) {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("clibin: %s is not a regular file", clean)
		}
		bin, err = readBoundedBinary(tr)
		if err != nil {
			return nil, fmt.Errorf("clibin: extract %s: %w", clean, err)
		}
		return bin, nil
	}
	return nil, fmt.Errorf("clibin: putnami binary not found in archive")
}

func archiveBinaryPaths(goos string) []string {
	if goos == "windows" {
		return []string{"compiled/putnami.exe", archiveBinaryPath}
	}
	return []string{archiveBinaryPath}
}

func containsArchivePath(paths []string, p string) bool {
	for _, candidate := range paths {
		if p == candidate {
			return true
		}
	}
	return false
}

func httpFetch(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	return fetchHTTP(ctx, rawURL, nil)
}

// fetchHTTP GETs rawURL. authorize attaches a credential, or returns why the
// request goes out without one so a refusal can name it. An authorize error
// fails the fetch before the request goes out.
func fetchHTTP(ctx context.Context, rawURL string, authorize func(*http.Request) (string, error)) (io.ReadCloser, error) {
	if err := extension.ValidateRegistryURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	missing := ""
	if authorize != nil {
		if missing, err = authorize(req); err != nil {
			return nil, err
		}
	}
	// Share the hardened registry client (timeout, user-agent) used by extension
	// installs and self-update, so a slow or wedged registry cannot hang the
	// launcher indefinitely, and share their retry: a gateway that resets its
	// upstream answers 502 before any header (an archive upload got one on a
	// main-branch run), and one more attempt serves the same pin.
	resp, err := extension.DoWithRetry(ctx, extension.NewRegistryHTTPClient(), req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if err := extension.RegistryRefusalError(rawURL, resp.StatusCode, missing); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("clibin: resolver returned %s", resp.Status)
	}
	return resp.Body, nil
}

// validateURL rejects an insecure (non-loopback http://) registry before any
// download, mirroring the guard every other registry path uses: a
// tampered PUTNAMI_REGISTRY_URL must not silently downgrade the trust-anchoring
// CLI download to a MITM-able channel. The user can opt in with
// PUTNAMI_ALLOW_INSECURE_REGISTRY=1.
func (r *Resolver) validateURL() error {
	return extension.ValidateRegistryURL(r.baseURL)
}
