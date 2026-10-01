package installscript

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The first-use smoke is the only thing that runs the documented block as a
// reader pastes it, on a machine that holds nothing but what the installer
// needs. These tests run the real script in host mode against a local registry
// and a stub CLI, so a broken leg is caught before a release depends on it.
// Image mode needs Docker, which the contributor gate does not have: its
// command line and the image it builds are pinned through a recording `docker`.

const (
	firstUseHelperEnv   = "GO_WANT_FIRST_USE_HELPER"
	firstUseScenarioEnv = "FIRST_USE_SCENARIO"
	firstUseChannelEnv  = "FIRST_USE_EXPECT_CHANNEL"
)

// firstUseBlock is the block of the getting-started page, as the fixtures
// print it.
const firstUseBlock = "curl -fsSL https://putnami.dev/install.sh | bash\n" +
	"export PATH=\"$HOME/.putnami/bin:$PATH\"\n" +
	"putnami init --project webapp --extension ts\n" +
	"putnami serve webapp\n"

func firstUseScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	script := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "smoke-first-use.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("smoke-first-use.sh not found: %v", err)
	}
	return script
}

// firstUsePage writes a page whose first bash block is block, after a block of
// another language and before a second bash block, and returns its path.
func firstUsePage(t *testing.T, block string) string {
	t.Helper()
	page := filepath.Join(t.TempDir(), "index.md")
	content := "# Getting Started\n\n```text\nnot a command\n```\n\n```bash\n" + block + "```\n\n" +
		"```bash\nputnami upgrade --global\n```\n"
	if err := os.WriteFile(page, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return page
}

// firstUseStub is the installed "binary": it re-executes this test binary as
// the CLI helper, with the scenario and the channel the helper must observe.
func firstUseStub(t *testing.T, scenario, channel string) []byte {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []byte("#!/bin/sh\n" +
		firstUseHelperEnv + "=1 " +
		firstUseScenarioEnv + "=" + shellQuote(scenario) + " " +
		firstUseChannelEnv + "=" + shellQuote(channel) + " " +
		"exec " + shellQuote(executable) + " -test.run='^TestFirstUseCLIHelperProcess$' -- \"$@\"\n")
}

func runFirstUse(t *testing.T, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command("bash", append([]string{firstUseScriptPath(t)}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + cmd.Dir,
		// The host-mode legs must not inherit any of these.
		"PUTNAMI_OUTPUT=must-not-reach-the-legs",
		"PUTNAMI_HOME=must-not-reach-the-legs",
		"BUN_INSTALL=must-not-reach-the-legs",
	}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run smoke-first-use.sh: %v\n%s", err, out)
		}
	}
	return result{exitCode: code, output: string(out)}
}

