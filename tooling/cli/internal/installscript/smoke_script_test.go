package installscript

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var smokeNeutralEnvironment = []string{
	"AWS_PROFILE",
	"AWS_CONFIG_FILE",
	"AWS_SHARED_CREDENTIALS_FILE",
	"NODE_AUTH_TOKEN",
	"BUN_AUTH_TOKEN",
	"BUN_SENTINEL_FUTURE",
	"SSH_AUTH_SOCK",
	"GIT_ASKPASS",
	"SSH_ASKPASS",
	"BASH_ENV",
	"ENV",
	"GIT_CONFIG_PARAMETERS",
	"PUTNAMI_HOME",
	"PUTNAMI_INSTALL_DIR",
	"PUTNAMI_WORKSPACE_ROOT",
	"PUTNAMI_EXTENSION_CACHE_DIR",
	"PUTNAMI_EXTENSION_CACHE_ROOT",
	"PUTNAMI_CACHE_ROOT",
	"PUTNAMI_GO_CACHE_DIR",
	"PUTNAMI_PROFILE",
	"PUTNAMI_UNSAFE_INSTALL",
	"PUTNAMI_DOWNLOAD_URL",
	"PUTNAMI_EXPECTED_SHA256",
	"PUTNAMI_VARIANT",
	"PUTNAMI_NO_AUTO_INSTALL",
	"PUTNAMI_OUTPUT",
	"PUTNAMI_SENTINEL_FUTURE",
	"NPM_CONFIG_REGISTRY",
	"NPM_CONFIG_SENTINEL_FUTURE",
}

var smokeExpectedEnvironment = map[string]string{
	"GIT_CONFIG_COUNT":           "0",
	"PUTNAMI_TELEMETRY":          "off",
	"PUTNAMI_TELEMETRY_ENDPOINT": "",
	"DO_NOT_TRACK":               "1",
}

// The release smoke is the only thing that runs the public golden path end to
// end against released artifacts. These tests point both public URLs at a local
// server through the documented overrides so a broken leg is caught before a
// release depends on it, not for the first time during one.
//
// The stub CLI answers --version and fails everything else, so each run stops at
// the init leg. Reaching the init leg is the assertion: it proves the channel,
// install, discovery, and stamp legs all passed against a real install.sh run.

func smokeScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	script := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "smoke-check-release.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("smoke-check-release.sh not found: %v", err)
	}
	return script
}

// stubCLIVersionOnly answers --version and refuses everything else, so the smoke
// stops at its first real CLI command instead of waiting out the serve deadline.
func stubCLIVersionOnly(version string) []byte {
	return []byte("#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  echo \"Putnami   " + version + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"stub cli: $* is not implemented\" >&2\n" +
		"exit 3\n")
}

func stubCLIWithNoisyInitFailure(version string) []byte {
	return []byte("#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  echo \"Putnami   " + version + "\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"i=0\n" +
		"while [ \"$i\" -lt 250 ]; do echo \"diagnostic-$i\"; i=$((i + 1)); done\n" +
		"printf '%300000s\\n' x\n" +
		"exit 3\n")
}

func TestSmokeScriptPinsExactPublicCommands(t *testing.T) {
	script, err := os.ReadFile(smokeScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, command := range []string{
		"curl -fsSL https://putnami.dev/install.sh | bash",
		"putnami init --project webapp --extension ts",
		"putnami serve webapp",
		`curl -fsSL "https://putnami.dev/install.sh?run=${run_command}" | bash`,
	} {
		if !strings.Contains(text, command) {
			t.Fatalf("release smoke does not execute exact public command %q", command)
		}
	}
	if strings.Contains(text, "workspace init --workspace release-smoke") || strings.Contains(text, "--extension go") {
		t.Fatal("release smoke still substitutes the old Go workspace path")
	}
}

// stubBunDir returns a directory whose `bun` reports the smoke gate's minimum
// version, keeping the leg tests hermetic w.r.t. the host machine's bun.
func stubBunDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bun"), []byte("#!/bin/sh\necho 1.4.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runSmoke(t *testing.T, registryURL string) result {
	t.Helper()
	return runSmokeWithInstallerURL(t, registryURL, registryURL+"/install.sh", "")
}

func runSmokeWithInstallerURL(t *testing.T, registryURL, installerURL, diagnosticsDir string) result {
	t.Helper()
	return runSmokeFrom(t, registryURL, installerURL, diagnosticsDir, t.TempDir())
}

