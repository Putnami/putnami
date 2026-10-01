//go:build unix

package hostedcold

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const (
	// pinnedVersion is the private CLI the workspace locks; bootstrapVersion
	// is the CLI that starts the run.
	pinnedVersion    = "4.2.1"
	bootstrapVersion = "4.2.0"
	extensionName    = "@acme/private-tool"
	extensionVersion = "1.0.0"
	extensionPath    = "/acme/private-tool/download"
	cliPath          = "/putnami/cli/download"
	providerName     = "@acme/user-credentials"
	providerVersion  = "1.0.0"
	runCredential    = "prc_hostedcold_run_credential_5d1e"
	otherCredential  = "prc_hostedcold_other_credential_77b0"
	// runTimeout bounds one CLI run.
	runTimeout = 2 * time.Minute
)

// lockedFiles are the files a hosted install must leave byte-identical.
var lockedFiles = []string{"bun.lock", ".npmrc", "putnami.lock.json"}

// A hosted install of a workspace that pins a private CLI other than the one
// that starts the run works from an empty store. The run starts with the run
// credential on descriptor 3. The registry serves the pinned CLI and the
// private extension only for the bearer that the user scope's credential
// provider derives from that credential in initialize. The starting CLI
// downloads the pinned CLI through the provider, checks it accepts
// --credential-fd, and execs it with the credential. The pinned CLI downloads
// the extension through the provider, fetches, and installs. Every locked file
// keeps its bytes and the checkout stays clean. Without the run credential, or
// with another one, the pinned CLI never downloads and nothing changes.
//
// It builds the CLI twice from this module, in parallel: about 5 s for the
// whole test on a warm build cache, about 15 s on a cold one. Each run is its
// own process with a fresh home and a fresh checkout.
func TestHostedInstallOfAPrivatePinWorksCold(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "cold-install-leaves-locks-unchanged", "private-pin-installs-cold")
	clitest.RequireShell(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	bootstrapCLI, pinnedCLI := buildCLIs(t)
	pinned, err := os.ReadFile(pinnedCLI)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "runs.txt")
	ext := newPrivateExtension(t, log)
	reg := newPrivateRegistry(t, pinned, ext.archive)
	fixture := fixtureLock(reg.url, sha256Hex(pinned), ext)

	for _, tc := range []struct {
		name       string
		credential string
		// wantProvider is the run credential the provider receives, "" when
		// it never starts.
		wantProvider string
	}{
		{name: "without the run credential", credential: ""},
		{name: "with another run credential", credential: otherCredential, wantProvider: otherCredential},
	} {
		t.Run(tc.name+" the pinned CLI does not download", func(t *testing.T) {
			reg.reset()
			resetLog(t, log)
			home, record := newHome(t, reg.host)
			ws := newWorkspace(t, reg.url, fixture)
			before := lockedDigests(t, ws)
			code, stdout, stderr := runInstall(t, bootstrapCLI, ws, home, reg.url, tc.credential)
			if code == 0 {
				t.Fatalf("the install succeeded\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
			if !strings.Contains(stderr, "HTTP 401") || !strings.Contains(stderr, pinnedVersion) {
				t.Errorf("stderr does not name the refused download of %s:\n%s", pinnedVersion, stderr)
			}
			for _, request := range reg.snapshot() {
				if request.status == http.StatusOK {
					t.Errorf("the registry served %s", request.path)
				}
			}
			store := artifactstore.New(storeRoot(home))
			if store.HasCLI(sha256Hex(pinned)) || store.Has(ext.archiveSHA) {
				t.Error("the store holds a download")
			}
			if runs, err := os.ReadFile(log); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("an extension task ran: %q, %v", runs, err)
			}
			assertUnchanged(t, ws, home, before)
			assertProvider(t, record, tc.wantProvider, 1)
		})
	}

	t.Run("with the run credential the install works cold", func(t *testing.T) {
		reg.reset()
		resetLog(t, log)
		home, record := newHome(t, reg.host)
		ws := newWorkspace(t, reg.url, fixture)
		before := lockedDigests(t, ws)
		store := artifactstore.New(storeRoot(home))
		if store.HasCLI(sha256Hex(pinned)) || store.Has(ext.archiveSHA) {
			t.Fatal("the store holds a download before the run")
		}

		code, stdout, stderr := runInstall(t, bootstrapCLI, ws, home, reg.url, runCredential)
		if code != 0 {
			t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		for _, secret := range []string{runCredential, registryBearer(runCredential)} {
			if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
				t.Errorf("a credential reached the output\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
		}

		// The starting CLI downloaded the pinned CLI, and the pinned CLI the
		// extension, each with the provider's bearer.
		var served []string
		for _, request := range reg.snapshot() {
			if request.status != http.StatusOK {
				t.Errorf("the registry answered %d to %s from %s", request.status, request.path, request.userAgent)
				continue
			}
			served = append(served, request.path+" by "+request.userAgent)
		}
		want := []string{
			cliPath + " by putnami-cli/" + bootstrapVersion,
			extensionPath + " by putnami-cli/" + pinnedVersion,
		}
		if !slices.Equal(served, want) {
			t.Errorf("the registry served %q, want %q", served, want)
		}

		if !store.HasCLI(sha256Hex(pinned)) {
			t.Error("the pinned CLI is not in the store")
		}
		if !store.Has(ext.archiveSHA) {
			t.Error("the private extension is not in the store")
		}
		resolved, err := filepath.EvalSymlinks(layout.StableDir(ws, layout.Extensions, extensionName))
		if want, _ := filepath.EvalSymlinks(store.Path(ext.archiveSHA)); err != nil || resolved != want {
			t.Errorf("the workspace resolves the private extension to %q (%v), want the store entry %q", resolved, err, want)
		}
		if runs, _ := os.ReadFile(log); string(runs) != "fetch offline=\ninstall offline=1\n" {
			t.Errorf("the extension's tasks ran as %q, want the fetch, then the install offline", runs)
		}
		assertUnchanged(t, ws, home, before)
		// One provider served the pinned CLI download in the starting CLI, one
		// the extension download in the pinned CLI.
		assertProvider(t, record, runCredential, 2)
	})
}

// buildCLIs builds the CLI of this module twice, stamped bootstrapVersion and
// pinnedVersion, and returns both paths.
func buildCLIs(t *testing.T) (bootstrapCLI, pinnedCLI string) {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go unavailable")
	}
	module, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	build := func(version string) (string, error) {
		output := filepath.Join(dir, version, "putnami")
		cmd := exec.Command(goBinary, "build", "-o", output,
			"-ldflags", "-X go.putnami.dev/tooling/cli/internal/cli.Version="+version, "./cmd/putnami")
		cmd.Dir = module
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("build the CLI %s: %w\n%s", version, err, out)
		}
		return output, nil
	}
	var (
		wg                      sync.WaitGroup
		bootstrapErr, pinnedErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); bootstrapCLI, bootstrapErr = build(bootstrapVersion) }()
	go func() { defer wg.Done(); pinnedCLI, pinnedErr = build(pinnedVersion) }()
	wg.Wait()
	if err := errors.Join(bootstrapErr, pinnedErr); err != nil {
		t.Fatal(err)
	}
	return bootstrapCLI, pinnedCLI
}

