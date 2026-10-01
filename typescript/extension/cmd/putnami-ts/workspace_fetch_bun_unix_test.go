//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
)

// fetchTestBearer is the read credential the fake registry requires.
const fetchTestBearer = "pkt_fetch_test_bearer_7f3a9c"

// credentialFD is the descriptor number the job credential arrives on in the
// helper process. It is above the descriptors a shell or `ls` opens first, so
// a listing that shows it proves the descriptor leaked into that process.
const credentialFD = 9

// postinstallScript is the trusted lifecycle script of @fake/marker. It
// records that it ran, the environment it ran with, and what it could read
// from the user's and the workspace's .npmrc.
const postinstallScript = `#!/bin/sh
echo ran > "$MARKER_WORKSPACE/postinstall.marker"
env > "$MARKER_WORKSPACE/postinstall.env"
cat "$HOME/.npmrc" > "$MARKER_WORKSPACE/postinstall.home-npmrc" 2>&1 || true
cat "$MARKER_WORKSPACE/.npmrc" > "$MARKER_WORKSPACE/postinstall.workspace-npmrc" 2>&1 || true
`

// requireBun returns the bun on PATH.
func requireBun(t *testing.T) string {
	t.Helper()
	bun, err := stdexec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	return bun
}

// fetchFixture is a workspace with a committed bun.lock that names a private
// package with a trusted postinstall script and a plain one, and the
// environment its jobs run with: a home of their own whose bun cache is the
// default one, and a temporary directory of their own.
type fetchFixture struct {
	root     string
	ctx      *pctx.Context
	registry *fakeNPMRegistry
	lockSum  [32]byte
	npmrcSum [32]byte
	home     string
	cache    string
	tmp      string
}

func newFetchFixture(t *testing.T, bun, token, linker string) *fetchFixture {
	t.Helper()
	fixture := &fetchFixture{
		registry: startFakeNPMRegistry(t, token,
			fakeNPMPackage{
				name:    "@fake/marker",
				version: "1.0.0",
				files:   map[string]string{"postinstall.sh": postinstallScript},
				scripts: map[string]string{"postinstall": "sh ./postinstall.sh"},
			},
			fakeNPMPackage{
				name:    "@fake/plain",
				version: "2.0.0",
				files:   map[string]string{"index.js": "module.exports = 2\n"},
			},
			// No manifest names @fake/extra when the lock is written.
			fakeNPMPackage{
				name:    "@fake/extra",
				version: "1.0.0",
				files:   map[string]string{"index.js": "module.exports = 3\n"},
			},
		),
		root: t.TempDir(),
		home: t.TempDir(),
		tmp:  t.TempDir(),
	}
	extensionRoot := t.TempDir()
	authorHome := t.TempDir()
	authorCache := t.TempDir()
	fixture.cache = filepath.Join(fixture.home, ".bun", "install", "cache")

	writeProbeFile(t, filepath.Join(extensionRoot, "putnami.extension.json"), `{}`)
	writeExtensionBiomeDefault(t, extensionRoot)
	root := fixture.root
	writeProbeFile(t, filepath.Join(root, "package.json"),
		`{"name":"fetch-fixture","private":true,"workspaces":["pkgs/*"],"dependencies":{"@fake/marker":"1.0.0"},"trustedDependencies":["@fake/marker"]}`)
	writeProbeFile(t, filepath.Join(root, "pkgs", "a", "package.json"),
		`{"name":"a","version":"0.0.0","dependencies":{"@fake/plain":"2.0.0"}}`)
	writeProbeFile(t, filepath.Join(root, "bunfig.toml"), "[install]\nlinker = \""+linker+"\"\n")
	// The repository's own credential line reads a variable the job
	// environment does not hold; copied as is, it would override the handed
	// credential with an empty one.
	writeProbeFile(t, filepath.Join(root, ".npmrc"),
		"@fake:registry="+fixture.registry.URL()+"\n//"+fixture.registry.Host()+"/:_authToken=${NPM_TOKEN}\n")

	author := stdexec.Command(bun, "install", "--lockfile-only")
	author.Dir = root
	author.Env = append(scrubbedBunEnv(), "HOME="+authorHome, "BUN_INSTALL_CACHE_DIR="+authorCache, "NPM_TOKEN="+fetchTestBearer)
	if out, err := author.CombinedOutput(); err != nil {
		t.Fatalf("author the lock: %v\n%s", err, out)
	}
	lock, err := os.ReadFile(filepath.Join(root, committedLockFile))
	if err != nil {
		t.Fatal(err)
	}
	fixture.lockSum = sha256.Sum256(lock)
	npmrc, err := os.ReadFile(filepath.Join(root, ".npmrc"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.npmrcSum = sha256.Sum256(npmrc)

	fixture.ctx = &pctx.Context{
		WorkspaceRoot: root,
		Extension:     pctx.Extension{Name: "@putnami/typescript", Root: extensionRoot},
		Params:        pctx.Params{},
	}
	withRegistries(t, fixture.ctx, `{"npm":{"scopes":{"@fake":"`+fixture.registry.URL()+`","@other":"https://npm.other.test"}}}`)

	// The jobs' environment: bun's cache resolves from the home alone.
	for _, name := range bunLocationVariables {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", fixture.home)
	t.Setenv("MARKER_WORKSPACE", root)
	t.Setenv("TMPDIR", fixture.tmp)
	return fixture
}

// bunLocationVariables are the variables that move bun's cache or its user
// configuration, or carry a registry credential.
var bunLocationVariables = []string{
	"BUN_INSTALL", "BUN_INSTALL_CACHE_DIR", "PUTNAMI_BUN_CACHE_DIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "NPM_TOKEN",
	extensionproto.OfflineDependenciesEnv, extensionproto.JobCredentialFDEnv,
}

// scrubbedBunEnv is this process's environment without the variables that
// move bun's cache or configuration.
func scrubbedBunEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || slices.Contains(bunLocationVariables, name) {
			continue
		}
		env = append(env, entry)
	}
	return env
}

// assertNoFileHolds fails for every regular file under roots that contains
// secret. Symbolic links are not followed; their targets are scanned under
// their own root.
func assertNoFileHolds(t *testing.T, secret string, roots ...string) {
	t.Helper()
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr == nil && bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds the bearer", path)
			}
			return nil
		})
	}
}