func runSmokeFrom(t *testing.T, registryURL, installerURL, diagnosticsDir, launchDir string, extra ...string) result {
	t.Helper()
	cmd := exec.Command("bash", smokeScriptPath(t), "latest")
	cmd.Dir = launchDir
	smokePath := os.Getenv("SMOKE_TEST_PATH")
	if smokePath == "" {
		// The prerequisites leg gates on the workspace's Bun floor before any
		// other leg runs. These tests assert the legs that FOLLOW, so they must
		// not depend on the host machine's bun meeting the floor — a stub
		// reporting the floor version goes first on the PATH. The floor gate
		// itself is covered by the tests that set SMOKE_TEST_PATH.
		smokePath = stubBunDir(t) + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	cmd.Env = []string{
		"PATH=" + smokePath,
		"HOME=" + launchDir,
		"SHELL=/bin/sh",
		"PUTNAMI_REGISTRY_URL=" + registryURL,
		"SMOKE_INSTALL_URL=" + installerURL,
		"SMOKE_DIAGNOSTICS_DIR=" + diagnosticsDir,
		"GO_WANT_SMOKE_CLI_HELPER=" + os.Getenv("GO_WANT_SMOKE_CLI_HELPER"),
		// One attempt: a local registry either answers or it does not, and the
		// default retry budget spends 50s proving it.
		"SMOKE_RETRIES=1",
		"SMOKE_STARTUP_TIMEOUT=5",
	}
	for _, name := range smokeNeutralEnvironment {
		cmd.Env = append(cmd.Env, name+"="+os.Getenv(name))
	}
	for name := range smokeExpectedEnvironment {
		cmd.Env = append(cmd.Env, name+"="+os.Getenv(name))
	}
	cmd.Env = append(cmd.Env, "SMOKE_TEST_HTTP_STATUS="+os.Getenv("SMOKE_TEST_HTTP_STATUS"))
	cmd.Env = append(cmd.Env, "SMOKE_TEST_NPM_AUTH="+os.Getenv("SMOKE_TEST_NPM_AUTH"))
	// extra comes last, so it wins over anything above (os/exec keeps the last
	// value of a duplicated key). It lets a test that runs in parallel set what
	// the others read from the process environment.
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run smoke-check-release.sh: %v\n%s", err, out)
		}
	}
	return result{exitCode: code, output: string(out)}
}

func TestSmokeNamesMissingBunBeforeFetchingTheChannel(t *testing.T) {
	requireBash(t)
	bin := t.TempDir()
	for _, name := range []string{"head", "mkdir", "mktemp", "rm", "tr", "uname"} {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("locate prerequisite %s: %v", name, err)
		}
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatalf("link prerequisite %s: %v", name, err)
		}
	}
	t.Setenv("SMOKE_TEST_PATH", bin)

	res := runSmoke(t, "http://127.0.0.1:1")
	if res.exitCode == 0 {
		t.Fatalf("smoke passed without Bun:\n%s", res.output)
	}
	if !res.contains("::error::prerequisites leg:") || !res.contains("Bun v1.4.0 or later") {
		t.Fatalf("smoke did not name the missing Bun prerequisite:\n%s", res.output)
	}
	if res.contains("smoke: GET") {
		t.Fatalf("smoke fetched the release channel before checking Bun:\n%s", res.output)
	}
}

func TestSmokeRejectsBunOlderThanTheWebGoldenPathMinimum(t *testing.T) {
	requireBash(t)
	bin := t.TempDir()
	for _, name := range []string{"head", "mkdir", "mktemp", "rm", "tr", "uname"} {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("locate prerequisite %s: %v", name, err)
		}
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatalf("link prerequisite %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "bun"), []byte("#!/bin/sh\necho 1.3.14\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SMOKE_TEST_PATH", bin)

	res := runSmoke(t, "http://127.0.0.1:1")
	if res.exitCode == 0 {
		t.Fatalf("smoke passed with Bun 1.3.14:\n%s", res.output)
	}
	if !res.contains("reports unsupported version '1.3.14'") || !res.contains("Bun v1.4.0 or later") {
		t.Fatalf("smoke did not enforce the TypeScript web Bun minimum:\n%s", res.output)
	}
	if res.contains("smoke: GET") {
		t.Fatalf("smoke fetched the release channel before validating Bun's version:\n%s", res.output)
	}
}

func serveInstaller(t *testing.T, script []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/install.sh" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(script)
	}))
	t.Cleanup(server.Close)
	return server
}

