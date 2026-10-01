//go:build windows

package installscript

// Runs scripts/smoke-check-release.ps1 unmodified through Windows PowerShell
// 5.1 against an httptest registry that serves this test binary as the CLI.
// Copied as putnami.exe and run with GO_WANT_SMOKE_CLI_HELPER=1, the binary
// answers the golden-path commands the way TestSmokeCLIHelperProcess does for
// smoke-check-release.sh, and serves HTTP until it receives the
// CTRL_BREAK_EVENT the smoke sends. Copied as bun.exe, git.exe or sh.exe, it
// stands in for the prerequisites the smoke names. With SMOKE_RUN_COMMAND set
// to smokeRunCommand, the smoke also runs the optional run leg against the
// command map the registry publishes next to install.ps1.
//
// The smoke runs install.ps1, which writes the user's Path in
// HKCU\Environment, so these tests run only with
// PUTNAMI_INSTALLSCRIPT_WINDOWS_E2E=1, on a disposable host.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"

	"go.putnami.dev/tooling/cli/internal/git"
)

const smokeHelperEnv = "GO_WANT_SMOKE_CLI_HELPER"

const smokeOKLine = "smoke: OK - channel 'latest' passes irm install -> TypeScript init -> serve -> HTTP -> clean stop on windows/amd64"

const smokeRunOKLine = "smoke: OK - channel 'latest' passes irm install -> TypeScript init -> serve -> HTTP -> clean stop -> run " + smokeRunCommand + " on windows/amd64"

// smokeWindowsEnvironment is what the Windows smoke sets for every program it
// runs, in place of the shell smoke's smokeExpectedEnvironment: Git's long
// paths are on (decision D-W8), through the command-line configuration.
var smokeWindowsEnvironment = map[string]string{
	"GIT_CONFIG_COUNT":           "1",
	"GIT_CONFIG_KEY_0":           "core.longpaths",
	"GIT_CONFIG_VALUE_0":         "true",
	"GIT_CONFIG_NOSYSTEM":        "1",
	"PUTNAMI_TELEMETRY":          "off",
	"PUTNAMI_TELEMETRY_ENDPOINT": "",
	"DO_NOT_TRACK":               "1",
	"PUTNAMI_VERSION":            "latest",
}

// smokeEnvironmentProblem names the first variable of the smoke's neutral
// environment that is wrong for the command joined, or returns "".
func smokeEnvironmentProblem(joined string) string {
	for _, name := range smokeNeutralEnvironment {
		want := ""
		if name == "PUTNAMI_OUTPUT" && joined == "serve\x00webapp" {
			want = "jsonl"
		}
		if got := os.Getenv(name); got != want {
			return fmt.Sprintf("%s reached the golden path as %q", name, got)
		}
	}
	for name, want := range smokeWindowsEnvironment {
		if got := os.Getenv(name); got != want {
			return fmt.Sprintf("%s = %q, want %q", name, got, want)
		}
	}
	profile := os.Getenv("USERPROFILE")
	if !strings.Contains(profile, `\putnami-smoke-`) || os.Getenv("HOME") != profile {
		return fmt.Sprintf("USERPROFILE %q and HOME %q are not the smoke's neutral home", profile, os.Getenv("HOME"))
	}
	for _, name := range []string{"APPDATA", "LOCALAPPDATA", "PUTNAMI_STORE_DIR", "BUN_INSTALL"} {
		if !strings.Contains(os.Getenv(name), `\putnami-smoke-`) {
			return fmt.Sprintf("%s %q is outside the smoke workdir", name, os.Getenv(name))
		}
	}
	return ""
}

