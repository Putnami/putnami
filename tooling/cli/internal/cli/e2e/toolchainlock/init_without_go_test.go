package toolchainlock

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// goExtensionFixtureEnv makes the test binary run as the runtime of the
// fixture @putnami/go extension; its value is the directory the runtime
// records its runs in.
const goExtensionFixtureEnv = "PUTNAMI_TOOLCHAINLOCK_GO_EXTENSION_FIXTURE"

const (
	// initGoRelease is the Go release the fixture template declares, and the
	// one the release index offers.
	initGoRelease = "1.26.1"
	// initFrameworkVersion is the version the fixture Go module proxy answers
	// for the Go framework's @latest.
	initFrameworkVersion = "v0.7.3"
)

// A consumer who installed only Putnami runs `putnami init --extension go
// --project app` on a host with no Go at all. Init installs the Go extension
// and the go-server template from the registry. Create writes go.work, pins
// the Go it declares from the release index, and runs workspace-install over
// the new project, which downloads the pinned archive and checks it against
// the pinned SHA-256 before it installs it. The project is set up with that
// Go, and a build runs with it. The chain is the real one: WorkspaceInit → DepsInstall and
// ProjectsCreate → RunWorkspaceJob → Engine.Run → runtime toolchain resolution
// → the extension's task. The registry, the Go release index and download, and
// the Go framework proxy are local servers, so nothing reaches the network.
//
// The extension is a fixture whose workspace-install does what @putnami/go's
// does for these steps: it installs nothing while nothing declares Go, and
// installs the release the lock pins, verified, where its candidates look.
func TestInit_SetsAGoProjectUpOnAHostWithoutGo(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "create-installs-a-missing-go",
		"init-and-create-install-the-pinned-go-through-the-engine")
	clitest.RequireShell(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	wsRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(wsRoot)
	hometest.Temp(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(t.TempDir(), "artifacts"))
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
	t.Setenv("DO_NOT_TRACK", "1")
	t.Setenv("PATH", t.TempDir())
	if found, err := exec.LookPath("go"); err == nil {
		t.Fatalf("PATH still offers go at %s; this test guards nothing", found)
	}

	records := t.TempDir()
	goCalls := filepath.Join(records, "go-calls.txt")
	goArchive := goReleaseArchive(t, goCalls)
	goArchiveName := fmt.Sprintf("go%s.%s-%s.tar.gz", initGoRelease, runtime.GOOS, runtime.GOARCH)
	goArchiveDigest := sha256Hex(goArchive)
	var goDownloads atomic.Int32
	goDev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]goDevRelease{{
				Version: "go" + initGoRelease,
				Files: []goDevFile{{
					Filename: goArchiveName, OS: runtime.GOOS, Arch: runtime.GOARCH,
					SHA256: goArchiveDigest, Kind: "archive",
				}},
			}})
		case "/dl/" + goArchiveName:
			goDownloads.Add(1)
			_, _ = w.Write(goArchive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(goDev.Close)
	t.Cleanup(versioncmd.SetGoReleaseEndpoints(goDev.URL+"/dl/?mode=json&include=all", goDev.URL+"/dl/"))

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/go.putnami.dev/app/@latest" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"Version":"`+initFrameworkVersion+`"}`)
	}))
	t.Cleanup(proxy.Close)
	// The Go settings of the host must not reach the lookup: the fixture proxy
	// is the only one, and no go env file or private pattern applies.
	t.Setenv("GOPROXY", proxy.URL)
	t.Setenv("GOENV", "off")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GOPRIVATE", "")

	registry := newFixtureRegistry(t, map[string][]byte{
		"/putnami/go/download":        goExtensionArchive(t, records),
		"/putnami/go-server/download": goServerTemplateArchive(t),
	})
	t.Setenv("PUTNAMI_REGISTRY_URL", registry.URL)

	env := lifecycle.LifecycleEnv{Out: io.Discard, RunJob: cli.RunWorkspaceJob}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	var initErr error
	transcript := clitest.CaptureStdoutStderr(t, func() {
		initErr = lifecycle.WorkspaceInit(context.Background(), "",
			[]string{"--workspace", "go-ws", "--extension", "go", "--project", "app"}, env)
	})
	if initErr != nil {
		t.Fatalf("putnami init --extension go --project app on a host without go: %v\n%s", initErr, transcript)
	}
	// Create's workspace-install starts before the pinned Go exists and
	// installs it. It says so at info level and warns about no toolchain.
	if got := logs.String(); strings.Contains(got, `level=WARN msg="runtime toolchain`) ||
		!strings.Contains(got, "workspace-install installed the pinned toolchain") {
		t.Fatalf("runtime toolchain logs of init =\n%s\nwant the installed Go reported at info level and no warning", got)
	}

	// Before the project exists, workspace-install matches no project. Create's
	// run installs the pinned Go, and init's last install finds it in place.
	if got, want := fixtureRuns(t, records, "install"), []string{"installed", "present"}; !slices.Equal(got, want) {
		t.Fatalf("workspace-install runs = %v, want %v: the pinned Go installed for the new project, then found in place\n%s",
			got, want, transcript)
	}
	if got := goDownloads.Load(); got != 1 {
		t.Fatalf("the Go archive was downloaded %d times, want once", got)
	}
	lock, err := lockfile.ReadLockFile(wsRoot)
	if err != nil || lock == nil {
		t.Fatalf("read the lock init left: %v", err)
	}
	pin, pinned := lock.GetToolchain("go")
	if !pinned || pin.Version != initGoRelease || pin.Source != goDev.URL+"/dl/" ||
		pin.Integrities[lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)] != goArchiveDigest {
		t.Fatalf("toolchains.go = %+v (present %v), want %s from %s/dl/ with the archive's SHA-256 %s",
			pin, pinned, initGoRelease, goDev.URL, goArchiveDigest)
	}
	if data, err := os.ReadFile(filepath.Join(wsRoot, "go.work")); err != nil || string(data) != "go "+initGoRelease+"\n\nuse ./app\n" {
		t.Fatalf("go.work = %q, %v; want the bytes create writes for app", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(wsRoot, "app", "go.mod")); err != nil ||
		!strings.Contains(string(data), "go.putnami.dev/app "+initFrameworkVersion) {
		t.Fatalf("app/go.mod = %q, %v; want the framework release the local proxy lists", data, err)
	}
	managedGo := filepath.Join(wsRoot, ".putnami", "extensions", "@putnami-go", "libs", "go-"+initGoRelease, "go", "bin", "go")
	wantCalls := []string{
		"work use ./app|" + managedGo,
		"work edit -go=" + initGoRelease + "|" + managedGo,
		"mod edit -go=" + initGoRelease + "|" + managedGo,
		"mod tidy|" + managedGo,
	}
	if got := readLines(t, goCalls); !slices.Equal(got, wantCalls) {
		t.Fatalf("go calls =\n%s\nwant, with the installed Go:\n%s", strings.Join(got, "\n"), strings.Join(wantCalls, "\n"))
	}

	// The tasks accept the Go the install left: a build runs with it on PATH.
	workspace.InvalidateLoadCache(wsRoot)
	var result lifecycle.WorkspaceJobResult
	var buildErr error
	transcript = clitest.CaptureStdoutStderr(t, func() {
		result, buildErr = cli.RunWorkspaceJob(context.Background(), lifecycle.WorkspaceJobRequest{
			WorkspaceRoot: wsRoot, Config: wsproto.Load(wsRoot), Job: "build", Out: io.Discard,
		})
	})
	if buildErr != nil || result.Outcome != lifecycle.WorkspaceJobOK {
		t.Fatalf("build after init: outcome %v, %v\n%s", result.Outcome, buildErr, transcript)
	}
	if got := fixtureRuns(t, records, "build"); !slices.Equal(got, []string{managedGo}) {
		t.Fatalf("build runs = %v, want one run with %s first on PATH\n%s", got, managedGo, transcript)
	}
	if got := goDownloads.Load(); got != 1 {
		t.Fatalf("the build downloaded the Go archive again: %d downloads", got)
	}
	if unexpected := registry.unexpected(); len(unexpected) > 0 {
		t.Fatalf("the registry was asked for artifacts the fixture does not serve: %v", unexpected)
	}
}