func fullGoldenPathStub(t *testing.T) []byte {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []byte("#!/bin/sh\nexec " + shellQuote(executable) +
		" -test.run=^TestSmokeCLIHelperProcess$ -- \"$@\"\n")
}

// TestSmokeCLIHelperProcess is re-executed through the installed stub binary.
// It accepts only the argv the public contract requires, authors the minimum
// complete generated state the smoke asserts, and serves a real HTTP listener
// after emitting the same typed readiness shape as a published extension.
func TestSmokeCLIHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SMOKE_CLI_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	joined := strings.Join(args, "\x00")
	for _, name := range smokeNeutralEnvironment {
		want := ""
		if name == "PUTNAMI_OUTPUT" && joined == "serve\x00webapp" {
			want = "jsonl"
		}
		if got := os.Getenv(name); got != want {
			t.Fatalf("credential or shell-hook environment %s reached golden-path helper: %q", name, got)
		}
	}
	for name, want := range smokeExpectedEnvironment {
		if got := os.Getenv(name); got != want {
			t.Fatalf("golden-path helper environment %s = %q, want %q", name, got, want)
		}
	}
	if shell := filepath.Base(os.Getenv("SHELL")); shell != "bash" {
		t.Fatalf("golden-path helper received SHELL %q, want resolved Bash", os.Getenv("SHELL"))
	}
	switch joined {
	case "--version":
		_, _ = os.Stdout.WriteString("Putnami   " + stubVersion + "\n")
		return
	case "init\x00--project\x00webapp\x00--extension\x00ts":
		writeGoldenPathFixture(t)
		_, _ = os.Stdout.WriteString("  ✓ Workspace ready: smoke\n")
		return
	case "workspace\x00describe\x00--output=jsonl":
		_, _ = os.Stdout.WriteString("{\"workspace\":{\"name\":\"smoke\"}}\n")
		return
	case "projects\x00describe\x00webapp\x00--output=jsonl":
		_, _ = os.Stdout.WriteString("{\"project\":{\"name\":\"webapp\",\"extensions\":[\"@putnami/typescript\"]}}\n")
		return
	case "extensions\x00list\x00--output=jsonl":
		_, _ = os.Stdout.WriteString("{\"name\":\"@putnami/typescript\",\"version\":\"1.2.3\"}\n")
		return
	case "build\x00--projects\x00webapp\x00--plan\x00--output=jsonl":
		_, _ = os.Stdout.WriteString("{\"plan\":{\"projects\":[\"webapp\"],\"command\":\"build\"}}\n")
		return
	case "serve\x00webapp":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_, _ = os.Stdout.WriteString(`{"v":2,"type":"ready","data":{"target":"server","endpoints":[{"scheme":"http","host":"127.0.0.1","port":` + fmt.Sprint(port) + `}]}}` + "\n")
		httpStatus := http.StatusOK
		if configured := os.Getenv("SMOKE_TEST_HTTP_STATUS"); configured != "" {
			httpStatus, err = strconv.Atoi(configured)
			if err != nil {
				t.Fatalf("invalid helper HTTP status %q: %v", configured, err)
			}
		}
		_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(httpStatus)
			_, _ = w.Write([]byte("<!doctype html><title>Welcome to Putnami</title>"))
		}))
		return
	case "extensions\x00install\x00--user\x00--latest\x00" + smokeRunExtension:
		_, _ = os.Stdout.WriteString("  ✓ Pinned " + smokeRunExtension + " for this user\n")
		return
	case smokeRunCommand:
		// The run leg's caller is a fresh git directory, never the workspace.
		if info, err := os.Stat(".git"); err != nil || !info.IsDir() {
			t.Fatalf("putnami %s ran outside the run leg's git directory", smokeRunCommand)
		}
		if os.Getenv("SMOKE_TEST_RUN_DIRTY") == "1" {
			if err := os.WriteFile("stray-output.txt", []byte("written by the command\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = os.Stdout.WriteString("putnami " + smokeRunCommand + " ran\n")
		if configured := os.Getenv("SMOKE_TEST_RUN_EXIT"); configured != "" {
			code, err := strconv.Atoi(configured)
			if err != nil {
				t.Fatalf("invalid helper exit status %q: %v", configured, err)
			}
			os.Exit(code)
		}
		return
	default:
		t.Fatalf("unexpected golden-path argv: %q", args)
	}
}

func writeGoldenPathFixture(t *testing.T) {
	t.Helper()
	if err := writeFixture(goldenPathFixture()); err != nil {
		t.Fatal(err)
	}
}

// goldenPathFixture is the minimum complete state `putnami init --project
// webapp --extension ts` generates, by workspace-relative path.
func goldenPathFixture() map[string]string {
	npmrc := "@putnami:registry=https://npm.putnami.dev\n"
	if os.Getenv("SMOKE_TEST_NPM_AUTH") == "1" {
		npmrc += "//npm.putnami.dev/:_authToken=must-not-be-persisted\n"
	}
	// Putnami writes the guidance block into the entrypoints and registers its
	// MCP server in .mcp.json (ADR 0040).
	return map[string]string{
		"putnami.workspace.json": `{"name":"smoke","extensions":["@putnami/typescript"],"templates":["typescript-web"],"includes":["webapp"]}`,
		"putnami.lock.json":      `{"version":4,"extensions":{"@putnami/typescript":{"version":"1.2.3"}},"templates":{"typescript-web":{"version":"1.2.3"}}}`,
		"package.json":           `{"name":"smoke","private":true,"workspaces":["webapp"]}`,
		"bun.lock":               "lockfileVersion = 1\n",
		".npmrc":                 npmrc,
		"CLAUDE.md":              "# CLAUDE.md\n\n@AGENTS.md\n",
		"AGENTS.md":              "# AGENTS.md\n\nThis is a Putnami workspace.\n",
		".mcp.json":              `{"mcpServers":{"putnami":{"command":"putnami","args":["mcp"]}}}`,
		"webapp/putnami.json":    `{"name": "webapp","extensions":["@putnami/typescript"]}`,
		"webapp/package.json":    `{"name":"webapp"}`,
	}
}

// writeFixture writes each file, with a final newline, relative to the
// working directory.
func writeFixture(files map[string]string) error {
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func TestSmokeRejectsGeneratedPackageRegistryCredentials(t *testing.T) {
	requireBash(t)
	t.Setenv("GO_WANT_SMOKE_CLI_HELPER", "1")
	t.Setenv("SMOKE_TEST_NPM_AUTH", "1")
	body := fullGoldenPathStub(t)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})

	res := runSmoke(t, server.URL)
	if res.exitCode == 0 {
		t.Fatalf("smoke accepted generated package-registry credentials:\n%s", res.output)
	}
	if !res.contains("::error::init leg:") || !res.contains("authentication directive _authToken") {
		t.Fatalf("smoke did not reject generated credentials without printing their value:\n%s", res.output)
	}
	if res.contains("must-not-be-persisted") {
		t.Fatalf("smoke printed the rejected credential value:\n%s", res.output)
	}
}

