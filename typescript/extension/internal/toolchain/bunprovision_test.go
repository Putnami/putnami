package toolchain

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	bunFeature     = "typescript/typescript-project-toolchain"
	bunRequirement = "bun-under-the-putnami-home"
)

// bunZip is the archive a Bun release publishes for target: one directory,
// bun-<target>, that holds the program. The program is a file that holds the
// version it reports.
func bunZip(t *testing.T, target, program, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	dir := &zip.FileHeader{Name: "bun-" + target + "/"}
	dir.SetMode(fs.ModeDir | 0o755)
	if _, err := zw.CreateHeader(dir); err != nil {
		t.Fatal(err)
	}
	file := &zip.FileHeader{Name: "bun-" + target + "/" + program, Method: zip.Deflate}
	file.SetMode(0o755)
	w, err := zw.CreateHeader(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(version + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// bunMirror serves Bun archives by request path and records every request.
type bunMirror struct {
	*httptest.Server
	mu       sync.Mutex
	archives map[string][]byte
	paths    []string
}

func newBunMirror(t *testing.T) *bunMirror {
	t.Helper()
	mirror := &bunMirror{archives: map[string][]byte{}}
	mirror.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirror.mu.Lock()
		mirror.paths = append(mirror.paths, r.URL.Path)
		data, ok := mirror.archives[r.URL.Path]
		mirror.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(mirror.Close)
	return mirror
}

func (m *bunMirror) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.paths)
}

// writeBunLock writes a workspace lock that pins release.
func writeBunLock(t *testing.T, workspace string, release BunRelease) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"version":    3,
		"toolchains": map[string]any{"bun": release},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// declareBun writes a root package.json that declares packageManager.
func declareBun(t *testing.T, workspace, packageManager string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"name": "ws", "packageManager": packageManager})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// bunFiles lists every file named bun or bun.exe under root.
func bunFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.IsDir() && (entry.Name() == "bun" || entry.Name() == "bun.exe") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// logLines collects the progress lines of a request.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) log(level, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+": "+message)
}

func (l *logLines) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// An archive whose SHA-256 differs from the pin is refused, and so is a
// release the lock records no SHA-256 for: nothing is extracted, no bun is
// left on disk, and the error names the release, the archive without the
// credentials of its URL, and the command to run next.
func TestProvisionBun_RefusesAnArchiveItCannotVerifyAndLeavesNoBun(t *testing.T) {
	spectest.Proves(t, bunFeature, bunRequirement, "a-refused-archive-leaves-no-bun-on-disk")
	const user, secret = "mirror-user", "s3cr3t-token"
	mirror := newBunMirror(t)
	mirror.archives["/dl/bun-linux-aarch64.zip"] = bunZip(t, "linux-aarch64", "bun", "1.9.9")
	source := strings.Replace(mirror.URL, "http://", "http://"+user+":"+secret+"@", 1) + "/dl/"

	t.Run("the digest differs", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		workspace := t.TempDir()
		writeBunLock(t, workspace, BunRelease{
			Version:     "1.9.9",
			Integrities: map[string]string{"linux/arm64": sha256Hex([]byte("another archive"))},
			Source:      source,
		})
		var logs logLines
		before := len(mirror.requests())

		_, err := provisionBun(context.Background(), BunRequest{
			WorkspaceRoot: workspace, Version: fakeBunVersion(nil), Log: logs.log,
		}, host.bunHost)
		if err == nil {
			t.Fatal("an archive whose SHA-256 differs from the pin was installed")
		}
		for _, want := range []string{"Bun 1.9.9", mirror.URL + "/dl/bun-linux-aarch64.zip", "`putnami install`", "SHA-256"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
		if text := err.Error() + logs.all(); strings.Contains(text, secret) || strings.Contains(text, user) {
			t.Fatalf("the credentials of the source are printed: %s", text)
		}
		if got := len(mirror.requests()) - before; got != 1 {
			t.Fatalf("the archive was requested %d times, want once", got)
		}
		if left := bunFiles(t, host.home); len(left) != 0 {
			t.Fatalf("the refused install left %v on disk", left)
		}
		if _, statErr := os.Stat(filepath.Join(host.toolchainRoot(), "bun-1.9.9")); !os.IsNotExist(statErr) {
			t.Fatalf("the refused install published its directory (stat error %v)", statErr)
		}
	})

	t.Run("the lock records no digest", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		workspace := t.TempDir()
		writeBunLock(t, workspace, BunRelease{
			Version:     "1.9.9",
			Integrities: map[string]string{"darwin/arm64": sha256Hex(mirror.archives["/dl/bun-linux-aarch64.zip"])},
			Source:      source,
		})
		before := len(mirror.requests())

		_, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}, host.bunHost)
		if err == nil {
			t.Fatal("a release without a SHA-256 for the host platform was installed")
		}
		for _, want := range []string{"Bun 1.9.9", "linux/arm64", "`putnami install`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the credentials of the source are printed: %v", err)
		}
		if got := len(mirror.requests()) - before; got != 0 {
			t.Fatalf("an unverifiable archive was requested %d times", got)
		}
		if left := bunFiles(t, host.home); len(left) != 0 {
			t.Fatalf("the refused install left %v on disk", left)
		}
	})

	t.Run("the archive holds a link", func(t *testing.T) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		link := &zip.FileHeader{Name: "bun-linux-aarch64/bun"}
		link.SetMode(fs.ModeSymlink | 0o777)
		w, err := zw.CreateHeader(link)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("/bin/sh")); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		mirror.mu.Lock()
		mirror.archives["/links/bun-linux-aarch64.zip"] = buf.Bytes()
		mirror.mu.Unlock()

		host := newFakeBunHost(t, "linux", "arm64")
		workspace := t.TempDir()
		writeBunLock(t, workspace, BunRelease{
			Version:     "1.9.9",
			Integrities: map[string]string{"linux/arm64": sha256Hex(buf.Bytes())},
			Source:      mirror.URL + "/links",
		})
		_, err = provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}, host.bunHost)
		if err == nil || !strings.Contains(err.Error(), "Bun 1.9.9") {
			t.Fatalf("an archive that holds a link: err = %v, want a refusal that names Bun 1.9.9", err)
		}
		if left := bunFiles(t, host.home); len(left) != 0 {
			t.Fatalf("the refused install left %v on disk", left)
		}
	})
}