// goDevRelease is one entry of the go.dev release index
// (https://go.dev/dl/?mode=json&include=all), as the fake go.dev serves it.
type goDevRelease struct {
	Version string      `json:"version"`
	Files   []goDevFile `json:"files"`
}

// goDevFile is one downloadable file of a goDevRelease.
type goDevFile struct {
	Filename string `json:"filename"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	SHA256   string `json:"sha256"`
	Kind     string `json:"kind"`
}

// fixtureRegistry serves extension and template archives by download path,
// with the headers the registry sends, and answers anything else 404.
type fixtureRegistry struct {
	*httptest.Server
	mu    sync.Mutex
	other []string
}

// optionalArtifacts are the registry paths a request may miss without the
// test failing: init installs the starter's agent content and goes on when it
// does not resolve.
var optionalArtifacts = []string{"/putnami/contributor/download"}

func newFixtureRegistry(t *testing.T, archives map[string][]byte) *fixtureRegistry {
	t.Helper()
	registry := &fixtureRegistry{}
	registry.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archive, ok := archives[r.URL.Path]
		if !ok {
			if !slices.Contains(optionalArtifacts, r.URL.Path) {
				registry.mu.Lock()
				registry.other = append(registry.other, r.URL.String())
				registry.mu.Unlock()
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Resolved-Version", "0.1.0")
		w.Header().Set("X-Integrity", sha256Hex(archive))
		_, _ = w.Write(archive)
	}))
	t.Cleanup(registry.Close)
	return registry
}

func (r *fixtureRegistry) unexpected() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.other...)
}

// goExtensionArchive is the fixture @putnami/go: its runtime execs the test
// binary as the extension (runGoExtensionFixture), and its run toolchain
// resolves the Go the lock pins from PATH or from the install inside the
// workspace, the candidates @putnami/go declares for a host without Go.
func goExtensionArchive(t *testing.T, records string) []byte {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "name": "@putnami/go",
  "version": "0.1.0",
  "cliContract": ` + fmt.Sprint(protocolcli.CurrentContract) + `,
  "runtime": {
    "executable": "bin/runtime",
    "toolchains": {
      "runtimeCompiler": {
        "lock": "go",
        "candidates": [
          {"from": "path", "path": "go"},
          {"from": "environment", "environment": "PUTNAMI_WORKSPACE_ROOT", "path": ".putnami/extensions/@putnami-go/libs/go-{version}/go/bin/go"}
        ],
        "probe": {"args": ["env", "GOVERSION"], "expect": "go{version}"},
        "prependPath": true
      }
    },
    "runToolchains": ["runtimeCompiler"]
  },
  "commands": {
    "workspace-install": {
      "description": "Install the pinned Go toolchain.",
      "run": [{ "id": "workspace-install", "task": "workspace-install-exec" }]
    },
    "build": {
      "description": "Build a Go project.",
      "run": [{ "id": "compile", "task": "build-task" }]
    }
  },
  "tasks": {
    "workspace-install-exec": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["workspace-install"],
      "cwd": "{workspaceRoot}",
      "cache": false,
      "timeoutMs": 60000
    },
    "build-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["build"],
      "cache": false,
      "timeoutMs": 60000
    }
  }
}`
	runtimeScript := "#!/bin/sh\n" +
		goExtensionFixtureEnv + "='" + records + "'\n" +
		"export " + goExtensionFixtureEnv + "\n" +
		"exec '" + testBinary + "' \"$@\"\n"
	return tarGz(t, []tarEntry{
		{name: "bin/runtime", mode: 0o755, content: runtimeScript},
		{name: "putnami.extension.json", mode: 0o644, content: manifest},
	})
}