func TestSmokeInstallsThroughTheInstallerAndDiscoversTheCommand(t *testing.T) {
	requireBash(t)
	body := stubCLIVersionOnly(stubVersion)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})

	res := runSmoke(t, server.URL)

	for _, want := range []string{
		"resolved " + stubVersion + ", advertised sha256:" + sha256Hex(body) + ", bytes match",
		"installer verified the download against the advertised digest",
		"smoke: discovered ",
		"installed binary reports " + stubVersion,
	} {
		if !res.contains(want) {
			t.Fatalf("smoke did not report %q:\n%s", want, res.output)
		}
	}
	// The stub cannot initialize a workspace, so the run must stop there — and it
	// must say which leg failed rather than which command did.
	if !res.contains("::error::init leg:") {
		t.Fatalf("smoke did not fail at the init leg with an ::error:: line:\n%s", res.output)
	}
	if res.exitCode == 0 {
		t.Fatalf("smoke passed with a CLI that cannot initialize anything:\n%s", res.output)
	}
}

func TestSmokeRunsExactTypeScriptGoldenPathThroughHTTPAndCleanStop(t *testing.T) {
	requireBash(t)
	t.Setenv("GO_WANT_SMOKE_CLI_HELPER", "1")
	hook := filepath.Join(t.TempDir(), "shell-hook")
	if err := os.WriteFile(hook, []byte(":\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range smokeNeutralEnvironment {
		value := "must-not-reach-golden-path"
		if name == "BASH_ENV" || name == "ENV" {
			value = hook
		}
		t.Setenv(name, value)
	}
	for name := range smokeExpectedEnvironment {
		t.Setenv(name, "must-not-reach-golden-path")
	}
	body := fullGoldenPathStub(t)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})

	res := runSmoke(t, server.URL)
	if res.exitCode != 0 {
		t.Fatalf("full golden path failed (exit %d):\n%s", res.exitCode, res.output)
	}
	for _, want := range []string{
		"curl -fsSL " + server.URL + "/install.sh | bash",
		"putnami init --project webapp --extension ts",
		"generated workspace, webapp, locks, extension and MCP registration verified; build plan validates",
		"putnami serve webapp",
		"smoke: GET http://127.0.0.1:",
		"HTTP listener is unreachable after clean stop",
		"HTTP → clean stop",
	} {
		if !res.contains(want) {
			t.Fatalf("full golden path did not report %q:\n%s", want, res.output)
		}
	}
	// The run leg is opt-in through SMOKE_RUN_COMMAND.
	if res.contains("?run=") || res.contains("→ run ") {
		t.Fatalf("the run leg ran without SMOKE_RUN_COMMAND:\n%s", res.output)
	}
}