// A bun of the host that reports the pinned release is the job's bun, and so
// is any bun of the host when the workspace names no release. Nothing is
// downloaded and the environment is left alone.
func TestProvisionBun_AMatchingBunOnPathStartsNoDownload(t *testing.T) {
	spectest.Proves(t, bunFeature, bunRequirement, "a-matching-bun-on-path-starts-no-download")
	mirror := newBunMirror(t)
	mirror.archives["/bun-linux-aarch64.zip"] = bunZip(t, "linux-aarch64", "bun", "1.9.9")

	t.Run("the lock pins its release", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		onPath := writeFakeBun(t, t.TempDir(), "bun", "1.9.9")
		host.env["PATH"] = filepath.Dir(onPath)
		workspace := t.TempDir()
		writeBunLock(t, workspace, BunRelease{
			Version:     "1.9.9",
			Integrities: map[string]string{"linux/arm64": sha256Hex(mirror.archives["/bun-linux-aarch64.zip"])},
			Source:      mirror.URL,
		})

		bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}, host.bunHost)
		if err != nil {
			t.Fatal(err)
		}
		if want := (Bun{Path: onPath, Version: "1.9.9"}); bun != want {
			t.Fatalf("provisionBun = %+v, want %+v", bun, want)
		}
		if got := mirror.requests(); len(got) != 0 {
			t.Fatalf("a matching bun on PATH still downloaded %v", got)
		}
		if _, statErr := os.Stat(filepath.Join(host.home, ".putnami")); !os.IsNotExist(statErr) {
			t.Fatalf("a Putnami home was written for a bun of the host (stat error %v)", statErr)
		}
		if _, set := host.env["BUN_INSTALL"]; set {
			t.Fatalf("BUN_INSTALL = %q for a bun of the host, which keeps its own locations", host.env["BUN_INSTALL"])
		}
	})

	t.Run("nothing names a release", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		onPath := writeFakeBun(t, t.TempDir(), "bun", "1.2.3")
		host.env["PATH"] = filepath.Dir(onPath)
		var asked []string

		bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: t.TempDir(), Version: fakeBunVersion(&asked)}, host.bunHost)
		if err != nil {
			t.Fatal(err)
		}
		if want := (Bun{Path: onPath}); bun != want {
			t.Fatalf("provisionBun = %+v, want %+v", bun, want)
		}
		if got := mirror.requests(); len(got) != 0 {
			t.Fatalf("a bun on PATH still downloaded %v", got)
		}
		if len(asked) != 0 {
			t.Fatalf("the bun was asked its version (%v) although no release is named", asked)
		}
	})
}

