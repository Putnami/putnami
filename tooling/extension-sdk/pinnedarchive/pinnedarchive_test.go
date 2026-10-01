package pinnedarchive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type archiveEntry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	link     string
}

func tarGzArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Mode: mode, Typeflag: typeflag, Linkname: e.link}
		if typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := fs.FileMode(e.mode)
		if mode == 0 {
			mode = 0o644
		}
		switch e.typeflag {
		case tar.TypeDir:
			mode |= fs.ModeDir
		case tar.TypeSymlink:
			mode |= fs.ModeSymlink
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		body := e.body
		if e.typeflag == tar.TypeSymlink {
			body = e.link
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// archiveServer serves one archive and counts the requests it answers.
func archiveServer(t *testing.T, data []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func goLayout() []archiveEntry {
	return []archiveEntry{
		{name: "go/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "go/VERSION", body: "go1.99.0\ntime 2026-01-01T00:00:00Z\n"},
		{name: "go/bin/go", body: "#!/bin/sh\necho go\n", mode: 0o755},
		{name: "go/src/fmt/print.go", body: "package fmt\n"},
	}
}

func goComplete(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "go", "bin", "go"))
	return err == nil && info.Mode().IsRegular()
}

// siblings lists the names beside dest, the lock file included.
func siblings(t *testing.T, parent string) []string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestInstallRefusesAPinWithoutDigestBeforeAnyRequest(t *testing.T) {
	server, requests := archiveServer(t, tarGzArchive(t, goLayout()...))
	parent := t.TempDir()
	for _, digest := range []string{"", "   ", "abc", strings.Repeat("z", 64), "sha256:" + strings.Repeat("a", 64)} {
		_, err := Install(context.Background(), Pin{URL: server.URL + "/go.tar.gz", SHA256: digest, Format: TarGz},
			filepath.Join(parent, "go-1.99.0"), Options{})
		if !errors.Is(err, ErrNoDigest) {
			t.Errorf("Install with digest %q: err = %v, want ErrNoDigest", digest, err)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("a pin without a digest reached the network %d time(s)", got)
	}
	if names := siblings(t, parent); len(names) != 0 {
		t.Fatalf("a refused pin left %v behind", names)
	}
}

func TestInstallRefusesAnArchiveWhoseDigestDiffers(t *testing.T) {
	data := tarGzArchive(t, goLayout()...)
	server, _ := archiveServer(t, data)
	parent := t.TempDir()
	dest := filepath.Join(parent, "go-1.99.0")
	wrong := sha(append([]byte("tampered"), data...))

	installed, err := Install(context.Background(), Pin{URL: server.URL + "/go.tar.gz", SHA256: wrong, Format: TarGz},
		dest, Options{Complete: goComplete})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Install: err = %v, want ErrDigestMismatch", err)
	}
	if installed {
		t.Fatal("Install reported an install for a refused archive")
	}
	if !strings.Contains(err.Error(), sha(data)) || !strings.Contains(err.Error(), wrong) {
		t.Errorf("the refusal does not name both digests: %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("a refused archive was installed at %s: %v", dest, statErr)
	}
	if names := siblings(t, parent); len(names) != 1 || names[0] != "go-1.99.0.lock" {
		t.Fatalf("a refused archive left %v behind; only the lock file may remain", names)
	}
}

func TestInstallExtractsATarGzAndReusesIt(t *testing.T) {
	data := tarGzArchive(t, goLayout()...)
	server, requests := archiveServer(t, data)
	dest := filepath.Join(t.TempDir(), "libs", "go-1.99.0")
	pin := Pin{URL: server.URL + "/go1.99.0.linux-amd64.tar.gz", SHA256: strings.ToUpper(sha(data)), Format: TarGz}

	installed, err := Install(context.Background(), pin, dest, Options{Complete: goComplete, UserAgent: "putnami-test"})
	if err != nil || !installed {
		t.Fatalf("Install = (%v, %v), want (true, nil)", installed, err)
	}
	version, err := os.ReadFile(filepath.Join(dest, "go", "VERSION"))
	if err != nil || !strings.HasPrefix(string(version), "go1.99.0\n") {
		t.Fatalf("go/VERSION = %q, %v", version, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dest, "go", "bin", "go"))
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("go/bin/go mode = %v, %v; want 0755", info, err)
		}
		info, err = os.Stat(filepath.Join(dest, "go", "src", "fmt", "print.go"))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("print.go mode = %v, %v; want 0644", info, err)
		}
	}

	installed, err = Install(context.Background(), pin, dest, Options{Complete: goComplete})
	if err != nil || installed {
		t.Fatalf("second Install = (%v, %v), want (false, nil)", installed, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("the archive was downloaded %d times, want once", got)
	}
}

// The zip path is the Windows distribution format; it is exercised on every
// platform because extraction does not depend on the host.
func TestInstallExtractsAZip(t *testing.T) {
	entries := []archiveEntry{
		{name: "go/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "go/VERSION", body: "go1.99.0\n"},
		{name: "go/bin/go", body: "MZ fake executable", mode: 0o755},
		// No directory entry for go/pkg: extraction creates parents itself.
		{name: "go/pkg/tool/compile", body: "tool", mode: 0o755},
	}
	data := zipArchive(t, entries...)
	server, _ := archiveServer(t, data)
	dest := filepath.Join(t.TempDir(), "go-1.99.0")

	installed, err := Install(context.Background(), Pin{URL: server.URL + "/go1.99.0.windows-amd64.zip", SHA256: sha(data), Format: Zip},
		dest, Options{Complete: goComplete})
	if err != nil || !installed {
		t.Fatalf("Install = (%v, %v), want (true, nil)", installed, err)
	}
	for _, e := range entries[1:] {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(e.name)))
		if err != nil || string(got) != e.body {
			t.Errorf("%s = %q, %v; want %q", e.name, got, err, e.body)
		}
	}
}

func TestExtractRefusesUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry archiveEntry
	}{
		{"parent traversal", archiveEntry{name: "../evil", body: "x"}},
		{"nested traversal", archiveEntry{name: "go/../../evil", body: "x"}},
		{"absolute path", archiveEntry{name: "/tmp/evil", body: "x"}},
		{"backslash", archiveEntry{name: "go\\..\\..\\evil", body: "x"}},
		{"drive or stream", archiveEntry{name: "c:evil", body: "x"}},
		{"drive path", archiveEntry{name: "C:/Windows/evil", body: "x"}},
		{"UNC path", archiveEntry{name: `\\server\share\evil`, body: "x"}},
		{"slash UNC path", archiveEntry{name: "//server/share/evil", body: "x"}},
		{"symbolic link", archiveEntry{name: "go/link", typeflag: tar.TypeSymlink, link: "/etc/passwd"}},
		{"relative symbolic link", archiveEntry{name: "go/link", typeflag: tar.TypeSymlink, link: "bin/go"}},
		{"hard link", archiveEntry{name: "go/hard", typeflag: tar.TypeLink, link: "go/VERSION"}},
		{"fifo", archiveEntry{name: "go/fifo", typeflag: tar.TypeFifo}},
	} {
		t.Run("tar.gz "+tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "a.tar.gz")
			if err := os.WriteFile(archive, tarGzArchive(t, append(goLayout(), tc.entry)...), 0o644); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			dir := filepath.Join(outside, "out")
			err := Extract(context.Background(), archive, TarGz, dir, Limits{})
			if !errors.Is(err, ErrUnsafeEntry) {
				t.Fatalf("Extract: err = %v, want ErrUnsafeEntry", err)
			}
			assertNothingEscaped(t, outside)
		})
		if tc.entry.typeflag == tar.TypeLink || tc.entry.typeflag == tar.TypeFifo {
			continue // zip has no spelling for these
		}
		t.Run("zip "+tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "a.zip")
			if err := os.WriteFile(archive, zipArchive(t, append(goLayout(), tc.entry)...), 0o644); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			dir := filepath.Join(outside, "out")
			err := Extract(context.Background(), archive, Zip, dir, Limits{})
			if !errors.Is(err, ErrUnsafeEntry) {
				t.Fatalf("Extract: err = %v, want ErrUnsafeEntry", err)
			}
			assertNothingEscaped(t, outside)
		})
	}
}

