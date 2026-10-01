// Package pinnedarchive installs a directory from an archive pinned by its
// SHA-256 digest.
//
// Install downloads the archive a pin names, refuses it unless its SHA-256 is
// the pinned one, extracts it into a sibling staging directory, and publishes
// that directory with one rename. A pin without a valid digest is refused
// before any network access: an unverifiable archive is never downloaded, let
// alone run.
//
// Extraction accepts regular files and directories only. It creates no link:
// an archive that holds a symbolic link, a hard link, a device or a FIFO is
// refused as a whole, and so is an entry whose name is absolute, climbs out
// with "..", or carries a backslash or a colon. Every write goes through an
// os.Root opened on the staging directory, so no entry can land outside it.
//
// Concurrent installers of one destination exclude each other with an
// exclusive filelock lock on "<dest>.lock", which is never removed. A reader
// that does not take the lock sees either no destination or a complete one,
// because the destination only ever appears through the final rename.
//
// A destination may have a writer that does not take the lock, such as a
// bootstrap script that publishes the same directory. Install removes only a
// destination that is not a complete install: one such a writer publishes
// while Install downloads and extracts is kept, and Install reports that it
// installed nothing.
//
// A caller whose pin URL may carry userinfo passes the pin through
// WithoutCredentials first, so that no log line and no error names the
// credentials.
package pinnedarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/robustio"
)

// Format is the container format of a pinned archive.
type Format string

const (
	// TarGz is a gzip-compressed tar archive, the Go distribution format on
	// every platform except Windows.
	TarGz Format = "tar.gz"
	// Zip is a zip archive, the Go distribution format on Windows and the Bun
	// distribution format on every platform.
	Zip Format = "zip"
)

var (
	// ErrNoDigest is returned, before any network access, when a pin carries
	// no SHA-256 digest or one that is not 64 hexadecimal characters.
	ErrNoDigest = errors.New("the pin carries no valid SHA-256 digest")

	// ErrDigestMismatch is returned when the downloaded archive's SHA-256
	// differs from the pinned one. Nothing is extracted.
	ErrDigestMismatch = errors.New("the archive SHA-256 does not match the pin")

	// ErrUnsafeEntry is returned when an archive holds an entry extraction
	// refuses: a link, a device, a FIFO, or a name that could leave the
	// destination.
	ErrUnsafeEntry = errors.New("the archive holds an unsafe entry")

	// ErrTooLarge is returned when an archive or its content exceeds Limits.
	ErrTooLarge = errors.New("the archive exceeds its size limit")

	// ErrIncomplete is returned when the extracted archive does not satisfy
	// Options.Complete, so it is not the install the caller expects.
	ErrIncomplete = errors.New("the extracted archive is not a complete install")
)

// Pin names one archive and the digest it must have.
type Pin struct {
	// URL is the http or https location of the archive.
	URL string
	// SHA256 is the archive's digest as 64 hexadecimal characters.
	SHA256 string
	// Format is the archive's container format.
	Format Format
}

// Limits bound what an archive may cost before its content is trusted.
type Limits struct {
	// ArchiveBytes bounds the download.
	ArchiveBytes int64
	// ExtractedBytes bounds the sum of the extracted file sizes.
	ExtractedBytes int64
	// Entries bounds the number of archive entries.
	Entries int
}

// DefaultLimits are the limits Install applies when Options.Limits is zero.
// A Go distribution is about 70 MB compressed, 260 MB and 15,000 entries
// extracted.
func DefaultLimits() Limits {
	return Limits{ArchiveBytes: 1 << 30, ExtractedBytes: 4 << 30, Entries: 200_000}
}

