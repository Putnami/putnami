//go:build unix

package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/clibin"
	"go.putnami.dev/tooling/cli/internal/credentialprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/runnerprovider"
)

const (
	bootstrapProviderRecordEnv = "CLI_BOOTSTRAP_PROVIDER_RECORD"
	bootstrapProviderBearerEnv = "CLI_BOOTSTRAP_PROVIDER_BEARER"
	bootstrapProviderHostsEnv  = "CLI_BOOTSTRAP_PROVIDER_HOSTS"
	bootstrapProviderRun       = "-test.run=^TestBootstrapProviderHelper$"
	bootstrapRunCredential     = "prc_bootstrap_run_credential"
)

// TestBootstrapProviderHelper is not a test: it is the credential provider an
// extension of the tests below declares, in a child of this test binary. It
// serves credentials for the hosts of CLI_BOOTSTRAP_PROVIDER_HOSTS and
// appends its environment and every request it reads to the file
// CLI_BOOTSTRAP_PROVIDER_RECORD names. It exits after shutdown, so the test
// framework writes nothing on the protocol's stream. It returns at once unless
// a test started it.
func TestBootstrapProviderHelper(t *testing.T) {
	t.Parallel()
	record := os.Getenv(bootstrapProviderRecordEnv)
	if record == "" {
		return
	}
	os.Exit(serveBootstrapProvider(record, os.Getenv(bootstrapProviderBearerEnv), strings.Split(os.Getenv(bootstrapProviderHostsEnv), ",")))
}

func serveBootstrapProvider(record, bearer string, hosts []string) int {
	file, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 3
	}
	defer func() { _ = file.Close() }()
	note := func(line string) { _, _ = file.WriteString(line + "\n") }
	for _, entry := range os.Environ() {
		note("env " + entry)
	}
	write := func(response registry.CredentialResponse) {
		line, _ := json.Marshal(response)
		_, _ = os.Stdout.Write(append(line, '\n'))
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 4<<10), registry.MaxCredentialLineBytes)
	for scanner.Scan() {
		request, err := registry.ParseCredentialRequest(scanner.Bytes())
		if err != nil {
			return 4
		}
		switch request.Op {
		case registry.CredentialOpInitialize:
			note("initialize " + scanner.Text())
			payload, _ := json.Marshal(registry.CredentialInitializeResult{
				ProtocolVersion: 1, ProviderName: "bootstrap-fixture", Capabilities: []string{registry.CapabilityCredentialV1},
			})
			write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true, Payload: payload})
		case registry.CredentialOpCredential:
			params, _ := registry.ParseCredentialParams(request.Payload)
			note("credential " + params.Purpose)
			credential := registry.Credential{
				Bearer:    bearer + "-" + params.Purpose,
				ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				Hosts:     hosts,
			}
			payload, _ := json.Marshal(registry.CredentialResult{Credential: &credential})
			write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true, Payload: payload})
		case registry.CredentialOpShutdown:
			note("shutdown")
			write(registry.CredentialResponse{ProtocolVersion: 1, ID: request.ID, OK: true})
			return 0
		}
	}
	note("eof")
	return 0
}