// goServerTemplateArchive is the fixture go-server template: a Go module on
// the fixture Go release that requires the Go framework.
func goServerTemplateArchive(t *testing.T) []byte {
	t.Helper()
	return tarGz(t, []tarEntry{
		{name: "go.mod.template", mode: 0o644, content: "module <%= projectModule %>\n\ngo " + initGoRelease +
			"\n\nrequire go.putnami.dev/app <%= goFrameworkVersion %>\n"},
		{name: "putnami.json.template", mode: 0o644, content: `{"name":"<%= projectName %>","extensions":["@putnami/go"]}`},
		{name: "putnami.template.json", mode: 0o644, content: `{"name":"go-server","description":"Go server","extension":"@putnami/go","version":"0.1.0"}`},
	})
}

// goReleaseArchive is the Go release archive: a go command that reports the
// fixture release and records every other invocation in calls.
func goReleaseArchive(t *testing.T, calls string) []byte {
	t.Helper()
	goCommand := "#!/bin/sh\n" +
		"if [ \"$1\" = env ] && [ \"$2\" = GOVERSION ]; then printf 'go%s\\n' '" + initGoRelease + "'; exit 0; fi\n" +
		"printf '%s|%s\\n' \"$*\" \"$0\" >> '" + calls + "'\n"
	return tarGz(t, []tarEntry{
		{name: "go/VERSION", mode: 0o644, content: "go" + initGoRelease + "\n"},
		{name: "go/bin/go", mode: 0o755, content: goCommand},
	})
}

