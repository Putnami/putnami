package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// provisionedProgram is where the CLI leaves the uv program of uv 0.11.32 on
// this host: the executable its archive holds, as the pin names it.
func (h *fakeUVHost) provisionedProgram(t *testing.T) string {
	t.Helper()
	archive, ok := uvtoolchain.ArchiveFor(h.goos, h.goarch)
	if !ok {
		t.Fatalf("uv publishes no archive for %s/%s", h.goos, h.goarch)
	}
	return filepath.Join(h.managedDir(), archive.Dir, uvProgram(h.goos))
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

// A task runs only a uv that reports the release the workspace pins: uv on
// PATH first, then the release the CLI provisioned under the Putnami home. A
// uv of another release is never selected, and without the pinned one the
// error names `putnami install`.
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

	t.Run("the release under the Putnami home", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "0.5.18")
		provisioned := writeFakeUV(t, host.provisionedProgram(t), "0.11.32")
		workspace := t.TempDir()
		writeUVLock(t, workspace, pinned)

		got, err := resolveUV(host.uvHost, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if got != provisioned {
			t.Fatalf("resolveUV = %q, want the release under the Putnami home %q", got, provisioned)
		}
	})

	t.Run("the pinned release on PATH comes first", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		host.onPath = writeFakeUV(t, filepath.Join(t.TempDir(), "uv"), "0.11.32")
		writeFakeUV(t, host.provisionedProgram(t), "0.11.32")
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

// The task runtime lists the release the CLI provisions under the Putnami home
// as a candidate: the executable the pin names inside the release directory,
// uv-<target>/uv for a tar.gz archive and uv.exe at the top for a zip archive.
// A job that finds no uv on PATH runs it.
func TestResolveUV_RunsTheReleaseUnderThePutnamiHome(t *testing.T) {
	spectest.Proves(t, uvFeature, uvRequirement, "a-job-without-a-uv-on-path-runs-the-release-under-the-putnami-home")
	for _, tc := range []struct{ goos, goarch, executable string }{
		{"linux", "amd64", "uv-x86_64-unknown-linux-musl/uv"},
		{"darwin", "arm64", "uv-aarch64-apple-darwin/uv"},
		{"windows", "amd64", "uv.exe"},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			host := newFakeUVHost(t, tc.goos, tc.goarch)
			workspace := t.TempDir()
			writeUVLock(t, workspace, UVRelease{Version: "0.11.32"})
			want := writeFakeUV(t, filepath.Join(host.managedDir(), filepath.FromSlash(tc.executable)), "0.11.32")
			if got := host.provisionedProgram(t); got != want {
				t.Fatalf("provisionedProgram = %q, want the pin's executable %q", got, want)
			}

			got, err := resolveUV(host.uvHost, workspace)
			if err != nil || got != want {
				t.Fatalf("resolveUV = %q, %v; want %q", got, err, want)
			}
		})
	}

	t.Run("a uv at the top of a tar.gz release directory", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "arm64")
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{Version: "0.11.32"})
		want := writeFakeUV(t, filepath.Join(host.managedDir(), "uv"), "0.11.32")
		if got, err := resolveUV(host.uvHost, workspace); err != nil || got != want {
			t.Fatalf("resolveUV = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("a provisioned uv of another release", func(t *testing.T) {
		host := newFakeUVHost(t, "linux", "amd64")
		workspace := t.TempDir()
		writeUVLock(t, workspace, UVRelease{Version: "0.11.32"})
		other := writeFakeUV(t, host.provisionedProgram(t), "0.11.31")
		_, err := resolveUV(host.uvHost, workspace)
		if err == nil || !strings.Contains(err.Error(), other+" (uv 0.11.31)") {
			t.Fatalf("err = %v, want the uv of another release passed over", err)
		}
	})
}