// runSmokeStubPutnami is putnami.exe during a smoke run.
func runSmokeStubPutnami(args []string) int {
	joined := strings.Join(args, "\x00")
	if problem := smokeEnvironmentProblem(joined); problem != "" {
		fmt.Fprintln(os.Stderr, "smoke stub: "+problem)
		return 4
	}
	switch joined {
	case "--version":
		fmt.Println("Putnami   " + stubVersion)
		return 0
	case "init\x00--project\x00webapp\x00--extension\x00ts":
		files := goldenPathFixture()
		files[".gitattributes"] = git.LFPolicyAttributes
		if err := writeFixture(files); err != nil {
			fmt.Fprintln(os.Stderr, "smoke stub: "+err.Error())
			return 4
		}
		fmt.Println("  Workspace ready: smoke")
		return 0
	case "workspace\x00describe\x00--output=jsonl":
		fmt.Println(`{"workspace":{"name":"smoke"}}`)
		return 0
	case "projects\x00describe\x00webapp\x00--output=jsonl":
		fmt.Println(`{"project":{"name":"webapp","extensions":["@putnami/typescript"]}}`)
		return 0
	case "extensions\x00list\x00--output=jsonl":
		fmt.Println(`{"name":"@putnami/typescript","version":"1.2.3"}`)
		return 0
	case "build\x00--projects\x00webapp\x00--plan\x00--output=jsonl":
		fmt.Println(`{"plan":{"projects":["webapp"],"command":"build"}}`)
		return 0
	case "serve\x00webapp":
		return runSmokeStubServe()
	case "extensions\x00install\x00--user\x00--latest\x00" + smokeRunExtension:
		fmt.Println("  Pinned " + smokeRunExtension + " for this user")
		return 0
	case smokeRunCommand:
		return runSmokeStubRunCommand()
	}
	fmt.Fprintf(os.Stderr, "smoke stub: unexpected argv %q\n", args)
	return 3
}

// runSmokeStubRunCommand is putnami smoke-run in the run leg. It needs the
// run leg's fresh git directory as its working directory, and exits 6 in any
// other one. With SMOKE_TEST_RUN_DIRTY=1 it writes a file there, and it exits
// SMOKE_TEST_RUN_EXIT when that is set.
func runSmokeStubRunCommand() int {
	if info, err := os.Stat(".git"); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "smoke stub: putnami %s ran outside the run leg's git directory\n", smokeRunCommand)
		return 6
	}
	if os.Getenv("SMOKE_TEST_RUN_DIRTY") == "1" {
		if err := os.WriteFile("stray-output.txt", []byte("written by the command\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "smoke stub: "+err.Error())
			return 6
		}
	}
	fmt.Println("putnami " + smokeRunCommand + " ran")
	if configured := os.Getenv("SMOKE_TEST_RUN_EXIT"); configured != "" {
		code, err := strconv.Atoi(configured)
		if err != nil {
			fmt.Fprintln(os.Stderr, "smoke stub: "+err.Error())
			return 6
		}
		return code
	}
	return 0
}

// runSmokeStubServe emits the ready record with the port the system
// assigned, serves HTTP, and stops when it receives CTRL_BREAK_EVENT, which Go
// delivers as os.Interrupt. With SMOKE_TEST_IGNORE_BREAK=1 it keeps serving.
func runSmokeStubServe() int {
	if os.Getenv("PORT") != "0" {
		fmt.Fprintf(os.Stderr, "smoke stub: serve got PORT %q, want 0\n", os.Getenv("PORT"))
		return 4
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke stub: "+err.Error())
		return 4
	}
	status := http.StatusOK
	if configured := os.Getenv("SMOKE_TEST_HTTP_STATUS"); configured != "" {
		if status, err = strconv.Atoi(configured); err != nil {
			fmt.Fprintln(os.Stderr, "smoke stub: "+err.Error())
			return 4
		}
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<!doctype html><title>Welcome to Putnami</title>"))
	})}
	go func() { _ = server.Serve(listener) }()
	fmt.Println(smokeReadyRecord(listener.Addr().(*net.TCPAddr).Port))
	deadline := time.After(5 * time.Minute)
	for {
		select {
		case <-interrupt:
			fmt.Println(`{"v":2,"type":"interrupt"}`)
			if os.Getenv("SMOKE_TEST_IGNORE_BREAK") == "1" {
				continue
			}
			_ = server.Close()
			return 0
		case <-deadline:
			return 5
		}
	}
}