// Options configures Install and Download.
type Options struct {
	// Client performs the download. Nil uses a client whose timeout is
	// DefaultTimeout.
	Client *http.Client
	// UserAgent, when set, is sent as the User-Agent header.
	UserAgent string
	// Limits bounds the archive. The zero value selects DefaultLimits.
	Limits Limits
	// Complete reports whether dir holds a whole install. Install returns an
	// existing destination only when Complete accepts it, replaces one it
	// rejects, and refuses an archive whose extraction it rejects. Nil
	// accepts any existing directory.
	Complete func(dir string) bool
	// Prepare, when set, shapes the extracted archive into the install: Install
	// calls it once on the staging directory, after the extraction and before
	// Complete judges that directory. An archive that nests its content under
	// a directory of its own is moved into place here. An error refuses the
	// install and nothing is published.
	Prepare func(stage string) error
	// LockPollInterval is how often Install retries a busy lock. Zero selects
	// 100 ms. The wait ends when the context does.
	LockPollInterval time.Duration
}

// DefaultTimeout bounds a download made with the default client.
const DefaultTimeout = 10 * time.Minute

// Install makes dest hold the content of the archive pin names and reports
// whether this call installed it. A complete dest is returned as is, without
// any network access, and one that becomes complete before this call
// publishes is kept. dest's parent directory is created when missing.
func Install(ctx context.Context, pin Pin, dest string, opts Options) (installed bool, err error) {
	digest, err := checkPin(pin)
	if err != nil {
		return false, err
	}
	dest = filepath.Clean(dest)
	parent := filepath.Dir(dest)
	base := filepath.Base(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", parent, err)
	}

	lock, err := acquire(ctx, dest+".lock", opts.LockPollInterval)
	if err != nil {
		return false, fmt.Errorf("lock %s: %w", dest, err)
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil && err == nil {
			err = fmt.Errorf("release the lock on %s: %w", dest, releaseErr)
		}
	}()

	complete := opts.Complete
	if complete == nil {
		complete = isDir
	}
	if complete(dest) {
		return false, nil
	}
	// The lock proves no other installer of dest is running, so staging
	// leftovers are a crashed installer's and are removed here rather than
	// accumulating beside dest.
	removeStaleStaging(parent, base)

	archive, err := os.CreateTemp(parent, "."+base+".archive-*")
	if err != nil {
		return false, fmt.Errorf("create the download file: %w", err)
	}
	archivePath := archive.Name()
	defer func() { _ = os.Remove(archivePath) }()
	downloadErr := download(ctx, pin, digest, archive, opts)
	closeErr := archive.Close()
	if downloadErr != nil {
		return false, downloadErr
	}
	if closeErr != nil {
		return false, fmt.Errorf("write %s: %w", archivePath, closeErr)
	}

	stage, err := os.MkdirTemp(parent, "."+base+".stage-*")
	if err != nil {
		return false, fmt.Errorf("create the staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := Extract(ctx, archivePath, pin.Format, stage, opts.Limits); err != nil {
		return false, err
	}
	if opts.Prepare != nil {
		if err := opts.Prepare(stage); err != nil {
			return false, fmt.Errorf("%w: %s: %w", ErrIncomplete, pin.URL, err)
		}
	}
	if !complete(stage) {
		return false, fmt.Errorf("%w: %s", ErrIncomplete, pin.URL)
	}

	// A writer that does not take the lock may have published dest since the
	// check above. Programs may already run from a complete dest, so it is kept.
	if complete(dest) {
		return false, nil
	}
	if _, statErr := os.Lstat(dest); statErr == nil {
		if err := os.RemoveAll(dest); err != nil {
			return false, fmt.Errorf("remove the incomplete %s: %w", dest, err)
		}
	}
	// An antivirus scanner or the indexer opens freshly extracted executables
	// without sharing delete access; on Windows the rename fails until it lets
	// go of them.
	if err := robustio.Rename(stage, dest); err != nil {
		return false, fmt.Errorf("publish %s: %w", dest, err)
	}
	return true, nil
}

// Download writes the archive pin names to path and returns nil only when its
// SHA-256 is the pinned one. On any error path is removed.
func Download(ctx context.Context, pin Pin, path string, opts Options) (err error) {
	digest, err := checkPin(pin)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := f.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	return download(ctx, pin, digest, f, opts)
}

func download(ctx context.Context, pin Pin, digest string, w io.Writer, opts Options) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pin.URL, nil)
	if err != nil {
		return fmt.Errorf("build the request for %s: %w", pin.URL, err)
	}
	if opts.UserAgent != "" {
		req.Header.Set("User-Agent", opts.UserAgent)
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	//nolint:gosec // G107/G704: the URL is the caller's pin, and its bytes are verified against the pinned digest before use.
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", pin.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", pin.URL, resp.StatusCode)
	}

	limit := limitsOrDefault(opts.Limits).ArchiveBytes
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, hash), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", pin.URL, err)
	}
	if n > limit {
		return fmt.Errorf("%w: %s is larger than %d bytes", ErrTooLarge, pin.URL, limit)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != digest {
		return fmt.Errorf("%w: %s has SHA-256 %s, the pin requires %s", ErrDigestMismatch, pin.URL, got, digest)
	}
	return nil
}