// A lock that pins another release than the bun on PATH no longer stops the
// job: the pinned release is installed under the Putnami home, from the lock's
// source, and the job runs with it. A second workspace of the machine starts
// no download.
func TestProvisionBun_InstallsThePinnedReleaseWhenThePathBunDiffers(t *testing.T) {
	spectest.Proves(t, bunFeature, bunRequirement, "a-pinned-release-is-installed-when-the-path-bun-differs")
	const user, secret = "mirror-user", "s3cr3t-token"
	mirror := newBunMirror(t)
	archive := bunZip(t, "linux-aarch64", "bun", "1.9.9")
	mirror.archives["/mirror/bun-linux-aarch64.zip"] = archive

	host := newFakeBunHost(t, "linux", "arm64")
	onPath := writeFakeBun(t, t.TempDir(), "bun", "1.0.0")
	host.env["PATH"] = filepath.Dir(onPath)
	release := BunRelease{
		Version:     "1.9.9",
		Integrities: map[string]string{"linux/arm64": sha256Hex(archive)},
		// No trailing slash, and credentials the transcript must not show.
		Source: strings.Replace(mirror.URL, "http://", "http://"+user+":"+secret+"@", 1) + "/mirror",
	}
	workspace := t.TempDir()
	writeBunLock(t, workspace, release)
	var logs logLines

	bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil), Log: logs.log}, host.bunHost)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(host.toolchainRoot(), "bun-1.9.9")
	if want := (Bun{Path: filepath.Join(dir, "bin", "bun"), Version: "1.9.9", Managed: true}); bun != want {
		t.Fatalf("provisionBun = %+v, want %+v", bun, want)
	}
	if got := mirror.requests(); !slices.Equal(got, []string{"/mirror/bun-linux-aarch64.zip"}) {
		t.Fatalf("requests = %v, want the archive of the lock's source once", got)
	}
	if strings.Contains(logs.all(), secret) || strings.Contains(logs.all(), user) {
		t.Fatalf("the credentials of the source are logged: %s", logs.all())
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(bun.Path)
		if err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable: %v, %v", bun.Path, info, err)
		}
	}
	// The install holds the program and nothing else of the archive.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "bin" {
		t.Fatalf("%s holds %v (%v), want bin only", dir, entries, err)
	}

	// The job and the programs it starts run the installed Bun, which keeps
	// its files under its install directory.
	if host.env["BUN_INSTALL"] != dir {
		t.Fatalf("BUN_INSTALL = %q, want %q", host.env["BUN_INSTALL"], dir)
	}
	if got, want := host.env["BUN_RUNTIME_TRANSPILER_CACHE_PATH"], filepath.Join(dir, "install", "cache", "@t@"); got != want {
		t.Fatalf("BUN_RUNTIME_TRANSPILER_CACHE_PATH = %q, want %q", got, want)
	}
	if first, _, _ := strings.Cut(host.env["PATH"], string(os.PathListSeparator)); first != filepath.Join(dir, "bin") {
		t.Fatalf("PATH = %q, want the installed bun first", host.env["PATH"])
	}
	if _, statErr := os.Stat(filepath.Join(host.home, ".bun")); !os.IsNotExist(statErr) {
		t.Fatalf(".bun exists in the user's home directory (stat error %v)", statErr)
	}

	// Another workspace of the machine, and a machine state without the CLI's
	// environment: the install is found, nothing is downloaded.
	other := newFakeBunHost(t, "linux", "arm64")
	other.home = host.home
	other.env["HOME"] = host.home
	other.env["PATH"] = filepath.Dir(onPath)
	otherWorkspace := t.TempDir()
	writeBunLock(t, otherWorkspace, release)
	again, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: otherWorkspace, Version: fakeBunVersion(nil)}, other.bunHost)
	if err != nil || again != bun {
		t.Fatalf("a second workspace: provisionBun = (%+v, %v), want %+v", again, err, bun)
	}
	if got := mirror.requests(); len(got) != 1 {
		t.Fatalf("a second workspace downloaded again: %v", got)
	}
}