// runSmokePrerequisite stands in for bun.exe, git.exe and sh.exe. bun
// reports SMOKE_TEST_BUN_VERSION, or the smoke's minimum. For the run leg,
// git init creates .git in the working directory, and git status reports
// every other entry there as untracked, as git does with those options.
func runSmokePrerequisite(name string, args []string) int {
	if name == "bun.exe" && len(args) == 1 && args[0] == "--version" {
		version := os.Getenv("SMOKE_TEST_BUN_VERSION")
		if version == "" {
			version = "1.4.0"
		}
		fmt.Println(version)
	}
	if name != "git.exe" || len(args) == 0 {
		return 0
	}
	switch strings.Join(args, " ") {
	case "init -q":
		if err := os.Mkdir(".git", 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "fake git: "+err.Error())
			return 1
		}
	case "status --porcelain --untracked-files=all --ignored":
		entries, err := os.ReadDir(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake git: "+err.Error())
			return 1
		}
		for _, entry := range entries {
			if entry.Name() != ".git" {
				fmt.Println("?? " + entry.Name())
			}
		}
	}
	return 0
}

// newSmokeHost is a Windows host where the smoke installs from server, or
// from a healthy local registry when server is nil, with the prerequisites on
// PATH and every variable of the neutral environment set to a value the smoke
// must clear. The healthy registry publishes a command map that lists
// smokeRunCommand; only a smoke with SMOKE_RUN_COMMAND set reads it.
func newSmokeHost(t *testing.T, server *ps1Registry, prerequisites ...string) (*windowsHost, *ps1Registry) {
	t.Helper()
	h := newWindowsHost(t)
	for _, name := range prerequisites {
		if err := os.WriteFile(filepath.Join(h.fakeBin, name), h.binary, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if server == nil {
		server = windowsRegistryWithMap(t, h.binary, commandMap(smokeRunCommand+" "+smokeRunExtension))
	}
	h.set("PUTNAMI_REGISTRY_URL", server.URL).
		set("SMOKE_INSTALL_URL", server.URL+"/install.ps1").
		set("SMOKE_RETRIES", "1").
		set("SMOKE_STARTUP_TIMEOUT", "60").
		set(smokeHelperEnv, "1")
	for _, name := range smokeNeutralEnvironment {
		h.set(name, "leaked-"+name)
	}
	return h, server
}

func (h *windowsHost) smoke(t *testing.T) psResult {
	t.Helper()
	return h.run(t, "-File", smokePS1Path(t), "latest")
}

// userPathState is the user's Path value in HKCU\Environment and its kind.
func userPathState(t *testing.T) string {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	value, kind, err := key.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "<none>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%s", kind, value)
}

// smokeRequests counts the requests server received for path.
func smokeRequests(server *ps1Registry, path string) int {
	count := 0
	for _, request := range server.recorded() {
		if request.path == path {
			count++
		}
	}
	return count
}

func assertNoSmokeWorkdir(t *testing.T, h *windowsHost) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(h.temp, "putnami-smoke-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Fatalf("the smoke left its workdir behind: %v", left)
	}
}

