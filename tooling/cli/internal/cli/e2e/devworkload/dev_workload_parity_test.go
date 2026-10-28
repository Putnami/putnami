//go:build unix

// Package devworkload holds the acceptance of moving `putnami compose` and
// `putnami qualify` to @putnami/dev: on the same fixture workspace, the
// extension's executable, launched by the real CLI through its dispatch of
// an interactive command-group subcommand with the shipped manifest, answers
// what the core's built-in commands answered. The members are served by
// nested `putnami serve` runs of that same CLI. The tests start processes and
// signal them, so they run in their own test binary.
//
// # The oracle is recorded
//
// testdata/dev-workload-parity holds one answer of the core per case: the
// exit code, stdout and stderr. TestRecordDevWorkloadParity wrote them with a
// putnami binary built from commit 4687a0e9, whose core still served
// both commands. A binary built from this tree has no such command, so it
// cannot record them: it answers that the command moved to @putnami/dev, and
// the comparison then fails. To record again, build the CLI in a worktree of
// that commit and name it in PUTNAMI_RECORD_DEV_WORKLOAD_PARITY.
//
// # Nothing differs
//
// A recording is compared as it was recorded. A composition stopped by a
// signal exits 0, as the core's did: the CLI keeps the exit code of an
// interactive command that exited by itself after the signal. The extension
// writes each error with the "putnami: " prefix the core wrote, so stderr is
// compared verbatim.
//
// Both sides normalize what depends on the moment a case runs: the
// composition id, ports, durations, timestamps, the worktree fingerprint and
// commit, and the fixture's directory. The replay fixture also holds the
// @putnami/dev extension, which the recording core did not load. Every other
// byte of stdout and stderr is compared verbatim, the stderr of a case that
// starts members included: it carries the members' serve logs, which the
// core's composition rendered in its own process and each nested serve run
// renders now.
package devworkload

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	gitexec "go.putnami.dev/tooling/cli/internal/git"
)

// devWorkloadCase is one invocation, compared against its recorded answer.
type devWorkloadCase struct {
	// name identifies the subtest and, through paritySlug, the recording.
	name string
	// args returns the CLI invocation. target is the URL of the case's
	// deterministic deployment, empty when the case starts none.
	args func(target string) []string
	// before prepares the fixture, whose root it receives, and commits it.
	before func(t *testing.T, root string)
	// deployment starts a deterministic deployment of /app that reports the
	// given sha on /version.
	deployment string
	// stopWhen, when set, is the stdout the run must print before the case
	// sends it SIGTERM: a composition runs until it is stopped.
	stopWhen *regexp.Regexp
	// wantExit is the exit code the extension must return. It is stated so a
	// recording edited into another verdict shows here.
	wantExit int
}

// deploymentSHA is the sha the deterministic deployment reports, and
// otherSHA one it is not.
const (
	deploymentSHA = "0123456789abcdef0123456789abcdef01234567"
	otherSHA      = "fedcba9876543210fedcba9876543210fedcba98"
)

func fixed(args ...string) func(string) []string {
	return func(string) []string { return args }
}