// assertNothingEscaped fails when extraction wrote beside its directory, or
// created a link anywhere.
func assertNothingEscaped(t *testing.T, outside string) {
	t.Helper()
	err := filepath.WalkDir(outside, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("extraction created the link %s", path)
		}
		rel, _ := filepath.Rel(outside, path)
		if rel != "." && rel != "out" && !strings.HasPrefix(rel, "out"+string(filepath.Separator)) {
			t.Errorf("extraction wrote %s outside its directory", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExtractAcceptsARootDirectoryEntryAndPaxGlobalHeader(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, hdr := range []*tar.Header{
		{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "abc"}},
		{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755},
		{Typeflag: tar.TypeReg, Name: "./go/VERSION", Mode: 0o644, Size: 3},
	} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte("go1")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Extract(context.Background(), archive, TarGz, dir, Limits{}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "go", "VERSION")); err != nil || string(got) != "go1" {
		t.Fatalf("go/VERSION = %q, %v", got, err)
	}
}

func TestLimitsBoundTheArchive(t *testing.T) {
	data := tarGzArchive(t, goLayout()...)
	server, _ := archiveServer(t, data)

	_, err := Install(context.Background(), Pin{URL: server.URL, SHA256: sha(data), Format: TarGz},
		filepath.Join(t.TempDir(), "go"), Options{Limits: Limits{ArchiveBytes: int64(len(data) - 1)}})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("an archive larger than ArchiveBytes: err = %v, want ErrTooLarge", err)
	}

	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(archive, data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, limits := range []Limits{{Entries: 2}, {ExtractedBytes: 10}} {
		if err := Extract(context.Background(), archive, TarGz, t.TempDir(), limits); !errors.Is(err, ErrTooLarge) {
			t.Errorf("Extract with %+v: err = %v, want ErrTooLarge", limits, err)
		}
	}
	zipped := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(zipped, zipArchive(t, goLayout()...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), zipped, Zip, t.TempDir(), Limits{ExtractedBytes: 10}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("zip Extract over ExtractedBytes: err = %v, want ErrTooLarge", err)
	}

	// A zip entry may declare a size no int64 holds; it is too large, not a
	// negative size that slips under the budget.
	var overflow bytes.Buffer
	zw := zip.NewWriter(&overflow)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "go/VERSION", Method: zip.Store,
		CRC32: crc32.ChecksumIEEE([]byte("go1")), CompressedSize64: 3, UncompressedSize64: math.MaxUint64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("go1")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	overflowed := filepath.Join(t.TempDir(), "overflow.zip")
	if err := os.WriteFile(overflowed, overflow.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	if err := Extract(context.Background(), overflowed, Zip, dir, Limits{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("zip entry declaring %d bytes: err = %v, want ErrTooLarge", uint64(math.MaxUint64), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go", "VERSION")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused entry was written (stat error %v)", err)
	}
}

func TestInstallRefusesAnArchiveThatIsNotAnInstall(t *testing.T) {
	data := tarGzArchive(t, archiveEntry{name: "README", body: "not a toolchain"})
	server, _ := archiveServer(t, data)
	dest := filepath.Join(t.TempDir(), "go-1.99.0")
	_, err := Install(context.Background(), Pin{URL: server.URL, SHA256: sha(data), Format: TarGz}, dest,
		Options{Complete: goComplete})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Install: err = %v, want ErrIncomplete", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("an incomplete archive was published at %s", dest)
	}
}

func TestInstallReplacesAnIncompleteDestinationAndStaleStaging(t *testing.T) {
	data := tarGzArchive(t, goLayout()...)
	server, _ := archiveServer(t, data)
	parent := t.TempDir()
	dest := filepath.Join(parent, "go-1.99.0")
	// What an interrupted, pre-lock extraction leaves: a directory without
	// the binary, plus a crashed installer's staging directory.
	if err := os.MkdirAll(filepath.Join(dest, "go", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, ".go-1.99.0.stage-123"), 0o755); err != nil {
		t.Fatal(err)
	}

	installed, err := Install(context.Background(), Pin{URL: server.URL, SHA256: sha(data), Format: TarGz}, dest,
		Options{Complete: goComplete})
	if err != nil || !installed {
		t.Fatalf("Install = (%v, %v), want (true, nil)", installed, err)
	}
	if !goComplete(dest) {
		t.Fatal("the incomplete destination was not replaced")
	}
	if names := siblings(t, parent); len(names) != 2 {
		t.Fatalf("siblings after install = %v, want the destination and its lock", names)
	}
}

// A destination can have a writer that does not take the install lock. When
// that writer publishes a complete destination while Install downloads, the
// destination is kept as published, since programs may already run from it,
// and Install reports that it installed nothing.
func TestInstallKeepsADestinationPublishedDuringTheDownload(t *testing.T) {
	data := tarGzArchive(t, goLayout()...)
	parent := t.TempDir()
	dest := filepath.Join(parent, "go-1.99.0")
	marker := filepath.Join(dest, "go", "published-by-another-writer")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The other writer publishes the whole directory with one rename.
		other := filepath.Join(parent, ".stage.other")
		if err := os.MkdirAll(filepath.Join(other, "go", "bin"), 0o755); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(other, "go", "bin", "go"), []byte("#!/bin/sh\necho go\n"), 0o755); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(other, "go", filepath.Base(marker)), nil, 0o644); err != nil {
			t.Error(err)
		}
		if err := os.Rename(other, dest); err != nil {
			t.Error(err)
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)

	installed, err := Install(context.Background(), Pin{URL: server.URL, SHA256: sha(data), Format: TarGz}, dest,
		Options{Complete: goComplete})
	if err != nil || installed {
		t.Fatalf("Install = (%v, %v), want (false, nil): the destination is another writer's", installed, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the destination another writer published was replaced: %v", err)
	}
	if names := siblings(t, parent); len(names) != 2 {
		t.Fatalf("siblings after install = %v, want the destination and its lock", names)
	}
}

func TestConcurrentInstallersDownloadOnce(t *testing.T) {
	data := tarGzArchive(t, goLayout()...)
	server, requests := archiveServer(t, data)
	dest := filepath.Join(t.TempDir(), "go-1.99.0")
	pin := Pin{URL: server.URL, SHA256: sha(data), Format: TarGz}

	const installers = 6
	var wg sync.WaitGroup
	var installedCount atomic.Int64
	errs := make(chan error, installers)
	for range installers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			installed, err := Install(context.Background(), pin, dest, Options{Complete: goComplete, LockPollInterval: 5 * time.Millisecond})
			if installed {
				installedCount.Add(1)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent Install failed: %v", err)
		}
	}
	if got := installedCount.Load(); got != 1 {
		t.Errorf("%d installers reported the install, want exactly one", got)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("the archive was downloaded %d times, want once", got)
	}
}

