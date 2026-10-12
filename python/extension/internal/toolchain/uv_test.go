package toolchain

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
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/sdk/extension/uvtoolchain"
)

const (
	uvFeature     = "python/experimental-workspace-integration"
	uvRequirement = "pinned-uv"
)

// fakeUVHost is a machine whose uv programs are files that hold the release
// they report.
type fakeUVHost struct {
	uvHost
	// home is the Putnami home.
	home string
	// onPath is the uv PATH holds, or "".
	onPath string
}

func newFakeUVHost(t *testing.T, goos, goarch string) *fakeUVHost {
	t.Helper()
	host := &fakeUVHost{home: t.TempDir()}
	host.uvHost = uvHost{
		goos:   goos,
		goarch: goarch,
		lookup: func(key string) string {
			if key == putnamihome.Env {
				return host.home
			}
			return ""
		},
		lookPath: func(string) (string, error) {
			if host.onPath == "" {
				return "", exec.ErrNotFound
			}
			return host.onPath, nil
		},
		version: func(path string) (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			if version := strings.TrimSpace(string(data)); version != "" {
				return version, nil
			}
			return "", errors.New("uv self version printed nothing")
		},
	}
	return host
}

// managedDir is where the Putnami home holds uv 0.11.32, the release these
// tests pin.
func (h *fakeUVHost) managedDir() string {
	return filepath.Join(h.home, "toolchains", "uv", "uv-0.11.32")
}

