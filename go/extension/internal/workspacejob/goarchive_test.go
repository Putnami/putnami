package workspacejob_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/sdk/extension/pinnedarchive"
)

const lockedGo = "1.99.1"

type archiveFile struct {
	name    string
	content string
	mode    int64
}

// goDistribution is the layout of a Go release for this host: a go/ directory
// holding bin/go and the VERSION file naming the release.
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

// provisionGo places Go lockedGo under root as the CLI provisions it there, a
// go-<version> directory holding the unpacked distribution, and returns its go
// command.
func provisionGo(t *testing.T, root string) string {
	t.Helper()
	for _, file := range goDistribution() {
		path := filepath.Join(workspacejob.ManagedGoDir(root, lockedGo), filepath.FromSlash(file.name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.content), os.FileMode(file.mode)); err != nil {
			t.Fatal(err)
		}
	}
	return workspacejob.ManagedGoBinary(root, lockedGo)
}

// forbiddenSource is a download source that fails the test when it is asked
// for anything: a lock that names it proves the job downloads no Go.
func forbiddenSource(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the job requested %s", r.URL)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/dl"
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

// lockedWorkspace is a workspace that requires Go 1.99.0 and whose lock pins
// lockedGo, published by a source the job must never ask.
func lockedWorkspace(t *testing.T) string {
	t.Helper()
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
	writeLock(t, ws, map[string]any{
		"version":     lockedGo,
		"integrities": map[string]string{hostPlatform(): strings.Repeat("a", 64)},
		"source":      forbiddenSource(t),
	})
	return ws
}

func hostPlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// shellFreeJob is a job whose PATH holds only traps: every shell and every
// tool a shell script would start fails the test when started, and no go command
// can be found there. Its Putnami home is its own, and empty.
func shellFreeJob(t *testing.T, ws string) (*workspacejob.Job, *jobtest.Recorder, *jobtest.Fakes) {
	t.Helper()
	return shellFreeJobAt(t, ws, jobtest.RealTempDir(t))
}

// shellFreeJobAt is shellFreeJob with putnamiHome as the job's Putnami home,
// so several jobs, of one workspace or of several, share the Go provisioned
// there as the jobs of one machine do.
func shellFreeJobAt(t *testing.T, ws, putnamiHome string) (*workspacejob.Job, *jobtest.Recorder, *jobtest.Fakes) {
	t.Helper()
	fakes := jobtest.NewFakes(t)
	env := append([]string{
		"PATH=" + fakes.Dir,
		"HOME=" + t.TempDir(),
		"PUTNAMI_HOME=" + putnamiHome,
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

// entryNames returns the sorted names of the entries of dir.
func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// The Go distribution ships Windows as a zip and every other platform as a
// gzip-compressed tar, named go<version>.<goos>-<goarch> under the lock's
// source, go.dev/dl by default.
func TestGoArchivePinNamesTheDistributionArchive(t *testing.T) {
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
		{"path in the version", `{"toolchains":{"go":{"version":"1.25.7/../../../../opt/x"}}}`, "", "not a Go release name"},
		{"parent in the version", `{"toolchains":{"go":{"version":"../1.25.7"}}}`, "", "not a Go release name"},
		{"suffix in the version", `{"toolchains":{"go":{"version":"1.25.7-custom"}}}`, "", "not a Go release name"},
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

// A lock whose Go version is not a release name selects no Go: the version
// names a directory every workspace of the machine shares, so a go command at
// the path it would name outside the toolchain root is never run.
func TestALockGoVersionThatIsNotAReleaseNameSelectsNoGo(t *testing.T) {
	for _, version := range []string{"1.24.0/../../../x", "../1.25.7", "1.25.7/x", "1.25.7-custom"} {
		ws := jobtest.RealTempDir(t)
		jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
		writeLock(t, ws, map[string]any{
			"version":     version,
			"integrities": map[string]string{hostPlatform(): strings.Repeat("a", 64)},
			"source":      forbiddenSource(t),
		})
		j, rec, _ := shellFreeJobAt(t, ws, filepath.Join(jobtest.RealTempDir(t), "home"))
		planted := workspacejob.ManagedGoBinary(j.GoToolchainRoot(), version)
		if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(planted, []byte("not a program"), 0o755); err != nil {
			t.Fatal(err)
		}
		if j.FindGoBinary() {
			t.Fatalf("version %q: FindGoBinary selected %q:\n%s", version, j.GoBinary, rec.Transcript())
		}
	}
	for _, version := range []string{"1.25.7", "1.26", "1.26rc1", "1.26beta2"} {
		if !workspacejob.IsPlainGoRelease(version) {
			t.Errorf("IsPlainGoRelease(%q) = false, want true", version)
		}
	}
	for _, version := range []string{"", "1", "1.25.7/x", "../1.25.7", "1.25.7-custom"} {
		if workspacejob.IsPlainGoRelease(version) {
			t.Errorf("IsPlainGoRelease(%q) = true, want false", version)
		}
	}
}