// fetchLeftovers lists the fetch's private directories left in dir.
func fetchLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	var left []string
	for _, name := range tempEntries(t, dir) {
		if strings.HasPrefix(name, "putnami-ts-fetch-") {
			left = append(left, name)
		}
	}
	return left
}

// helperResult is what the workspace-fetch helper process reports.
type helperResult struct {
	Status  string   `json:"status"`
	Error   string   `json:"error"`
	Environ []string `json:"environ"`
}

// TestWorkspaceFetchDescriptorHelper is the workspace-fetch job process that
// TestWorkspaceFetchThenHostedInstall starts with the credential on an
// inherited descriptor. It does nothing in any other run.
func TestWorkspaceFetchDescriptorHelper(t *testing.T) {
	resultPath := os.Getenv("PUTNAMI_TS_FETCH_HELPER_RESULT")
	if resultPath == "" {
		return
	}
	var ctx pctx.Context
	if err := json.Unmarshal([]byte(os.Getenv("PUTNAMI_TS_FETCH_HELPER_CONTEXT")), &ctx); err != nil {
		t.Fatal(err)
	}
	status, _, err := runWorkspaceFetch(&ctx, jsonl.New(), nil)
	result := helperResult{Status: status, Environ: os.Environ()}
	if err != nil {
		result.Error = err.Error()
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// runFetchWithDescriptor runs workspace-fetch in a process of its own that
// inherits the credential on descriptor credentialFD, as the engine starts it,
// with a bun that records its environment, its arguments and its open
// descriptors in dumps.
func runFetchWithDescriptor(t *testing.T, fixture *fetchFixture, bun, dumps string) helperResult {
	t.Helper()
	wrapperDir := t.TempDir()
	wrapper := "#!/bin/sh\n" +
		"env > \"$PUTNAMI_TS_FETCH_DUMPS/bun-$$.env\"\n" +
		"echo \"$*\" > \"$PUTNAMI_TS_FETCH_DUMPS/bun-$$.args\"\n" +
		"ls /dev/fd > \"$PUTNAMI_TS_FETCH_DUMPS/bun-$$.fds\"\n" +
		"exec '" + bun + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapperDir, "bun"), []byte(wrapper), 0o755); err != nil { //nolint:gosec // an executable test wrapper
		t.Fatal(err)
	}

	credential, err := os.CreateTemp(t.TempDir(), "credential")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = credential.Close() }()
	document, err := json.Marshal(registry.CredentialResult{Credential: &registry.Credential{
		Bearer:    fetchTestBearer,
		ExpiresAt: "2099-01-01T00:00:00Z",
		Hosts:     []string{fixture.registry.Host()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credential.Write(append(document, '\n')); err != nil {
		t.Fatal(err)
	}
	if _, err := credential.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	// Only the open descriptor carries the credential from here on.
	if err := os.Remove(credential.Name()); err != nil {
		t.Fatal(err)
	}

	jobContext, err := json.Marshal(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(dumps, "helper-result.json")
	cmd := stdexec.Command(os.Args[0], "-test.run=^TestWorkspaceFetchDescriptorHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"PATH="+wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PUTNAMI_TS_FETCH_DUMPS="+dumps,
		"PUTNAMI_TS_FETCH_HELPER_RESULT="+resultPath,
		"PUTNAMI_TS_FETCH_HELPER_CONTEXT="+string(jobContext),
		extensionproto.JobCredentialFDEnv+"="+strconv.Itoa(credentialFD),
	)
	cmd.ExtraFiles = make([]*os.File, credentialFD-2)
	cmd.ExtraFiles[credentialFD-3] = credential
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workspace-fetch process: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte(fetchTestBearer)) {
		t.Errorf("the workspace-fetch output holds the bearer:\n%s", out)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("workspace-fetch process wrote no result: %v\n%s", err, out)
	}
	var result helperResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "OK" {
		t.Fatalf("workspace-fetch = %q, %s\n%s", result.Status, result.Error, out)
	}
	return result
}

// installHosted stops the registry and runs workspace-install as a hosted run
// does, failing the test if it refreshes a native credential.
func installHosted(t *testing.T, fixture *fetchFixture) {
	t.Helper()
	fixture.registry.Close()
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	orig := ensureNpmRegistryCredential
	t.Cleanup(func() { ensureNpmRegistryCredential = orig })
	ensureNpmRegistryCredential = func(_ context.Context, _, host string) registrycred.Outcome {
		t.Errorf("the hosted install refreshed a credential for %s", host)
		return registrycred.Outcome{Kind: registrycred.KindSkipped}
	}
	status, _, err := runWorkspaceInstall(fixture.ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("hosted workspace-install = %q, %v; want OK", status, err)
	}
}

// assertInstalledFromTheCache checks what the hosted install left: the
// trusted postinstall ran, the plain package is in place, the lock is
// unchanged, and nothing the postinstall could read holds the bearer.
func assertInstalledFromTheCache(t *testing.T, fixture *fetchFixture) {
	t.Helper()
	root := fixture.root
	if _, err := os.Stat(filepath.Join(root, "postinstall.marker")); err != nil {
		t.Errorf("the trusted postinstall did not run: %v", err)
	}
	// The isolated linker links the member's dependency beside the member;
	// the hoisted one puts it in the root node_modules.
	isolated := filepath.Join(root, "pkgs", "a", "node_modules", "@fake", "plain", "package.json")
	hoisted := filepath.Join(root, "node_modules", "@fake", "plain", "package.json")
	if !isRegularFile(isolated) && !isRegularFile(hoisted) {
		t.Errorf("@fake/plain is installed neither at %s nor at %s", isolated, hoisted)
	}
	lock, err := os.ReadFile(filepath.Join(root, committedLockFile))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(lock) != fixture.lockSum {
		t.Errorf("the install rewrote %s", committedLockFile)
	}
	if npmrc, err := os.ReadFile(filepath.Join(root, ".npmrc")); err != nil || sha256.Sum256(npmrc) != fixture.npmrcSum {
		t.Errorf("the fetch or the install rewrote .npmrc (%v):\n%s", err, npmrc)
	}
	for _, dump := range []string{"postinstall.env", "postinstall.home-npmrc", "postinstall.workspace-npmrc"} {
		data, err := os.ReadFile(filepath.Join(root, dump))
		if err != nil {
			t.Errorf("the postinstall left no %s: %v", dump, err)
			continue
		}
		if bytes.Contains(data, []byte(fetchTestBearer)) {
			t.Errorf("the postinstall read the bearer through %s", dump)
		}
	}
	assertNoFileHolds(t, fetchTestBearer, root, fixture.tmp, fixture.home)
}

// A hosted run fetches with the credential handed on a descriptor, then
// installs from the cache with the registry gone: the trusted postinstall runs
// there and only there, the lock never changes, and no environment and no file
// any later process can read holds the bearer.
func TestWorkspaceFetchThenHostedInstall(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"a-hosted-run-installs-from-the-fetched-cache-and-no-process-reads-the-bearer")
	bun := requireBun(t)
	for _, linker := range []string{"isolated", "hoisted"} {
		t.Run(linker, func(t *testing.T) {
			fixture := newFetchFixture(t, bun, fetchTestBearer, linker)
			dumps := t.TempDir()

			result := runFetchWithDescriptor(t, fixture, bun, dumps)

			if served, refused := fixture.registry.served.Load(), fixture.registry.unauthorized.Load(); served == 0 || refused != 0 {
				t.Fatalf("registry served %d and refused %d requests; want bun to send the handed bearer", served, refused)
			}
			for _, entry := range result.Environ {
				if strings.Contains(entry, fetchTestBearer) || strings.HasPrefix(entry, extensionproto.JobCredentialFDEnv+"=") {
					t.Errorf("the workspace-fetch environment holds %q", entry)
				}
			}
			assertFetchBunProcesses(t, fixture, dumps)
			if left := fetchLeftovers(t, fixture.tmp); len(left) != 0 {
				t.Errorf("workspace-fetch left %v", left)
			}
			for _, rel := range []string{"node_modules", "postinstall.marker"} {
				if _, err := os.Stat(filepath.Join(fixture.root, rel)); !os.IsNotExist(err) {
					t.Errorf("workspace-fetch left %s in the workspace (%v)", rel, err)
				}
			}
			assertNoFileHolds(t, fetchTestBearer, fixture.root, fixture.tmp, fixture.home, dumps)

			installHosted(t, fixture)
			assertInstalledFromTheCache(t, fixture)
		})
	}
}

// assertFetchBunProcesses reads what every bun of the fetch recorded: none
// inherited the credential descriptor or saw the bearer in its environment,
// and the scratch install ran with a private home, the default cache of the
// real home, and the scratch arguments.
func assertFetchBunProcesses(t *testing.T, fixture *fetchFixture, dumps string) {
	t.Helper()
	envs, err := filepath.Glob(filepath.Join(dumps, "bun-*.env"))
	if err != nil || len(envs) == 0 {
		t.Fatalf("no bun process recorded its environment (%v)", err)
	}
	var sawInstall bool
	for _, envPath := range envs {
		base := strings.TrimSuffix(envPath, ".env")
		env := readTextFile(t, envPath)
		args := strings.TrimSpace(readTextFile(t, base+".args"))
		fds := strings.Fields(readTextFile(t, base+".fds"))
		if strings.Contains(env, fetchTestBearer) {
			t.Errorf("bun %s saw the bearer in its environment", args)
		}
		if slices.Contains(fds, strconv.Itoa(credentialFD)) {
			t.Errorf("bun %s inherited descriptor %d: %v", args, credentialFD, fds)
		}
		switch args {
		case "--version":
			t.Errorf("workspace-fetch probed the bun version, which only shaping the manifests does")
		case "install --ignore-scripts --frozen-lockfile":
			sawInstall = true
			variables := map[string]string{}
			for _, line := range strings.Split(env, "\n") {
				if name, value, ok := strings.Cut(line, "="); ok {
					variables[name] = value
				}
			}
			home := variables["HOME"]
			if home == fixture.home || !strings.HasPrefix(filepath.Base(home), "putnami-ts-fetch-home-") {
				t.Errorf("the scratch install ran with HOME=%q, want a private home", home)
			}
			if variables["XDG_CONFIG_HOME"] != home {
				t.Errorf("the scratch install ran with XDG_CONFIG_HOME=%q, want the private home", variables["XDG_CONFIG_HOME"])
			}
			if !sameDirectory(variables["BUN_INSTALL_CACHE_DIR"], fixture.cache) {
				t.Errorf("the scratch install cached into %q, want the real home's %q", variables["BUN_INSTALL_CACHE_DIR"], fixture.cache)
			}
		}
	}
	if !sawInstall {
		t.Errorf("recorded bun runs lack the scratch install")
	}
}

// sameDirectory compares two paths after resolving symbolic links, which
// macOS puts in front of the temporary directory.
func sameDirectory(a, b string) bool {
	resolve := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		return filepath.Clean(path)
	}
	return a != "" && resolve(a) == resolve(b)
}

// Without a handed credential the fetch downloads public packages with the
// machine's own configuration, and the hosted install still runs from the
// cache.
func TestWorkspaceFetchWithoutACredentialThenHostedInstall(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"a-hosted-run-installs-from-the-fetched-cache-and-no-process-reads-the-bearer")
	bun := requireBun(t)
	fixture := newFetchFixture(t, bun, "", "isolated")
	var events []string
	handCredential(t, nil, &events)
	refreshed := recordCredentialRefresh(t)

	status, _, err := runWorkspaceFetch(fixture.ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("workspace-fetch = %q, %v; want OK", status, err)
	}
	if fixture.registry.served.Load() == 0 {
		t.Fatal("workspace-fetch downloaded nothing")
	}
	if !slices.Equal(*refreshed, []string{"npm.other.test"}) {
		t.Errorf("native refresh hosts = %v, want the declared https scope", *refreshed)
	}
	if left := fetchLeftovers(t, fixture.tmp); len(left) != 0 {
		t.Errorf("workspace-fetch left %v", left)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "postinstall.marker")); !os.IsNotExist(err) {
		t.Fatalf("a lifecycle script ran during the fetch (%v)", err)
	}

	installHosted(t, fixture)
	assertInstalledFromTheCache(t, fixture)

	// A forced reinstall asks nothing of the stopped registry either.
	if err := os.Remove(filepath.Join(fixture.root, "postinstall.marker")); err != nil {
		t.Fatal(err)
	}
	fixture.ctx.Params["force"] = json.RawMessage(`true`)
	installHosted(t, fixture)
	assertInstalledFromTheCache(t, fixture)
}