// systemPathWithout returns a directory that links every command of the
// system directories except the named ones. A PATH made of it alone is a
// machine that holds the system tools and none of the excluded ones, wherever
// the host installed them.
func systemPathWithout(t *testing.T, excluded ...string) string {
	t.Helper()
	dir := t.TempDir()
	skip := map[string]bool{}
	for _, name := range excluded {
		skip[name] = true
	}
	for _, system := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		entries, err := os.ReadDir(system)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if skip[entry.Name()] || entry.IsDir() {
				continue
			}
			if err := os.Symlink(filepath.Join(system, entry.Name()), filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrExist) {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// bareTools are the tools the first-use path must be proven without.
var bareTools = []string{"bun", "git", "node", "go"}

// runFirstUseOnHost runs the legs in host mode against a registry that serves
// the real installer and the stub CLI. The legs see the system tools and none
// of the bare tools, so every check of the home leg applies.
func runFirstUseOnHost(t *testing.T, scenario, channel string) result {
	t.Helper()
	requireBash(t)
	body := firstUseStub(t, scenario, channel)
	server := newRegistry(t, registryOptions{
		body:      body,
		integrity: "sha256:" + sha256Hex(body),
		resolved:  stubVersion,
	})
	return runFirstUse(t, []string{
		"SMOKE_FIRST_USE_MODE=host",
		"SMOKE_FIRST_USE_PATH=" + systemPathWithout(t, bareTools...),
		"SMOKE_FIRST_USE_PAGE=" + firstUsePage(t, firstUseBlock),
		"SMOKE_INSTALL_URL=" + server.URL + "/install.sh",
		"PUTNAMI_REGISTRY_URL=" + server.URL,
		"SMOKE_STARTUP_TIMEOUT=60",
	}, channel)
}

// TestFirstUseCLIHelperProcess is re-executed through the installed stub. It
// accepts only the argv of the first-use path, from the directory and with the
// environment that path gives them, and serves a real HTTP listener after
// printing the ready record `putnami serve` prints.
func TestFirstUseCLIHelperProcess(t *testing.T) {
	if os.Getenv(firstUseHelperEnv) != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	scenario := os.Getenv(firstUseScenarioEnv)
	home := os.Getenv("HOME")
	refuse := func(format string, values ...any) {
		fmt.Fprintf(os.Stderr, "first-use helper: "+format+"\n", values...)
		os.Exit(70)
	}
	failWhen := func(name string) {
		if scenario == name {
			fmt.Fprintf(os.Stderr, "stub cli: %s\n", name)
			os.Exit(1)
		}
	}
	inDirectory := func(name string) string {
		directory, err := os.Getwd()
		if err != nil {
			refuse("getwd: %v", err)
		}
		if filepath.Base(directory) != name || filepath.Dir(directory) != evalSymlinks(home) && filepath.Dir(directory) != home {
			refuse("%q ran in %s, want %s under the home directory", args, directory, name)
		}
		return directory
	}
	touchExecutable := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			refuse("%v", err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			refuse("%v", err)
		}
	}
	serve := func(statuses map[string]int) {
		if _, err := os.Stat("putnami.workspace.json"); err != nil {
			fmt.Fprintln(os.Stderr, "stub cli: no workspace here")
			os.Exit(1)
		}
		if got := os.Getenv("PUTNAMI_OUTPUT"); got != "jsonl" {
			refuse("serve ran with PUTNAMI_OUTPUT=%q, want jsonl", got)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			refuse("%v", err)
		}
		_, _ = os.Stdout.WriteString(smokeReadyRecord(listener.Addr().(*net.TCPAddr).Port) + "\n")
		_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			status, known := statuses[r.URL.Path]
			if !known {
				status = http.StatusNotFound
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte("<!doctype html><title>Welcome to Putnami</title>"))
		}))
		os.Exit(0)
	}
	requireHumanOutput := func() {
		if got, set := os.LookupEnv("PUTNAMI_OUTPUT"); set {
			refuse("%q ran with PUTNAMI_OUTPUT=%q, want it unset", args, got)
		}
	}

	if got := os.Getenv("PUTNAMI_TELEMETRY"); got != "off" && strings.Join(args, " ") != "--version" {
		refuse("%q ran with PUTNAMI_TELEMETRY=%q, want off", args, got)
	}
	for _, name := range []string{"PUTNAMI_HOME", "BUN_INSTALL"} {
		if got, set := os.LookupEnv(name); set {
			refuse("the caller's %s=%q reached %q", name, got, args)
		}
	}
	if channel := os.Getenv(firstUseChannelEnv); channel != "latest" {
		if got := os.Getenv("PUTNAMI_VERSION"); got != channel {
			refuse("%q ran with PUTNAMI_VERSION=%q, want %q", args, got, channel)
		}
		if got := os.Getenv("PUTNAMI_NO_RELAUNCH"); got != "1" {
			refuse("%q ran with PUTNAMI_NO_RELAUNCH=%q, want 1", args, got)
		}
	} else if got, set := os.LookupEnv("PUTNAMI_VERSION"); set {
		refuse("%q ran with PUTNAMI_VERSION=%q on the public channel, want it unset", args, got)
	}

	switch strings.Join(args, "\x00") {
	case "--version":
		_, _ = os.Stdout.WriteString("Putnami   " + stubVersion + "\n")
	case "init\x00--project\x00webapp\x00--extension\x00ts":
		directory := inDirectory("first-use")
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			refuse("init ran in a directory that is not empty: %v %v", entries, err)
		}
		failWhen("init-fails")
		if err := os.WriteFile("putnami.workspace.json", []byte("{}\n"), 0o644); err != nil {
			refuse("%v", err)
		}
		switch scenario {
		case "bun-outside-home":
			touchExecutable(filepath.Join(home, ".bun", "bin", "bun"))
			touchExecutable(filepath.Join(home, ".putnami", "toolchains", "bun", "bun-1.4.2", "bin", "bun"))
		case "no-bun-under-home":
		default:
			touchExecutable(filepath.Join(home, ".putnami", "toolchains", "bun", "bun-1.4.2", "bin", "bun"))
		}
	case "serve\x00webapp":
		inDirectory("first-use")
		statuses := map[string]int{"/": http.StatusOK, "/about": http.StatusOK, "/guestbook": http.StatusOK}
		if scenario == "page-404" {
			delete(statuses, "/guestbook")
		}
		serve(statuses)
	case "lint,test,build\x00webapp":
		inDirectory("first-use")
		requireHumanOutput()
		failWhen("check-fails")
	case "init\x00--project\x00api\x00--extension\x00go":
		directory := inDirectory("first-use-go")
		requireHumanOutput()
		failWhen("go-init-fails")
		if err := os.WriteFile("putnami.workspace.json", []byte("{}\n"), 0o644); err != nil {
			refuse("%v", err)
		}
		switch scenario {
		case "go-in-workspace":
			touchExecutable(filepath.Join(directory, ".putnami", "extensions", "@putnami-go", "libs", "go-1.25.7", "go", "bin", "go"))
			touchExecutable(filepath.Join(home, ".putnami", "toolchains", "go", "go-1.25.7", "go", "bin", "go"))
		case "no-go-under-home":
		default:
			touchExecutable(filepath.Join(home, ".putnami", "toolchains", "go", "go-1.25.7", "go", "bin", "go"))
		}
	case "serve\x00api":
		inDirectory("first-use-go")
		status := http.StatusOK
		if scenario == "go-page-500" {
			status = http.StatusInternalServerError
		}
		serve(map[string]int{"/": status})
	case "lint,test,build\x00api":
		inDirectory("first-use-go")
		requireHumanOutput()
		failWhen("go-check-fails")
	default:
		refuse("unexpected first-use argv: %q", args)
	}
	os.Exit(0)
}