// With no network and no bun, the job fails with one message that names Bun,
// the release, and the command to run next.
func TestProvisionBun_NoNetworkAndNoBunFailsWithOneMessage(t *testing.T) {
	spectest.Proves(t, bunFeature, bunRequirement, "no-network-and-no-bun-fails-with-one-message")
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachableURL := unreachable.URL
	unreachable.Close()

	t.Run("the lock pins a release", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		workspace := t.TempDir()
		writeBunLock(t, workspace, BunRelease{
			Version:     "1.9.9",
			Integrities: map[string]string{"linux/arm64": sha256Hex([]byte("archive"))},
			Source:      unreachableURL + "/",
		})
		_, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}, host.bunHost)
		if err == nil {
			t.Fatal("provisionBun succeeded without a network and without a bun")
		}
		for _, want := range []string{"Bun 1.9.9", unreachableURL + "/bun-linux-aarch64.zip", "`putnami install`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "bun.sh") {
			t.Fatalf("the error is not one message, or asks the user to install Bun: %q", err.Error())
		}
		if left := bunFiles(t, host.home); len(left) != 0 {
			t.Fatalf("the failed install left %v on disk", left)
		}
	})

	t.Run("nothing names a release", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		var requested []string
		host.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requested = append(requested, req.URL.String())
			return nil, errors.New("network is unreachable")
		})}
		_, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: t.TempDir(), Version: fakeBunVersion(nil)}, host.bunHost)
		if err == nil {
			t.Fatal("provisionBun succeeded without a network and without a bun")
		}
		archive := "https://github.com/oven-sh/bun/releases/download/bun-v" + DefaultBunVersion + "/bun-linux-aarch64.zip"
		for _, want := range []string{"Bun " + DefaultBunVersion, archive, "`putnami install`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
		if !slices.Equal(requested, []string{archive}) {
			t.Fatalf("requests = %v, want the archive of the default release only", requested)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// On Windows the release is installed from bun-windows-x64.zip, and its
// program is bin\bun.exe under the same directory of the Putnami home. The
// selection and the extraction do not depend on the machine the test runs on.
func TestProvisionBun_WindowsInstallsFromTheWindowsArchive(t *testing.T) {
	spectest.Proves(t, bunFeature, bunRequirement, "windows-installs-from-the-windows-archive")
	mirror := newBunMirror(t)
	archive := bunZip(t, "windows-x64", "bun.exe", "1.9.9")
	mirror.archives["/bun-windows-x64.zip"] = archive

	host := newFakeBunHost(t, "windows", "amd64")
	workspace := t.TempDir()
	writeBunLock(t, workspace, BunRelease{
		Version: "1.9.9",
		Integrities: map[string]string{
			"windows/amd64": sha256Hex(archive),
			"linux/amd64":   sha256Hex([]byte("the linux archive")),
		},
		Source: mirror.URL + "/",
	})

	bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}, host.bunHost)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(host.toolchainRoot(), "bun-1.9.9")
	if want := (Bun{Path: filepath.Join(dir, "bin", "bun.exe"), Version: "1.9.9", Managed: true}); bun != want {
		t.Fatalf("provisionBun = %+v, want %+v", bun, want)
	}
	if got := mirror.requests(); !slices.Equal(got, []string{"/bun-windows-x64.zip"}) {
		t.Fatalf("requests = %v, want bun-windows-x64.zip once", got)
	}
	if got := bunFiles(t, host.home); !slices.Equal(got, []string{bun.Path}) {
		t.Fatalf("bun files under the home = %v, want %s only", got, bun.Path)
	}

	// A bun.exe that Bun's own installer wrote, of the pinned release, is
	// used as it is on a Windows host too.
	own := newFakeBunHost(t, "windows", "amd64")
	installed := writeFakeBun(t, filepath.Join(own.home, ".bun", "bin"), "bun.exe", "1.9.9")
	got, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}, own.bunHost)
	if err != nil || got != (Bun{Path: installed, Version: "1.9.9"}) {
		t.Fatalf("provisionBun with the installer's bun.exe = (%+v, %v), want %s", got, err, installed)
	}
	if requests := mirror.requests(); len(requests) != 1 {
		t.Fatalf("a matching bun.exe still downloaded: %v", requests)
	}
}

// withDefaultBunArchive makes the default release installable from a test
// archive: the digest the extension ships for platform becomes the archive's,
// and host downloads the vendor's URL from the returned record instead of the
// network.
func withDefaultBunArchive(t *testing.T, host *fakeBunHost, platform string, archive []byte) *[]string {
	t.Helper()
	original := defaultBunIntegrities[platform]
	defaultBunIntegrities[platform] = sha256Hex(archive)
	t.Cleanup(func() { defaultBunIntegrities[platform] = original })
	requested := &[]string{}
	var mu sync.Mutex
	host.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		*requested = append(*requested, req.URL.String())
		mu.Unlock()
		recorder := httptest.NewRecorder()
		_, _ = recorder.Write(archive)
		return recorder.Result(), nil
	})}
	return requested
}