// WithoutCredentials returns pin with the userinfo of its URL removed, and the
// client to download it with. A URL that carries userinfo gets a client that
// sends it as basic authentication with every request to the URL's scheme and
// host, a redirect's included, and with no request to any other, as curl does
// with the userinfo of a URL; any other URL gets nil, the default client of
// Install and Download. Every URL a caller logs from the returned pin, and
// every error Install and Download return for it, then names the archive
// without its credentials. A URL that does not parse is refused without being
// repeated, since it may hold them.
func WithoutCredentials(pin Pin) (Pin, *http.Client, error) {
	parsed, err := url.Parse(pin.URL)
	if err != nil {
		return Pin{}, nil, errors.New("the archive URL does not parse")
	}
	if parsed.User == nil {
		return pin, nil, nil
	}
	transport := basicAuthTransport{scheme: parsed.Scheme, host: parsed.Host, user: parsed.User, base: http.DefaultTransport}
	parsed.User = nil
	pin.URL = parsed.String()
	return pin, &http.Client{Timeout: DefaultTimeout, Transport: transport}, nil
}

// basicAuthTransport sends user as basic authentication with every request to
// scheme and host that carries no Authorization header of its own.
type basicAuthTransport struct {
	scheme, host string
	user         *url.Userinfo
	base         http.RoundTripper
}

func (t basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.EqualFold(req.URL.Scheme, t.scheme) && strings.EqualFold(req.URL.Host, t.host) && req.Header.Get("Authorization") == "" {
		password, _ := t.user.Password()
		req = req.Clone(req.Context())
		req.SetBasicAuth(t.user.Username(), password)
	}
	return t.base.RoundTrip(req)
}

// checkPin validates everything about a pin that can be checked without the
// network, and returns its digest in lowercase.
func checkPin(pin Pin) (string, error) {
	digest := strings.ToLower(strings.TrimSpace(pin.SHA256))
	if len(digest) != sha256.Size*2 {
		return "", fmt.Errorf("%w: %q", ErrNoDigest, pin.SHA256)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("%w: %q", ErrNoDigest, pin.SHA256)
	}
	switch pin.Format {
	case TarGz, Zip:
	default:
		return "", fmt.Errorf("unsupported archive format %q", pin.Format)
	}
	parsed, err := url.Parse(pin.URL)
	if err != nil {
		return "", fmt.Errorf("invalid archive URL %q: %w", pin.URL, err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("unsupported archive URL scheme in %q", pin.URL)
	}
	return digest, nil
}

func limitsOrDefault(l Limits) Limits {
	def := DefaultLimits()
	if l.ArchiveBytes <= 0 {
		l.ArchiveBytes = def.ArchiveBytes
	}
	if l.ExtractedBytes <= 0 {
		l.ExtractedBytes = def.ExtractedBytes
	}
	if l.Entries <= 0 {
		l.Entries = def.Entries
	}
	return l
}

// acquire takes the exclusive install lock, polling without blocking so the
// wait ends with the context.
func acquire(ctx context.Context, path string, interval time.Duration) (*filelock.Lock, error) {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	for {
		lock, err := filelock.Acquire(path, true, true)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// removeStaleStaging removes the download files and staging directories a
// crashed installer of base left in parent.
func removeStaleStaging(parent, base string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "."+base+".archive-") || strings.HasPrefix(name, "."+base+".stage-") {
			_ = os.RemoveAll(filepath.Join(parent, name))
		}
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