func devWorkloadCases() []devWorkloadCase {
	startDocument := regexp.MustCompile(`^\{.*"command":"compose".*\}\n`)
	startTable := regexp.MustCompile(`(?m)^    /app  http://`)
	return []devWorkloadCase{
		{name: "compose without a project", args: fixed("compose"), wantExit: 2},
		{name: "compose two projects", args: fixed("compose", "app", "provider"), wantExit: 2},
		{name: "compose unknown project", args: fixed("compose", "nope"), wantExit: 2},
		{name: "compose port out of range", args: fixed("compose", "app", "--port", "70000"), wantExit: 2},
		{name: "compose port not a number", args: fixed("compose", "app", "--port", "web"), wantExit: 2},
		{name: "compose ready timeout not a duration", args: fixed("compose", "app", "--ready-timeout", "soon"), wantExit: 2},
		{name: "compose ready timeout negative", args: fixed("compose", "app", "--ready-timeout", "-5s"), wantExit: 2},
		{name: "compose library", args: fixed("compose", "lib"), wantExit: 1},
		{name: "compose library jsonl", args: fixed("compose", "lib", "--output=jsonl"), wantExit: 1},
		{name: "compose library json", args: fixed("compose", "/lib", "--output=json"), wantExit: 1},
		{name: "compose disabled serve", args: fixed("compose", "app", "--output=jsonl"), before: disableProviderServe, wantExit: 1},
		{name: "compose unknown runsWith", args: fixed("compose", "app", "--output=jsonl"), before: unknownRunsWith, wantExit: 1},
		{
			name: "compose jsonl", args: fixed("compose", "app", "--port", "0", "--no-watch", "--ready-timeout", "60s", "--output=jsonl"),
			stopWhen: startDocument, wantExit: 0,
		},
		{
			name: "compose by id with an inline port", args: fixed("compose", "/app", "--port=0", "--no-watch", "--output=jsonl"),
			stopWhen: startDocument, wantExit: 0,
		},
		{
			name: "compose watch false", args: fixed("compose", "app", "--port", "0", "--watch=false", "--output=jsonl"),
			stopWhen: startDocument, wantExit: 0,
		},
		{
			name: "compose text", args: fixed("compose", "app", "--port", "0", "--no-watch"),
			stopWhen: startTable, wantExit: 0,
		},
		{
			name: "compose member exited", args: fixed("compose", "app", "--port", "0", "--no-watch", "--output=jsonl"),
			before: providerExitsEarly, wantExit: 1,
		},
		{name: "qualify without a project", args: fixed("qualify", "--target", "local"), wantExit: 2},
		{name: "qualify without a target", args: fixed("qualify", "app"), wantExit: 2},
		{name: "qualify unknown project", args: fixed("qualify", "nope", "--target", "local"), wantExit: 2},
		{name: "qualify local with expect-sha", args: fixed("qualify", "app", "--target", "local", "--expect-sha", deploymentSHA), wantExit: 2},
		{name: "qualify url without expect-sha", args: fixed("qualify", "app", "--target", "http://127.0.0.1:9"), wantExit: 2},
		{name: "qualify url with credentials", args: fixed("qualify", "app", "--target", "http://user:pw@127.0.0.1:9", "--expect-sha", deploymentSHA), wantExit: 2},
		{name: "qualify short expect-sha", args: fixed("qualify", "app", "--target", "http://127.0.0.1:9", "--expect-sha", "abc"), wantExit: 2},
		{name: "qualify bad platform prefix", args: fixed("qualify", "app", "--target", "local", "--platform-prefix", "/ops?x"), wantExit: 2},
		{name: "qualify print-contract with a target", args: fixed("qualify", "app", "--print-contract", "--target", "local"), wantExit: 2},
		{name: "qualify ready timeout not a duration", args: fixed("qualify", "app", "--target", "local", "--ready-timeout", "soon"), wantExit: 2},
		{name: "qualify print-contract", args: fixed("qualify", "app", "--print-contract")},
		{name: "qualify print-contract json", args: fixed("qualify", "app", "--print-contract", "--output=json")},
		{name: "qualify print-contract without routes", args: fixed("qualify", "provider", "--print-contract")},
		{name: "qualify print-contract without routes jsonl", args: fixed("qualify", "provider", "--print-contract", "--output=jsonl")},
		{name: "qualify local", args: fixed("qualify", "app", "--target", "local")},
		{name: "qualify local json", args: fixed("qualify", "app", "--target", "local", "--output=json")},
		{name: "qualify local jsonl", args: fixed("qualify", "/app", "--target", "local", "--output=jsonl")},
		{name: "qualify local composition failed", args: fixed("qualify", "app", "--target", "local", "--output=json"), before: providerExitsEarly, wantExit: 1},
		{name: "qualify local library", args: fixed("qualify", "lib", "--target", "local", "--output=json"), wantExit: 1},
		{
			name: "qualify url", deployment: deploymentSHA, wantExit: 0,
			args: func(target string) []string {
				return []string{"qualify", "app", "--target", target, "--expect-sha", deploymentSHA[:12], "--output=json"}
			},
		},
		{
			name: "qualify url text", deployment: deploymentSHA, wantExit: 0,
			args: func(target string) []string {
				return []string{"qualify", "app", "--target", target, "--expect-sha", deploymentSHA}
			},
		},
		{
			name: "qualify url digest mismatch", deployment: otherSHA, wantExit: 1,
			args: func(target string) []string {
				return []string{"qualify", "app", "--target", target, "--expect-sha", deploymentSHA, "--output=json"}
			},
		},
		{
			name: "qualify url digest mismatch text", deployment: otherSHA, wantExit: 1,
			args: func(target string) []string {
				return []string{"qualify", "app", "--target", target, "--expect-sha", deploymentSHA}
			},
		},
	}
}

