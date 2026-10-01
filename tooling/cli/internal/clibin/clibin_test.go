package clibin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// makeArchive builds a gzip+tar archive containing compiled/putnami=content and
// returns the archive bytes plus the SHA-256 of the binary (the cli/<sha>/ key).
func makeArchive(t *testing.T, content string) (archive []byte, binSHA string) {
	t.Helper()
	return makeArchiveAt(t, "compiled/putnami", content)
}

// makeArchiveAt is makeArchive with the binary at name.
func makeArchiveAt(t *testing.T, name, content string) (archive []byte, binSHA string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(content)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func fetcherFor(archive []byte, calls *int) Fetcher {
	return func(context.Context, string) (io.ReadCloser, error) {
		if calls != nil {
			*calls++
		}
		return io.NopCloser(bytes.NewReader(archive)), nil
	}
}

func entryFor(version, goos, goarch, sha string) *lockfile.LockEntry {
	e := &lockfile.LockEntry{Version: version}
	e.SetPlatformIntegrity(lockfile.PlatformKey(goos, goarch), sha)
	return e
}

func TestResolveDownloadsVerifiesAdmitsThenFastPath(t *testing.T) {
	archive, sha := makeArchive(t, "putnami-binary-v1")
	store := artifactstore.New(t.TempDir())
	calls := 0
	r := newLegacyResolver(store, WithFetcher(fetcherFor(archive, &calls)), WithBaseURL("https://example.test/dl"))

	entry := entryFor("1.4.2", "linux", "amd64", sha)
	got, err := r.Resolve(context.Background(), entry, "linux", "amd64")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != store.CLIBinary(sha) {
		t.Errorf("path = %q, want %q", got, store.CLIBinary(sha))
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "putnami-binary-v1" {
		t.Fatalf("admitted binary wrong: data=%q err=%v", data, err)
	}
	// Windows has no execute permission bit; putnami.exe runs by its name.
	if info, _ := os.Stat(got); runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Error("admitted binary is not executable")
	}

	// Second resolve hits the shared store — no second download.
	if _, err := r.Resolve(context.Background(), entry, "linux", "amd64"); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if calls != 1 {
		t.Errorf("fetcher called %d times, want 1 (fast path must skip download)", calls)
	}
}

// TestResolveFastPathSharesPutnamiwCache proves the Go resolver reuses a blob
// the bash wrapper already published at cli/<sha>/putnami, without downloading.
func TestResolveFastPathSharesPutnamiwCache(t *testing.T) {
	store := artifactstore.New(t.TempDir())
	sha := "2222222222222222222222222222222222222222222222222222222222222222"

	if err := os.MkdirAll(store.CLIDir(sha), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.CLIBinary(sha), []byte("from-putnamiw"), 0o755); err != nil {
		t.Fatal(err)
	}

	failFetch := func(context.Context, string) (io.ReadCloser, error) {
		return nil, errors.New("must not download on a shared-cache hit")
	}
	r := newLegacyResolver(store, WithFetcher(failFetch))

	got, err := r.Resolve(context.Background(), entryFor("1.0.0", "linux", "amd64", sha), "linux", "amd64")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != store.CLIBinary(sha) {
		t.Errorf("path = %q, want %q", got, store.CLIBinary(sha))
	}
}

func TestResolveIntegrityMismatchFailsClosed(t *testing.T) {
	archive, realSHA := makeArchive(t, "real-binary")
	store := artifactstore.New(t.TempDir())
	r := newLegacyResolver(store, WithFetcher(fetcherFor(archive, nil)))

	wrongSHA := "3333333333333333333333333333333333333333333333333333333333333333"
	if wrongSHA == realSHA {
		t.Fatal("test setup: wrongSHA collided with realSHA")
	}
	_, err := r.Resolve(context.Background(), entryFor("1.0.0", "linux", "amd64", wrongSHA), "linux", "amd64")
	if err == nil {
		t.Fatal("expected integrity failure")
	}
	// The launcher fails a pinned run closed and has to say WHY: an integrity
	// failure sends the user to `putnami pin`, a download failure does not.
	if !errors.Is(err, ErrIntegrity) {
		t.Errorf("err = %v, want ErrIntegrity", err)
	}
	if errors.Is(err, ErrDownload) || errors.Is(err, ErrStore) {
		t.Errorf("err = %v must not also claim a download or store failure", err)
	}
	if store.HasCLI(wrongSHA) {
		t.Error("a failed integrity check must admit nothing to the shared store")
	}
}

// A corrupt payload (right digest impossible to reach, unreadable archive) is an
// integrity failure, not a download one: the bytes arrived, they are just not a
// pinned CLI.
func TestResolveCorruptPayloadIsIntegrityFailure(t *testing.T) {
	store := artifactstore.New(t.TempDir())
	// gzip magic with a truncated body: extraction fails after a clean fetch.
	r := newLegacyResolver(store, WithFetcher(func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte{0x1f, 0x8b, 0x08, 0x00})), nil
	}))
	sha := "6666666666666666666666666666666666666666666666666666666666666666"
	_, err := r.Resolve(context.Background(), entryFor("1.0.0", "linux", "amd64", sha), "linux", "amd64")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if store.HasCLI(sha) {
		t.Error("a corrupt payload must admit nothing to the shared store")
	}
}