// The acceptance path: the whole Windows golden path passes against a healthy
// release, and the host is as it was afterwards.
func TestWindowsSmokePS1RunsTheGoldenPath(t *testing.T) {
	h, server := newSmokeHost(t, nil, "bun.exe", "git.exe", "sh.exe")
	before := userPathState(t)

	res := h.smoke(t)
	if res.exitCode != 0 || !res.contains(smokeOKLine) {
		t.Fatalf("the smoke failed (exit %d):\n%s", res.exitCode, res.output())
	}
	for _, want := range []string{
		"smoke: prerequisites include Bun 1.4.0 at " + filepath.Join(h.fakeBin, "bun.exe"),
		"smoke: channel 'latest' resolved " + stubVersion,
		"smoke: installer verified the download against the advertised digest",
		"smoke: a new terminal resolves putnami to ",
		"smoke: selected executable windows/amd64, digest ",
		"smoke: installed binary reports " + stubVersion,
		"smoke: HTTP listener is unreachable after clean stop",
	} {
		if !res.contains(want) {
			t.Fatalf("the smoke output does not contain %q:\n%s", want, res.output())
		}
	}
	if after := userPathState(t); after != before {
		t.Fatalf("the smoke left the user Path changed:\nbefore %s\nafter  %s", before, after)
	}
	assertNoSmokeWorkdir(t, h)
	downloads := 0
	for _, request := range server.recorded() {
		if request.path != "/putnami/cli/download" {
			continue
		}
		downloads++
		if got := request.query.Get("os") + "/" + request.query.Get("arch") + "@" + request.query.Get("channel"); got != "windows/amd64@latest" {
			t.Fatalf("a download asked for %s, want windows/amd64@latest", got)
		}
	}
	if downloads < 2 {
		t.Fatalf("the channel preflight and the installer made %d downloads, want 2", downloads)
	}
	// Without SMOKE_RUN_COMMAND there is no run leg.
	if res.contains("run leg") || smokeRequests(server, "/install-commands.txt") != 0 {
		t.Fatalf("the smoke ran the run leg without SMOKE_RUN_COMMAND:\n%s", res.output())
	}
}

// The smoke reads the port of the ready line when "type" comes before "port",
// as in an extension's own event line; the golden path above covers the CLI's
// record, where "port" comes first.
func TestWindowsSmokePS1ReadsTheReadyPortWhenTypeComesFirst(t *testing.T) {
	h, _ := newSmokeHost(t, nil, "bun.exe", "git.exe", "sh.exe")
	h.set(smokeReadyShapeEnv, "extension")

	res := h.smoke(t)
	if res.exitCode != 0 || !res.contains(smokeOKLine) {
		t.Fatalf("the smoke failed (exit %d):\n%s", res.exitCode, res.output())
	}
}

// The optional run leg: with SMOKE_RUN_COMMAND set, the smoke runs the
// installer's run form from script text, in a fresh git directory outside the
// workspace, after the golden path. The installer reads the command map
// served next to it, pins the extension the map names, and runs the command
// in that directory, which stays as git init left it.
func TestWindowsSmokePS1RunsTheOptionalRunLegInAnUntouchedDirectory(t *testing.T) {
	h, server := newSmokeHost(t, nil, "bun.exe", "git.exe", "sh.exe")
	h.set("SMOKE_RUN_COMMAND", smokeRunCommand)
	before := userPathState(t)

	res := h.smoke(t)
	if res.exitCode != 0 || !res.contains(smokeRunOKLine) {
		t.Fatalf("the smoke with a run leg failed (exit %d):\n%s", res.exitCode, res.output())
	}
	for _, want := range []string{
		smokeOKLine[:strings.Index(smokeOKLine, " on windows/amd64")],
		"smoke: HTTP listener is unreachable after clean stop",
		"smoke: irm '" + server.URL + "/install.ps1?run=" + smokeRunCommand + "' | iex",
		"smoke: putnami " + smokeRunCommand + " exited 0 and left ",
	} {
		if !res.contains(want) {
			t.Fatalf("the smoke output does not contain %q:\n%s", want, res.output())
		}
	}
	if res.contains(smokeOKLine) {
		t.Fatalf("the smoke reported the golden path without the run leg it ran:\n%s", res.output())
	}
	baked := 0
	for _, request := range server.recorded() {
		if request.path == "/install.ps1" && request.query.Get("run") == smokeRunCommand {
			baked++
		}
	}
	if baked != 1 || smokeRequests(server, "/install-commands.txt") != 1 {
		t.Fatalf("the run leg fetched install.ps1?run=%s %d times and the command map %d times, want 1 and 1", smokeRunCommand, baked, smokeRequests(server, "/install-commands.txt"))
	}
	if after := userPathState(t); after != before {
		t.Fatalf("the smoke left the user Path changed:\nbefore %s\nafter  %s", before, after)
	}
	assertNoSmokeWorkdir(t, h)
}