// With no uv that reports the release, the error names the release and
// `putnami install`, which provisions it, whether the lock pins the release or
// the root pyproject.toml declares it. Nothing is installed.
func TestResolveUV_WithoutTheReleaseNamesPutnamiInstall(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, workspace string) string{
		"pinned": func(t *testing.T, workspace string) string {
			writeUVLock(t, workspace, DefaultUVRelease())
			return DefaultUVVersion
		},
		"declared": func(t *testing.T, workspace string) string { declareUV(t, workspace, "==0.12.0"); return "0.12.0" },
		"default":  func(*testing.T, string) string { return DefaultUVVersion },
	} {
		t.Run(name, func(t *testing.T) {
			host := newFakeUVHost(t, "linux", "amd64")
			workspace := t.TempDir()
			version := prepare(t, workspace)
			_, err := resolveUV(host.uvHost, workspace)
			if err == nil {
				t.Fatal("a uv resolved on a machine that holds none")
			}
			for _, want := range []string{"no uv " + version + " on this machine", "`putnami install`", "provisions it"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %q: %v", want, err)
				}
			}
			if entries, err := os.ReadDir(host.home); err != nil || len(entries) != 0 {
				t.Fatalf("the Putnami home holds %v, %v; want nothing installed", entries, err)
			}
		})
	}
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
		if want.version != "0.12.0" {
			t.Fatalf("wantedUV = %+v, want the lock's 0.12.0", want)
		}
	})

	t.Run("a declaration without a pin", func(t *testing.T) {
		workspace := t.TempDir()
		declareUV(t, workspace, "==0.11.20")
		want, err := wantedUV(workspace)
		if err != nil {
			t.Fatal(err)
		}
		if want.version != "0.11.20" {
			t.Fatalf("wantedUV = %+v, want the declared 0.11.20", want)
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

// With nothing pinned or declared, the default release is pinned from the uv
// release downloads with the digests the extension ships; every platform must
// name an archive and a digest.
func TestDefaultUVRelease_NamesAnArchiveForEveryPlatform(t *testing.T) {
	release := DefaultUVRelease()
	shipped := uvtoolchain.DefaultIntegrities()
	for platform, digest := range shipped {
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
	if platforms := uvtoolchain.Platforms(); len(shipped) != len(platforms) {
		t.Fatalf("the default release has %d digests for the %d platforms %v", len(shipped), len(platforms), platforms)
	}
	if err := uvtoolchain.Supported(DefaultUVVersion); err != nil {
		t.Fatalf("the default release is not one Putnami runs: %v", err)
	}
	if _, ok := uvArchiveFor(release, "freebsd", "amd64"); ok {
		t.Fatal("an archive was named for a platform uv publishes none for")
	}
}

// The uv the CLI resolves for a task and the uv the extension runs are the
// same program, probed the same way: the manifest lists uv on PATH, then the
// provisioned release, whose program is the executable the extension's pin
// names for the host platform.
func TestProvisionedUVMatchesTheManifestCandidate(t *testing.T) {
	spectest.Proves(t, uvFeature, uvRequirement, "the-provisioned-uv-is-a-task-runtime-candidate")
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
	if len(declared.Candidates) != 2 || declared.Candidates[0].From != "path" || declared.Candidates[0].Path != "uv" ||
		declared.Candidates[1].From != "provisioned" || declared.Candidates[1].Path != "" {
		t.Fatalf("candidates = %+v, want uv on PATH, which the extension searches first, then the provisioned release", declared.Candidates)
	}
	for _, platform := range uvtoolchain.Platforms() {
		goos, goarch, _ := strings.Cut(platform, "/")
		host := newFakeUVHost(t, goos, goarch)
		pin, err := uvLockPin(DefaultUVRelease())
		if err != nil {
			t.Fatal(err)
		}
		// The CLI unpacks the pinned archive in toolchains/uv/uv-<version>.
		want := filepath.Join(host.home, "toolchains", "uv", "uv-"+DefaultUVVersion, filepath.FromSlash(pin.Archives[platform].Executable))
		search := newUVSearch(host.uvHost, t.TempDir(), uvWant{version: DefaultUVVersion})
		writeFakeUV(t, want, DefaultUVVersion)
		if got := search.provisioned(); got != want {
			t.Fatalf("%s: the extension runs %q, the pin resolves %q", platform, got, want)
		}
	}
	if !slices.Equal(declared.Probe.Args, uvVersionArgs) {
		t.Fatalf("the manifest probes uv with %v, the extension with %v", declared.Probe.Args, uvVersionArgs)
	}
}