func TestSmokeRejectsRedirectAsSuccessfulHTTP(t *testing.T) {
	requireBash(t)
	t.Setenv("GO_WANT_SMOKE_CLI_HELPER", "1")
	t.Setenv("SMOKE_TEST_HTTP_STATUS", "302")
	body := fullGoldenPathStub(t)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})

	res := runSmoke(t, server.URL)
	if res.exitCode == 0 {
		t.Fatalf("smoke accepted a redirect as a successful HTTP response:\n%s", res.output)
	}
	if !res.contains("::error::http leg:") || !res.contains("HTTP 302") || !res.contains("expected an explicit 2xx response") {
		t.Fatalf("smoke did not reject the redirect at the HTTP leg:\n%s", res.output)
	}
}

func TestSmokeCapturesOnlyBoundedOptInFailureDiagnostics(t *testing.T) {
	requireBash(t)
	body := stubCLIWithNoisyInitFailure(stubVersion)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})
	diagnostics := t.TempDir()

	res := runSmokeWithInstallerURL(t, server.URL, server.URL+"/install.sh", diagnostics)
	if res.exitCode == 0 {
		t.Fatalf("version-only stub unexpectedly passed:\n%s", res.output)
	}
	target := filepath.Join(diagnostics, "latest-"+runtime.GOOS+"-"+runtime.GOARCH)
	data, err := os.ReadFile(filepath.Join(target, "init.log"))
	if err != nil {
		t.Fatalf("bounded init diagnostics were not persisted: %v\n%s", err, res.output)
	}
	if lines := strings.Count(string(data), "\n"); lines > 200 {
		t.Fatalf("persisted diagnostics contain %d lines, want at most 200", lines)
	}
	if len(data) > 256*1024 {
		t.Fatalf("persisted diagnostics contain %d bytes, want at most 256 KiB", len(data))
	}
	if !res.contains("bounded failure diagnostics written to") {
		t.Fatalf("smoke did not name the persisted diagnostic path:\n%s", res.output)
	}
}

func TestSmokeKeepsRelativeDiagnosticsOutsideTheRemovedWorkdir(t *testing.T) {
	requireBash(t)
	body := stubCLIVersionOnly(stubVersion)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})
	launchDir := t.TempDir()

	res := runSmokeFrom(t, server.URL, server.URL+"/install.sh", "evidence", launchDir)
	if res.exitCode == 0 {
		t.Fatalf("version-only stub unexpectedly passed:\n%s", res.output)
	}
	path := filepath.Join(launchDir, "evidence", "latest-"+runtime.GOOS+"-"+runtime.GOARCH, "init.log")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("relative diagnostics did not survive temp-workdir cleanup at %s: %v\n%s", path, err, res.output)
	}
}

func TestSmokeFailsTheReleaseWhenNoDigestIsAdvertised(t *testing.T) {
	requireBash(t)
	body := stubCLIVersionOnly(stubVersion)
	server := newRegistry(t, registryOptions{body: body, resolved: stubVersion})

	res := runSmoke(t, server.URL)
	if res.exitCode == 0 {
		t.Fatalf("smoke passed a release the installer would refuse:\n%s", res.output)
	}
	if !res.contains("::error::channel leg:") || !res.contains("advertised no SHA-256") {
		t.Fatalf("smoke did not name the missing digest at the channel leg:\n%s", res.output)
	}
}

