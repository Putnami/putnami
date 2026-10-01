package workspacejob_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

const lockedGo = "1.99.1"

type archiveFile struct {
	name    string
	content string
	mode    int64
}

// goDistribution is the layout of the locked Go release archive for this host:
// a go/ directory holding bin/go and the VERSION file naming the release.
func goDistribution() []archiveFile {
	binary := "go/bin/go"
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	return []archiveFile{
		{name: "go/VERSION", content: "go" + lockedGo + "\ntime 2026-09-01T00:00:00Z\n", mode: 0o644},
		{name: binary, content: "not a program", mode: 0o755},
		{name: "go/src/fmt/print.go", content: "package fmt\n", mode: 0o644},
	}
}

func tarGz(t *testing.T, files []archiveFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, file := range files {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
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

func zipArchive(t *testing.T, files []archiveFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, file := range files {
		header := &zip.FileHeader{Name: file.name, Method: zip.Deflate}
		header.SetMode(os.FileMode(file.mode))
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// hostArchive is the archive the Go distribution publishes for this host: a
// zip on Windows, a gzip-compressed tar elsewhere.
func hostArchive(t *testing.T, files []archiveFile) []byte {
	t.Helper()
	if runtime.GOOS == "windows" {
		return zipArchive(t, files)
	}
	return tarGz(t, files)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// distributionServer serves one archive at every path and counts requests.
func distributionServer(t *testing.T, archive []byte, status int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func writeLock(t *testing.T, ws string, goLock any) {
	t.Helper()
	document := map[string]any{"lockVersion": 1}
	if goLock != nil {
		document["toolchains"] = map[string]any{"go": goLock}
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	jobtest.WriteFile(t, ws, workspacejob.LockFileName, string(data))
}

func hostPlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// shellFreeJob is a job whose PATH holds only traps: every shell and every
// tool the former script used fails the test when started, and no go command
// can be found, so the job has to install the one the lock pins.
func shellFreeJob(t *testing.T, ws string) (*workspacejob.Job, *jobtest.Recorder, *jobtest.Fakes) {
	t.Helper()
	fakes := jobtest.NewFakes(t)
	env := append([]string{
		"PATH=" + fakes.Dir,
		"HOME=" + t.TempDir(),
		"PUTNAMI_HOME=" + t.TempDir(),
		"PUTNAMI_GO_CACHE_DIR=" + t.TempDir(),
	}, fakes.Environ()...)
	rec := &jobtest.Recorder{}
	j, stdout, _ := jobtest.NewJob(t, rec, env, ws)
	t.Cleanup(func() {
		if stdout.Len() > 0 {
			t.Errorf("a command wrote to the job event stream:\n%s", stdout)
		}
	})
	return j, rec, fakes
}

// TestResolveGoBinaryInstallsTheLockedGoWithoutAShell is the acceptance case
// for the toolchain half of `putnami install`: with no go command anywhere,
// the job downloads the archive the lock names, verifies its SHA-256, unpacks
// it and links bin/go, and starts no shell, no curl, no tar and no unzip on
// the way.
func TestResolveGoBinaryInstallsTheLockedGoWithoutAShell(t *testing.T) {
	archive := hostArchive(t, goDistribution())
	server, requests := distributionServer(t, archive, http.StatusOK)
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
	writeLock(t, ws, map[string]any{
		"version":     lockedGo,
		"integrities": map[string]string{hostPlatform(): digest(archive), "plan9/mips": strings.Repeat("0", 64)},
		"source":      server.URL + "/dl",
	})

	j, rec, fakes := shellFreeJob(t, ws)
	if !j.ResolveGoBinary() {
		t.Fatalf("ResolveGoBinary failed:\n%s", rec.Transcript())
	}
	stateRoot := j.ExtensionStateRoot()
	binary := workspacejob.ManagedGoBinary(stateRoot, lockedGo)
	if j.GoBinary != binary {
		t.Fatalf("GoBinary = %q, want the locked install %q", j.GoBinary, binary)
	}
	extension := ".tar.gz"
	if runtime.GOOS == "windows" {
		extension = ".zip"
	}
	wantURL := server.URL + "/dl/go" + lockedGo + "." + runtime.GOOS + "-" + runtime.GOARCH + extension
	for _, want := range []string{
		"Installing Go " + lockedGo + "...",
		"Downloading Go " + lockedGo + " from " + wantURL + "...",
		"Go " + lockedGo + " installed successfully",
	} {
		if !rec.Contains(want) {
			t.Errorf("missing log %q:\n%s", want, rec.Transcript())
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("archive requests = %d, want 1", got)
	}
	if !workspacejob.GoInstallComplete(lockedGo)(workspacejob.ManagedGoDir(stateRoot, lockedGo)) {
		t.Fatal("the managed install is not complete")
	}
	// Windows gets no link: the toolchain resolver finds the install under
	// libs/ there.
	link := filepath.Join(stateRoot, "bin", pkgmeta.ExecutableName(runtime.GOOS, "go"))
	if runtime.GOOS == "windows" {
		if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("bin/go.exe exists on Windows: %v", err)
		}
	} else if target, err := os.Readlink(link); err != nil {
		t.Fatalf("bin/go was not linked: %v", err)
	} else if target != binary {
		t.Fatalf("bin/go links to %q, want %q", target, binary)
	}
	if staged, _ := filepath.Glob(link + ".tmp.*"); len(staged) != 0 {
		t.Fatalf("staged links were left behind: %v", staged)
	}

	// A managed go runs from its own GOROOT, ahead of any other go.
	j.SetupGoEnv("")
	goRoot := filepath.Join(workspacejob.ManagedGoDir(stateRoot, lockedGo), "go")
	if got := j.Env.Get("GOROOT"); got != goRoot {
		t.Fatalf("GOROOT = %q, want %q", got, goRoot)
	}
	if got := j.Env.Get("PATH"); !strings.HasPrefix(got, filepath.Join(goRoot, "bin")+string(filepath.ListSeparator)) {
		t.Fatalf("PATH = %q, want the managed go first", got)
	}

	// The next job finds the complete install and downloads nothing.
	again, rec2, _ := shellFreeJob(t, ws)
	if !again.ResolveGoBinary() || again.GoBinary != binary {
		t.Fatalf("second ResolveGoBinary = %q:\n%s", again.GoBinary, rec2.Transcript())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("a complete install was downloaded again: %d requests", got)
	}

	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Fatalf("the toolchain install started shell programs: %v", invocations)
	}
}

func TestResolveGoBinaryRefusesAnUnverifiedToolchain(t *testing.T) {
	good := hostArchive(t, goDistribution())
	unsafe := hostArchive(t, append(goDistribution(),
		archiveFile{name: "go/../../escaped", content: "owned", mode: 0o644}))
	incomplete := hostArchive(t, goDistribution()[1:])

	for _, tc := range []struct {
		name     string
		archive  []byte
		status   int
		lock     func(url string) any
		wants    []string
		requests int64
	}{
		{
			name: "digest mismatch", archive: good, status: http.StatusOK,
			lock: func(url string) any {
				return map[string]any{"version": lockedGo, "source": url,
					"integrities": map[string]string{hostPlatform(): digest([]byte("other bytes"))}}
			},
			wants:    []string{"Refusing Go " + lockedGo + " from ", pinnedarchive.ErrDigestMismatch.Error()},
			requests: 1,
		},
		{
			name: "no digest for this platform", archive: good, status: http.StatusOK,
			lock: func(url string) any {
				return map[string]any{"version": lockedGo, "source": url,
					"integrities": map[string]string{"plan9/mips": digest(good)}}
			},
			wants: []string{"Refusing to download Go " + lockedGo + ": " + workspacejob.LockFileName +
				" records no SHA-256 for " + hostPlatform()},
		},
		{
			name: "malformed digest", archive: good, status: http.StatusOK,
			lock: func(url string) any {
				return map[string]any{"version": lockedGo, "source": url,
					"integrities": map[string]string{hostPlatform(): "abc"}}
			},
			wants: []string{"Refusing to download Go " + lockedGo + ": the " + hostPlatform() + " SHA-256 in " +
				workspacejob.LockFileName + " is not valid"},
		},
		{
			name: "no lock", archive: good, status: http.StatusOK,
			wants: []string{"Cannot install a verified Go toolchain", "Failed to install Go 1.99.0."},
		},
		{
			name: "lock without a Go toolchain", archive: good, status: http.StatusOK,
			lock:  func(string) any { return nil },
			wants: []string{"Cannot install a verified Go toolchain", "pins no Go toolchain"},
		},
		{
			name: "lock older than the workspace", archive: good, status: http.StatusOK,
			lock: func(url string) any {
				return map[string]any{"version": "1.98.3", "source": url,
					"integrities": map[string]string{hostPlatform(): digest(good)}}
			},
			wants: []string{"Cannot install Go 1.99.0: " + workspacejob.LockFileName +
				" pins Go 1.98.3, which is older than the workspace requires"},
		},
		{
			name: "archive not served", archive: good, status: http.StatusNotFound,
			lock: func(url string) any {
				return map[string]any{"version": lockedGo, "source": url,
					"integrities": map[string]string{hostPlatform(): digest(good)}}
			},
			wants:    []string{"Failed to download Go " + lockedGo + " from "},
			requests: 1,
		},
		{
			name: "entry outside the archive root", archive: unsafe, status: http.StatusOK,
			lock: func(url string) any {
				return map[string]any{"version": lockedGo, "source": url,
					"integrities": map[string]string{hostPlatform(): digest(unsafe)}}
			},
			wants:    []string{"Failed to extract Go archive"},
			requests: 1,
		},
		{
			name: "archive without VERSION", archive: incomplete, status: http.StatusOK,
			lock: func(url string) any {
				return map[string]any{"version": lockedGo, "source": url,
					"integrities": map[string]string{hostPlatform(): digest(incomplete)}}
			},
			wants:    []string{"after extraction"},
			requests: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, requests := distributionServer(t, tc.archive, tc.status)
			parent := jobtest.RealTempDir(t)
			ws := filepath.Join(parent, "ws")
			jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
			if tc.lock != nil {
				writeLock(t, ws, tc.lock(server.URL))
			}

			j, rec, fakes := shellFreeJob(t, ws)
			if j.ResolveGoBinary() {
				t.Fatalf("an unverified toolchain was accepted: %q\n%s", j.GoBinary, rec.Transcript())
			}
			for _, want := range tc.wants {
				if !rec.Contains(want) {
					t.Errorf("missing %q:\n%s", want, rec.Transcript())
				}
			}
			if got := requests.Load(); got != tc.requests {
				t.Errorf("archive requests = %d, want %d", got, tc.requests)
			}
			stateRoot := j.ExtensionStateRoot()
			for _, version := range []string{lockedGo, "1.98.3", "1.99.0"} {
				if _, err := os.Stat(workspacejob.ManagedGoDir(stateRoot, version)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a refused install left %s behind: %v", workspacejob.ManagedGoDir(stateRoot, version), err)
				}
			}
			if _, err := os.Lstat(filepath.Join(stateRoot, "bin", pkgmeta.ExecutableName(runtime.GOOS, "go"))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused install linked bin/go: %v", err)
			}
			// The unsafe entry resolves, from the install directory or the
			// staging directory beside it, to "escaped" beside both; no entry of
			// that name may appear anywhere under the test's root.
			if _, err := os.Lstat(filepath.Join(filepath.Dir(workspacejob.ManagedGoDir(stateRoot, lockedGo)), "escaped")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("an archive entry escaped the install directory: %v", err)
			}
			if escaped := findEntries(t, parent, "escaped"); len(escaped) != 0 {
				t.Errorf("an archive entry escaped the install directory: %v", escaped)
			}
			if invocations := fakes.Invocations(t); len(invocations) != 0 {
				t.Fatalf("the refused install started shell programs: %v", invocations)
			}
		})
	}
}

// The Go distribution ships Windows as a zip. The pin names it, and the same
// verified install unpacks it into the layout the job checks for.
func TestTheWindowsGoDistributionIsAVerifiedZip(t *testing.T) {
	lock := workspacejob.GoLock{
		Version: lockedGo,
		Integrities: map[string]string{
			"windows/amd64": strings.Repeat("a", 64),
			"linux/arm64":   strings.Repeat("b", 64),
		},
	}
	windows := workspacejob.GoArchivePin(lock, "windows", "amd64")
	if windows.Format != pinnedarchive.Zip || windows.URL != "https://go.dev/dl/go"+lockedGo+".windows-amd64.zip" ||
		windows.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("windows pin = %+v", windows)
	}
	linux := workspacejob.GoArchivePin(lock, "linux", "arm64")
	if linux.Format != pinnedarchive.TarGz || linux.URL != "https://go.dev/dl/go"+lockedGo+".linux-arm64.tar.gz" ||
		linux.SHA256 != strings.Repeat("b", 64) {
		t.Fatalf("linux pin = %+v", linux)
	}
	lock.Source = "https://mirror.example.test/go"
	if got := workspacejob.GoArchivePin(lock, "darwin", "arm64"); got.URL != "https://mirror.example.test/go/go"+lockedGo+".darwin-arm64.tar.gz" || got.SHA256 != "" {
		t.Fatalf("darwin pin = %+v", got)
	}

	archive := zipArchive(t, goDistribution())
	server, requests := distributionServer(t, archive, http.StatusOK)
	lock.Source = server.URL
	lock.Integrities["windows/amd64"] = digest(archive)
	pin := workspacejob.GoArchivePin(lock, "windows", "amd64")
	pin.URL = strings.Replace(pin.URL, "https://go.dev/dl/", server.URL+"/", 1)

	dest := filepath.Join(t.TempDir(), "go-"+lockedGo)
	installed, err := pinnedarchive.Install(context.Background(), pin, dest, pinnedarchive.Options{
		Complete: workspacejob.GoInstallComplete(lockedGo),
	})
	if err != nil || !installed {
		t.Fatalf("Install(zip) = %v, %v", installed, err)
	}
	if !workspacejob.GoInstallComplete(lockedGo)(dest) {
		t.Fatal("the unpacked zip is not a complete Go install")
	}

	tampered := pin
	tampered.SHA256 = digest([]byte("tampered"))
	if _, err := pinnedarchive.Install(context.Background(), tampered, filepath.Join(t.TempDir(), "go"), pinnedarchive.Options{
		Complete: workspacejob.GoInstallComplete(lockedGo),
	}); !errors.Is(err, pinnedarchive.ErrDigestMismatch) {
		t.Fatalf("a zip whose digest differs was not refused: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("archive requests = %d, want 2", got)
	}
}

func TestReadGoLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, workspacejob.LockFileName)
	for _, tc := range []struct {
		name, document, version, problem string
	}{
		{"exact", `{"toolchains":{"go":{"version":"1.26.1","integrities":{"linux/amd64":"x"},"source":"s"}},"future":true}`, "1.26.1", ""},
		{"major minor", `{"toolchains":{"go":{"version":" go1.26 "}}}`, "1.26.0", ""},
		{"no go", `{"toolchains":{"bun":{}}}`, "", "pins no Go toolchain"},
		{"null go", `{"toolchains":{"go":null}}`, "", "pins no Go toolchain"},
		{"no version", `{"toolchains":{"go":{"integrities":{}}}}`, "", "pins no Go version"},
		{"malformed entry", `{"toolchains":{"go":{"version":3}}}`, "", "parse toolchains.go"},
		{"malformed lock", `{`, "", "parse "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.document), 0o644); err != nil {
				t.Fatal(err)
			}
			lock, err := workspacejob.ReadGoLock(path)
			if tc.problem != "" {
				if err == nil || !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("ReadGoLock error = %v, want %q", err, tc.problem)
				}
				return
			}
			if err != nil || lock.Version != tc.version {
				t.Fatalf("ReadGoLock = %+v, %v; want version %s", lock, err, tc.version)
			}
		})
	}
	if _, err := workspacejob.ReadGoLock(filepath.Join(dir, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadGoLock(missing) = %v", err)
	}
}

