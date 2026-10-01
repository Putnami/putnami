//go:build windows

package installscript

// Runs scripts/install.ps1 unmodified through Windows PowerShell 5.1, the
// PowerShell every supported Windows ships. The installer writes the user's own
// Path in HKCU\Environment, and these tests put the value back afterwards, so
// they run only with PUTNAMI_INSTALLSCRIPT_WINDOWS_E2E=1, on a disposable host.
//
// The test binary doubles as every program the installer runs: copied as
// putnami.exe it answers --version, copied as claude.exe or codex.exe it is a
// fake agent host. TestMain picks the role from the executable name.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"

	"go.putnami.dev/tooling/cli/internal/flock"
)

const windowsE2EEnv = "PUTNAMI_INSTALLSCRIPT_WINDOWS_E2E"

func TestMain(m *testing.M) {
	name := strings.ToLower(filepath.Base(os.Args[0]))
	switch {
	case name == "putnami.exe" || strings.HasPrefix(name, "putnami-"):
		os.Exit(runStubPutnami(os.Args[1:]))
	case name == "claude.exe" || name == "codex.exe":
		os.Exit(runFakeHostExe(strings.TrimSuffix(name, ".exe"), os.Args[1:]))
	case name == "bun.exe" || name == "git.exe" || name == "sh.exe":
		os.Exit(runSmokePrerequisite(name, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runStubPutnami answers --version with PUTNAMI_TEST_STUB_VERSION, the way the
// release binary prints its stamp. `wait <file>` runs until file exists, which
// keeps an installed putnami.exe running while another install replaces it.
// With PUTNAMI_TEST_RUN_LOG set, any other invocation plays the run form's CLI
// (runRunStubPutnami).
func runStubPutnami(args []string) int {
	if os.Getenv(smokeHelperEnv) == "1" {
		return runSmokeStubPutnami(args)
	}
	if len(args) == 1 && args[0] == "--version" {
		if log := os.Getenv("PUTNAMI_TEST_STUB_LOG"); log != "" {
			appendLine(log, fmt.Sprintf("no-relaunch=%s launched=%s", os.Getenv("PUTNAMI_NO_RELAUNCH"), os.Getenv("PUTNAMI_LAUNCHED")))
		}
		fmt.Println("Putnami   " + os.Getenv("PUTNAMI_TEST_STUB_VERSION"))
		return 0
	}
	if len(args) == 2 && args[0] == "wait" {
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(args[1]); err == nil {
				return 0
			}
			time.Sleep(50 * time.Millisecond)
		}
		return 3
	}
	if log := os.Getenv("PUTNAMI_TEST_RUN_LOG"); log != "" {
		return runRunStubPutnami(log, args)
	}
	return 0
}

// runRunStubPutnami is runStubCLI of run_command_test.go on Windows. It
// records `extensions ...` (the pin) and exits PUTNAMI_TEST_PIN_EXIT, and
// records every other invocation (the command) with its argv and working
// directory, prints one line to stdout, and exits PUTNAMI_TEST_COMMAND_EXIT.
func runRunStubPutnami(log string, args []string) int {
	status := func(name string) int {
		code, err := strconv.Atoi(os.Getenv(name))
		if err != nil {
			return 0
		}
		return code
	}
	if len(args) > 0 && args[0] == "extensions" {
		appendLine(log, "pin argv="+strings.Join(args, " "))
		return status("PUTNAMI_TEST_PIN_EXIT")
	}
	cwd, _ := os.Getwd()
	appendLine(log, "run argv="+strings.Join(args, " "))
	appendLine(log, "run cwd="+cwd)
	fmt.Println("output of putnami " + strings.Join(args, " "))
	return status("PUTNAMI_TEST_COMMAND_EXIT")
}

// runFakeHostExe behaves like the shell fakes in agent_hosts_test.go. It keeps
// its definition in PUTNAMI_TEST_HOST_STATE and stores the launcher that
// follows `--` in `mcp add`, so the definition shows exactly which arguments
// PowerShell passed.
func runFakeHostExe(name string, args []string) int {
	stateDir := os.Getenv("PUTNAMI_TEST_HOST_STATE")
	appendLine(filepath.Join(stateDir, name+"-calls.log"), strings.Join(args, " "))
	state := filepath.Join(stateDir, name+"-mcp-state.txt")
	jsonState := filepath.Join(stateDir, name+"-mcp-state.json")
	if len(args) >= 3 && args[0] == "mcp" && args[1] == "get" && args[2] == "putnami" {
		file := state
		if name == "codex" && len(args) == 4 && args[3] == "--json" {
			file = jsonState
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return 1
		}
		_, _ = os.Stdout.Write(data)
		return 0
	}
	if len(args) >= 2 && args[0] == "mcp" && args[1] == "add" {
		launcher := ""
		for i, arg := range args {
			if arg == "--" && i+1 < len(args) {
				launcher = args[i+1]
			}
		}
		if name == "claude" {
			_ = os.WriteFile(state, []byte("putnami:\n  Status: \u2714 Connected\n  Command: "+launcher+"\n  Args: mcp\n  Environment:\n    PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}\n"), 0o644)
			return 0
		}
		_ = os.WriteFile(state, []byte("putnami\n  enabled: true\n  transport: stdio\n  command: "+launcher+"\n  args: mcp\n  cwd: -\n  env: -\n"), 0o644)
		quoted := strings.ReplaceAll(launcher, `\`, `\\`)
		_ = os.WriteFile(jsonState, []byte(`{"name":"putnami","enabled":true,"transport":{"type":"stdio","command":"`+quoted+`","args":["mcp"],"env":null,"env_vars":[],"cwd":null},"enabled_tools":null,"disabled_tools":null}`+"\n"), 0o644)
		return 0
	}
	return 2
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

// windowsHost is one install run against a temp profile directory, with the
// user's real Path value in HKCU\Environment restored when the test ends.
type windowsHost struct {
	powershell string
	root       string
	profile    string
	temp       string
	state      string
	fakeBin    string
	workDir    string // the directory PowerShell starts in; defaults to profile
	binary     []byte
	vars       map[string]string
}

func newWindowsHost(t *testing.T) *windowsHost {
	t.Helper()
	if os.Getenv(windowsE2EEnv) != "1" {
		t.Skipf("set %s=1 to run install.ps1 against this host's user Path", windowsE2EEnv)
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatalf("Windows PowerShell is missing: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	h := &windowsHost{
		powershell: powershell,
		root:       root,
		profile:    filepath.Join(root, "profile"),
		temp:       filepath.Join(root, "temp"),
		state:      filepath.Join(root, "state"),
		fakeBin:    filepath.Join(root, "fakebin"),
		binary:     binary,
		vars:       map[string]string{},
	}
	for _, dir := range []string{h.profile, h.temp, h.state, h.fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	restoreUserPath(t)
	return h
}

// restoreUserPath puts the user's Path value, and its kind, back after the
// test, or removes the value when there was none.
func restoreUserPath(t *testing.T) {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Fatalf("open HKCU\\Environment: %v", err)
	}
	value, kind, err := key.GetStringValue("Path")
	missing := errors.Is(err, registry.ErrNotExist)
	if err != nil && !missing {
		key.Close()
		t.Fatalf("read the user Path: %v", err)
	}
	t.Cleanup(func() {
		defer key.Close()
		var err error
		switch {
		case missing:
			err = key.DeleteValue("Path")
			if errors.Is(err, registry.ErrNotExist) {
				err = nil
			}
		case kind == registry.EXPAND_SZ:
			err = key.SetExpandStringValue("Path", value)
		default:
			err = key.SetStringValue("Path", value)
		}
		if err != nil {
			t.Errorf("restore the user Path: %v", err)
		}
	})
}

func userPath(t *testing.T) (string, uint32) {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	value, kind, err := key.GetStringValue("Path")
	if err != nil {
		t.Fatalf("read the user Path: %v", err)
	}
	return value, kind
}

func (h *windowsHost) binDir() string { return filepath.Join(h.profile, ".putnami", "bin") }

func (h *windowsHost) set(key, value string) *windowsHost {
	h.vars[key] = value
	return h
}

// addFakeHost puts a fake agent host called name on the PATH of the run.
func (h *windowsHost) addFakeHost(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.fakeBin, name+".exe"), h.binary, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (h *windowsHost) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, h.powershell, append([]string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}, args...)...)
	systemRoot := os.Getenv("SystemRoot")
	env := append(os.Environ(),
		"USERPROFILE="+h.profile,
		"TEMP="+h.temp,
		"TMP="+h.temp,
		"PATH="+strings.Join([]string{
			h.fakeBin,
			filepath.Join(systemRoot, "System32"),
			systemRoot,
			filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0"),
		}, ";"),
		"PUTNAMI_TEST_STUB_VERSION="+stubVersion,
		"PUTNAMI_TEST_STUB_LOG="+filepath.Join(h.state, "stub.log"),
		"PUTNAMI_TEST_HOST_STATE="+h.state,
		"PUTNAMI_NO_RELAUNCH=",
		"PUTNAMI_LAUNCHED=",
	)
	for k, v := range h.vars {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Dir = h.profile
	if h.workDir != "" {
		cmd.Dir = h.workDir
	}
	return cmd
}

func (h *windowsHost) run(t *testing.T, args ...string) psResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := h.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run Windows PowerShell: %v\n%s%s", err, stdout.String(), stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return psResult{exitCode: code, stdout: stdout.String(), stderr: stderr.String()}
}

// install runs install.ps1 as a file, with the install.sh flags.
func (h *windowsHost) install(t *testing.T, args ...string) psResult {
	t.Helper()
	return h.run(t, append([]string{"-File", installPS1Path(t)}, args...)...)
}

// windowsRegistry serves an archive of the given executable the way the
// registry does.
func windowsRegistry(t *testing.T, executable []byte) *ps1Registry {
	t.Helper()
	return windowsRegistryWithMap(t, executable, "")
}

// windowsRegistryWithMap is windowsRegistry that also publishes commandMap as
// install-commands.txt next to install.ps1, or answers 404 there when
// commandMap is empty.
func windowsRegistryWithMap(t *testing.T, executable []byte, commandMap string) *ps1Registry {
	t.Helper()
	archive := windowsArchive(t, "putnami.exe", executable)
	return newPS1Registry(t, ps1RegistryOptions{registryOptions: registryOptions{
		body:       archive,
		integrity:  "sha256:" + sha256Hex(archive),
		resolved:   stubVersion,
		commandMap: commandMap,
	}})
}

// The acceptance path: install.ps1 run by Windows PowerShell installs the
// versioned layout, puts the directory first in the user's Path, registers the
// agent hosts, and the installed putnami.exe answers --version.
func TestWindowsInstallPS1InstallsThroughWindowsPowerShell(t *testing.T) {
	h := newWindowsHost(t)
	server := windowsRegistry(t, h.binary)
	h.set("PUTNAMI_REGISTRY_URL", server.URL)
	h.addFakeHost(t, "claude")
	h.addFakeHost(t, "codex")

	res := h.install(t)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	installed := filepath.Join(h.binDir(), "putnami.exe")
	for _, want := range []string{
		"Integrity verified (sha256:",
		"Version stamp verified (" + stubVersion + ")",
		"Installed to " + filepath.Join(h.binDir(), "putnami-go-latest.exe"),
		"Active: putnami.exe (a copy of putnami-go-latest.exe)",
		"Added " + h.binDir() + " to your user PATH (HKCU\\Environment) for new terminals",
		"Claude Code MCP configuration and connection verified for new sessions",
		"Codex MCP configuration verified for new sessions",
	} {
		if !res.contains(want) {
			t.Fatalf("output does not contain %q:\n%s", want, res.output())
		}
	}
	for _, name := range []string{"putnami-go-latest.exe", "putnami.exe"} {
		if got := readFile(t, filepath.Join(h.binDir(), name)); !bytes.Equal(got, h.binary) {
			t.Fatalf("%s is not the executable the archive carried", name)
		}
	}

	value, kind := userPath(t)
	if first := strings.Split(value, ";")[0]; first != h.binDir() {
		t.Fatalf("the user Path starts with %q, want %q", first, h.binDir())
	}
	if kind != registry.EXPAND_SZ && kind != registry.SZ {
		t.Fatalf("the user Path kind is %d", kind)
	}

	stubRuns := strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(h.state, "stub.log")))), "\n")
	if strings.TrimSpace(stubRuns[0]) != "no-relaunch=1 launched=" {
		t.Fatalf("the stamp check ran with %q, want relaunch disabled", stubRuns[0])
	}

	// PowerShell 5.1 passed every argument, `--` included, to the hosts.
	claudeCalls := string(readFile(t, filepath.Join(h.state, "claude-calls.log")))
	if want := "mcp add --scope user --env PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.} --transport stdio putnami -- " + installed + " mcp"; !strings.Contains(claudeCalls, want) {
		t.Fatalf("Claude registration differs from install.sh; want %q in:\n%s", want, claudeCalls)
	}
	codexCalls := string(readFile(t, filepath.Join(h.state, "codex-calls.log")))
	if want := "mcp add putnami -- " + installed + " mcp"; !strings.Contains(codexCalls, want) {
		t.Fatalf("Codex registration differs from install.sh; want %q in:\n%s", want, codexCalls)
	}

	// The acceptance check a user runs next.
	version := exec.Command(installed, "--version")
	version.Env = append(os.Environ(), "PUTNAMI_TEST_STUB_VERSION="+stubVersion)
	out, err := version.Output()
	if err != nil || strings.TrimSpace(string(out)) != "Putnami   "+stubVersion {
		t.Fatalf("putnami --version = %q, %v", out, err)
	}
}

// The one-liner users paste: `irm | iex` of the served script, under
// `powershell -c`.
func TestWindowsInstallPS1OneLiner(t *testing.T) {
	h := newWindowsHost(t)
	server := windowsRegistry(t, h.binary)
	h.set("PUTNAMI_REGISTRY_URL", server.URL).set("PUTNAMI_NO_AGENT_HOSTS", "1")

	res := h.run(t, "-Command", "irm "+server.URL+"/install.ps1 | iex")
	if res.exitCode != 0 {
		t.Fatalf("the one-liner failed (exit %d):\n%s", res.exitCode, res.output())
	}
	if got := readFile(t, filepath.Join(h.binDir(), "putnami.exe")); !bytes.Equal(got, h.binary) {
		t.Fatal("the one-liner did not install the served executable")
	}

	// `irm | iex` passes the script no arguments; flags reach it through the
	// script block form the header and --help document.
	res = h.run(t, "-Command", "& ([scriptblock]::Create((irm "+server.URL+"/install.ps1))) --version 1.2.3")
	if res.exitCode != 0 {
		t.Fatalf("the script block form failed (exit %d):\n%s", res.exitCode, res.output())
	}
	if got := server.recorded(); got[len(got)-1].query.Get("channel") != "v1.2.3" {
		t.Fatalf("--version through the script block form was not sent as v1.2.3: %v", got[len(got)-1].query)
	}

	// A refusal ends `powershell -c` with a non-zero exit.
	res = h.set("PUTNAMI_VARIANT", "rust").run(t, "-Command", "irm "+server.URL+"/install.ps1 | iex")
	if res.exitCode == 0 || !res.contains(`Unknown variant "rust"`) {
		t.Fatalf("a refused one-liner exited %d:\n%s", res.exitCode, res.output())
	}
}

// install.ps1 waits for the switch lock `putnami upgrade` and `putnami version
// use` hold, and installs nothing until it is released.
func TestWindowsInstallPS1WaitsForTheSwitchLock(t *testing.T) {
	h := newWindowsHost(t)
	server := windowsRegistry(t, h.binary)
	h.set("PUTNAMI_REGISTRY_URL", server.URL).set("PUTNAMI_NO_AGENT_HOSTS", "1")
	if err := os.MkdirAll(h.binDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := flock.Acquire(filepath.Join(h.binDir(), ".putnami-switch.lock"), true, false)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = lock.Release()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := h.command(ctx, "-File", installPS1Path(t))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	scanner := bufio.NewScanner(stdout)
	waiting := false
	for scanner.Scan() {
		seen.WriteString(scanner.Text() + "\n")
		if strings.Contains(scanner.Text(), "Waiting for another Putnami process to finish switching binaries") {
			waiting = true
			break
		}
	}
	if !waiting {
		_ = cmd.Wait()
		t.Fatalf("install.ps1 did not wait for the held lock:\n%s%s", seen.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(h.binDir(), "putnami.exe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("install.ps1 installed while another process held the switch lock")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	released = true
	rest, _ := io.ReadAll(stdout)
	seen.Write(rest)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("install after the lock was released failed: %v\n%s%s", err, seen.String(), stderr.String())
	}
	if got := readFile(t, filepath.Join(h.binDir(), "putnami.exe")); !bytes.Equal(got, h.binary) {
		t.Fatal("putnami.exe was not installed after the lock was released")
	}
}

// Windows refuses to overwrite or delete a running executable. A reinstall
// while putnami.exe runs moves it aside, and a later install deletes it once it
// has stopped.
func TestWindowsInstallPS1ReplacesARunningPutnami(t *testing.T) {
	h := newWindowsHost(t)
	first := windowsRegistry(t, h.binary)
	h.set("PUTNAMI_NO_AGENT_HOSTS", "1")
	if res := h.set("PUTNAMI_REGISTRY_URL", first.URL).install(t); res.exitCode != 0 {
		t.Fatalf("first install failed (exit %d):\n%s", res.exitCode, res.output())
	}

	installed := filepath.Join(h.binDir(), "putnami.exe")
	stop := filepath.Join(h.root, "stop")
	running := exec.Command(installed, "wait", stop)
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(stop, nil, 0o644)
		_ = running.Wait()
	}()

	// Bytes after the image leave the executable runnable and tell the two
	// builds apart.
	second := append(append([]byte(nil), h.binary...), []byte("second build")...)
	if res := h.set("PUTNAMI_REGISTRY_URL", windowsRegistry(t, second).URL).install(t); res.exitCode != 0 {
		t.Fatalf("install over a running putnami.exe failed (exit %d):\n%s", res.exitCode, res.output())
	}
	if got := readFile(t, installed); !bytes.Equal(got, second) {
		t.Fatal("putnami.exe was not replaced while the old one ran")
	}
	aside, err := filepath.Glob(filepath.Join(h.binDir(), ".putnami.exe.old-*"))
	if err != nil || len(aside) != 1 {
		t.Fatalf("want the running putnami.exe moved aside once, found %v (%v)", aside, err)
	}

	if err := os.WriteFile(stop, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := running.Wait(); err != nil {
		t.Fatalf("the moved-aside putnami.exe did not keep running: %v", err)
	}
	if res := h.install(t); res.exitCode != 0 {
		t.Fatalf("third install failed (exit %d):\n%s", res.exitCode, res.output())
	}
	if left, _ := filepath.Glob(filepath.Join(h.binDir(), ".putnami*.old-*")); len(left) != 0 {
		t.Fatalf("a stopped moved-aside binary was not deleted: %v", left)
	}
}

// --- The run form ------------------------------------------------------------

// windowsRunHost is a Windows host set up for the run form: a registry that
// serves the test binary and commandMap, a CLI log, and an empty caller's
// directory PowerShell starts in, outside the profile.
type windowsRunHost struct {
	*windowsHost
	registry  *ps1Registry
	callerDir string
	cliLog    string
}

func newWindowsRunHost(t *testing.T, commandMap string) *windowsRunHost {
	t.Helper()
	h := newWindowsHost(t)
	registry := windowsRegistryWithMap(t, h.binary, commandMap)
	callerDir := filepath.Join(h.root, "caller")
	if err := os.MkdirAll(callerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.workDir = callerDir
	cliLog := filepath.Join(h.root, "cli-calls.log")
	h.set("PUTNAMI_REGISTRY_URL", registry.URL).
		set("PUTNAMI_COMMAND_MAP_URL", registry.URL+"/install-commands.txt").
		set("PUTNAMI_TEST_RUN_LOG", cliLog).
		set("PUTNAMI_NO_AGENT_HOSTS", "1")
	return &windowsRunHost{windowsHost: h, registry: registry, callerDir: callerDir, cliLog: cliLog}
}

func (h *windowsRunHost) cliCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.cliLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")), "\n")
}

func (h *windowsRunHost) requests(path string) int {
	count := 0
	for _, request := range h.registry.recorded() {
		if request.path == path {
			count++
		}
	}
	return count
}

// assertRan proves the run pinned @acme/deploy, then ran `putnami deploy` in
// the caller's directory, whose output alone is on stdout.
func (h *windowsRunHost) assertRan(t *testing.T, res psResult) {
	t.Helper()
	calls := h.cliCalls(t)
	if len(calls) != 3 || calls[0] != "pin argv=extensions install --user --latest @acme/deploy" || calls[1] != "run argv=deploy" || !strings.HasPrefix(calls[2], "run cwd=") {
		t.Fatalf("CLI calls = %q, want the pin, then putnami deploy\n%s", calls, res.output())
	}
	ran, err := os.Stat(strings.TrimPrefix(calls[2], "run cwd="))
	if err != nil {
		t.Fatal(err)
	}
	caller, err := os.Stat(h.callerDir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(ran, caller) {
		t.Fatalf("putnami deploy ran in %s, not in the caller's directory %s", calls[2], h.callerDir)
	}
	if got := strings.ReplaceAll(res.stdout, "\r\n", "\n"); got != "output of putnami deploy\n" {
		t.Fatalf("stdout carries more than the command's output:\n%s", res.output())
	}
	if !strings.Contains(res.stderr, "Pinned @acme/deploy") || strings.Contains(res.output(), "Next:") {
		t.Fatalf("stderr does not carry the installer's run-mode messages:\n%s", res.output())
	}
	h.assertCallerDirUntouched(t)
}

func (h *windowsRunHost) assertCallerDirUntouched(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(h.callerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the installer wrote %s into the caller's directory", entries[0].Name())
	}
}

// Run from a file, the installer exits with the command's status, which is how
// cmd.exe and CI read it.
func TestWindowsInstallPS1RunFromAFile(t *testing.T) {
	h := newWindowsRunHost(t, commandMap("deploy @acme/deploy"))
	h.set("PUTNAMI_TEST_COMMAND_EXIT", "7")

	res := h.install(t, "--run", "deploy")
	if res.exitCode != 7 {
		t.Fatalf("exit = %d, want the command's 7:\n%s", res.exitCode, res.output())
	}
	h.assertRan(t, res)
	if got := readFile(t, filepath.Join(h.binDir(), "putnami.exe")); !bytes.Equal(got, h.binary) {
		t.Fatal("the run form did not install the served executable")
	}
}

func TestWindowsInstallPS1RunDoesNotRunTheCommandWhenThePinFails(t *testing.T) {
	h := newWindowsRunHost(t, commandMap("deploy @acme/deploy"))
	h.set("PUTNAMI_TEST_PIN_EXIT", "5")

	res := h.install(t, "--run", "deploy")
	if res.exitCode != 5 {
		t.Fatalf("exit = %d, want the pin's 5:\n%s", res.exitCode, res.output())
	}
	if calls := h.cliCalls(t); len(calls) != 1 || calls[0] != "pin argv=extensions install --user --latest @acme/deploy" {
		t.Fatalf("the command ran after a failed pin: %q", calls)
	}
	if !strings.Contains(res.stderr, "putnami extensions install --user --latest @acme/deploy failed (exit 5); putnami deploy was not run.") || res.stdout != "" {
		t.Fatalf("the failed pin was not named on stderr alone:\n%s", res.output())
	}
	h.assertCallerDirUntouched(t)
}

// A command the installer refuses installs nothing: an invalid command makes no
// request at all, and a command the map does not list, or a map that is not
// one, stops before the CLI download.
func TestWindowsInstallPS1RunRefusesBeforeInstalling(t *testing.T) {
	for _, tc := range []struct {
		name, commandMap, command, want string
		anyRequest                      bool
	}{
		{name: "invalid command", commandMap: commandMap("deploy @acme/deploy"), command: "Deploy", want: "must match ^[a-z][a-z0-9-]{0,63}$. Nothing was installed."},
		{name: "unlisted command", commandMap: commandMap("deploy @acme/deploy"), command: "depoly", want: "does not list it", anyRequest: true},
		{name: "map after a byte order mark", commandMap: "\ufeff" + commandMap("deploy @acme/deploy"), command: "deploy", want: "is not a command map", anyRequest: true},
		{name: "map with CRLF line endings", commandMap: strings.ReplaceAll(commandMap("deploy @acme/deploy"), "\n", "\r\n"), command: "deploy", want: "is not a command map", anyRequest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWindowsRunHost(t, tc.commandMap)
			res := h.install(t, "--run", tc.command)
			if res.exitCode == 0 || !strings.Contains(res.stderr, tc.want) || res.stdout != "" {
				t.Fatalf("the run was not refused with %q on stderr alone (exit %d):\n%s", tc.want, res.exitCode, res.output())
			}
			if got := len(h.registry.recorded()); !tc.anyRequest && got != 0 {
				t.Fatalf("an invalid command made %d request(s)", got)
			}
			if got := h.requests("/putnami/cli/download"); got != 0 {
				t.Fatalf("the CLI was downloaded before the refusal")
			}
			if _, err := os.Stat(filepath.Join(h.profile, ".putnami")); err == nil {
				t.Fatal("a refused run created .putnami")
			}
			if calls := h.cliCalls(t); len(calls) != 0 {
				t.Fatalf("a refused run invoked the CLI: %q", calls)
			}
			h.assertCallerDirUntouched(t)
			if left, _ := filepath.Glob(filepath.Join(h.temp, "putnami-install-*")); len(left) != 0 {
				t.Fatalf("a refused run left %v", left)
			}
		})
	}
}

// The forms a user types, as the documentation gives them: the served
// one-liner with the command baked in, and the script block form with --run.
// From text the installer never exits, so the session goes on and reads the
// command's status in $LASTEXITCODE. Each form runs as script text in a
// script run with -File, as the smoke runs the one-liner: Microsoft Defender
// blocks the one-liner passed to powershell on a command line.
func TestWindowsInstallPS1RunOneLiner(t *testing.T) {
	for _, tc := range []struct{ name, form string }{
		{name: "baked one-liner", form: ps1RunOneLinerForm},
		{name: "script block form", form: ps1RunScriptBlockForm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWindowsRunHost(t, commandMap("deploy @acme/deploy"))
			h.set("PUTNAMI_TEST_COMMAND_EXIT", "7")
			script := filepath.Join(h.root, "one-liner.ps1")
			body := ps1RunForm(tc.form, h.registry.URL, "deploy") + "\r\n" +
				"[Console]::Error.WriteLine('session went on')\r\n" +
				"exit $LASTEXITCODE\r\n"
			if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			res := h.run(t, "-File", script)
			if res.exitCode != 7 || !strings.Contains(res.stderr, "session went on") {
				t.Fatalf("exit = %d, want the command's 7 read from $LASTEXITCODE after the session went on:\n%s", res.exitCode, res.output())
			}
			h.assertRan(t, res)
		})
	}
}

// The cmd.exe form the documentation gives: curl.exe saves the script under
// %TEMP%, not in the caller's directory, and powershell -File runs it, so the
// exit code is the command's.
func TestWindowsInstallPS1RunFromCmd(t *testing.T) {
	h := newWindowsRunHost(t, commandMap("deploy @acme/deploy"))
	h.set("PUTNAMI_TEST_COMMAND_EXIT", "7")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := h.command(ctx)
	comspec := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	cmd.Path = comspec
	cmd.Args = []string{comspec}
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `/d /s /c "` + ps1RunForm(ps1RunFromCmdForm, h.registry.URL, "deploy") + `"`}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := psResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("cmd.exe form: %v, want exit 7:\n%s", err, res.output())
	}
	h.assertRan(t, res)
}