func TestSmokeFailsTheReleaseWhenTheAdvertisedDigestDoesNotMatch(t *testing.T) {
	requireBash(t)
	body := stubCLIVersionOnly(stubVersion)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + strings.Repeat("c", 64),
		resolved:  stubVersion,
	})

	res := runSmoke(t, server.URL)
	if res.exitCode == 0 {
		t.Fatalf("smoke passed a release whose digest describes other bytes:\n%s", res.output)
	}
	if !res.contains("::error::channel leg:") || !res.contains("streamed bytes hashing to") {
		t.Fatalf("smoke did not name the digest disagreement:\n%s", res.output)
	}
}

// This check exists because of past releases that served a stale binary: the
// channel resolved a version, but the installed binary reported an older one.
func TestSmokeFailsTheReleaseOnAStaleStamp(t *testing.T) {
	requireBash(t)
	body := stubCLIVersionOnly(stubVersion)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  "1.0.0-freshtag",
	})

	res := runSmoke(t, server.URL)
	if res.exitCode == 0 {
		t.Fatalf("smoke passed a release serving a stale binary:\n%s", res.output)
	}
	// The installer refuses first, so the smoke reports the install leg and shows
	// the installer's own evidence.
	if !res.contains("::error::install leg:") {
		t.Fatalf("smoke did not fail at the install leg:\n%s", res.output)
	}
	if !res.contains("refusing to install a stale build") {
		t.Fatalf("smoke did not surface the installer's refusal:\n%s", res.output)
	}
}

// The smoke's privilege guard is only worth having if it fires. The real
// installer never escalates, so the guard is proved against a stand-in that
// does — otherwise a guard that stopped working would look identical to a
// release that behaved.
func TestSmokeFailsTheReleaseWhenTheInstallerEscalates(t *testing.T) {
	requireBash(t)
	body := stubCLIVersionOnly(stubVersion)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})

	script := "#!/bin/bash\necho 'Integrity verified (sha256:deadbeef)'\nsudo ln -sf /bin/true \"$HOME/.local/bin/putnami\"\nexit 0\n"
	installer := serveInstaller(t, []byte(script))

	res := runSmokeWithInstallerURL(t, server.URL, installer.URL+"/install.sh", "")
	if res.exitCode == 0 {
		t.Fatalf("smoke passed an installer that prompts for a password:\n%s", res.output)
	}
	if !res.contains("::error::install leg:") || !res.contains("escalated privileges") {
		t.Fatalf("smoke did not name the escalation:\n%s", res.output)
	}
	if !res.contains("sudo ln -sf") {
		t.Fatalf("smoke did not show what was attempted:\n%s", res.output)
	}
}

func TestSmokeFailsTheReleaseWhenTheChannelIs404(t *testing.T) {
	requireBash(t)
	// A server that answers nothing at the download path is what a past release
	// looked like: packaged every release, uploaded never.
	server := newRegistry(t, registryOptions{body: nil, statusCode: 404})

	res := runSmoke(t, server.URL)
	if res.exitCode == 0 {
		t.Fatalf("smoke passed a channel that 404s:\n%s", res.output)
	}
	if !res.contains("::error::channel leg:") || !res.contains("was not published") {
		t.Fatalf("smoke did not name the unpublished channel:\n%s", res.output)
	}
}

// The stamp leg reads the installed binary's --version through a pipeline. Under
// `set -o pipefail` a binary that exits non-zero fails that pipeline and `set -e`
// kills the smoke immediately — no ::error:: line, and the ${reported:-<none>}
// fallback two lines down never runs, in exactly the case it was written for. A
// release that shipped an unrunnable binary would report an unexplained exit 1.
//
// Every other stub here exits 0 on --version, so the `|| true` that prevents this
// is invisible to them; removing it keeps them all green.
func TestSmokeNamesTheStampLegWhenTheBinaryCannotReportItsVersion(t *testing.T) {
	requireBash(t)
	body := []byte("#!/bin/sh\nexit 126\n")
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})

	res := runSmoke(t, server.URL)

	if res.exitCode == 0 {
		t.Fatalf("smoke passed a release whose binary cannot report its version:\n%s", res.output)
	}
	if !res.contains("::error::stamp leg:") {
		t.Fatalf("smoke failed without naming the stamp leg:\n%s", res.output)
	}
	if !res.contains("<none>") {
		t.Fatalf("smoke did not report the absent version as <none>:\n%s", res.output)
	}
}