type tarEntry struct {
	name    string
	mode    int64
	content string
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
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

// fixtureRuns returns the lines the fixture runtime recorded for a command.
func fixtureRuns(t *testing.T, records, command string) []string {
	t.Helper()
	return readLines(t, filepath.Join(records, command+"-runs.txt"))
}

func readLines(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// runGoExtensionFixture is the fixture @putnami/go runtime, run by the test
// binary under goExtensionFixtureEnv. It answers the runtime handshake,
// installs the Go the lock pins on workspace-install, and records each run in
// the directory the variable names.
func runGoExtensionFixture(args []string) int {
	if len(args) >= 2 && args[0] == "__putnami" && args[1] == "runtime-info" {
		fmt.Printf(`{"extension":"@putnami/go","version":"0.1.0","platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`+"\n",
			runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
			runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
		return 0
	}
	records := os.Getenv(goExtensionFixtureEnv)
	root := os.Getenv("PUTNAMI_WORKSPACE_ROOT")
	if root == "" {
		var err error
		if root, err = os.Getwd(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	var outcome string
	var err error
	switch command {
	case "workspace-install":
		outcome, err = installPinnedGoFixture(root)
		command = "install"
	case "build":
		outcome, err = exec.LookPath("go")
	default:
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := appendLine(filepath.Join(records, command+"-runs.txt"), outcome); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// installPinnedGoFixture installs the Go the workspace lock pins where the
// fixture's candidates look, after checking the downloaded archive against the
// pinned SHA-256. Without a pin it installs nothing while nothing declares Go,
// and fails once go.work does.
func installPinnedGoFixture(root string) (string, error) {
	lock, err := lockfile.ReadLockFile(root)
	if err != nil {
		return "", err
	}
	var pin lockfile.LockEntry
	pinned := false
	if lock != nil {
		pin, pinned = lock.GetToolchain("go")
	}
	if !pinned || pin.Version == "" {
		if _, err := os.Stat(filepath.Join(root, "go.work")); err == nil {
			return "", errors.New("go.work declares Go, but the workspace lock pins none")
		}
		return "nothing-declared", nil
	}
	dir := filepath.Join(root, ".putnami", "extensions", "@putnami-go", "libs", "go-"+pin.Version)
	if _, err := os.Stat(filepath.Join(dir, "go", "bin", "go")); err == nil {
		return "present", nil
	}
	want := pin.Integrities[lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)]
	name := fmt.Sprintf("go%s.%s-%s.tar.gz", pin.Version, runtime.GOOS, runtime.GOARCH)
	resp, err := http.Get(pin.Source + name) //nolint:gosec,noctx // G107: the source the lock pins, a loopback server here
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s%s: HTTP %d", pin.Source, name, resp.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", err
	}
	if got := sha256Hex(archive); got != want {
		return "", fmt.Errorf("%s has SHA-256 %s, the lock pins %s", name, got, want)
	}
	if err := extractTarGz(archive, dir); err != nil {
		return "", err
	}
	return "installed", nil
}

func extractTarGz(archive []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	entries := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		clean := path.Clean(header.Name)
		if header.Typeflag != tar.TypeReg || clean != header.Name || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return fmt.Errorf("unexpected archive entry %q", header.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, os.FileMode(header.Mode).Perm()); err != nil {
			return err
		}
		entries++
	}
	if entries == 0 {
		return errors.New("empty archive")
	}
	return nil
}

func appendLine(file, line string) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