// A workspace that names no release, on a host without bun, gets the default
// release of the extension, verified with the digest the extension ships. The
// only request is the archive: no release metadata is read.
func TestProvisionBun_InstallsTheDefaultReleaseWhenNothingIsDeclared(t *testing.T) {
	spectest.Proves(t, bunFeature, bunRequirement, "nothing-declared-installs-the-default-release")
	host := newFakeBunHost(t, "linux", "arm64")
	requested := withDefaultBunArchive(t, host, "linux/arm64", bunZip(t, "linux-aarch64", "bun", DefaultBunVersion))

	bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: t.TempDir(), Version: fakeBunVersion(nil)}, host.bunHost)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(host.toolchainRoot(), "bun-"+DefaultBunVersion)
	if want := (Bun{Path: filepath.Join(dir, "bin", "bun"), Version: DefaultBunVersion, Managed: true}); bun != want {
		t.Fatalf("provisionBun = %+v, want %+v", bun, want)
	}
	archive := "https://github.com/oven-sh/bun/releases/download/bun-v" + DefaultBunVersion + "/bun-linux-aarch64.zip"
	if !slices.Equal(*requested, []string{archive}) {
		t.Fatalf("requests = %v, want %s only", *requested, archive)
	}
}

// The default release ships one valid SHA-256 for each platform Bun publishes
// an archive for, and it is the release this repository's own lock pins, with
// the digests the CLI read from the vendor when it wrote that lock.
func TestDefaultBunRelease_ShipsTheVendorDigests(t *testing.T) {
	release := DefaultBunRelease()
	if !IsPlainBunRelease(release.Version) {
		t.Fatalf("the default version %q is not a release", release.Version)
	}
	if len(release.Integrities) != 6 {
		t.Fatalf("the default release ships %d digests, want 6", len(release.Integrities))
	}
	for platform := range bunTargets {
		digest := release.Integrities[platform]
		if raw, err := hex.DecodeString(digest); err != nil || len(raw) != sha256.Size || strings.ToLower(digest) != digest {
			t.Errorf("the %s digest %q is not a lowercase SHA-256", platform, digest)
		}
	}
	// The returned release is a copy: a caller cannot change what the
	// extension ships.
	release.Integrities["linux/amd64"] = "changed"
	if DefaultBunRelease().Integrities["linux/amd64"] == "changed" {
		t.Fatal("DefaultBunRelease hands out the shipped digests themselves")
	}

	root := repositoryRoot(t)
	if root == "" {
		t.Skip("not inside the repository: no lock to compare with")
	}
	locked, pinned, err := lockedBunRelease(root)
	if err != nil || !pinned {
		t.Fatalf("the repository lock pins no Bun: %v", err)
	}
	want := DefaultBunRelease()
	if locked.Version != want.Version || locked.Source != want.Source {
		t.Fatalf("the repository lock pins Bun %s from %s; the default release is Bun %s from %s",
			locked.Version, locked.Source, want.Version, want.Source)
	}
	for platform, digest := range want.Integrities {
		if locked.Integrities[platform] != digest {
			t.Errorf("%s: the default release ships %s, the repository lock records %s", platform, digest, locked.Integrities[platform])
		}
	}
}

// repositoryRoot returns the directory above this package that holds the
// repository's lock, or "".
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if isRegularFile(filepath.Join(dir, WorkspaceLockFile)) && isRegularFile(filepath.Join(dir, "putnamiw")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Two workspaces that install the same release at the same time leave one
// complete install, and the archive is downloaded once.
func TestProvisionBun_ConcurrentWorkspacesLeaveOneInstall(t *testing.T) {
	spectest.Proves(t, bunFeature, bunRequirement, "two-workspaces-leave-one-install")
	archive := bunZip(t, "linux-aarch64", "bun", "1.9.9")
	var downloads atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		<-release
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)

	home := t.TempDir()
	const workspaces = 4
	results := make([]Bun, workspaces)
	errs := make([]error, workspaces)
	var started, done sync.WaitGroup
	for i := range workspaces {
		host := newFakeBunHost(t, "linux", "arm64")
		host.home = home
		host.env["HOME"] = home
		workspace := t.TempDir()
		writeBunLock(t, workspace, BunRelease{
			Version:     "1.9.9",
			Integrities: map[string]string{"linux/arm64": sha256Hex(archive)},
			Source:      server.URL,
		})
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			started.Done()
			results[i], errs[i] = provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}, host.bunHost)
		}()
	}
	started.Wait()
	close(release)
	done.Wait()

	root := filepath.Join(home, ".putnami", "toolchains", "bun")
	want := Bun{Path: filepath.Join(root, "bun-1.9.9", "bin", "bun"), Version: "1.9.9", Managed: true}
	for i := range workspaces {
		if errs[i] != nil || results[i] != want {
			t.Fatalf("workspace %d: provisionBun = (%+v, %v), want %+v", i, results[i], errs[i], want)
		}
	}
	if got := downloads.Load(); got != 1 {
		t.Fatalf("the archive was downloaded %d times, want once", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"bun-1.9.9", "bun-1.9.9.lock"}) {
		t.Fatalf("%s holds %v, want the install and its lock file", root, names)
	}
}