// TestDevWorkloadCommandsMatchTheRecordedCore is the acceptance: same
// fixture, same arguments, the same exit code, stdout and stderr the core's
// built-in commands produced.
func TestDevWorkloadCommandsMatchTheRecordedCore(t *testing.T) {
	t.Parallel()
	requireFixtureTools(t)
	cli := fixtureproc.CLI(t)
	runtimePath := devRuntime(t)
	for _, tc := range devWorkloadCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			limitConcurrency(t)
			root := newFixture(t, runtimePath)
			got := runCase(t, cli, root, tc)
			want := expectedAnswer(t, tc)
			if got.exit != want.exit {
				t.Errorf("exit code: extension %d, recorded core %d", got.exit, want.exit)
			}
			for _, part := range []struct{ name, got, want string }{
				{"stdout", got.stdout, want.stdout},
				{"stderr", got.stderr, want.stderr},
			} {
				if part.got != part.want {
					t.Errorf("%s differs from the recorded core answer.\n--- recorded ---\n%s\n--- extension ---\n%s", part.name, part.want, part.got)
				}
			}
		})
	}
}

// expectedAnswer is the recorded answer of tc.
func expectedAnswer(t *testing.T, tc devWorkloadCase) answer {
	t.Helper()
	recorded := readAnswer(t, filepath.Join(recordingDir(), paritySlug(tc.name)+".txt"))
	if recorded.exit != tc.wantExit {
		t.Errorf("the recording exits %d, but the case claims %d", recorded.exit, tc.wantExit)
	}
	return recorded
}

// TestDevWorkloadRecordingsCoverEveryCaseAndNothingElse keeps the
// recordings and the case table in step: a recording no case reads is a
// comparison that silently stopped.
func TestDevWorkloadRecordingsCoverEveryCaseAndNothingElse(t *testing.T) {
	t.Parallel()
	expected := map[string]bool{}
	for _, tc := range devWorkloadCases() {
		expected[paritySlug(tc.name)+".txt"] = true
	}
	if len(expected) != len(devWorkloadCases()) {
		t.Fatalf("the table names %d cases but only %d distinct recordings: paritySlug collided", len(devWorkloadCases()), len(expected))
	}
	entries, err := os.ReadDir(recordingDir())
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		found = append(found, entry.Name())
		if !expected[entry.Name()] {
			t.Errorf("%s is a recording no case compares against", entry.Name())
		}
	}
	for name := range expected {
		if !slices.Contains(found, name) {
			t.Errorf("%s has no recording", name)
		}
	}
}

// recordEnv names the putnami binary TestRecordDevWorkloadParity records the
// core's answers with.
const recordEnv = "PUTNAMI_RECORD_DEV_WORKLOAD_PARITY"