// privateExtension is a registry archive of an extension that fetches and
// installs the workspace, each task appending its name and the offline signal
// it sees to a log. The fetch runs through the extension's own runtime, the one
// fetch a hosted install runs.
type privateExtension struct {
	archive     []byte
	archiveSHA  string
	manifestSHA string
}

func newPrivateExtension(t *testing.T, log string) privateExtension {
	t.Helper()
	manifest := fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "cliContract": %d,
  "runtime": { "executable": "bin/runtime" },
  "commands": {
    "workspace-fetch": {
      "description": "Fetch workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "fetch", "task": "fetch-task" }]
    },
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    }
  },
  "tasks": {
    "fetch-task": { "kind": "command", "command": "{extensionRuntime}", "args": ["fetch"], "cache": false, "timeoutMs": 30000 },
    "install-task": { "kind": "command", "command": "/bin/sh", "args": ["{extensionRoot}/install.sh"], "cache": false, "timeoutMs": 30000 }
  }
}
`, extensionName, extensionVersion, protocolcli.CurrentContract)
	task := func(name string) []byte {
		return fmt.Appendf(nil, "printf '%s offline=%%s\\n' \"$%s\" >> '%s'\n", name, extensionproto.OfflineDependenciesEnv, log)
	}
	info := fmt.Sprintf(
		`{"extension":%q,"version":%q,"platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		extensionName, extensionVersion, runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	runtimeScript := fmt.Appendf(nil, "#!/bin/sh\n"+
		"if [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n"+
		"  printf '%%s\\n' '%s'\n"+
		"  exit 0\n"+
		"fi\n"+
		"if [ \"$1\" = \"fetch\" ]; then\n"+
		"  %s"+
		"fi\n", info, task("fetch"))
	archive := tarGz(t,
		tarFile{name: "putnami.extension.json", mode: 0o644, data: []byte(manifest)},
		tarFile{name: "bin/runtime", mode: 0o755, data: runtimeScript},
		tarFile{name: "install.sh", mode: 0o644, data: task("install")},
	)
	return privateExtension{archive: archive, archiveSHA: sha256Hex(archive), manifestSHA: sha256Hex([]byte(manifest))}
}