// An unreachable registry is a download failure, so the launcher can report
// "offline" rather than accusing the pin of being wrong.
func TestResolveDownloadFailureIsClassified(t *testing.T) {
	store := artifactstore.New(t.TempDir())
	r := newLegacyResolver(store, WithFetcher(func(context.Context, string) (io.ReadCloser, error) {
		return nil, errors.New("dial tcp: no route to host")
	}))
	sha := "7777777777777777777777777777777777777777777777777777777777777777"
	_, err := r.Resolve(context.Background(), entryFor("1.0.0", "linux", "amd64", sha), "linux", "amd64")
	if !errors.Is(err, ErrDownload) {
		t.Fatalf("err = %v, want ErrDownload", err)
	}
	if errors.Is(err, ErrIntegrity) || errors.Is(err, ErrStore) {
		t.Errorf("err = %v must not also claim an integrity or store failure", err)
	}
	if !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("err = %v must keep the underlying cause", err)
	}
}

// A store that cannot accept the verified bytes is a local failure, distinct
// from both the network and the pin.
func TestResolveStoreFailureIsClassified(t *testing.T) {
	// A store root that is a FILE makes every staging mkdir fail, without
	// depending on permissions (root ignores 0o500 dirs).
	root := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive, sha := makeArchive(t, "verified-bytes")
	r := newLegacyResolver(artifactstore.New(root), WithFetcher(fetcherFor(archive, nil)))

	_, err := r.Resolve(context.Background(), entryFor("1.0.0", "linux", "amd64", sha), "linux", "amd64")
	if err == nil {
		t.Fatal("expected a store failure")
	}
	if !errors.Is(err, ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}
	if errors.Is(err, ErrDownload) || errors.Is(err, ErrIntegrity) {
		t.Errorf("err = %v must not blame the network or the pin", err)
	}
}

func TestResolveNoPlatformDigest(t *testing.T) {
	store := artifactstore.New(t.TempDir())
	r := newLegacyResolver(store, WithFetcher(func(context.Context, string) (io.ReadCloser, error) {
		return nil, errors.New("must not download")
	}))
	entry := entryFor("1.0.0", "linux", "amd64", "4444444444444444444444444444444444444444444444444444444444444444")
	if _, err := r.Resolve(context.Background(), entry, "darwin", "arm64"); !errors.Is(err, ErrNoPlatformDigest) {
		t.Errorf("err = %v, want ErrNoPlatformDigest", err)
	}
}