// The optional run leg: SMOKE_RUN_COMMAND names a command the command map
// published next to the installer lists. The helper answers the pin for
// smokeRunExtension and the command itself.
const (
	smokeRunCommand   = "smoke-run"
	smokeRunExtension = "@putnami-test/smoke-run"
)

// runSmokeWithRunLeg runs the whole golden path plus the run leg for
// smokeRunCommand, against a registry whose command map lists it. Each run
// takes seconds, so the run-leg tests run in parallel and pass their settings
// through extra instead of the process environment.
func runSmokeWithRunLeg(t *testing.T, extra ...string) (result, *httptest.Server) {
	t.Helper()
	requireBash(t)
	requireGit(t)
	body := fullGoldenPathStub(t)
	server := newRegistry(t, registryOptions{
		body:       body,
		integrity:  "sha256:" + sha256Hex(body),
		resolved:   stubVersion,
		commandMap: commandMap(smokeRunCommand + " " + smokeRunExtension),
	})
	env := append([]string{"GO_WANT_SMOKE_CLI_HELPER=1", "SMOKE_RUN_COMMAND=" + smokeRunCommand}, extra...)
	return runSmokeFrom(t, server.URL, server.URL+"/install.sh", "", t.TempDir(), env...), server
}

func TestSmokeRunsTheOptionalRunScenarioInAnUntouchedDirectory(t *testing.T) {
	t.Parallel()
	res, server := runSmokeWithRunLeg(t)
	if res.exitCode != 0 {
		t.Fatalf("run leg failed (exit %d):\n%s", res.exitCode, res.output)
	}
	for _, want := range []string{
		"HTTP listener is unreachable after clean stop",
		`smoke: curl -fsSL "` + server.URL + `/install.sh?run=` + smokeRunCommand + `" | bash`,
		"putnami " + smokeRunCommand + " exited 0 and left ",
		"clean stop → run " + smokeRunCommand + " on ",
	} {
		if !res.contains(want) {
			t.Fatalf("run leg did not report %q:\n%s", want, res.output)
		}
	}
}

func TestSmokeFailsTheRunScenarioWhenTheCommandWritesIntoTheCallerDirectory(t *testing.T) {
	t.Parallel()
	res, _ := runSmokeWithRunLeg(t, "SMOKE_TEST_RUN_DIRTY=1")
	if res.exitCode == 0 {
		t.Fatalf("smoke passed a command that wrote into its caller's directory:\n%s", res.output)
	}
	if !res.contains("::error::run leg:") || !res.contains("changed the directory it ran in") {
		t.Fatalf("smoke did not name the changed directory at the run leg:\n%s", res.output)
	}
	if !res.contains("stray-output.txt") {
		t.Fatalf("smoke did not show what changed:\n%s", res.output)
	}
}

func TestSmokeFailsTheRunScenarioWhenTheCommandFails(t *testing.T) {
	t.Parallel()
	res, _ := runSmokeWithRunLeg(t, "SMOKE_TEST_RUN_EXIT=4")
	if res.exitCode == 0 {
		t.Fatalf("smoke passed a command that exited 4:\n%s", res.output)
	}
	if !res.contains("::error::run leg: putnami "+smokeRunCommand+" through ") || !res.contains(" exited 4") {
		t.Fatalf("smoke did not name the command's exit status at the run leg:\n%s", res.output)
	}
}

func TestSmokeRejectsAnInvalidRunCommandBeforeFetchingTheChannel(t *testing.T) {
	t.Parallel()
	requireBash(t)
	res := runSmokeFrom(t, "http://127.0.0.1:1", "http://127.0.0.1:1/install.sh", "", t.TempDir(), "SMOKE_RUN_COMMAND=smoke;id")
	if res.exitCode == 0 {
		t.Fatalf("smoke accepted an invalid SMOKE_RUN_COMMAND:\n%s", res.output)
	}
	if !res.contains("::error::SMOKE_RUN_COMMAND must match") {
		t.Fatalf("smoke did not name the invalid SMOKE_RUN_COMMAND:\n%s", res.output)
	}
	if res.contains("smoke: GET") {
		t.Fatalf("smoke fetched the release channel before validating SMOKE_RUN_COMMAND:\n%s", res.output)
	}
}
