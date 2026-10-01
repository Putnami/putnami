package installscript

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The run form of the installer: `curl -fsSL "https://putnami.dev/install.sh?run=<command>" | bash`
// or `... | bash -s -- --run <command>`. The script resolves which extension
// provides <command> from the command map published next to it, installs the
// CLI exactly as without a command, pins that extension for the user through
// the installed CLI, and runs `putnami <command>` in the caller's directory.
//
// These tests drive the real script against one local server that plays the
// site (install.sh with the ?run= substitution, and the command map) and the
// registry (the CLI download). The CLI is a stub that records every invocation
// to a log outside the caller's directory.

// runPlaceholderLine is the one line of install.sh the site rewrites for
// ?run=<command>. sites/putnami.dev pins the same literal.
const runPlaceholderLine = `RUN_COMMAND_DEFAULT=""`

const commandMapHeader = "putnami.install-commands.v1"

// stdinSentinel follows the script on the installer's stdin. A command that
// read the installer's stdin would record it; the command must record EOF.
const stdinSentinel = "# the installer's stdin, which the command must never read"

// siteRunCommandPattern is the rule the site applies to ?run= before it sets the
// placeholder. The script holds --run, PUTNAMI_RUN, and the baked value to the
// same rule.
var siteRunCommandPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// runStubCLI stands in for the published binary. It answers --version with the
// version the registry resolves, records `extensions ...` (the pin) and exits
// PUTNAMI_TEST_PIN_EXIT, and records every other invocation (the command) with
// its argv, physical working directory, and what its stdin held, prints one
// line to stdout, and exits PUTNAMI_TEST_COMMAND_EXIT.
func runStubCLI() []byte {
	return []byte(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo "Putnami   ` + stubVersion + `"
  exit 0
fi
log="${PUTNAMI_TEST_RUN_LOG:?PUTNAMI_TEST_RUN_LOG is not set}"
if [ "$1" = "extensions" ]; then
  printf 'pin argv=%s\n' "$*" >> "$log"
  exit "${PUTNAMI_TEST_PIN_EXIT:-0}"
fi
if IFS= read -r line; then stdin="read:$line"; else stdin="eof"; fi
printf 'run argv=%s\n' "$*" >> "$log"
printf 'run cwd=%s\n' "$(pwd -P)" >> "$log"
printf 'run stdin=%s\n' "$stdin" >> "$log"
echo "output of putnami $*"
exit "${PUTNAMI_TEST_COMMAND_EXIT:-0}"
`)
}

// bakeRunCommand does to install.sh what the site does for ?run=<command>:
// it sets the value of the one placeholder line and changes nothing else.
func bakeRunCommand(script []byte, command string) ([]byte, error) {
	lines := strings.Split(string(script), "\n")
	found := -1
	for i, line := range lines {
		if line != runPlaceholderLine {
			continue
		}
		if found >= 0 {
			return nil, fmt.Errorf("install.sh carries the run placeholder on lines %d and %d; the site substitutes exactly one", found+1, i+1)
		}
		found = i
	}
	if found < 0 {
		return nil, fmt.Errorf("install.sh carries no %q line for the site to substitute", runPlaceholderLine)
	}
	lines[found] = `RUN_COMMAND_DEFAULT="` + command + `"`
	return []byte(strings.Join(lines, "\n")), nil
}

// serveInstallScript answers /install.sh the way the site does: the script as
// is without ?run=, the script with the command baked in for one valid run=
// value, and 400 for anything else.
func serveInstallScript(w http.ResponseWriter, r *http.Request, installer []byte) {
	w.Header().Set("Content-Type", "text/x-shellscript")
	runs, asked := r.URL.Query()["run"]
	if !asked {
		_, _ = w.Write(installer)
		return
	}
	if len(runs) != 1 || !siteRunCommandPattern.MatchString(runs[0]) {
		http.Error(w, "invalid run command", http.StatusBadRequest)
		return
	}
	baked, err := bakeRunCommand(installer, runs[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(baked)
}

type runSiteOptions struct {
	commandMap string // body of /install-commands.txt
	mapStatus  int    // status of /install-commands.txt; defaults to 200
	cli        []byte // the CLI the registry serves; defaults to runStubCLI()
}

// runSite is one local origin serving what the run form fetches: install.sh,
// with the site's ?run= substitution; the command map; and the CLI download,
// with a matching digest and version stamp. It counts requests per path.
type runSite struct {
	*httptest.Server
	mu       sync.Mutex
	requests map[string]int
}

func (s *runSite) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[path]
}

func (s *runSite) mapURL() string { return s.URL + "/install-commands.txt" }

func newRunSite(t *testing.T, opts runSiteOptions) *runSite {
	t.Helper()
	installer, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatalf("read install script: %v", err)
	}
	cli := opts.cli
	if cli == nil {
		cli = runStubCLI()
	}
	site := &runSite{requests: map[string]int{}}
	site.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.mu.Lock()
		site.requests[r.URL.Path]++
		site.mu.Unlock()
		switch r.URL.Path {
		case "/install.sh":
			serveInstallScript(w, r, installer)
		case "/install-commands.txt":
			// Announce the size, as a static file server does, so the
			// installer's size limit applies on every curl version.
			w.Header().Set("Content-Length", strconv.Itoa(len(opts.commandMap)))
			if opts.mapStatus != 0 {
				w.WriteHeader(opts.mapStatus)
			}
			_, _ = w.Write([]byte(opts.commandMap))
		case "/putnami/cli/download":
			w.Header().Set("X-Integrity", "sha256:"+sha256Hex(cli))
			w.Header().Set("X-Resolved-Version", stubVersion)
			_, _ = w.Write(cli)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(site.Close)
	return site
}

// commandMap renders a well-formed map with the given entry lines.
func commandMap(entries ...string) string {
	return commandMapHeader + "\n# test map\n\n" + strings.Join(entries, "\n") + "\n"
}

// runEnv is the hermetic installer environment plus the caller's directory: a
// fresh `git init` directory outside HOME, so git itself can prove nothing was
// written to it, and a CLI log that lives outside it.
type runEnv struct {
	*env
	callerDir string
	cliLog    string
}

func requireGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	return git
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(requireGit(t), args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func newRunEnv(t *testing.T, site *runSite) *runEnv {
	t.Helper()
	requireBash(t)
	root := t.TempDir()
	callerDir := filepath.Join(root, "caller")
	if err := os.MkdirAll(callerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, callerDir, "init", "-q")
	cliLog := filepath.Join(root, "cli-calls.log")
	e := newEnv(t).
		set("PUTNAMI_REGISTRY_URL", site.URL).
		set("PUTNAMI_COMMAND_MAP_URL", site.mapURL()).
		set("PUTNAMI_TEST_RUN_LOG", cliLog)
	return &runEnv{env: e, callerDir: callerDir, cliLog: cliLog}
}

type runOutcome struct {
	exitCode int
	stdout   string
	stderr   string
}

func (o runOutcome) transcript() string {
	return "--- stdout ---\n" + o.stdout + "--- stderr ---\n" + o.stderr
}

// invoke runs the installer from the caller's directory, the way a user runs
// it. With piped set, the script arrives on stdin (`curl ... | bash -s -- args`);
// otherwise bash runs the file. Either way the installer's stdin carries
// stdinSentinel after anything else, and the process has no controlling
// terminal, so /dev/tty cannot be opened and the command's stdin must be
// /dev/null.
func (r *runEnv) invoke(t *testing.T, piped []byte, args ...string) runOutcome {
	t.Helper()
	var cmd *exec.Cmd
	stdin := stdinSentinel + "\n"
	if piped != nil {
		cmd = exec.Command("bash", append([]string{"-s", "--"}, args...)...)
		stdin = string(piped) + "\n" + stdin
	} else {
		cmd = exec.Command("bash", append([]string{installScriptPath(t)}, args...)...)
	}
	cmd.Dir = r.callerDir
	cmd.Env = r.materialize()
	cmd.Stdin = strings.NewReader(stdin)
	withoutControllingTerminal(t, cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run install.sh: %v\n%s%s", err, stdout.String(), stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return runOutcome{exitCode: code, stdout: stdout.String(), stderr: stderr.String()}
}

// cliCalls returns the stub CLI's log, one recorded fact per line.
func (r *runEnv) cliCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.cliLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// assertCallerDirUntouched proves nothing was written to the caller's
// directory.
func (r *runEnv) assertCallerDirUntouched(t *testing.T) {
	t.Helper()
	assertGitDirUntouched(t, r.callerDir)
}

// assertGitDirUntouched proves nothing was written to dir, a directory the test
// created with `git init`: it holds only .git, and git reports no change,
// untracked file, or ignored file.
func assertGitDirUntouched(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".git" {
			t.Fatalf("the installer wrote %s into the caller's directory", entry.Name())
		}
	}
	if status := gitCommand(t, dir, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
		t.Fatalf("the caller's directory changed:\n%s", status)
	}
}

// assertRefusedBeforeInstalling proves a refusal happened before the CLI was
// downloaded and before anything was written: no binary, no ~/.putnami, and no
// CLI invocation at all.
func (r *runEnv) assertRefusedBeforeInstalling(t *testing.T, site *runSite, out runOutcome) {
	t.Helper()
	if out.exitCode == 0 {
		t.Fatalf("the run was not refused:\n%s", out.transcript())
	}
	if got := site.count("/putnami/cli/download"); got != 0 {
		t.Fatalf("the CLI was downloaded %d time(s) before the refusal:\n%s", got, out.transcript())
	}
	r.assertNothingInstalled(t)
	if _, err := os.Stat(filepath.Join(r.home, ".putnami")); err == nil {
		t.Fatalf("a refused run created %s:\n%s", filepath.Join(r.home, ".putnami"), out.transcript())
	}
	if calls := r.cliCalls(t); len(calls) != 0 {
		t.Fatalf("a refused run invoked the CLI: %q", calls)
	}
	if out.stdout != "" {
		t.Fatalf("a refused run wrote to stdout, which belongs to the command:\n%s", out.transcript())
	}
	r.assertCallerDirUntouched(t)
	r.assertNeverEscalated(t)
}

func physicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestInstallScriptCarriesExactlyOneRunPlaceholder(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	baked, err := bakeRunCommand(script, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	before := strings.Split(string(script), "\n")
	after := strings.Split(string(baked), "\n")
	if len(before) != len(after) {
		t.Fatalf("baking a command changed the line count from %d to %d", len(before), len(after))
	}
	changed := 0
	for i := range before {
		if before[i] != after[i] {
			changed++
			if after[i] != `RUN_COMMAND_DEFAULT="deploy"` {
				t.Fatalf("baking a command changed line %d to %q", i+1, after[i])
			}
		}
	}
	if changed != 1 {
		t.Fatalf("baking a command changed %d lines, want exactly the placeholder", changed)
	}
}

func TestRunResolvesPinsAndRunsTheCommandInTheCallerDirectory(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap(
		"lint-docs @acme/docs",
		"deploy @acme/deploy@^1.2.0",
	)})
	r := newRunEnv(t, site)

	out := r.invoke(t, nil, "--run", "deploy")
	if out.exitCode != 0 {
		t.Fatalf("run failed (exit %d):\n%s", out.exitCode, out.transcript())
	}

	want := []string{
		"pin argv=extensions install --user --latest @acme/deploy@^1.2.0",
		"run argv=deploy",
		"run cwd=" + physicalPath(t, r.callerDir),
		"run stdin=eof",
	}
	if got := r.cliCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("CLI calls = %q, want %q\n%s", got, want, out.transcript())
	}

	// stdout belongs to the command; everything the installer says is on stderr.
	if out.stdout != "output of putnami deploy\n" {
		t.Fatalf("stdout carries more than the command's output:\n%s", out.transcript())
	}
	for _, line := range []string{
		"putnami deploy is provided by @acme/deploy@^1.2.0",
		"Integrity verified (sha256:" + sha256Hex(runStubCLI()) + ")",
		"Version stamp verified (" + stubVersion + ")",
		"Pinned @acme/deploy@^1.2.0",
		"Running putnami deploy in " + physicalPath(t, r.callerDir),
	} {
		if !strings.Contains(out.stderr, line) {
			t.Fatalf("stderr does not report %q:\n%s", line, out.transcript())
		}
	}
	if strings.Contains(out.stderr+out.stdout, "Next:") {
		t.Fatalf("run mode printed the install footer:\n%s", out.transcript())
	}
	if strings.Contains(out.stderr+out.stdout, "\x1b[") {
		t.Fatalf("piped run output contains ANSI escapes:\n%q", out.stderr)
	}

	// The CLI is installed exactly as without a command.
	if _, err := os.Stat(filepath.Join(r.home, ".putnami", "bin", "putnami")); err != nil {
		t.Fatalf("the CLI was not installed: %v\n%s", err, out.transcript())
	}
	r.assertCallerDirUntouched(t)
	r.assertNeverEscalated(t)
}

func TestRunExitsWithTheCommandsExitStatus(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap("deploy @acme/deploy")})
	r := newRunEnv(t, site)
	r.set("PUTNAMI_TEST_COMMAND_EXIT", "7")

	out := r.invoke(t, nil, "--run", "deploy")
	if out.exitCode != 7 {
		t.Fatalf("exit = %d, want the command's 7:\n%s", out.exitCode, out.transcript())
	}
	if out.stdout != "output of putnami deploy\n" {
		t.Fatalf("the failing command's output did not reach stdout:\n%s", out.transcript())
	}
	r.assertCallerDirUntouched(t)
}

func TestRunDoesNotRunTheCommandWhenThePinFails(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap("deploy @acme/deploy")})
	r := newRunEnv(t, site)
	r.set("PUTNAMI_TEST_PIN_EXIT", "5")

	out := r.invoke(t, nil, "--run", "deploy")
	if out.exitCode != 5 {
		t.Fatalf("exit = %d, want the pin's 5:\n%s", out.exitCode, out.transcript())
	}
	if got := r.cliCalls(t); len(got) != 1 || got[0] != "pin argv=extensions install --user --latest @acme/deploy" {
		t.Fatalf("the command ran after a failed pin: %q", got)
	}
	if !strings.Contains(out.stderr, "putnami extensions install --user --latest @acme/deploy failed (exit 5); putnami deploy was not run") {
		t.Fatalf("the failed pin was not named:\n%s", out.transcript())
	}
	if out.stdout != "" {
		t.Fatalf("a failed pin wrote to stdout:\n%s", out.transcript())
	}
	r.assertCallerDirUntouched(t)
}

// The public entry point pipes the script into bash, so bash reads the script
// from the same stdin a child would inherit.
func TestRunWorksWhenPipedIntoBash(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap("deploy @acme/deploy")})
	r := newRunEnv(t, site)
	script, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}

	out := r.invoke(t, script, "--run", "deploy")
	if out.exitCode != 0 {
		t.Fatalf("piped run failed (exit %d):\n%s", out.exitCode, out.transcript())
	}
	want := []string{
		"pin argv=extensions install --user --latest @acme/deploy",
		"run argv=deploy",
		"run cwd=" + physicalPath(t, r.callerDir),
		"run stdin=eof",
	}
	if got := r.cliCalls(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("CLI calls = %q, want %q\n%s", got, want, out.transcript())
	}
	if out.stdout != "output of putnami deploy\n" {
		t.Fatalf("stdout carries more than the command's output:\n%s", out.transcript())
	}
	r.assertCallerDirUntouched(t)
	r.assertNeverEscalated(t)
}

// /install.sh?run=<command> serves the script with its placeholder set, and
// `curl ... | bash` then runs it with no argument at all.
func TestRunUsesTheCommandTheSiteBakedIn(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap(
		"deploy @acme/deploy",
		"preview @acme/preview",
	)})
	served := func(t *testing.T, command string) []byte {
		t.Helper()
		res, err := http.Get(site.URL + "/install.sh?run=" + command)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body bytes.Buffer
		if _, err := body.ReadFrom(res.Body); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET /install.sh?run=%s: %d %s", command, res.StatusCode, body.String())
		}
		return body.Bytes()
	}

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		wantRun string
	}{
		{name: "baked value alone", wantRun: "deploy"},
		{name: "--run wins over the baked value", args: []string{"--run", "preview"}, wantRun: "preview"},
		{name: "PUTNAMI_RUN wins over the baked value", env: map[string]string{"PUTNAMI_RUN": "preview"}, wantRun: "preview"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRunEnv(t, site)
			for k, v := range tc.env {
				r.set(k, v)
			}
			out := r.invoke(t, served(t, "deploy"), tc.args...)
			if out.exitCode != 0 {
				t.Fatalf("baked run failed (exit %d):\n%s", out.exitCode, out.transcript())
			}
			calls := r.cliCalls(t)
			if len(calls) < 2 || calls[1] != "run argv="+tc.wantRun {
				t.Fatalf("CLI calls = %q, want putnami %s", calls, tc.wantRun)
			}
			if out.stdout != "output of putnami "+tc.wantRun+"\n" {
				t.Fatalf("stdout = %q, want the output of putnami %s", out.stdout, tc.wantRun)
			}
			r.assertCallerDirUntouched(t)
		})
	}
}

func TestRunRefusesACommandTheMapDoesNotList(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap("deploy @acme/deploy")})
	r := newRunEnv(t, site)

	out := r.invoke(t, nil, "--run", "depoly")
	r.assertRefusedBeforeInstalling(t, site, out)
	if !strings.Contains(out.stderr, "putnami depoly is not a command the installer can run: "+site.mapURL()+" does not list it") {
		t.Fatalf("the refusal does not name the command and the map:\n%s", out.transcript())
	}
	if !strings.Contains(out.stderr, "Nothing was installed") {
		t.Fatalf("the refusal does not say nothing was installed:\n%s", out.transcript())
	}
}

// shippedCommands lists every command the shipped map provides, with the
// extension reference on its line. A change to install-commands.txt updates
// this list in the same change.
var shippedCommands = map[string]string{
	"agent-readiness": "@putnami/intelligence",
}

// The shipped map is well-formed, lists exactly shippedCommands, and the
// installer pins and runs each of them. A command it does not list is refused.
func TestShippedCommandMapResolvesEveryListedCommand(t *testing.T) {
	t.Parallel()
	shipped, err := os.ReadFile(filepath.Join(filepath.Dir(installScriptPath(t)), "install-commands.txt"))
	if err != nil {
		t.Fatalf("the command map is not shipped next to install.sh: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(shipped), "\n"), "\n")
	if lines[0] != commandMapHeader {
		t.Fatalf("the shipped map starts with %q, want %q", lines[0], commandMapHeader)
	}
	listed := map[string]string{}
	for i, line := range lines[1:] {
		if content := strings.TrimLeft(line, " \t"); content == "" || strings.HasPrefix(content, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("the shipped map line %d is not \"<command> <@scope/name[@constraint]>\": %q", i+2, line)
		}
		listed[fields[0]] = fields[1]
	}
	if !maps.Equal(listed, shippedCommands) {
		t.Fatalf("the shipped map lists %v, want %v", listed, shippedCommands)
	}

	for command, extension := range shippedCommands {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			site := newRunSite(t, runSiteOptions{commandMap: string(shipped)})
			r := newRunEnv(t, site)
			out := r.invoke(t, nil, "--run", command)
			if out.exitCode != 0 {
				t.Fatalf("run failed (exit %d):\n%s", out.exitCode, out.transcript())
			}
			want := []string{
				"pin argv=extensions install --user --latest " + extension,
				"run argv=" + command,
			}
			if got := r.cliCalls(t); len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("CLI calls = %q, want %q first\n%s", got, want, out.transcript())
			}
			r.assertCallerDirUntouched(t)
		})
	}

	// The installer reads the shipped bytes as a well-formed map, so a command
	// it does not list is refused as unlisted, not as a malformed map.
	site := newRunSite(t, runSiteOptions{commandMap: string(shipped)})
	r := newRunEnv(t, site)
	out := r.invoke(t, nil, "--run", "deploy")
	r.assertRefusedBeforeInstalling(t, site, out)
	if !strings.Contains(out.stderr, "does not list it") {
		t.Fatalf("the shipped map did not refuse an unlisted command as unlisted:\n%s", out.transcript())
	}
}

// Blank lines and comments are ignored wherever they sit and however they are
// indented, as the map's documentation says.
func TestRunIgnoresBlankAndIndentedCommentLines(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap(
		"   ",
		"\t",
		"  # an indented comment",
		"\t# a tab-indented comment",
		"deploy @acme/deploy",
		" ",
	)})
	r := newRunEnv(t, site)

	out := r.invoke(t, nil, "--run", "deploy")
	if out.exitCode != 0 {
		t.Fatalf("run failed (exit %d):\n%s", out.exitCode, out.transcript())
	}
	if got := r.cliCalls(t); len(got) == 0 || got[0] != "pin argv=extensions install --user --latest @acme/deploy" {
		t.Fatalf("CLI calls = %q, want the pin of @acme/deploy first\n%s", got, out.transcript())
	}
	r.assertCallerDirUntouched(t)
}

func TestRunFailsClosedOnAMalformedCommandMap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{name: "no header", body: "deploy @acme/deploy\n", want: "is not a command map: its first line is not " + commandMapHeader},
		{name: "another format version", body: "putnami.install-commands.v2\ndeploy @acme/deploy\n", want: "is not a command map"},
		{name: "header with a carriage return", body: commandMapHeader + "\r\ndeploy @acme/deploy\r\n", want: "is not a command map"},
		{name: "empty file", body: "", want: "is empty, not a command map"},
		{name: "one field", body: commandMap("deploy"), want: "line 4 is not \"<command> <@scope/name[@constraint]>\""},
		{name: "three fields", body: commandMap("deploy @acme/deploy extra"), want: "line 4 is not"},
		{name: "indented entry", body: commandMap(" deploy @acme/deploy"), want: "line 4 is not"},
		{name: "uppercase command", body: commandMap("Deploy @acme/deploy"), want: "line 4 names a command that is not"},
		{name: "unscoped extension", body: commandMap("deploy acme/deploy"), want: "line 4 names an extension that is not"},
		{name: "local path extension", body: commandMap("deploy /tmp/extension"), want: "line 4 names an extension that is not"},
		{name: "substitution in the constraint", body: commandMap("deploy @acme/deploy@$(id)"), want: "line 4 names an extension that is not"},
		{name: "entry with a carriage return", body: commandMap("deploy @acme/deploy\r"), want: "line 4 names an extension that is not"},
		{name: "line holding a carriage return only", body: commandMap("\r", "deploy @acme/deploy"), want: "line 4 is not"},
		{name: "command listed twice", body: commandMap("deploy @acme/deploy", "deploy @other/deploy"), want: "lists deploy twice (line 5)"},
		{name: "malformed line after the match", body: commandMap("deploy @acme/deploy", "broken"), want: "line 5 is not"},
		{name: "map not found", body: "not found", status: http.StatusNotFound, want: "Could not download the command map from "},
		// Well-formed and listing the command, but over the 1 MiB limit: only the
		// limit refuses it.
		{name: "map larger than 1 MiB", body: commandMap(strings.Repeat("#\n", 1<<19) + "deploy @acme/deploy"), want: "Could not download the command map from "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			site := newRunSite(t, runSiteOptions{commandMap: tc.body, mapStatus: tc.status})
			r := newRunEnv(t, site)

			out := r.invoke(t, nil, "--run", "deploy")
			r.assertRefusedBeforeInstalling(t, site, out)
			if !strings.Contains(out.stderr, tc.want) {
				t.Fatalf("the refusal does not say %q:\n%s", tc.want, out.transcript())
			}
		})
	}
}

// The map decides which extension gets installed, so it is held to the
// registry's transport rule: https, loopback http, or an explicit opt-in.
func TestRunRefusesAnInsecureCommandMapURL(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap("deploy @acme/deploy")})

	t.Run("plaintext non-loopback", func(t *testing.T) {
		t.Parallel()
		r := newRunEnv(t, site)
		r.set("PUTNAMI_COMMAND_MAP_URL", "http://map.invalid/install-commands.txt")
		out := r.invoke(t, nil, "--run", "deploy")
		r.assertRefusedBeforeInstalling(t, site, out)
		if !strings.Contains(out.stderr, "command map URL \"http://map.invalid/install-commands.txt\" uses plaintext http://") {
			t.Fatalf("the refusal does not name the plaintext map URL:\n%s", out.transcript())
		}
	})

	t.Run("unsupported scheme", func(t *testing.T) {
		t.Parallel()
		r := newRunEnv(t, site)
		r.set("PUTNAMI_COMMAND_MAP_URL", "file:///etc/install-commands.txt")
		out := r.invoke(t, nil, "--run", "deploy")
		r.assertRefusedBeforeInstalling(t, site, out)
		if !strings.Contains(out.stderr, "has unsupported scheme \"file\"") {
			t.Fatalf("the refusal does not name the scheme:\n%s", out.transcript())
		}
	})

	t.Run("plaintext with the explicit opt-in", func(t *testing.T) {
		t.Parallel()
		r := newRunEnv(t, site)
		r.set("PUTNAMI_COMMAND_MAP_URL", "http://map.invalid/install-commands.txt").
			set("PUTNAMI_ALLOW_INSECURE_REGISTRY", "1")
		out := r.invoke(t, nil, "--run", "deploy")
		// The host does not exist: the opt-in moves the failure from the scheme
		// check to the download.
		r.assertRefusedBeforeInstalling(t, site, out)
		if strings.Contains(out.stderr, "plaintext http://") || !strings.Contains(out.stderr, "Could not download the command map") {
			t.Fatalf("the opt-in did not lift the scheme check:\n%s", out.transcript())
		}
	})
}

func TestRunRejectsAnInvalidCommandBeforeAnyNetworkCall(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap("deploy @acme/deploy")})
	script, err := os.ReadFile(installScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		piped []byte
		args  []string
		env   map[string]string
		want  string
	}{
		{name: "uppercase", args: []string{"--run", "Deploy"}},
		{name: "leading dash", args: []string{"--run", "-deploy"}},
		{name: "command separator", args: []string{"--run", "deploy;id"}},
		{name: "command substitution", args: []string{"--run", "$(id)"}},
		{name: "backquotes", args: []string{"--run", "`id`"}},
		{name: "quote", args: []string{"--run", "deploy'"}},
		{name: "newline", args: []string{"--run", "deploy\nid"}},
		{name: "space", env: map[string]string{"PUTNAMI_RUN": "deploy now"}},
		{name: "too long", args: []string{"--run", strings.Repeat("a", 65)}},
		{name: "baked value the site would refuse", piped: []byte(strings.Replace(string(script), runPlaceholderLine, `RUN_COMMAND_DEFAULT="Deploy"`, 1))},
		{name: "missing value", args: []string{"--run", ""}, want: "--run requires a command"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRunEnv(t, site)
			for k, v := range tc.env {
				r.set(k, v)
			}
			before := site.count("/install-commands.txt")
			out := r.invoke(t, tc.piped, tc.args...)
			r.assertRefusedBeforeInstalling(t, site, out)
			want := tc.want
			if want == "" {
				want = "must match ^[a-z][a-z0-9-]{0,63}$. Nothing was installed."
			}
			if !strings.Contains(out.stderr, want) {
				t.Fatalf("the refusal does not say %q:\n%s", want, out.transcript())
			}
			if got := site.count("/install-commands.txt"); got != before {
				t.Fatalf("an invalid command fetched the command map:\n%s", out.transcript())
			}
		})
	}
}

// Declining agent-host registration is independent of running a command.
func TestRunHonorsNoAgentHosts(t *testing.T) {
	t.Parallel()
	site := newRunSite(t, runSiteOptions{commandMap: commandMap("deploy @acme/deploy")})
	for _, decline := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "flag", args: []string{"--no-agent-hosts", "--run", "deploy"}},
		{name: "environment", args: []string{"--run", "deploy"}, env: map[string]string{"PUTNAMI_NO_AGENT_HOSTS": "1"}},
	} {
		t.Run(decline.name, func(t *testing.T) {
			t.Parallel()
			r := newRunEnv(t, site)
			for k, v := range decline.env {
				r.set(k, v)
			}
			claude := installFakeAgentHost(t, r.env, "claude", false)
			codex := installFakeAgentHost(t, r.env, "codex", false)

			out := r.invoke(t, nil, decline.args...)
			if out.exitCode != 0 {
				t.Fatalf("run failed (exit %d):\n%s", out.exitCode, out.transcript())
			}
			if !strings.Contains(out.stderr, "Skipping agent-host registration") {
				t.Fatalf("the declined registration was not reported:\n%s", out.transcript())
			}
			for _, host := range []fakeAgentHost{claude, codex} {
				if data, err := os.ReadFile(host.log); err == nil && len(data) != 0 {
					t.Fatalf("a declined run still called %s: %s", host.name, data)
				}
			}
			if out.stdout != "output of putnami deploy\n" {
				t.Fatalf("the command did not run:\n%s", out.transcript())
			}
			r.assertCallerDirUntouched(t)
		})
	}
}