// resetLog removes the extension's log, so a run's log holds its own tasks.
func resetLog(t *testing.T, log string) {
	t.Helper()
	if err := os.Remove(log); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

type tarFile struct {
	name string
	mode int64
	data []byte
}

// tarGz is a .tar.gz archive of files, in order.
func tarGz(t *testing.T, files ...tarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(gz)
	for _, file := range files {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(file.data); err != nil {
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

// privateRegistry serves the pinned CLI, as a release archive with the binary
// at compiled/putnami, and the private extension to a request that carries
// registryBearer(runCredential), and answers 401 to any other. It records every
// request.
type privateRegistry struct {
	url, host string
	mu        sync.Mutex
	requests  []registryRequest
}

type registryRequest struct {
	path, userAgent string
	status          int
}

func newPrivateRegistry(t *testing.T, binary, archive []byte) *privateRegistry {
	t.Helper()
	reg := &privateRegistry{}
	authorization := "Bearer " + registryBearer(runCredential)
	cli := tarGz(t, tarFile{name: "compiled/putnami", mode: 0o755, data: binary})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		platform := query.Get("os") == runtime.GOOS && query.Get("arch") == runtime.GOARCH
		var body []byte
		switch {
		case r.URL.Path == cliPath && platform && query.Get("channel") == pinnedVersion:
			body = cli
		case r.URL.Path == extensionPath && platform && query.Get("channel") == extensionVersion:
			body = archive
			w.Header().Set("X-Resolved-Version", extensionVersion)
		}
		status := http.StatusOK
		switch {
		case r.Header.Get("Authorization") != authorization:
			status = http.StatusUnauthorized
		case body == nil:
			status = http.StatusNotFound
		}
		reg.mu.Lock()
		reg.requests = append(reg.requests, registryRequest{path: r.URL.Path, userAgent: r.Header.Get("User-Agent"), status: status})
		reg.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	reg.url, reg.host = server.URL, parsed.Host
	return reg
}

func (r *privateRegistry) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = nil
}

func (r *privateRegistry) snapshot() []registryRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// fixtureLock is the lock the workspace commits: the private CLI pin for this
// platform as `putnami pin` from another CLI version records it, without the
// protocolVersion an install without the run credential adds, and the private
// extension. A hosted install must leave it as committed.
func fixtureLock(registryURL, pinnedSHA string, ext privateExtension) *lockfile.LockFile {
	platform := lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)
	query := url.Values{"channel": {pinnedVersion}, "os": {runtime.GOOS}, "arch": {runtime.GOARCH}}
	lf := lockfile.NewLockFile()
	cli := lockfile.LockEntry{
		Version: pinnedVersion,
		Source:  registryURL + cliPath + "?" + query.Encode(),
	}
	cli.SetPlatformIntegrity(platform, pinnedSHA)
	lf.SetCLI(cli)
	lf.SetExtension(extensionName, lockfile.LockEntry{
		Version:      extensionVersion,
		ManifestHash: ext.manifestSHA,
		Integrities:  map[string]string{platform: ext.archiveSHA},
	})
	return lf
}

// newWorkspace writes a committed workspace that locks lf and declares the
// private extension, with a bun.lock and an .npmrc, and the files `putnami
// init` scaffolds beside them. It declares no registry: a hosted run reads the
// registry from its environment only.
func newWorkspace(t *testing.T, registryURL string, lf *lockfile.LockFile) string {
	t.Helper()
	ws := t.TempDir()
	config, err := json.MarshalIndent(struct {
		Name       string            `json:"name"`
		Includes   []string          `json:"includes"`
		Extensions map[string]string `json:"extensions"`
	}{
		Name:       "hosted-cold",
		Includes:   []string{"app"},
		Extensions: map[string]string{extensionName: extensionVersion},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(ws, "putnami.workspace.json"), string(config)+"\n")
	clitest.WriteFile(t, filepath.Join(ws, "app", "putnami.json"), `{"name":"app"}`+"\n")
	clitest.WriteFile(t, filepath.Join(ws, "bun.lock"), `{"lockfileVersion":1,"workspaces":{"":{"name":"hosted-cold"}},"packages":{}}`+"\n")
	clitest.WriteFile(t, filepath.Join(ws, ".npmrc"), "@acme:registry="+registryURL+"/npm/\n")
	clitest.WriteFile(t, filepath.Join(ws, ".gitignore"), "node_modules\ndist\n.putnami\ncoverage\n.gen\n.generated\n")
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	// No AGENTS.md and no .mcp.json: an install without the run credential
	// writes both, and the empty git status proves a hosted one does not.

	hooks := t.TempDir()
	clitest.RunGit(t, ws, "init", "--quiet")
	for _, setting := range [][2]string{
		{"user.email", "test@example.com"}, {"user.name", "test"},
		{"commit.gpgsign", "false"}, {"core.hooksPath", hooks},
	} {
		clitest.RunGit(t, ws, "config", setting[0], setting[1])
	}
	clitest.RunGit(t, ws, "add", "-A")
	clitest.RunGit(t, ws, "commit", "--quiet", "-m", "fixture")
	return ws
}

// newHome returns a fresh home whose user scope declares the credential
// provider this test binary serves for host, and the file it records to. The
// provider extension is in the artifact store, linked from the user scope as a
// user-scope install links it. Its runtime is a copy of this test binary, and
// its provider command is that native runtime, as a hosted run requires. The
// store holds nothing else.
func newHome(t *testing.T, host string) (home, record string) {
	t.Helper()
	home = t.TempDir()
	record = filepath.Join(t.TempDir(), "provider-record")
	disabled := false
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: providerName, Version: providerVersion, CLIContract: protocolcli.CurrentContract,
		Runtime: &extensionproto.RuntimeDefinition{Executable: "compiled/provider"},
		Commands: map[string]extensionproto.CommandDefinition{
			registry.CredentialProviderCommand: {Description: "Registry credentials.", Run: []extensionproto.PipelineStep{{ID: "run", Task: "credentials"}}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{"credentials": {
			Kind: "command", Command: "{extensionRuntime}", Args: []string{registry.CredentialProviderCommand},
			Cache: &extensionproto.TaskCachePolicy{Enabled: &disabled},
			Env:   map[string]string{providerRecordEnv: record, providerHostsEnv: host},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	userRoot := filepath.Join(home, ".putnami", "user")
	installed, err := artifactstore.New(storeRoot(home)).Admit(sha256Hex(manifest), func(stageDir string) error {
		fixtureproc.Binary(t, filepath.Join(stageDir, "compiled", "provider"))
		return os.WriteFile(filepath.Join(stageDir, "putnami.extension.json"), manifest, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.LinkArtifactGlobal(userRoot, layout.Extensions, providerName, installed); err != nil {
		t.Fatal(err)
	}
	lf := lockfile.NewLockFile()
	lf.SetExtension(providerName, lockfile.LockEntry{Version: providerVersion})
	if err := lockfile.WriteLockFile(userRoot, lf); err != nil {
		t.Fatal(err)
	}
	return home, record
}

// storeRoot is the artifact store a CLI run with home uses.
func storeRoot(home string) string {
	return filepath.Join(home, ".putnami", "artifacts")
}

// baseEnv is the environment of a process that runs with home as the user's
// home, and nothing of the test's environment beyond PATH.
func baseEnv(home string) []string {
	return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir()}
}

// runInstall runs `putnami install` with cli in ws, with credential on
// descriptor 3 and --credential-fd 3 when it is set. The environment is the
// whole environment of the run, as a hosted runner starts the binary: home,
// the registry, and no telemetry.
func runInstall(t *testing.T, cli, ws, home, registryURL, credential string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	args := []string{"install"}
	cmd := exec.CommandContext(ctx, cli)
	if credential != "" {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		go func() {
			_, _ = w.WriteString(credential + "\n")
			_ = w.Close()
		}()
		cmd.ExtraFiles = []*os.File{r}
		args = append(args, "--credential-fd", "3")
	}
	cmd.Args = append([]string{cli}, args...)
	cmd.Dir = ws
	cmd.Env = append(baseEnv(home),
		"PUTNAMI_REGISTRY_URL="+registryURL,
		"PUTNAMI_TELEMETRY=off", "DO_NOT_TRACK=1",
		"PUTNAMI_HTTP_RETRY_BACKOFF=0")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v\nstdout:\n%s\nstderr:\n%s", cli, err, out.String(), errOut.String())
	}
	return code, out.String(), errOut.String()
}

func lockedDigests(t *testing.T, ws string) map[string]string {
	t.Helper()
	digests := map[string]string{}
	for _, name := range lockedFiles {
		data, err := os.ReadFile(filepath.Join(ws, name))
		if err != nil {
			t.Fatal(err)
		}
		digests[name] = sha256Hex(data)
	}
	return digests
}

// assertUnchanged checks that every locked file of ws keeps the digest it had
// before the run and that the checkout is clean, read with home as the user's
// home so no global ignore file hides a change.
func assertUnchanged(t *testing.T, ws, home string, before map[string]string) {
	t.Helper()
	for name, digest := range lockedDigests(t, ws) {
		if digest != before[name] {
			t.Errorf("%s changed: sha256 %s, was %s", name, digest, before[name])
		}
	}
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = ws
	cmd.Env = append(baseEnv(home), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if len(out) != 0 {
		diff := exec.Command("git", "diff")
		diff.Dir = ws
		diff.Env = cmd.Env
		patch, _ := diff.CombinedOutput()
		t.Errorf("the checkout is not clean:\n%s\n%s", out, patch)
	}
}

// assertProvider checks the provider record. Each of processes provider
// processes read initialize with runCredential, one read credential, and
// shutdown, and none held a credential in its environment. With runCredential
// empty, no provider started.
func assertProvider(t *testing.T, record, runCredential string, processes int) {
	t.Helper()
	raw, err := os.ReadFile(record)
	if runCredential == "" {
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a provider started without the run credential: %v\n%s", err, raw)
		}
		return
	}
	if err != nil {
		t.Fatalf("read the provider record: %v", err)
	}
	var requests []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if entry, ok := strings.CutPrefix(line, "env "); ok {
			for _, secret := range []string{runCredential, registryBearer(runCredential)} {
				if strings.Contains(entry, secret) {
					t.Errorf("the provider's environment holds a credential: %s", entry)
				}
			}
			continue
		}
		requests = append(requests, line)
	}
	var want []string
	for range processes {
		want = append(want, "start", "initialize run-credential="+sha256Hex([]byte(runCredential)), "credential "+registry.PurposeRead, "shutdown")
	}
	if !slices.Equal(requests, want) {
		t.Errorf("the provider read %q, want %q", requests, want)
	}
}