func evalSymlinks(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// The smoke runs the block of the page as printed, reaches `putnami` through
// the block's own export line, then runs the checks and the Go path.
func TestFirstUseSmokeRunsTheDocumentedBlockThenTheChecksAndTheGoPath(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "first-use-smoke-legs", "the-smoke-runs-the-documented-block-then-the-checks-and-the-go-path")
	res := runFirstUseOnHost(t, "", "latest")
	if res.exitCode != 0 {
		t.Fatalf("first-use smoke failed (exit %d):\n%s", res.exitCode, res.output)
	}
	for _, want := range []string{
		"smoke:   export PATH=\"$HOME/.putnami/bin:$PATH\"",
		"smoke:   putnami init --project webapp --extension ts",
		"smoke:   putnami serve webapp",
		"/ answered 200",
		"/about answered 200",
		"/guestbook answered 200",
		"the documented block installed, initialized and served webapp; the starter stopped cleanly",
		"smoke: putnami lint,test,build webapp",
		"smoke: putnami init --project api --extension go",
		"smoke: putnami serve api",
		"smoke: putnami lint,test,build api",
		"toolchains/bun/bun-1.4.2/bin/bun is installed under the Putnami home",
		"toolchains/go/go-1.25.7/go/bin/go is installed under the Putnami home",
		"smoke: OK — channel 'latest' passes the documented block",
	} {
		if !res.contains(want) {
			t.Fatalf("first-use smoke did not report %q:\n%s", want, res.output)
		}
	}
}

// A candidate channel reaches the installer and the CLI through the
// environment; the commands keep their public form.
func TestFirstUseSmokeSelectsACandidateChannelThroughTheEnvironment(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "first-use-smoke-legs", "a-candidate-channel-is-selected-through-the-environment")
	res := runFirstUseOnHost(t, "", "canary")
	if res.exitCode != 0 {
		t.Fatalf("first-use smoke failed on a candidate channel (exit %d):\n%s", res.exitCode, res.output)
	}
	if !res.contains("smoke: OK — channel 'canary' passes the documented block") {
		t.Fatalf("first-use smoke did not name the candidate channel:\n%s", res.output)
	}
}

// Each leg fails the release by name, with a non-zero exit status.
func TestFirstUseSmokeFailsUnderTheLegThatBroke(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "first-use-smoke-legs", "the-smoke-fails-under-the-leg-that-broke")
	for _, tc := range []struct {
		scenario string
		want     string
	}{
		{"init-fails", "::error::block leg: the commands ended before the starter was ready"},
		{"page-404", "/guestbook answered HTTP 404; expected 200"},
		{"check-fails", "::error::check leg: 'putnami lint,test,build webapp' failed"},
		{"go-init-fails", "::error::go-init leg: 'putnami init --project api --extension go' failed"},
		{"go-page-500", "::error::go-pages leg:"},
		{"go-check-fails", "::error::go-check leg: 'putnami lint,test,build api' failed"},
		{"bun-outside-home", "/.bun exists; Bun must be installed under"},
		{"no-bun-under-home", "::error::home leg: no Bun under"},
		{"no-go-under-home", "::error::home leg: no Go under"},
		{"go-in-workspace", "is a copy of Go inside a workspace"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			t.Parallel()
			res := runFirstUseOnHost(t, tc.scenario, "latest")
			if res.exitCode == 0 {
				t.Fatalf("first-use smoke passed scenario %s:\n%s", tc.scenario, res.output)
			}
			if !res.contains(tc.want) {
				t.Fatalf("first-use smoke did not report %q:\n%s", tc.want, res.output)
			}
			if res.contains("smoke: OK") {
				t.Fatalf("first-use smoke reported OK for scenario %s:\n%s", tc.scenario, res.output)
			}
		})
	}
}