// TestRecordDevWorkloadParity writes testdata/dev-workload-parity: it runs
// each case with the putnami binary recordEnv names, which must serve
// `compose` and `qualify` itself, and records its answer. It runs only when
// that variable is set.
func TestRecordDevWorkloadParity(t *testing.T) {
	t.Parallel()
	core := os.Getenv(recordEnv)
	if core == "" {
		t.Skip("recording: " + recordEnv + " names no putnami binary")
	}
	requireFixtureTools(t)
	if err := os.MkdirAll(recordingDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range devWorkloadCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			limitConcurrency(t)
			root := newFixture(t, "")
			recorded := runCase(t, core, root, tc)
			path := filepath.Join(recordingDir(), paritySlug(tc.name)+".txt")
			if err := os.WriteFile(path, []byte(recorded.String()), 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// runCase runs tc with the putnami binary cli in the fixture root and
// returns its normalized answer.
func runCase(t *testing.T, cli, root string, tc devWorkloadCase) answer {
	t.Helper()
	if tc.before != nil {
		tc.before(t, root)
		commitFixture(t, root, "the case's change")
	}
	target := ""
	if tc.deployment != "" {
		target = startDeployment(t, tc.deployment)
	}
	cmd := exec.Command(cli, tc.args(target)...)
	cmd.Dir = root
	cmd.Env = runEnv(t)
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if tc.stopWhen != nil {
		awaitOutput(t, stdout, tc.stopWhen, done, stderr)
		awaitServeLogs(t, stderr, done)
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatalf("putnami %v did not exit within 3m\nstdout:\n%s\nstderr:\n%s", tc.args(target), stdout.String(), stderr.String())
	}
	code := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	result := answer{exit: code, stdout: stdout.String(), stderr: stderr.String()}
	return result.normalized(root)
}

// awaitOutput waits until stdout matches pattern, failing the test when the
// run ends first or nothing matches within a minute.
func awaitOutput(t *testing.T, stdout *lockedBuffer, pattern *regexp.Regexp, done <-chan error, stderr fmt.Stringer) {
	t.Helper()
	deadline := time.After(time.Minute)
	for !pattern.MatchString(stdout.String()) {
		select {
		case err := <-done:
			t.Fatalf("putnami exited (%v) before it printed what the case waits for\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		case <-deadline:
			t.Fatalf("putnami printed nothing the case waits for within a minute\nstdout:\n%s", stdout.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// membersServeLogs are the lines the members' serve runs log on stderr before
// a composition of /app is ready.
var membersServeLogs = []string{"fixture /provider starting in serve", "fixture /app starting in started"}

func logsServe(stderr string) bool {
	for _, line := range membersServeLogs {
		if !strings.Contains(stderr, line) {
			return false
		}
	}
	return true
}

// awaitServeLogs waits until stderr carries the serve log the members wrote
// before they were ready. The members' serve logs reach stderr through pipes of
// their own, with no order against the start the composition prints on stdout:
// a signal sent once stdout shows the start can come before a member's line,
// and the CLI's interrupt notice then lands above it on stderr.
func awaitServeLogs(t *testing.T, stderr *lockedBuffer, done <-chan error) {
	t.Helper()
	deadline := time.After(time.Minute)
	for !logsServe(stderr.String()) {
		select {
		case err := <-done:
			t.Fatalf("putnami exited (%v) before the members' serve logs reached stderr\nstderr:\n%s", err, stderr.String())
		case <-deadline:
			t.Fatalf("the members' serve logs did not reach stderr within a minute\nstderr:\n%s", stderr.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// lockedBuffer is a buffer a process writes while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startDeployment serves a deterministic deployment of /app on the loopback
// interface: its platform endpoints, reporting sha on /version, and the
// route its contract derives. It returns its URL.
func startDeployment(t *testing.T, sha string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":"app","version":"1.2.3","sha":%q}`, sha)
	})
	mux.HandleFunc("/items", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String()
}

// runEnv is the environment of one run: this process's, without the
// variables that bind a run to a commit, select an output mode, open a host
// channel or point git elsewhere, but with the store and artifact roots
// TestMain set, and with git's global and system configuration out of reach.
func runEnv(t *testing.T) []string {
	t.Helper()
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || strings.HasPrefix(name, "GIT_") {
			continue
		}
		if strings.HasPrefix(name, "PUTNAMI_") && name != "PUTNAMI_STORE_DIR" && name != "PUTNAMI_ARTIFACT_DIR" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+t.TempDir(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C", "DO_NOT_TRACK=1")
}

// limitConcurrency bounds how many cases run at once: each one starts a CLI
// that starts its members' serve runs.
var concurrency = make(chan struct{}, 2)

func limitConcurrency(t *testing.T) {
	t.Helper()
	concurrency <- struct{}{}
	t.Cleanup(func() { <-concurrency })
}

func requireFixtureTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available, and a local target binds to a git worktree")
	}
}

// --- the fixture ------------------------------------------------------------

// fixtureRoutes is /app's committed route inventory: one exact, public GET
// route, from which qualify derives one smoke request.
const fixtureRoutes = `{
  "$schema": "https://putnami.dev/schemas/putnami-http-routes-v1.json",
  "protocol": "putnami.http-routes.v1",
  "routes": [
    {
      "match": "exact",
      "path": "/items",
      "methods": [
        "GET"
      ],
      "publicEdge": true,
      "provenance": {
        "project": "app",
        "sourceKind": "manual"
      }
    }
  ],
  "digest": "sha256:8d957da3be380f191d6329b5aca64ec414b207933dd26b5d978d84760a4b7a17"
}
`

// fixtureContract is /provider's committed client contract.
const fixtureContract = `{"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:acme:items"},"credentials":{}}}` + "\n"

// newFixture writes the workspace every case runs in, in a fresh directory,
// commits it with a fixed identity and returns its root. /provider serves,
// /app runs with it and reports completed startup, /lib is a library. Every
// member is served by @acme/parity-serve, whose serve task is this test
// binary in fixture mode. With runtimePath, the workspace also holds the
// @putnami/dev extension: its shipped manifest, with its tasks running the
// executable at runtimePath instead of the runtime it prepares.
func newFixture(t *testing.T, runtimePath string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, _ := json.Marshal(executable)
	includes := []string{"fixture-extension", "provider", "app", "lib"}
	if runtimePath != "" {
		includes = append(includes, "dev-extension")
		writeFixtureFile(t, root, "dev-extension/putnami.json", `{"name":"@putnami/dev"}`+"\n")
		writeFixtureFile(t, root, "dev-extension/putnami.extension.json", devManifest(t, runtimePath))
	}
	workspace, _ := json.Marshal(struct {
		Name     string   `json:"name"`
		Includes []string `json:"includes"`
	}{"dev-workload-parity", includes})
	for _, file := range [][2]string{
		// A serve run stamps .gen/version.json, which a workspace ignores: a
		// local verdict binds to the worktree, and an unignored stamp would
		// change it while the members start.
		{".gitignore", ".putnami/\n.gen/\n"},
		{"putnami.workspace.json", string(workspace) + "\n"},
		{"fixture-extension/putnami.json", `{"name":"@acme/parity-serve"}` + "\n"},
		{"fixture-extension/putnami.extension.json", fmt.Sprintf(`{
  "name": "@acme/parity-serve",
  "version": "0.1.0",
  "cliContract": 11,
  "commands": {
    "serve": {
      "description": "Serve a parity fixture workload.",
      "traits": {"workload": "serve"},
      "flags": {
        "port": {"type": "number", "default": 3000},
        "watch": {"type": "boolean", "default": true}
      },
      "run": [{"id": "serve", "task": "serve-run"}]
    }
  },
  "tasks": {
    "serve-run": {
      "description": "Long-lived fixture workload that announces typed readiness.",
      "kind": "command",
      "command": %s,
      "env": {%q: "1"},
      "cache": false,
      "timeoutMs": -1,
      "declares": {"effects": ["network", "process"]}
    }
  }
}
`, command, fixtureModeEnv)},
		{"provider/putnami.json", `{"name":"provider","extensions":["@acme/parity-serve"]}` + "\n"},
		{"provider/fixture.json", `{"mode":"serve","member":"/provider"}` + "\n"},
		{"provider/schema/openapi.json", fixtureContract},
		{"app/putnami.json", `{"name":"app","extensions":["@acme/parity-serve"],"runsWith":["provider"]}` + "\n"},
		{"app/fixture.json", `{"mode":"started","member":"/app"}` + "\n"},
		{"app/schema/http-routes.json", fixtureRoutes},
		{"lib/putnami.json", `{"name":"lib","type":"library","extensions":["@acme/parity-serve"]}` + "\n"},
	} {
		writeFixtureFile(t, root, file[0], file[1])
	}
	git(t, root, "init", "-q", "--initial-branch=main")
	for _, setting := range [][2]string{
		{"user.name", "Parity Fixture"}, {"user.email", "parity@example.invalid"},
		{"commit.gpgsign", "false"}, {"gc.auto", "0"}, {"maintenance.auto", "false"},
	} {
		git(t, root, "config", setting[0], setting[1])
	}
	commitFixture(t, root, "the workspace")
	return root
}

// devManifest is the shipped @putnami/dev manifest with its runtime replaced
// by the executable at runtimePath: no runtime to prepare, and every task
// that ran the prepared runtime runs that executable.
func devManifest(t *testing.T, runtimePath string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "dev", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read the shipped @putnami/dev manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	delete(manifest, "runtime")
	tasks, _ := manifest["tasks"].(map[string]any)
	for _, task := range tasks {
		if definition, ok := task.(map[string]any); ok && definition["command"] == "{extensionRuntime}" {
			definition["command"] = runtimePath
		}
	}
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out) + "\n"
}

// devRuntime builds the @putnami/dev executable for the test, as
// tooling/dev/bin/prepare does, and returns its path.
func devRuntime(t *testing.T) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "putnami-dev")
	cmd := exec.Command("go", "build", "-o", output, "./cmd/putnami-dev")
	cmd.Dir = filepath.Join("..", "..", "..", "..", "..", "dev")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the @putnami/dev runtime, so parity cannot be measured:\n%s", out)
	}
	return output
}

// disableProviderServe has /provider disable its serve command.
func disableProviderServe(t *testing.T, root string) {
	t.Helper()
	writeFixtureFile(t, root, "provider/putnami.json",
		`{"name":"provider","extensions":["@acme/parity-serve"],"disable":{"jobs":["serve"]}}`+"\n")
}

// unknownRunsWith has /app run with a project the workspace does not hold.
func unknownRunsWith(t *testing.T, root string) {
	t.Helper()
	writeFixtureFile(t, root, "app/putnami.json",
		`{"name":"app","extensions":["@acme/parity-serve"],"runsWith":["provider","ghost"]}`+"\n")
}

// providerExitsEarly has /provider's serve step exit before it announces
// readiness.
func providerExitsEarly(t *testing.T, root string) {
	t.Helper()
	writeFixtureFile(t, root, "provider/fixture.json", `{"mode":"exit-early","member":"/provider"}`+"\n")
}

func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitFixture commits every change of root with a fixed identity and date.
func commitFixture(t *testing.T, root, message string) {
	t.Helper()
	git(t, root, "add", "-A")
	cmd := exec.Command(gitexec.Executable(), "commit", "-q", "-m", message)
	cmd.Dir = root
	cmd.Env = append(gitEnv(),
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitexec.Executable(), args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
}

// --- the recordings ---------------------------------------------------------

// recordingDir holds the core's recorded answers.
func recordingDir() string { return filepath.Join("testdata", "dev-workload-parity") }

const (
	stdoutMarker = "--- stdout ---\n"
	stderrMarker = "--- stderr ---\n"
)

// answer is what one run answered.
type answer struct {
	exit           int
	stdout, stderr string
}

func (a answer) String() string {
	return "exit " + strconv.Itoa(a.exit) + "\n" + stdoutMarker + a.stdout + stderrMarker + a.stderr
}

var (
	loopbackPort  = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`)
	compositionID = regexp.MustCompile(`\b[0-9a-f]{16}\b`)
	portMember    = regexp.MustCompile(`("(?:backendPort|port)":\s*)[1-9][0-9]*`)
	timingMember  = regexp.MustCompile(`("(?:readyMs|durationMs)":\s*)[0-9]+`)
	fingerprint   = regexp.MustCompile(`("fingerprint":\s*)"[^"]+"`)
	headSHA       = regexp.MustCompile(`("headSHA":\s*)"[0-9a-f]{40}"`)
	timestamp     = regexp.MustCompile(`("(?:startedAt|finishedAt)":\s*)"[^"]+"`)
	textDuration  = regexp.MustCompile(`\b[0-9]+ms\b`)
	textTree      = regexp.MustCompile(`\btree [0-9a-f]{12}\b`)
)

// normalized replaces what depends on the moment of the run.
func (a answer) normalized(root string) answer {
	normalize := func(text string) string {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			text = strings.ReplaceAll(text, resolved, "<root>")
		}
		text = strings.ReplaceAll(text, root, "<root>")
		text = loopbackPort.ReplaceAllString(text, "127.0.0.1:<port>")
		text = compositionID.ReplaceAllString(text, "<composition>")
		text = portMember.ReplaceAllString(text, `${1}"<port>"`)
		text = timingMember.ReplaceAllString(text, `${1}"<ms>"`)
		text = fingerprint.ReplaceAllString(text, `${1}"<fingerprint>"`)
		text = headSHA.ReplaceAllString(text, `${1}"<sha>"`)
		text = timestamp.ReplaceAllString(text, `${1}"<time>"`)
		text = textTree.ReplaceAllString(text, "tree <fingerprint>")
		return textDuration.ReplaceAllString(text, "<ms>")
	}
	a.stdout, a.stderr = normalize(a.stdout), normalize(a.stderr)
	return a
}

// readAnswer reads the answer at path: an `exit <code>` line, then the
// stdout and stderr sections.
func readAnswer(t *testing.T, path string) answer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the recorded answer %s: %v\n"+
			"Only a CLI built from commit 4687a0e9, whose core served the commands, records it "+
			"(TestRecordDevWorkloadParity). A missing recording means a case was added without recording what the core answered.", path, err)
	}
	head, rest, found := strings.Cut(string(data), stdoutMarker)
	if !found {
		t.Fatalf("%s carries no stdout section", path)
	}
	stdout, stderr, found := strings.Cut(rest, stderrMarker)
	if !found {
		t.Fatalf("%s carries no stderr section", path)
	}
	if strings.Contains(stderr, stdoutMarker) || strings.Contains(stderr, stderrMarker) {
		t.Fatalf("%s repeats a section marker, so its framing is ambiguous", path)
	}
	exit, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(head), "exit")))
	if err != nil {
		t.Fatalf("%s does not start with an `exit <code>` line: %v", path, err)
	}
	return answer{exit: exit, stdout: stdout, stderr: stderr}
}

// paritySlug is the recording file name of a case name.
func paritySlug(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		default:
			return '-'
		}
	}, name)
}