// A manifest that no longer matches the committed lock fails the fetch, and
// the lock stays as committed.
func TestWorkspaceFetchRefusesALockThatDoesNotMatch(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"a-fetch-never-rewrites-the-committed-lock")
	bun := requireBun(t)
	fixture := newFetchFixture(t, bun, fetchTestBearer, "isolated")
	writeProbeFile(t, filepath.Join(fixture.root, "pkgs", "a", "package.json"),
		`{"name":"a","version":"0.0.0","dependencies":{"@fake/plain":"2.0.0","@fake/extra":"1.0.0"}}`)
	var events []string
	handCredential(t, &registry.Credential{
		Bearer:    fetchTestBearer,
		ExpiresAt: "2099-01-01T00:00:00Z",
		Hosts:     []string{fixture.registry.Host()},
	}, &events)

	status, _, err := runWorkspaceFetch(fixture.ctx, jsonl.New(), nil)
	if status != "FAILED" || err == nil {
		t.Fatalf("workspace-fetch = %q, %v; want a failure on a lock that does not match", status, err)
	}
	if strings.Contains(err.Error(), fetchTestBearer) {
		t.Fatalf("the failure echoes the bearer: %v", err)
	}
	lock, readErr := os.ReadFile(filepath.Join(fixture.root, committedLockFile))
	if readErr != nil || sha256.Sum256(lock) != fixture.lockSum {
		t.Fatalf("the fetch changed the lock (%v)", readErr)
	}
	if left := fetchLeftovers(t, fixture.tmp); len(left) != 0 {
		t.Errorf("workspace-fetch left %v", left)
	}
	assertNoFileHolds(t, fetchTestBearer, fixture.root, fixture.tmp, fixture.home)
}