// A hosted run installs no toolchain: it runs a bun the runner holds, on PATH
// or under the Putnami home, and fails with the release and where to put it
// when there is none. A bun the job refuses is never started.
func TestProvisionBun_AHostedRunInstallsNoBun(t *testing.T) {
	spectest.Proves(t, bunFeature, bunRequirement, "a-hosted-run-installs-no-bun")
	mirror := newBunMirror(t)
	archive := bunZip(t, "linux-aarch64", "bun", "1.9.9")
	mirror.archives["/bun-linux-aarch64.zip"] = archive
	release := BunRelease{Version: "1.9.9", Integrities: map[string]string{"linux/arm64": sha256Hex(archive)}, Source: mirror.URL}

	host := newFakeBunHost(t, "linux", "arm64")
	workspace := t.TempDir()
	writeBunLock(t, workspace, release)

	_, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Mode: BunFind, Version: fakeBunVersion(nil)}, host.bunHost)
	if err == nil || !strings.Contains(err.Error(), "Bun 1.9.9") || !strings.Contains(err.Error(), "hosted run") {
		t.Fatalf("a hosted run without bun: err = %v, want one that names Bun 1.9.9 and the hosted run", err)
	}
	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("a hosted run downloaded %v", got)
	}
	if left := bunFiles(t, host.home); len(left) != 0 {
		t.Fatalf("a hosted run wrote %v", left)
	}

	// The runner holds the release under the Putnami home.
	installed := writeFakeBun(t, filepath.Join(host.toolchainRoot(), "bun-1.9.9", "bin"), "bun", "1.9.9")
	bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Mode: BunFind, Version: fakeBunVersion(nil)}, host.bunHost)
	if err != nil || bun != (Bun{Path: installed, Version: "1.9.9", Managed: true}) {
		t.Fatalf("a hosted run with the release installed = (%+v, %v), want %s", bun, err, installed)
	}

	// A bun inside the workspace is refused before it is started, whether it
	// is on PATH or under a Putnami home that falls back into the workspace.
	homeless := newFakeBunHost(t, "linux", "arm64")
	delete(homeless.env, "HOME")
	delete(homeless.env, "USERPROFILE")
	homeless.userHome = func() (string, error) { return "", errors.New("no home") }
	committed := writeFakeBun(t, filepath.Join(workspace, "node_modules", ".bin"), "bun", "1.9.9")
	homeless.env["PATH"] = filepath.Dir(committed)
	inWorkspace := writeFakeBun(t, filepath.Join(workspace, ".putnami", "toolchains", "bun", "bun-1.9.9", "bin"), "bun", "1.9.9")
	var asked []string
	_, err = provisionBun(context.Background(), BunRequest{
		WorkspaceRoot: workspace, Mode: BunFind, Version: fakeBunVersion(&asked),
		Refuse: func(path string) string {
			if strings.HasPrefix(path, workspace+string(filepath.Separator)) {
				return "it is inside the workspace"
			}
			return ""
		},
	}, homeless.bunHost)
	if err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("a bun inside the workspace: err = %v, want a refusal", err)
	}
	for _, refused := range []string{committed, inWorkspace} {
		if !strings.Contains(err.Error(), refused) {
			t.Errorf("the error does not name %s: %v", refused, err)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("a refused bun was started to read its version: %v", asked)
	}
}