// The block is the first bash block of the page, printed unchanged; a page
// without one, or whose block does not install and then serve, is refused
// before anything runs.
func TestFirstUseSmokeReadsTheFirstBashBlockOfThePage(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "first-use-smoke-machine", "the-block-is-the-first-bash-block-of-the-page")
	requireBash(t)
	res := runFirstUse(t, []string{"SMOKE_FIRST_USE_PAGE=" + firstUsePage(t, firstUseBlock)}, "--print-block")
	if res.exitCode != 0 || res.output != firstUseBlock {
		t.Fatalf("--print-block = exit %d %q, want %q", res.exitCode, res.output, firstUseBlock)
	}

	for name, tc := range map[string]struct {
		page string
		want string
	}{
		"no bash block": {
			page: func() string {
				page := filepath.Join(t.TempDir(), "index.md")
				if err := os.WriteFile(page, []byte("# Getting Started\n\n```text\nnothing\n```\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return page
			}(),
			want: "::error::block leg: ",
		},
		"no installer": {
			page: firstUsePage(t, "putnami init --project webapp --extension ts\nputnami serve webapp\n"),
			want: "does not install with 'curl -fsSL https://putnami.dev/install.sh | bash'",
		},
		"no serve at the end": {
			page: firstUsePage(t, "curl -fsSL https://putnami.dev/install.sh | bash\nputnami init --project webapp --extension ts\n"),
			want: "does not end with 'putnami serve <project>'",
		},
		"missing page": {
			page: filepath.Join(t.TempDir(), "absent.md"),
			want: "::error::block leg: ",
		},
	} {
		res := runFirstUse(t, []string{"SMOKE_FIRST_USE_MODE=host", "SMOKE_FIRST_USE_PAGE=" + tc.page})
		if res.exitCode == 0 || !res.contains(tc.want) {
			t.Fatalf("%s: exit %d, want a failure naming %q:\n%s", name, res.exitCode, tc.want, res.output)
		}
	}
}

// recordingDocker writes a `docker` that records its build context and its run
// arguments under record, and returns the directory that holds it.
func recordingDocker(t *testing.T, record string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  build)\n" +
		"    for last in \"$@\"; do :; done\n" +
		"    cp -R \"$last\" " + shellQuote(filepath.Join(record, "context")) + "\n" +
		"    echo sha256:recorded\n" +
		"    ;;\n" +
		"  run) printf '%s\\n' \"$@\" > " + shellQuote(filepath.Join(record, "run-args")) + " ;;\n" +
		"  rmi) printf '%s\\n' \"$@\" > " + shellQuote(filepath.Join(record, "rmi-args")) + " ;;\n" +
		"  *) exit 64 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Image mode builds an image that adds only curl and its certificates to the
// base image, runs the legs in it as a non-root user, and removes it.
func TestFirstUseSmokeBuildsAnImageThatHoldsOnlyWhatTheInstallerNeeds(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "first-use-smoke-machine", "the-image-holds-only-what-the-installer-needs")
	requireBash(t)
	record := t.TempDir()
	page := firstUsePage(t, firstUseBlock)
	res := runFirstUse(t, []string{
		"PATH=" + recordingDocker(t, record) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"SMOKE_FIRST_USE_PAGE=" + page,
	}, "canary")
	if res.exitCode != 0 {
		t.Fatalf("image mode failed with a recording docker (exit %d):\n%s", res.exitCode, res.output)
	}

	dockerfile, err := os.ReadFile(filepath.Join(record, "context", "Dockerfile"))
	if err != nil {
		t.Fatalf("docker build received no Dockerfile: %v\n%s", err, res.output)
	}
	var installed []string
	for line := range strings.SplitSeq(string(dockerfile), "\n") {
		if _, packages, found := strings.Cut(line, "apt-get install -y --no-install-recommends "); found {
			installed = append(installed, strings.Fields(strings.TrimSuffix(strings.TrimSpace(packages), "\\"))...)
		}
	}
	if strings.Join(installed, " ") != "ca-certificates curl" {
		t.Fatalf("the bare image installs %q, want only ca-certificates and curl:\n%s", installed, dockerfile)
	}
	for _, want := range []string{"FROM ubuntu:24.04\n", "\nUSER dev\n", "COPY smoke-first-use.sh page.md /smoke/\n"} {
		if !strings.Contains(string(dockerfile), want) {
			t.Fatalf("the bare image's Dockerfile has no %q:\n%s", want, dockerfile)
		}
	}
	if runs := strings.Count(string(dockerfile), "\nRUN "); runs != 2 {
		t.Fatalf("the bare image's Dockerfile runs %d commands, want the package install and the user creation:\n%s", runs, dockerfile)
	}
	for _, name := range []string{"smoke-first-use.sh", "page.md"} {
		source := firstUseScriptPath(t)
		if name == "page.md" {
			source = page
		}
		want, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(record, "context", name))
		if err != nil || string(got) != string(want) {
			t.Fatalf("the image does not carry %s byte for byte: %v", name, err)
		}
	}

	runArgs, err := os.ReadFile(filepath.Join(record, "run-args"))
	if err != nil {
		t.Fatalf("docker run was not called: %v\n%s", err, res.output)
	}
	args := strings.Split(strings.TrimSpace(string(runArgs)), "\n")
	if got := strings.Join(args[len(args)-4:], " "); got != "sha256:recorded bash /smoke/smoke-first-use.sh canary" {
		t.Fatalf("docker run ends with %q, want the built image running the smoke on the channel", got)
	}
	for _, want := range []string{"--rm", "SMOKE_FIRST_USE_INSIDE=image", "SMOKE_FIRST_USE_PAGE=/smoke/page.md"} {
		found := false
		for _, arg := range args {
			found = found || arg == want
		}
		if !found {
			t.Fatalf("docker run has no argument %q: %q", want, args)
		}
	}
	for _, arg := range args {
		if arg == "-v" || arg == "--volume" || arg == "--privileged" || strings.HasPrefix(arg, "--mount") {
			t.Fatalf("docker run shares host state with the bare image through %q: %q", arg, args)
		}
	}
	if rmi, err := os.ReadFile(filepath.Join(record, "rmi-args")); err != nil || !strings.Contains(string(rmi), "sha256:recorded") {
		t.Fatalf("the built image was not removed: %v %q", err, rmi)
	}
}