// A command that writes into its caller's directory fails the run leg, which
// names what changed.
func TestWindowsSmokePS1FailsTheRunLegWhenTheCommandWritesIntoTheCallerDirectory(t *testing.T) {
	h, _ := newSmokeHost(t, nil, "bun.exe", "git.exe", "sh.exe")
	h.set("SMOKE_RUN_COMMAND", smokeRunCommand).set("SMOKE_TEST_RUN_DIRTY", "1")

	res := h.smoke(t)
	want := "::error::run leg: putnami " + smokeRunCommand + " changed the directory it ran in; the run form must leave the caller's directory untouched"
	if res.exitCode == 0 || !res.contains(want) {
		t.Fatalf("the smoke passed a command that wrote into its caller's directory (exit %d):\n%s", res.exitCode, res.output())
	}
	if !res.contains("?? stray-output.txt") {
		t.Fatalf("the smoke did not show what changed:\n%s", res.output())
	}
	assertNoSmokeWorkdir(t, h)
}

// A command that fails fails the run leg with the command's own status, which
// the installer's run form leaves in $LASTEXITCODE.
func TestWindowsSmokePS1FailsTheRunLegWhenTheCommandFails(t *testing.T) {
	h, server := newSmokeHost(t, nil, "bun.exe", "git.exe", "sh.exe")
	h.set("SMOKE_RUN_COMMAND", smokeRunCommand).set("SMOKE_TEST_RUN_EXIT", "4")

	res := h.smoke(t)
	want := "::error::run leg: putnami " + smokeRunCommand + " through " + server.URL + "/install.ps1?run=" + smokeRunCommand + " exited 4"
	if res.exitCode == 0 || !res.contains(want) {
		t.Fatalf("the smoke did not name the command's exit status (exit %d):\n%s", res.exitCode, res.output())
	}
	if !res.contains("putnami " + smokeRunCommand + " ran") {
		t.Fatalf("the failure is not the command's own status:\n%s", res.output())
	}
	assertNoSmokeWorkdir(t, h)
}

// An invalid SMOKE_RUN_COMMAND stops the smoke before it sends any request.
func TestWindowsSmokePS1RejectsAnInvalidRunCommandBeforeAnyRequest(t *testing.T) {
	for _, command := range []string{"smoke;id", "Smoke-run", "-smoke", "smoke run"} {
		t.Run(command, func(t *testing.T) {
			h, server := newSmokeHost(t, nil, "bun.exe", "git.exe", "sh.exe")
			h.set("SMOKE_RUN_COMMAND", command)

			res := h.smoke(t)
			if res.exitCode == 0 || !res.contains("::error::SMOKE_RUN_COMMAND must match ^[a-z][a-z0-9-]{0,63}$, got '"+command+"'") {
				t.Fatalf("the smoke accepted SMOKE_RUN_COMMAND %q (exit %d):\n%s", command, res.exitCode, res.output())
			}
			if res.contains("smoke: GET") || len(server.recorded()) != 0 {
				t.Fatalf("the smoke sent a request before validating SMOKE_RUN_COMMAND:\n%s", res.output())
			}
		})
	}
}