// findEntries returns the paths under root of the entries named name.
func findEntries(t *testing.T, root, name string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == name {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

// A lock whose source carries credentials installs through them, and neither
// the user nor the secret appears in what the job reports: the job downloads
// the URL without its userinfo and sends the userinfo as basic authentication
// to the source's host only, never to a host a redirect leads to.
func TestResolveGoBinaryKeepsTheSourceCredentialsOutOfTheTranscript(t *testing.T) {
	const user, secret = "mirror-user", "s3cret-token"
	archive := hostArchive(t, goDistribution())
	archiveName := "go" + lockedGo + "." + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	if runtime.GOOS == "windows" {
		archiveName = strings.TrimSuffix(archiveName, ".tar.gz") + ".zip"
	}
	var cdnAuthorized atomic.Bool
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			cdnAuthorized.Store(true)
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(cdn.Close)
	var mirrorRequests atomic.Int64
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorRequests.Add(1)
		gotUser, gotSecret, ok := r.BasicAuth()
		if !ok || gotUser != user || gotSecret != secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/dl/moved":
			http.Redirect(w, r, cdn.URL+"/archive", http.StatusFound)
		case "/dl/" + archiveName:
			_, _ = w.Write(archive)
		default:
			http.Redirect(w, r, "/dl/moved", http.StatusFound)
		}
	}))
	t.Cleanup(mirror.Close)
	host := strings.TrimPrefix(mirror.URL, "http://")

	for _, tc := range []struct {
		name, source string
		installs     bool
		wants        []string
		requests     int64
	}{
		{
			name: "credentials accepted", source: "http://" + user + ":" + secret + "@" + host + "/dl",
			installs: true, wants: []string{"Downloading Go " + lockedGo + " from " + mirror.URL + "/dl/go" + lockedGo + "."},
			requests: 1,
		},
		{
			name: "credentials refused", source: "http://" + user + ":wrong-" + secret + "@" + host + "/dl",
			wants: []string{"Failed to download Go " + lockedGo + " from " + mirror.URL + "/dl/go", "HTTP 401"}, requests: 1,
		},
		{
			name: "redirect to another host", source: "http://" + user + ":" + secret + "@" + host + "/elsewhere/",
			installs: true, wants: []string{"Downloading Go " + lockedGo + " from " + mirror.URL + "/elsewhere/go"}, requests: 2,
		},
		{
			name: "source that does not parse", source: "http://" + user + ":" + secret + "@[" + host + "/dl",
			wants: []string{"Refusing to download Go " + lockedGo + ": the source in " + workspacejob.LockFileName + " is not a valid URL"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mirrorRequests.Store(0)
			ws := jobtest.RealTempDir(t)
			jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
			writeLock(t, ws, map[string]any{
				"version":     lockedGo,
				"integrities": map[string]string{hostPlatform(): digest(archive)},
				"source":      tc.source,
			})

			j, rec, _ := shellFreeJob(t, ws)
			if got := j.ResolveGoBinary(); got != tc.installs {
				t.Fatalf("ResolveGoBinary = %v, want %v:\n%s", got, tc.installs, rec.Transcript())
			}
			for _, want := range tc.wants {
				if !rec.Contains(want) {
					t.Errorf("missing %q:\n%s", want, rec.Transcript())
				}
			}
			for _, leaked := range []string{user, secret} {
				if rec.Contains(leaked) {
					t.Errorf("the transcript names %q:\n%s", leaked, rec.Transcript())
				}
			}
			if got := mirrorRequests.Load(); got != tc.requests {
				t.Errorf("mirror requests = %d, want %d", got, tc.requests)
			}
			if cdnAuthorized.Load() {
				t.Error("the credentials were sent to the host a redirect led to")
			}
		})
	}
}