// Inside the image, the smoke refuses a machine that is not bare before it
// runs anything: a tool the path must be proven without, or a PATH directory
// the user can write to. Without Docker, image mode names the host mode.
func TestFirstUseSmokeRefusesAnImageThatIsNotBare(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "first-use-smoke-machine", "the-smoke-refuses-an-image-that-is-not-bare")
	requireBash(t)
	page := firstUsePage(t, firstUseBlock)
	stub := func(dir, name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, held := range bareTools {
		path := systemPathWithout(t, bareTools...)
		stub(path, held)
		res := runFirstUse(t, []string{
			"PATH=" + path,
			"SMOKE_FIRST_USE_INSIDE=image",
			"SMOKE_FIRST_USE_PAGE=" + page,
		})
		want := "::error::bare leg: the image holds " + held + ";"
		if res.exitCode == 0 || !res.contains(want) {
			t.Fatalf("exit %d, want a failure naming %q:\n%s", res.exitCode, want, res.output)
		}
	}

	if os.Getuid() != 0 {
		writable := systemPathWithout(t, bareTools...)
		res := runFirstUse(t, []string{
			"PATH=" + writable,
			"SMOKE_FIRST_USE_INSIDE=image",
			"SMOKE_FIRST_USE_PAGE=" + page,
		})
		want := "::error::bare leg: " + writable + " is on PATH and writable by the user"
		if res.exitCode == 0 || !res.contains(want) {
			t.Fatalf("exit %d, want a failure naming %q:\n%s", res.exitCode, want, res.output)
		}
	}

	res := runFirstUse(t, []string{
		"PATH=" + systemPathWithout(t, "docker"),
		"SMOKE_FIRST_USE_PAGE=" + page,
	})
	want := "::error::image leg: Docker is required"
	if res.exitCode == 0 || !res.contains(want) || !res.contains("SMOKE_FIRST_USE_MODE=host") {
		t.Fatalf("exit %d, want a failure naming %q and the host mode:\n%s", res.exitCode, want, res.output)
	}
}