// A failure writes bounded diagnostics next to the launch directory, never
// .npmrc, and still stops the server, restores the user Path and removes the
// workdir.
func TestWindowsSmokePS1WritesBoundedDiagnosticsOnFailure(t *testing.T) {
	h, _ := newSmokeHost(t, nil, "bun.exe", "git.exe", "sh.exe")
	h.set("SMOKE_TEST_HTTP_STATUS", "500").set("SMOKE_DIAGNOSTICS_DIR", "diagnostics")
	before := userPathState(t)

	res := h.smoke(t)
	if res.exitCode == 0 || !res.contains("::error::http leg: webapp returned HTTP 500; expected an explicit 2xx response") {
		t.Fatalf("the smoke did not fail the http leg (exit %d):\n%s", res.exitCode, res.output())
	}
	target := filepath.Join(h.profile, "diagnostics", "latest-windows-amd64")
	if !res.contains("smoke: bounded failure diagnostics written to " + target) {
		t.Fatalf("the smoke did not name its diagnostics directory %s:\n%s", target, res.output())
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Name()] = true
		if strings.Contains(entry.Name(), "npmrc") {
			t.Fatalf("the diagnostics copy .npmrc as %s", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 262144 {
			t.Fatalf("%s is %d bytes, over the diagnostic limit", entry.Name(), info.Size())
		}
	}
	for _, want := range []string{"install.log", "init.log", "inspect.jsonl", "serve.jsonl", "putnami.workspace.json", "webapp_putnami.json", ".gitattributes", "workspace-files.txt"} {
		if !names[want] {
			t.Fatalf("the diagnostics lack %s: %v", want, names)
		}
	}
	serveLog := string(readFile(t, filepath.Join(target, "serve.jsonl")))
	if !strings.Contains(serveLog, `"type":"interrupt"`) {
		t.Fatalf("the failed smoke did not stop the server with CTRL_BREAK_EVENT:\n%s", serveLog)
	}
	if after := userPathState(t); after != before {
		t.Fatalf("the smoke left the user Path changed:\nbefore %s\nafter  %s", before, after)
	}
	assertNoSmokeWorkdir(t, h)
}

// A server that ignores CTRL_BREAK_EVENT is killed, and the kill fails the
// release: on Windows, a stop that needs TerminateProcess is the orphan risk
// the shutdown leg exists for.
func TestWindowsSmokePS1FailsWhenTheServerNeedsAKill(t *testing.T) {
	h, _ := newSmokeHost(t, nil, "bun.exe", "git.exe", "sh.exe")
	h.set("SMOKE_TEST_IGNORE_BREAK", "1")

	res := h.smoke(t)
	if res.exitCode == 0 || !res.contains("::error::shutdown leg: webapp did not stop within the graceful shutdown budget") {
		t.Fatalf("the smoke accepted a killed server (exit %d):\n%s", res.exitCode, res.output())
	}
	assertNoSmokeWorkdir(t, h)
}

// Without Bun, the smoke names it and sends no request.
func TestWindowsSmokePS1NamesMissingBunBeforeAnyRequest(t *testing.T) {
	h, server := newSmokeHost(t, nil, "git.exe", "sh.exe")

	res := h.smoke(t)
	if res.exitCode == 0 || !res.contains("::error::prerequisites leg: Bun v1.4.0 or later is required") {
		t.Fatalf("the smoke did not name the missing Bun (exit %d):\n%s", res.exitCode, res.output())
	}
	if res.contains("smoke: GET") || len(server.recorded()) != 0 {
		t.Fatalf("the smoke sent a request before naming Bun:\n%s", res.output())
	}

	h, _ = newSmokeHost(t, server, "bun.exe", "git.exe", "sh.exe")
	res = h.set("SMOKE_TEST_BUN_VERSION", "1.3.14").smoke(t)
	if res.exitCode == 0 || !res.contains("reports unsupported version '1.3.14'; Bun v1.4.0 or later is required") {
		t.Fatalf("the smoke accepted Bun 1.3.14 (exit %d):\n%s", res.exitCode, res.output())
	}
	if len(server.recorded()) != 0 {
		t.Fatal("the smoke sent a request before checking Bun's version")
	}
}

// A channel that does not serve windows/amd64 fails the channel leg, matching
// a real past release failure.
func TestWindowsSmokePS1FailsWhenTheChannelIs404(t *testing.T) {
	server := newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{body: []byte("not found\n"), statusCode: http.StatusNotFound}})
	h, _ := newSmokeHost(t, server, "bun.exe", "git.exe", "sh.exe")

	res := h.smoke(t)
	want := "::error::channel leg: CLI download channel 'latest' returned HTTP 404 for windows/amd64 - the CLI binary was not published"
	if res.exitCode == 0 || !res.contains(want) {
		t.Fatalf("the smoke did not fail the channel leg (exit %d):\n%s", res.exitCode, res.output())
	}
	assertNoSmokeWorkdir(t, h)
}