func TestInstallWaitsForTheLockUntilTheContextEnds(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "go-1.99.0")
	held, err := acquire(context.Background(), dest+".lock", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = Install(ctx, Pin{URL: "https://example.invalid/go.tar.gz", SHA256: strings.Repeat("a", 64), Format: TarGz}, dest,
		Options{LockPollInterval: 5 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Install behind a held lock: err = %v, want the context deadline", err)
	}
}

func TestDownloadVerifiesAndRemovesARefusedArchive(t *testing.T) {
	data := []byte("archive bytes")
	server, _ := archiveServer(t, data)
	dir := t.TempDir()

	good := filepath.Join(dir, "good")
	if err := Download(context.Background(), Pin{URL: server.URL, SHA256: sha(data), Format: Zip}, good, Options{}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got, err := os.ReadFile(good); err != nil || string(got) != string(data) {
		t.Fatalf("downloaded %q, %v", got, err)
	}

	bad := filepath.Join(dir, "bad")
	err := Download(context.Background(), Pin{URL: server.URL, SHA256: strings.Repeat("0", 64), Format: Zip}, bad, Options{})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Download: err = %v, want ErrDigestMismatch", err)
	}
	if _, statErr := os.Stat(bad); !os.IsNotExist(statErr) {
		t.Fatal("a refused download was left on disk")
	}
}

func TestInstallRefusesInvalidPins(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, pin := range []Pin{
		{URL: "file:///etc/passwd", SHA256: digest, Format: TarGz},
		{URL: "ftp://example.test/go.tar.gz", SHA256: digest, Format: TarGz},
		{URL: "https://example.test/go.rar", SHA256: digest, Format: "rar"},
		{URL: "://bad", SHA256: digest, Format: Zip},
	} {
		if _, err := Install(context.Background(), pin, filepath.Join(t.TempDir(), "go"), Options{}); err == nil {
			t.Errorf("Install accepted %+v", pin)
		}
	}
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	_, err := Install(context.Background(), Pin{URL: server.URL, SHA256: digest, Format: TarGz}, filepath.Join(t.TempDir(), "go"), Options{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("Install from a 404: err = %v, want HTTP 404", err)
	}
}

// An archive that nests its content under a directory of its own is shaped by
// Prepare before Complete judges it, so the published destination holds the
// install layout and never the archive's.
func TestInstallPreparesTheStagingDirectoryBeforeItIsJudged(t *testing.T) {
	data := zipArchive(t,
		archiveEntry{name: "bun-linux-x64/", typeflag: tar.TypeDir, mode: 0o755},
		archiveEntry{name: "bun-linux-x64/bun", body: "#!/bin/sh\necho bun\n", mode: 0o755},
	)
	server, _ := archiveServer(t, data)
	parent := t.TempDir()
	dest := filepath.Join(parent, "bun-1.99.0")
	complete := func(dir string) bool {
		info, err := os.Stat(filepath.Join(dir, "bin", "bun"))
		return err == nil && info.Mode().IsRegular()
	}
	var prepared []string
	prepare := func(stage string) error {
		prepared = append(prepared, stage)
		if err := os.Rename(filepath.Join(stage, "bun-linux-x64"), filepath.Join(stage, "bin")); err != nil {
			return err
		}
		return nil
	}

	installed, err := Install(context.Background(), Pin{URL: server.URL, SHA256: sha(data), Format: Zip}, dest,
		Options{Complete: complete, Prepare: prepare})
	if err != nil || !installed {
		t.Fatalf("Install = (%v, %v), want (true, nil)", installed, err)
	}
	if len(prepared) != 1 || filepath.Dir(prepared[0]) != parent || prepared[0] == dest {
		t.Fatalf("Prepare ran on %v, want once on a staging directory beside %s", prepared, dest)
	}
	if !complete(dest) {
		t.Fatalf("%s does not hold bin/bun", dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "bun-linux-x64")); !os.IsNotExist(err) {
		t.Fatalf("the archive's own directory was published (stat error %v)", err)
	}

	// A complete destination is returned as is: Prepare shapes an extraction,
	// never an install.
	installed, err = Install(context.Background(), Pin{URL: server.URL, SHA256: sha(data), Format: Zip}, dest,
		Options{Complete: complete, Prepare: prepare})
	if err != nil || installed || len(prepared) != 1 {
		t.Fatalf("second Install = (%v, %v) after %d Prepare calls, want (false, nil) and one call", installed, err, len(prepared))
	}
}

// A Prepare that fails refuses the install: nothing is published and no
// staging directory is left beside the destination.
func TestInstallPublishesNothingWhenPrepareFails(t *testing.T) {
	data := zipArchive(t, archiveEntry{name: "bun-linux-x64/bun", body: "bun", mode: 0o755})
	server, _ := archiveServer(t, data)
	parent := t.TempDir()
	dest := filepath.Join(parent, "bun-1.99.0")
	refusal := errors.New("the archive holds no bun")

	installed, err := Install(context.Background(), Pin{URL: server.URL, SHA256: sha(data), Format: Zip}, dest,
		Options{Prepare: func(string) error { return refusal }})
	if installed || !errors.Is(err, ErrIncomplete) || !errors.Is(err, refusal) {
		t.Fatalf("Install = (%v, %v), want ErrIncomplete wrapping the Prepare error", installed, err)
	}
	if names := siblings(t, parent); len(names) != 1 || names[0] != "bun-1.99.0.lock" {
		t.Fatalf("siblings after the refusal = %v, want the lock file only", names)
	}
}

// A pin whose URL carries userinfo is downloaded with it as basic
// authentication, and the pin WithoutCredentials returns no longer names it:
// neither a caller's log line nor an error of Install can repeat the secret.
// The credentials go to the URL's scheme and host only, a redirect elsewhere
// gets none.
func TestWithoutCredentialsKeepsTheUserinfoOutOfThePinAndOffOtherHosts(t *testing.T) {
	const user, secret = "mirror-user", "s3cr3t-token"
	data := zipArchive(t, archiveEntry{name: "bin/bun", body: "bun", mode: 0o755})

	var elsewhereAuthorized atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			elsewhereAuthorized.Store(true)
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(elsewhere.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotSecret, ok := r.BasicAuth()
		if !ok || gotUser != user || gotSecret != secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/redirect.zip" {
			http.Redirect(w, r, elsewhere.URL+"/bun.zip", http.StatusFound)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(origin.Close)
	host := strings.TrimPrefix(origin.URL, "http://")

	pin, client, err := WithoutCredentials(Pin{URL: "http://" + user + ":" + secret + "@" + host + "/bun.zip", SHA256: sha(data), Format: Zip})
	if err != nil || client == nil {
		t.Fatalf("WithoutCredentials = (%v, %v), want a client", client, err)
	}
	if pin.URL != origin.URL+"/bun.zip" {
		t.Fatalf("pin.URL = %q, want %q", pin.URL, origin.URL+"/bun.zip")
	}
	dest := filepath.Join(t.TempDir(), "bun-1.99.0")
	if installed, err := Install(context.Background(), pin, dest, Options{Client: client}); err != nil || !installed {
		t.Fatalf("Install with the credentials = (%v, %v), want (true, nil)", installed, err)
	}

	// A wrong password is refused by the origin; the error names the URL
	// without it.
	wrong, wrongClient, err := WithoutCredentials(Pin{URL: "http://" + user + ":wrong-" + secret + "@" + host + "/bun.zip", SHA256: sha(data), Format: Zip})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Install(context.Background(), wrong, filepath.Join(t.TempDir(), "bun-1.99.0"), Options{Client: wrongClient})
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), user) {
		t.Fatalf("Install with a refused password: err = %v, want an error that names no credential", err)
	}

	// The redirect target is another host: it gets no Authorization header.
	redirected, redirectClient, err := WithoutCredentials(Pin{URL: "http://" + user + ":" + secret + "@" + host + "/redirect.zip", SHA256: sha(data), Format: Zip})
	if err != nil {
		t.Fatal(err)
	}
	if installed, err := Install(context.Background(), redirected, filepath.Join(t.TempDir(), "bun-1.99.0"), Options{Client: redirectClient}); err != nil || !installed {
		t.Fatalf("Install through a redirect = (%v, %v), want (true, nil)", installed, err)
	}
	if elsewhereAuthorized.Load() {
		t.Fatal("the credentials were sent to the host a redirect led to")
	}
}

// A URL without userinfo keeps the default client, and one that does not
// parse is refused without being repeated, since it may hold credentials.
func TestWithoutCredentialsLeavesAPlainPinAndRefusesAnUnparsableOne(t *testing.T) {
	plain := Pin{URL: "https://example.test/bun.zip", SHA256: strings.Repeat("a", 64), Format: Zip}
	pin, client, err := WithoutCredentials(plain)
	if err != nil || client != nil || pin != plain {
		t.Fatalf("WithoutCredentials(plain) = (%+v, %v, %v), want the pin unchanged and no client", pin, client, err)
	}

	const secret = "s3cr3t-token"
	pin, client, err = WithoutCredentials(Pin{URL: "https://user:" + secret + "@exa mple.test/%zz", SHA256: strings.Repeat("a", 64), Format: Zip})
	if err == nil || client != nil || pin != (Pin{}) {
		t.Fatalf("WithoutCredentials(unparsable) = (%+v, %v, %v), want an error and nothing else", pin, client, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the error repeats the URL: %v", err)
	}
}