func TestResolveNilEntry(t *testing.T) {
	r := newLegacyResolver(artifactstore.New(t.TempDir()))
	got, err := r.Resolve(context.Background(), nil, "linux", "amd64")
	if err != nil || got != "" {
		t.Errorf("nil entry → (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestPinComputesDigestAdmitsAndIsResolvable(t *testing.T) {
	archive, sha := makeArchive(t, "pinned-binary-bytes")
	store := artifactstore.New(t.TempDir())
	calls := 0
	r := newLegacyResolver(store, WithFetcher(fetcherFor(archive, &calls)), WithBaseURL("https://example.test/dl"))

	got, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if got != sha {
		t.Errorf("Pin digest = %q, want %q", got, sha)
	}
	if !store.HasCLI(sha) {
		t.Error("Pin must admit the binary to the store")
	}
	if data, _ := os.ReadFile(store.CLIBinary(sha)); string(data) != "pinned-binary-bytes" {
		t.Errorf("admitted binary = %q, want pinned-binary-bytes", data)
	}

	// The pin Pin produced is exactly what Resolve verifies: resolving the same
	// digest is a fast-path cache hit with no second download.
	entry := entryFor("1.2.3", "linux", "amd64", sha)
	path, err := r.Resolve(context.Background(), entry, "linux", "amd64")
	if err != nil {
		t.Fatalf("Resolve after Pin: %v", err)
	}
	if path != store.CLIBinary(sha) {
		t.Errorf("resolved path = %q, want %q", path, store.CLIBinary(sha))
	}
	if calls != 1 {
		t.Errorf("fetcher called %d times, want 1 (Pin downloads; Resolve is a cache hit)", calls)
	}
}

// A Windows archive carries the CLI at compiled/putnami.exe. Pin reads it from
// any host, admits it under this machine's entry name, and Resolve finds it.
func TestPinReadsTheWindowsArchiveExecutable(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-executables-by-name", "pin-reads-the-windows-archive-executable")
	archive, sha := makeArchiveAt(t, "compiled/putnami.exe", "windows-binary-bytes")
	store := artifactstore.New(t.TempDir())
	calls := 0
	r := newLegacyResolver(store, WithFetcher(fetcherFor(archive, &calls)), WithBaseURL("https://example.test/dl"))

	got, err := r.Pin(context.Background(), "1.2.3", "windows", "amd64")
	if err != nil {
		t.Fatalf("Pin windows/amd64: %v", err)
	}
	if got != sha {
		t.Errorf("Pin digest = %q, want the digest of compiled/putnami.exe %q", got, sha)
	}
	if filepath.Base(store.CLIBinary(sha)) != artifactstore.CLIBinaryName() {
		t.Errorf("admitted binary %q is not named %q", store.CLIBinary(sha), artifactstore.CLIBinaryName())
	}
	if data, _ := os.ReadFile(store.CLIBinary(sha)); string(data) != "windows-binary-bytes" {
		t.Errorf("admitted binary = %q, want windows-binary-bytes", data)
	}
	path, err := r.Resolve(context.Background(), entryFor("1.2.3", "windows", "amd64", sha), "windows", "amd64")
	if err != nil || path != store.CLIBinary(sha) {
		t.Fatalf("Resolve after Pin = (%q, %v), want %q", path, err, store.CLIBinary(sha))
	}
	if calls != 1 {
		t.Errorf("fetcher called %d times, want 1 (Pin downloads; Resolve is a cache hit)", calls)
	}

	// Only a Windows archive may carry the .exe name: a Linux pin of the same
	// archive finds no compiled/putnami.
	if _, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("linux Pin of a Windows archive = %v, want ErrIntegrity", err)
	}
}

func TestPinAcceptsRawRegistryBinary(t *testing.T) {
	raw := []byte("raw-putnami-binary")
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	store := artifactstore.New(t.TempDir())
	calls := 0
	r := newLegacyResolver(store, WithFetcher(func(context.Context, string) (io.ReadCloser, error) {
		calls++
		return io.NopCloser(bytes.NewReader(raw)), nil
	}), WithBaseURL("https://example.test/dl"))

	got, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64")
	if err != nil {
		t.Fatalf("Pin raw binary: %v", err)
	}
	if got != sha {
		t.Errorf("Pin digest = %q, want raw binary sha %q", got, sha)
	}
	data, err := os.ReadFile(store.CLIBinary(sha))
	if err != nil {
		t.Fatalf("read admitted raw binary: %v", err)
	}
	if string(data) != string(raw) {
		t.Errorf("admitted binary = %q, want %q", data, raw)
	}
	if calls != 1 {
		t.Errorf("fetcher called %d times, want 1", calls)
	}
}

func TestRejectsInsecureRegistry(t *testing.T) {
	t.Setenv("PUTNAMI_ALLOW_INSECURE_REGISTRY", "0")
	store := artifactstore.New(t.TempDir())
	r := newLegacyResolver(store, WithBaseURL("http://registry.example.com/dl"), WithFetcher(func(context.Context, string) (io.ReadCloser, error) {
		t.Fatal("must not fetch from a non-loopback http registry")
		return nil, nil
	}))

	if _, err := r.Pin(context.Background(), "1.2.3", "linux", "amd64"); err == nil {
		t.Error("Pin must reject a non-loopback http registry")
	}
	entry := entryFor("1.2.3", "linux", "amd64", "5555555555555555555555555555555555555555555555555555555555555555")
	if _, err := r.Resolve(context.Background(), entry, "linux", "amd64"); err == nil {
		t.Error("Resolve must reject a non-loopback http registry on the download path")
	}
}

func TestHTTPFetchRejectsInsecureRegistryDirectly(t *testing.T) {
	t.Setenv("PUTNAMI_ALLOW_INSECURE_REGISTRY", "0")
	if _, err := httpFetch(context.Background(), "http://registry.example.com/dl/putnami"); err == nil {
		t.Fatal("httpFetch must reject an insecure registry even when called outside Resolver")
	}
}

func TestExtractBinaryMissing(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "compiled/other", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	if _, err := extractBinary(&buf, "linux"); err == nil {
		t.Error("expected error when compiled/putnami is absent")
	}
}

func TestDownloadURL(t *testing.T) {
	r := newLegacyResolver(artifactstore.New(t.TempDir()), WithBaseURL("https://putnami.dev/dl/"))
	got := r.downloadURL("1.4.2", "darwin", "arm64")
	want := "https://putnami.dev/dl/putnami?platform=darwin&target=arm64&version=1.4.2"
	if got != want {
		t.Errorf("downloadURL = %q, want %q", got, want)
	}
}