// installCredentialDeclarer installs, under root, an extension that declares
// the credential-provider command served by TestBootstrapProviderHelper with
// bearer for hosts, and pins it in root's lock. The extension is admitted to
// the artifact store, and its provider is its native runtime, a copy of this
// test binary: a hosted run starts no other. It returns the file the provider
// records to.
func installCredentialDeclarer(t *testing.T, root, name, bearer string, hosts ...string) (record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "record")
	disabled := false
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: name, Version: "1.0.0", CLIContract: protocolcli.CurrentContract,
		Runtime: &extensionproto.RuntimeDefinition{Executable: fixtureRuntimeExecutable},
		Commands: map[string]extensionproto.CommandDefinition{
			registry.CredentialProviderCommand: {Description: "Registry credentials.", Run: []extensionproto.PipelineStep{{ID: "run", Task: "credentials"}}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{"credentials": {
			Kind: "command", Command: "{extensionRuntime}", Args: []string{bootstrapProviderRun},
			Cache: &extensionproto.TaskCachePolicy{Enabled: &disabled},
			Env: map[string]string{
				bootstrapProviderRecordEnv: record, bootstrapProviderBearerEnv: bearer,
				bootstrapProviderHostsEnv: strings.Join(hosts, ","),
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	installed, err := artifactstore.New(os.Getenv("PUTNAMI_ARTIFACT_DIR")).Admit(hex.EncodeToString(sum[:]), func(stage string) error {
		fixtureproc.Binary(t, filepath.Join(stage, filepath.FromSlash(fixtureRuntimeExecutable)))
		return os.WriteFile(filepath.Join(stage, "putnami.extension.json"), manifest, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.LinkArtifactGlobal(root, layout.Extensions, name, installed); err != nil {
		t.Fatal(err)
	}
	lf, err := lockfile.ReadLockFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if lf == nil {
		lf = lockfile.NewLockFile()
	}
	lf.SetExtension(name, lockfile.LockEntry{Version: "1.0.0"})
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatal(err)
	}
	return record
}

// bootstrapTestEnv isolates one test: HOME, and so the user scope, is a fresh
// directory, downloads land in a fresh artifact store, and the host-keyed
// credential seams answer without spawning a CLI.
func bootstrapTestEnv(t *testing.T) (userRoot string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	t.Setenv("PUTNAMI_ARTIFACTS_ENSURED", "")
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv(credentialprovider.ProvidersEnv, "")
	t.Setenv(runnerprovider.BoundRequestEnv, "")
	resolve, resolveWithCLI := extension.ResolveRegistryToken, extension.ResolveRegistryTokenWithCLI
	extension.ResolveRegistryToken = func(string) (string, string) { return "pkt_host_keyed", "" }
	extension.ResolveRegistryTokenWithCLI = func(context.Context, string, string) (string, string) { return "pkt_host_keyed", "" }
	t.Cleanup(func() {
		extension.ResolveRegistryToken, extension.ResolveRegistryTokenWithCLI = resolve, resolveWithCLI
	})
	userRoot, err := extension.ResolveUserScopeRoot()
	if err != nil {
		t.Fatal(err)
	}
	return userRoot
}

// recordingRegistry serves archive for a path that holds extensionName, and
// binary for any other. It remembers the Authorization of each request, keyed
// by "extension" or "cli".
func recordingRegistry(t *testing.T, extensionName string, archive, binary []byte) (server *httptest.Server, seen map[string][]string) {
	t.Helper()
	seen = map[string][]string{}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, body := "cli", binary
		if strings.Contains(r.URL.Path, extensionName) {
			kind, body = "extension", archive
		}
		seen[kind] = append(seen[kind], r.Header.Get("Authorization"))
		w.Header().Set("X-Resolved-Version", "1.0.0")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server, seen
}

func hostOf(t *testing.T, server *httptest.Server) string {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

// extensionArchive is a registry archive holding only manifest.
func extensionArchive(t *testing.T, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "putnami.extension.json", Mode: 0o644, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// resolvePinnedCLI downloads a pinned CLI from the registry at server the way
// the relaunch does, into a fresh store.
func resolvePinnedCLI(t *testing.T, server *httptest.Server, binary []byte) error {
	t.Helper()
	t.Setenv(extension.PutRegistryURLEnv, server.URL)
	sum := sha256.Sum256(binary)
	entry := &lockfile.LockEntry{Version: "1.2.3"}
	entry.SetPlatformIntegrity(lockfile.PlatformKey("linux", "amd64"), hex.EncodeToString(sum[:]))
	_, err := clibin.NewWorkspace(artifactstore.New(t.TempDir()), "", "/selected-cli").Resolve(context.Background(), entry, "linux", "amd64")
	return err
}

func readRecord(t *testing.T, record string) []string {
	t.Helper()
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read the provider record: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// The bootstrap provider is the user scope's credential provider, and it
// serves the pinned CLI and the lock-pinned extension downloads only. The test
// sets HOME, holds a run credential and installs a credential source for the
// whole process, so it runs alone.
func TestBootstrapProviderServesOnlyTheLockedDownloads(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "credential-on-a-descriptor", "bootstrap-provider-serves-only-the-locked-downloads")
	t.Run("only a user-scope extension is the provider", testBootstrapProviderIsOnlyTheUserScopes)
	t.Run("the provider serves the locked downloads", testBootstrapProviderServesTheLockedDownloads)
}

// A hosted run's bootstrap provider starts on the first download that needs
// it, receives the run credential in initialize only, and serves the read
// purpose. Its bearer reaches the pinned CLI download and the lock-pinned
// extension download on its hosts only. It is shut down when the extension
// pass ends, and no credential source remains installed.
func testBootstrapProviderServesTheLockedDownloads(t *testing.T) {
	userRoot := bootstrapTestEnv(t)
	defer runcredential.SetForTest(bootstrapRunCredential)()
	// The provider must not inherit the providers choice of this process.
	t.Setenv(credentialprovider.ProvidersEnv, runner.InvocationProviderInstall)

	binary := []byte("the-pinned-cli-behind-the-bootstrap-provider")
	const toolName = "@acme/tool"
	toolManifest := `{"name":"@acme/tool","version":"1.0.0","commands":{}}`
	archive := extensionArchive(t, toolManifest)
	served, servedSeen := recordingRegistry(t, "tool", archive, binary)
	other, otherSeen := recordingRegistry(t, "tool", archive, binary)
	record := installCredentialDeclarer(t, userRoot, "@acme/user-credentials", "pat_user_scope", hostOf(t, served))

	ws := t.TempDir()
	var stderr bytes.Buffer
	boot := openBootstrapProvider(context.Background(), []string{"build"}, ws, false, &stderr)
	if boot == nil {
		t.Fatalf("no bootstrap provider opened; stderr:\n%s", stderr.String())
	}
	defer boot.close()
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the provider started before a download needed it: %v", err)
	}

	// The pinned CLI download, as the relaunch runs it, on the provider's host
	// and on another.
	restore := boot.launchBootstrap().Serve()
	for _, server := range []*httptest.Server{served, other} {
		if err := resolvePinnedCLI(t, server, binary); err != nil {
			t.Fatalf("download the pinned CLI from %s: %v", server.URL, err)
		}
	}
	restore()
	if got, want := servedSeen["cli"], []string{"Bearer pat_user_scope-read"}; !slices.Equal(got, want) {
		t.Errorf("the pinned CLI download on the provider's host carried %q, want %q", got, want)
	}
	// A hosted run never asks the host-keyed seam: another host gets no
	// credential.
	if got, want := otherSeen["cli"], []string{""}; !slices.Equal(got, want) {
		t.Errorf("the pinned CLI download on another host carried %q, want no credential %q", got, want)
	}

	// The lock-pinned extension download.
	sum, manifestSum := sha256.Sum256(archive), sha256.Sum256([]byte(toolManifest))
	lf := lockfile.NewLockFile()
	lf.SetExtension(toolName, lockfile.LockEntry{
		Version: "1.0.0", ManifestHash: hex.EncodeToString(manifestSum[:]),
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): hex.EncodeToString(sum[:])},
	})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	t.Setenv(extension.PutRegistryURLEnv, served.URL)
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{toolName: "1.0.0"}}}
	boot.ensureArtifactsAndClose(context.Background(), ws, cfg, true)
	if got, want := servedSeen["extension"], []string{"Bearer pat_user_scope-read"}; !slices.Equal(got, want) {
		t.Errorf("the extension download carried %q, want %q", got, want)
	}

	// After the pass, a download on the provider's host no longer asks it, and
	// goes out without a credential.
	req, err := http.NewRequest(http.MethodGet, served.URL+"/after", nil)
	if err != nil {
		t.Fatal(err)
	}
	if missing, err := extension.AuthorizeRegistryRequest(req); err != nil || missing == "" || req.Header.Get("Authorization") != "" {
		t.Errorf("after the pass: Authorization %q, missing %q, err %v; want no credential", req.Header.Get("Authorization"), missing, err)
	}

	// One provider process read initialize with the run credential, one read
	// credential, and shutdown. The run credential and the providers choice
	// are not in its environment.
	var requests []string
	for _, line := range readRecord(t, record) {
		if entry, ok := strings.CutPrefix(line, "env "); ok {
			if strings.HasPrefix(entry, credentialprovider.ProvidersEnv+"=") || strings.Contains(entry, bootstrapRunCredential) {
				t.Errorf("the provider's environment holds %s", entry)
			}
			continue
		}
		if strings.HasPrefix(line, "initialize ") {
			if !strings.Contains(line, `"runCredential":"`+bootstrapRunCredential+`"`) {
				t.Errorf("initialize does not carry the run credential: %s", line)
			}
			line = "initialize"
		}
		requests = append(requests, line)
	}
	if want := []string{"initialize", "credential read", "shutdown"}; !slices.Equal(requests, want) {
		t.Errorf("the provider read %q, want %q", requests, want)
	}
}

// Only a user-scope extension can be the bootstrap provider: a provider that
// only the workspace declares gives none, and with both the user scope's
// serves alone. Without the run credential and without the install provider
// there is none, and neither is there for an invocation exempt from the
// relaunch, a provider child, or a run outside a workspace. With none, the
// relaunch and the extension pass get nothing to serve and no process starts.
func testBootstrapProviderIsOnlyTheUserScopes(t *testing.T) {
	userRoot := bootstrapTestEnv(t)
	const host = "put.example.test"
	ws := t.TempDir()
	workspaceRecord := installCredentialDeclarer(t, ws, "@acme/workspace-credentials", "pat_workspace", host)
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@acme/workspace-credentials": "1.0.0"}}}
	// The workspace's fixture is a provider the workspace itself would use.
	discovered, err := extension.DiscoverExtensions(ws, cfg, nil)
	broker := credentialprovider.New(ws, discovered, []string{runner.InvocationProviderInstall}, providersFromFlag, io.Discard)
	if err != nil || broker == nil {
		t.Fatalf("the workspace fixture declares no provider: %v", err)
	}
	_ = broker.Close()

	open := func(t *testing.T, hosted bool, env string, args []string, wsRoot string, child bool) *bootstrapProvider {
		t.Helper()
		bearer := ""
		if hosted {
			bearer = bootstrapRunCredential
		}
		defer runcredential.SetForTest(bearer)()
		t.Setenv(credentialprovider.ProvidersEnv, env)
		var stderr bytes.Buffer
		boot := openBootstrapProvider(context.Background(), args, wsRoot, child, &stderr)
		if stderr.Len() != 0 {
			t.Errorf("openBootstrapProvider wrote to stderr:\n%s", stderr.String())
		}
		return boot
	}
	build := []string{"build"}
	if boot := open(t, true, runner.InvocationProviderInstall, build, ws, false); boot != nil {
		t.Fatal("a provider only the workspace declares became the bootstrap provider")
	}

	userRecord := installCredentialDeclarer(t, userRoot, "@acme/user-credentials", "pat_user_scope", host)
	for _, c := range []struct {
		name   string
		hosted bool
		env    string
		args   []string
		wsRoot string
		child  bool
	}{
		{name: "flag off", args: build, wsRoot: ws},
		{name: "publish only", env: runner.InvocationProviderPublish, args: build, wsRoot: ws},
		{name: "exempt from the relaunch", hosted: true, args: []string{"pin", "1.0.0"}, wsRoot: ws},
		{name: "provider child", hosted: true, args: build, wsRoot: ws, child: true},
		{name: "outside a workspace", hosted: true, args: build},
	} {
		t.Run(c.name, func(t *testing.T) {
			boot := open(t, c.hosted, c.env, c.args, c.wsRoot, c.child)
			if boot != nil {
				t.Fatal("a bootstrap provider was opened")
			}
			if bootstrap := boot.launchBootstrap(); bootstrap.Serve != nil || bootstrap.Close != nil {
				t.Error("the relaunch got a bootstrap provider to serve")
			}
			boot.ensureArtifactsAndClose(context.Background(), ws, cfg, false)
			boot.close()
		})
	}
	if _, err := os.Stat(userRecord); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a provider process started: %v", err)
	}

	boot := open(t, true, runner.InvocationProviderInstall, build, ws, false)
	if boot == nil {
		t.Fatal("the user scope's provider is not the bootstrap provider")
	}
	restore := boot.launchBootstrap().Serve()
	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/putnami/cli/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = extension.AuthorizeRegistryRequest(req)
	restore()
	boot.close()
	if err != nil || req.Header.Get("Authorization") != "Bearer pat_user_scope-read" {
		t.Errorf("with both scopes declaring a provider: Authorization %q, err %v; want the user scope's bearer", req.Header.Get("Authorization"), err)
	}
	if _, err := os.Stat(workspaceRecord); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the workspace's provider started: %v", err)
	}
}