// A release that package.json declares and the lock does not pin yet has no
// digest to check its download against. The install then runs with a bun of
// the host of another release, else with the default release, and says so;
// the declared release is used as soon as the machine holds it.
func TestProvisionBun_ADeclaredReleaseTheLockDoesNotPinYet(t *testing.T) {
	workspace := t.TempDir()
	declareBun(t, workspace, "bun@1.9.9")

	t.Run("a bun of the host of another release", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		onPath := writeFakeBun(t, t.TempDir(), "bun", "1.0.0")
		host.env["PATH"] = filepath.Dir(onPath)
		host.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Errorf("unexpected request to %s", req.URL)
			return nil, errors.New("no network in this test")
		})}
		var logs logLines
		bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil), Log: logs.log}, host.bunHost)
		if err != nil || bun != (Bun{Path: onPath}) {
			t.Fatalf("provisionBun = (%+v, %v), want the bun on PATH", bun, err)
		}
		for _, want := range []string{"warn: ", "Bun 1.9.9", "Bun 1.0.0", "`putnami install`"} {
			if !strings.Contains(logs.all(), want) {
				t.Errorf("the warning does not name %q: %s", want, logs.all())
			}
		}
	})

	t.Run("no bun on the host", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		requested := withDefaultBunArchive(t, host, "linux/arm64", bunZip(t, "linux-aarch64", "bun", DefaultBunVersion))
		var logs logLines
		bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil), Log: logs.log}, host.bunHost)
		if err != nil || bun.Version != DefaultBunVersion || !bun.Managed {
			t.Fatalf("provisionBun = (%+v, %v), want the default release under the Putnami home", bun, err)
		}
		if len(*requested) != 1 {
			t.Fatalf("requests = %v, want the default archive once", *requested)
		}
		if !strings.Contains(logs.all(), "warn: ") || !strings.Contains(logs.all(), "Bun 1.9.9") {
			t.Errorf("no warning names the declared release: %s", logs.all())
		}
	})

	t.Run("the declared release is installed", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		installed := writeFakeBun(t, filepath.Join(host.toolchainRoot(), "bun-1.9.9", "bin"), "bun", "1.9.9")
		bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}, host.bunHost)
		if err != nil || bun != (Bun{Path: installed, Version: "1.9.9", Managed: true}) {
			t.Fatalf("provisionBun = (%+v, %v), want %s", bun, err, installed)
		}
	})

	t.Run("the declared release is the default one", func(t *testing.T) {
		defaulted := t.TempDir()
		declareBun(t, defaulted, "bun@v"+DefaultBunVersion)
		host := newFakeBunHost(t, "linux", "arm64")
		withDefaultBunArchive(t, host, "linux/arm64", bunZip(t, "linux-aarch64", "bun", DefaultBunVersion))
		var logs logLines
		bun, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: defaulted, Version: fakeBunVersion(nil), Log: logs.log}, host.bunHost)
		if err != nil || bun.Version != DefaultBunVersion || !bun.Managed {
			t.Fatalf("provisionBun = (%+v, %v), want the default release under the Putnami home", bun, err)
		}
		if strings.Contains(logs.all(), "warn: ") {
			t.Errorf("installing the declared default release warns: %s", logs.all())
		}
	})
}

// What the machine cannot run is refused with the release and the next step,
// before any request: a version that is not a published release, a musl host,
// a platform Bun publishes no archive for, and an install that reports
// another release than its directory names.
func TestProvisionBun_RefusesWhatTheHostCannotRun(t *testing.T) {
	mirror := newBunMirror(t)
	archive := bunZip(t, "linux-aarch64", "bun", "1.9.9")
	mirror.archives["/bun-linux-aarch64.zip"] = archive
	release := BunRelease{Version: "1.9.9", Integrities: map[string]string{"linux/arm64": sha256Hex(archive)}, Source: mirror.URL}
	workspace := t.TempDir()
	writeBunLock(t, workspace, release)
	request := BunRequest{WorkspaceRoot: workspace, Version: fakeBunVersion(nil)}

	t.Run("a musl host", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		host.platform.musl = true
		_, err := provisionBun(context.Background(), request, host.bunHost)
		if err == nil || !strings.Contains(err.Error(), "musl") || !strings.Contains(err.Error(), "Bun 1.9.9") {
			t.Fatalf("a musl host: err = %v, want a refusal that names musl and Bun 1.9.9", err)
		}
	})

	t.Run("a platform without an archive", func(t *testing.T) {
		host := newFakeBunHost(t, "freebsd", "amd64")
		_, err := provisionBun(context.Background(), request, host.bunHost)
		if err == nil || !strings.Contains(err.Error(), "freebsd/amd64") {
			t.Fatalf("freebsd: err = %v, want a refusal that names the platform", err)
		}
	})

	t.Run("a version that is no release", func(t *testing.T) {
		canary := t.TempDir()
		writeBunLock(t, canary, BunRelease{Version: "../../escape", Source: mirror.URL})
		host := newFakeBunHost(t, "linux", "arm64")
		_, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: canary, Version: fakeBunVersion(nil)}, host.bunHost)
		if err == nil || !strings.Contains(err.Error(), "MAJOR.MINOR.PATCH") {
			t.Fatalf("a version that is no release: err = %v, want a refusal", err)
		}
		if entries, readErr := os.ReadDir(filepath.Join(host.home, ".putnami")); readErr == nil {
			t.Fatalf("the refused version wrote %v under the Putnami home", entries)
		}
	})

	t.Run("an install of another release", func(t *testing.T) {
		host := newFakeBunHost(t, "linux", "arm64")
		dir := filepath.Join(host.toolchainRoot(), "bun-1.9.9")
		writeFakeBun(t, filepath.Join(dir, "bin"), "bun", "1.0.0")
		_, err := provisionBun(context.Background(), request, host.bunHost)
		if err == nil || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "Bun 1.0.0") {
			t.Fatalf("an install of another release: err = %v, want one that names %s and Bun 1.0.0", err, dir)
		}
	})

	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("a refusal still requested %v", got)
	}

	t.Run("a lock that does not parse", func(t *testing.T) {
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, WorkspaceLockFile), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		host := newFakeBunHost(t, "linux", "arm64")
		_, err := provisionBun(context.Background(), BunRequest{WorkspaceRoot: broken, Version: fakeBunVersion(nil)}, host.bunHost)
		if err == nil || !strings.Contains(err.Error(), WorkspaceLockFile) {
			t.Fatalf("a lock that does not parse: err = %v, want one that names the lock", err)
		}
	})
}