// writeFakeUV writes a uv program that reports version at path.
func writeFakeUV(t *testing.T, path, version string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeUVLock writes a workspace lock that pins release.
func writeUVLock(t *testing.T, workspace string, release UVRelease) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"version": 3, "toolchains": map[string]any{"uv": release}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// declareUV writes a root pyproject.toml whose [tool.uv] table declares spec.
func declareUV(t *testing.T, workspace, spec string) {
	t.Helper()
	content := "[project]\nname = \"ws\"\n\n[tool.uv]\nrequired-version = \"" + spec + "\"\n"
	if err := os.WriteFile(filepath.Join(workspace, RootPyproject), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// uvTarGz is the archive a uv release publishes for a Unix target: the
// directory uv-<target>, which holds uv and uvx. Each program holds the release
// it reports.
func uvTarGz(t *testing.T, target, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	dir := "uv-" + target + "/"
	if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, program := range []string{"uv", "uvx"} {
		body := []byte(version + "\n")
		if err := tw.WriteHeader(&tar.Header{Name: dir + program, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
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

// uvZip is the archive a uv release publishes for Windows: uv.exe, uvx.exe and
// uvw.exe at its top level.
func uvZip(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, program := range []string{"uv.exe", "uvx.exe", "uvw.exe"} {
		header := &zip.FileHeader{Name: program, Method: zip.Deflate}
		header.SetMode(0o644)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(version + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// uvMirror serves uv archives by request path and records every request.
type uvMirror struct {
	*httptest.Server
	mu       sync.Mutex
	archives map[string][]byte
	paths    []string
}

func newUVMirror(t *testing.T) *uvMirror {
	t.Helper()
	mirror := &uvMirror{archives: map[string][]byte{}}
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

func (m *uvMirror) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.paths)
}

// uvFiles lists every uv program under root.
func uvFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.IsDir() && (entry.Name() == "uv" || entry.Name() == "uv.exe") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// A task runs only a uv that reports the release the workspace pins: uv on
// PATH first, then the install under the Putnami home. A uv of another
// release is never selected, and without the pinned one the error names
// `putnami install`.
func TestResolveUV_RunsOnlyThePinnedRelease(t *testing.T) {
	spectest.Proves(t, uvFeature, uvRequirement, "a-uv-of-another-release-never-runs")
	pinned := UVRelease{Version: "0.11.32", Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)}}

	t.Run("a uv of another release on PATH", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "0.5.18")
		workspace := t.TempDir()
		writeUVLock(t, workspace, pinned)

		_, err := resolveUV(host.uvHost, workspace)
		if err == nil {
			t.Fatal("a uv of another release resolved")
		}
		for _, want := range []string{"no uv 0.11.32", host.onPath + " (uv 0.5.18)", "`putnami install`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
	})

	t.Run("the install under the Putnami home", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "0.5.18")
		managed := writeFakeUV(t, filepath.Join(host.managedDir(), "uv"), "0.11.32")
		workspace := t.TempDir()
		writeUVLock(t, workspace, pinned)

		got, err := resolveUV(host.uvHost, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if got != managed {
			t.Fatalf("resolveUV = %q, want the install under the Putnami home %q", got, managed)
		}
	})

	t.Run("the pinned release on PATH comes first", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "0.11.32")
		writeFakeUV(t, filepath.Join(host.managedDir(), "uv"), "0.11.32")
		workspace := t.TempDir()
		writeUVLock(t, workspace, pinned)

		got, err := resolveUV(host.uvHost, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if got != host.onPath {
			t.Fatalf("resolveUV = %q, want the uv on PATH %q", got, host.onPath)
		}
	})

	t.Run("a uv that reports no release", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "")
		workspace := t.TempDir()
		writeUVLock(t, workspace, pinned)

		if got, err := resolveUV(host.uvHost, workspace); err == nil {
			t.Fatalf("resolveUV = %q, want an error", got)
		}
	})
}

// The wanted release is the lock's pin, else the root pyproject.toml's
// declaration, else the default one.
func TestWantedUV_LockThenDeclarationThenDefault(t *testing.T) {
	t.Run("the lock wins over the declaration", func(t *testing.T) {
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{Version: "0.12.0"})
		declareUV(t, workspace, "==0.11.20")
		want, err := wantedUV(workspace)
		if err != nil {
			t.Fatal(err)
		}
		if want.version != "0.12.0" || want.origin != WorkspaceLockFile || want.release.Version != "0.12.0" {
			t.Fatalf("wantedUV = %+v, want the lock's 0.12.0", want)
		}
	})

	t.Run("a declaration without a pin has no digest", func(t *testing.T) {
		workspace := t.TempDir()
		declareUV(t, workspace, "==0.11.20")
		want, err := wantedUV(workspace)
		if err != nil {
			t.Fatal(err)
		}
		if want.version != "0.11.20" || want.origin != uvDeclarationOrigin || want.release.Version != "" {
			t.Fatalf("wantedUV = %+v, want the declared 0.11.20 with no release", want)
		}
	})

	t.Run("a declaration of the default release takes the shipped digests", func(t *testing.T) {
		workspace := t.TempDir()
		declareUV(t, workspace, DefaultUVVersion)
		want, err := wantedUV(workspace)
		if err != nil {
			t.Fatal(err)
		}
		if want.version != DefaultUVVersion || want.release.Integrities["darwin/arm64"] != defaultUVIntegrities["darwin/arm64"] {
			t.Fatalf("wantedUV = %+v, want the default release", want)
		}
	})

	// A required-version that names no single release pins nothing in the
	// CLI, so it fails here, pinned or not, and says what to declare.
	for _, spec := range []string{">=0.5", "==0.11.*", ""} {
		for _, pinned := range []bool{false, true} {
			t.Run(fmt.Sprintf("a declaration of %q, pinned %v", spec, pinned), func(t *testing.T) {
				workspace := t.TempDir()
				declareUV(t, workspace, spec)
				if pinned {
					writeUVLock(t, workspace, DefaultUVRelease())
				}
				want, err := wantedUV(workspace)
				if err == nil {
					t.Fatalf("wantedUV = %+v, want an error", want)
				}
				for _, part := range []string{uvDeclarationOrigin, "names no single uv release", `required-version = "==` + DefaultUVVersion + `"`, "`putnami install`"} {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("the error does not name %q: %v", part, err)
					}
				}
			})
		}
	}

	t.Run("a declared release older than the minimum", func(t *testing.T) {
		workspace := t.TempDir()
		declareUV(t, workspace, "==0.4.30")
		_, err := wantedUV(workspace)
		if err == nil || !strings.Contains(err.Error(), "uv 0.4.30 is older than uv "+uvtoolchain.MinimumVersion) ||
			!strings.Contains(err.Error(), uvDeclarationOrigin) {
			t.Fatalf("err = %v, want the refusal of the declared uv 0.4.30", err)
		}
	})

	t.Run("a pinned release older than the minimum", func(t *testing.T) {
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{Version: "0.11.7"})
		_, err := wantedUV(workspace)
		if err == nil || !strings.Contains(err.Error(), "uv 0.11.7 is older than uv "+uvtoolchain.MinimumVersion) ||
			!strings.Contains(err.Error(), WorkspaceLockFile) {
			t.Fatalf("err = %v, want the refusal of the pinned uv 0.11.7", err)
		}
	})

	t.Run("a pin that names no release", func(t *testing.T) {
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{Version: "../0.11.32"})
		if want, err := wantedUV(workspace); err == nil {
			t.Fatalf("wantedUV = %+v, want an error", want)
		}
	})

	t.Run("a pyproject.toml that cannot be read", func(t *testing.T) {
		workspace := t.TempDir()
		if err := os.Mkdir(filepath.Join(workspace, RootPyproject), 0o755); err != nil {
			t.Fatal(err)
		}
		if want, err := wantedUV(workspace); err == nil || !strings.Contains(err.Error(), "read "+RootPyproject) {
			t.Fatalf("wantedUV = %+v, %v; want the read error", want, err)
		}
	})

	t.Run("a lock that does not parse", func(t *testing.T) {
		workspace := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if want, err := wantedUV(workspace); err == nil {
			t.Fatalf("wantedUV = %+v, want an error", want)
		}
	})

	t.Run("a lock without a uv pin", func(t *testing.T) {
		workspace := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, WorkspaceLockFile), []byte(`{"version":3,"toolchains":{"go":{"version":"1.26.9"},"uv":null}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		want, err := wantedUV(workspace)
		if err != nil {
			t.Fatal(err)
		}
		if want.version != DefaultUVVersion {
			t.Fatalf("wantedUV = %+v, want the default release", want)
		}
	})
}

// `putnami install` installs the pinned uv under the Putnami home when the
// machine holds none: the archive the lock names for the host platform,
// checked against the lock's SHA-256, with the programs at the top of the
// install directory. A uv of another release on PATH does not count, and an
// installed release is not downloaded again.
func TestProvisionUV_InstallsThePinnedArchiveUnderThePutnamiHome(t *testing.T) {
	spectest.Proves(t, uvFeature, uvRequirement, "workspace-install-provisions-the-pinned-uv-and-declares-it")
	mirror := newUVMirror(t)
	mirror.archives["/dl/uv-aarch64-unknown-linux-musl.tar.gz"] = uvTarGz(t, "aarch64-unknown-linux-musl", "0.11.32")
	mirror.archives["/dl/uv-x86_64-pc-windows-msvc.zip"] = uvZip(t, "0.11.32")

	for _, tc := range []struct {
		goos, goarch, archive string
		programs              []string
	}{
		{"linux", "arm64", "/dl/uv-aarch64-unknown-linux-musl.tar.gz", []string{"uv", "uvx"}},
		{"windows", "amd64", "/dl/uv-x86_64-pc-windows-msvc.zip", []string{"uv.exe", "uvx.exe", "uvw.exe"}},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			host := newFakeUVHost(t, tc.goos, tc.goarch)
			host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "0.5.18")
			workspace := t.TempDir()
			writeUVLock(t, workspace, UVRelease{
				Version:     "0.11.32",
				Integrities: map[string]string{tc.goos + "/" + tc.goarch: sha256Hex(mirror.archives[tc.archive])},
				Source:      mirror.URL + "/dl/",
			})
			before := len(mirror.requests())

			uv, err := provisionUV(context.Background(), UVRequest{WorkspaceRoot: workspace, Mode: UVInstall}, host.uvHost)
			if err != nil {
				t.Fatal(err)
			}
			dir := host.managedDir()
			if want := (UV{Path: filepath.Join(dir, tc.programs[0]), Version: "0.11.32"}); uv != want {
				t.Fatalf("provisionUV = %+v, want %+v", uv, want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			slices.Sort(names)
			want := slices.Sorted(slices.Values(tc.programs))
			if !slices.Equal(names, want) {
				t.Fatalf("the install holds %v, want only %v at its top level", names, want)
			}
			if info, err := os.Stat(uv.Path); err != nil || info.Mode().Perm()&0o111 == 0 {
				t.Fatalf("the uv program is not executable: %v, %v", info, err)
			}
			if got := mirror.requests()[before:]; !slices.Equal(got, []string{tc.archive}) {
				t.Fatalf("requests = %v, want the one archive %s", got, tc.archive)
			}

			again, err := provisionUV(context.Background(), UVRequest{WorkspaceRoot: workspace, Mode: UVInstall}, host.uvHost)
			if err != nil {
				t.Fatal(err)
			}
			if again != uv {
				t.Fatalf("second provisionUV = %+v, want %+v", again, uv)
			}
			if got := len(mirror.requests()) - before; got != 1 {
				t.Fatalf("an installed release was downloaded again (%d requests)", got)
			}
		})
	}
}

// With nothing pinned or declared, the default release installs from the uv
// release downloads with the digests the extension ships; on this platform
// that must name an archive and a digest.
func TestDefaultUVRelease_NamesAnArchiveForEveryPlatform(t *testing.T) {
	release := DefaultUVRelease()
	for platform, digest := range defaultUVIntegrities {
		goos, goarch, _ := strings.Cut(platform, "/")
		archive, ok := uvArchiveFor(release, goos, goarch)
		if !ok {
			t.Fatalf("no archive for %s", platform)
		}
		if archive.pin.SHA256 != digest || len(digest) != 64 {
			t.Fatalf("%s digest = %q, want the shipped %q", platform, archive.pin.SHA256, digest)
		}
		if !strings.HasPrefix(archive.pin.URL, "https://github.com/astral-sh/uv/releases/download/"+DefaultUVVersion+"/uv-") {
			t.Fatalf("%s archive = %q, want the uv release downloads", platform, archive.pin.URL)
		}
	}
	if platforms := uvtoolchain.Platforms(); len(defaultUVIntegrities) != len(platforms) {
		t.Fatalf("the default release has %d digests for the %d platforms %v", len(defaultUVIntegrities), len(platforms), platforms)
	}
	if err := uvtoolchain.Supported(DefaultUVVersion); err != nil {
		t.Fatalf("the default release is not one Putnami runs: %v", err)
	}
	if _, ok := uvArchiveFor(release, "freebsd", "amd64"); ok {
		t.Fatal("an archive was named for a platform uv publishes none for")
	}
}

// An archive whose SHA-256 differs from the pin is refused, and so is a
// release that records no SHA-256 for the host: nothing is extracted and no uv
// is left on disk.
func TestProvisionUV_RefusesAnArchiveItCannotVerify(t *testing.T) {
	spectest.Proves(t, uvFeature, uvRequirement, "a-uv-archive-that-fails-its-digest-is-refused")
	mirror := newUVMirror(t)
	mirror.archives["/dl/uv-aarch64-unknown-linux-musl.tar.gz"] = uvTarGz(t, "aarch64-unknown-linux-musl", "0.11.32")

	t.Run("the digest differs", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "arm64")
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{
			Version:     "0.11.32",
			Integrities: map[string]string{"linux/arm64": sha256Hex([]byte("another archive"))},
			Source:      mirror.URL + "/dl/",
		})
		_, err := provisionUV(context.Background(), UVRequest{WorkspaceRoot: workspace, Mode: UVInstall}, host.uvHost)
		if err == nil {
			t.Fatal("an archive whose SHA-256 differs from the pin was installed")
		}
		for _, want := range []string{"uv 0.11.32", WorkspaceLockFile, "`putnami install`"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
		if left := uvFiles(t, host.home); len(left) != 0 {
			t.Fatalf("the refused install left %v on disk", left)
		}
	})

	t.Run("the lock records no digest for the host", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "arm64")
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{
			Version:     "0.11.32",
			Integrities: map[string]string{"darwin/arm64": strings.Repeat("a", 64)},
			Source:      mirror.URL + "/dl/",
		})
		before := len(mirror.requests())
		_, err := provisionUV(context.Background(), UVRequest{WorkspaceRoot: workspace, Mode: UVInstall}, host.uvHost)
		if err == nil || !strings.Contains(err.Error(), "linux/arm64") {
			t.Fatalf("err = %v, want a refusal that names linux/arm64", err)
		}
		if got := len(mirror.requests()) - before; got != 0 {
			t.Fatalf("an unverifiable archive was requested %d times", got)
		}
	})
}

// A hosted run installs no uv, and a release that is declared and not pinned
// yet has no digest to check its download against: both fail without a
// request, and name what to do.
func TestProvisionUV_InstallsNothingItCannotVerifyOrMustNot(t *testing.T) {
	spectest.Proves(t, uvFeature, uvRequirement, "a-hosted-run-installs-no-uv")
	mirror := newUVMirror(t)

	t.Run("a hosted run", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "0.5.18")
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{Version: "0.11.32", Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)}, Source: mirror.URL})
		_, err := provisionUV(context.Background(), UVRequest{WorkspaceRoot: workspace, Mode: UVFind}, host.uvHost)
		if err == nil || !strings.Contains(err.Error(), "hosted run installs no toolchain") {
			t.Fatalf("err = %v, want the hosted refusal", err)
		}
	})

	t.Run("a hosted run with the pinned uv on PATH", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "0.11.32")
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{Version: "0.11.32"})
		uv, err := provisionUV(context.Background(), UVRequest{WorkspaceRoot: workspace, Mode: UVFind}, host.uvHost)
		if err != nil {
			t.Fatal(err)
		}
		if want := (UV{Path: host.onPath, Version: "0.11.32"}); uv != want {
			t.Fatalf("provisionUV = %+v, want %+v", uv, want)
		}
	})

	t.Run("a declared release the lock does not pin", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		workspace := t.TempDir()
		declareUV(t, workspace, "==0.12.0")
		_, err := provisionUV(context.Background(), UVRequest{WorkspaceRoot: workspace, Mode: UVInstall}, host.uvHost)
		if err == nil || !strings.Contains(err.Error(), "`putnami install`") || !strings.Contains(err.Error(), "0.12.0") {
			t.Fatalf("err = %v, want a refusal that names uv 0.12.0 and `putnami install`", err)
		}
	})

	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("requests = %v, want none", got)
	}
}

// The digests the extension ships for its default release are the ones the
// repository's own lock pins for that release, which the CLI read from the
// release's sha256.sum. While the two name different releases there is
// nothing to compare.
func TestDefaultUVReleaseMatchesTheRepositoryLock(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	locked, pinned, err := lockedUVRelease(root)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Fatalf("the repository %s pins no uv", WorkspaceLockFile)
	}
	if locked.Version != DefaultUVVersion {
		t.Skipf("the repository lock pins uv %s and the extension ships uv %s", locked.Version, DefaultUVVersion)
	}
	if !maps.Equal(locked.Integrities, defaultUVIntegrities) {
		t.Fatalf("the repository lock pins %v for uv %s, the extension ships %v", locked.Integrities, DefaultUVVersion, defaultUVIntegrities)
	}
	if want := DefaultUVRelease().Source; locked.Source != want {
		t.Fatalf("the repository lock takes uv %s from %q, the extension from %q", DefaultUVVersion, locked.Source, want)
	}
}

// A managed directory that is complete but holds another release is kept by
// the archive install, which then reports nothing installed: the job does not
// claim it installed uv, downloads nothing, and names the directory to remove.
func TestProvisionUV_KeepsAndRefusesAStaleInstall(t *testing.T) {
	mirror := newUVMirror(t)
	host := newFakeUVHost(t, "linux", "amd64")
	writeFakeUV(t, filepath.Join(host.managedDir(), "uv"), "0.11.31")
	workspace := t.TempDir()
	writeUVLock(t, workspace, UVRelease{
		Version:     "0.11.32",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)},
		Source:      mirror.URL + "/dl/",
	})
	var logged []string
	_, err := provisionUV(context.Background(), UVRequest{
		WorkspaceRoot: workspace,
		Mode:          UVInstall,
		Log:           func(_, message string) { logged = append(logged, message) },
	}, host.uvHost)
	if err == nil || !strings.Contains(err.Error(), "holds uv 0.11.31, not uv 0.11.32") {
		t.Fatalf("err = %v, want the stale install refused", err)
	}
	for _, line := range logged {
		if strings.Contains(line, "uv 0.11.32 installed") {
			t.Fatalf("the job logged an install it did not make: %q", logged)
		}
	}
	if !slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "Kept the existing uv install") }) {
		t.Fatalf("logged %q, want the kept install named", logged)
	}
	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("requests = %v, want none", got)
	}
}

// The uv the CLI resolves for a task and the uv the extension runs are the
// same program, at the same place under the Putnami home, probed the same way.
func TestManagedUVDirMatchesTheManifestCandidate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Runtime struct {
			Toolchains map[string]struct {
				Lock       string `json:"lock"`
				Candidates []struct {
					From string `json:"from"`
					Path string `json:"path"`
				} `json:"candidates"`
				Probe struct {
					Args []string `json:"args"`
				} `json:"probe"`
			} `json:"toolchains"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	declared, ok := manifest.Runtime.Toolchains[uvToolchainName]
	if !ok || declared.Lock != "uv" {
		t.Fatalf("the manifest declares no uv runtime toolchain locked on uv: %+v", manifest.Runtime.Toolchains)
	}
	host := newFakeUVHost(t, "linux", "amd64")
	search := newUVSearch(host.uvHost, t.TempDir(), uvWant{version: "0.11.32"})
	var homeCandidate string
	for _, candidate := range declared.Candidates {
		if candidate.From == "putnami-home" {
			homeCandidate = strings.ReplaceAll(candidate.Path, "{version}", "0.11.32")
		}
	}
	if want := filepath.Join(host.home, filepath.FromSlash(homeCandidate)); search.program() != want {
		t.Fatalf("the extension runs %q, the manifest resolves %q", search.program(), want)
	}
	if !slices.Equal(declared.Probe.Args, uvVersionArgs) {
		t.Fatalf("the manifest probes uv with %v, the extension with %v", declared.Probe.Args, uvVersionArgs)
	}
	if declared.Candidates[0].From != "path" || declared.Candidates[0].Path != "uv" {
		t.Fatalf("the first candidate is %+v, want uv on PATH as the extension searches first", declared.Candidates[0])
	}
}