// The archive of a release is bun-<target>.zip under the release's source,
// for the six platforms Bun publishes, and the vendor's downloads when the
// release names no source.
func TestBunArchiveFor(t *testing.T) {
	release := BunRelease{Version: "1.9.9", Integrities: map[string]string{"darwin/arm64": "digest"}}
	for platform, target := range map[string]string{
		"darwin/amd64": "darwin-x64", "darwin/arm64": "darwin-aarch64",
		"linux/amd64": "linux-x64", "linux/arm64": "linux-aarch64",
		"windows/amd64": "windows-x64", "windows/arm64": "windows-aarch64",
	} {
		goos, goarch, _ := strings.Cut(platform, "/")
		archive, ok := bunArchiveFor(release, goos, goarch)
		wantURL := "https://github.com/oven-sh/bun/releases/download/bun-v1.9.9/bun-" + target + ".zip"
		if !ok || archive.pin.URL != wantURL || archive.dir != "bun-"+target || archive.pin.SHA256 != release.Integrities[platform] {
			t.Errorf("%s: bunArchiveFor = (%+v, %v), want %s", platform, archive, ok, wantURL)
		}
	}
	if _, ok := bunArchiveFor(release, "linux", "riscv64"); ok {
		t.Error("linux/riscv64 has an archive")
	}
}

// The declared release is read the way the CLI reads it before it pins the
// lock.
func TestDeclaredBunVersion(t *testing.T) {
	for _, tc := range []struct{ declaration, want string }{
		{"bun@1.4.0", "1.4.0"},
		{" bun@v1.4.0", "1.4.0"},
		{"bun@1.4", "1.4.0"},
		{"pnpm@9.0.0", ""},
		{"", ""},
	} {
		workspace := t.TempDir()
		declareBun(t, workspace, tc.declaration)
		if got := declaredBunVersion(workspace); got != tc.want {
			t.Errorf("packageManager %q: declaredBunVersion = %q, want %q", tc.declaration, got, tc.want)
		}
	}
	if got := declaredBunVersion(t.TempDir()); got != "" {
		t.Errorf("no package.json: declaredBunVersion = %q, want none", got)
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "package.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := declaredBunVersion(broken); got != "" {
		t.Errorf("a package.json that does not parse: declaredBunVersion = %q, want none", got)
	}
}

func TestLockedBunRelease(t *testing.T) {
	if _, pinned, err := lockedBunRelease(t.TempDir()); pinned || err != nil {
		t.Fatalf("no lock = (%v, %v), want no pin", pinned, err)
	}
	for name, content := range map[string]string{
		"no toolchains":   `{"version":3}`,
		"no bun":          `{"toolchains":{"go":{"version":"1.26.1"}}}`,
		"a null bun":      `{"toolchains":{"bun":null}}`,
		"a blank version": `{"toolchains":{"bun":{"version":"  "}}}`,
	} {
		workspace := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, pinned, err := lockedBunRelease(workspace); pinned || err != nil {
			t.Errorf("%s = (%v, %v), want no pin", name, pinned, err)
		}
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), []byte(`{"toolchains":{"bun":"1.4.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lockedBunRelease(workspace); err == nil {
		t.Error("a bun entry of the wrong shape was read")
	}
	// A document that grew a field this extension does not know is read.
	if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile),
		[]byte(`{"future":true,"toolchains":{"bun":{"version":" 1.4.0 ","future":1,"source":"https://example.test/"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	release, pinned, err := lockedBunRelease(workspace)
	if err != nil || !pinned || release.Version != "1.4.0" || release.Source != "https://example.test/" {
		t.Fatalf("lockedBunRelease = (%+v, %v, %v), want Bun 1.4.0", release, pinned, err)
	}
}
