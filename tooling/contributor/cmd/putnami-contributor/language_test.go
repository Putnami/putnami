package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// languageFeature and languageRequirement name the requirement the tests
// below prove.
const (
	languageFeature     = "cli/contributor-workflows"
	languageRequirement = "change-language-parity"

	// coreFailureLine is the line the core prints after a language failure
	// that exits 2. The core's command dispatcher adds it, not the language
	// check, so the executable does not print it.
	coreFailureLine = "putnami: contributor language failed\n"
	// coreName and changeName are the one substitution applied to a golden
	// before the comparison: the executable's messages name the command the
	// user typed, and the core's name its own.
	coreName   = "contributor language"
	changeName = "change language"
)

var (
	builtOnce sync.Once
	builtPath string
	builtErr  error
	builtDir  string
)

// TestMain removes the directory the shared executable is built in.
func TestMain(m *testing.M) {
	code := m.Run()
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
	os.Exit(code)
}

// languageExecutable returns the executable built from this module the way
// bin/prepare builds it: outside the Go workspace, without cgo. The package
// builds it once. A missing go or git fails the test, as the
// repository's other binary-building tests do; -short and Windows skip it.
func languageExecutable(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("compiles the executable")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the fixture repository is built by a bash script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	for _, tool := range []string{"go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not installed: %v", tool, err)
		}
	}
	builtOnce.Do(func() {
		builtDir, builtErr = os.MkdirTemp("", "putnami-contributor-build")
		if builtErr != nil {
			return
		}
		builtPath = filepath.Join(builtDir, "putnami-contributor")
		build := exec.Command("go", "build", "-o", builtPath, ".")
		build.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
		if output, err := build.CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("%w\n%s", err, output)
		}
	})
	if builtErr != nil {
		t.Fatalf("build the executable: %v", builtErr)
	}
	return builtPath
}

// languageFixture builds the fixture repository fixture.sh describes.
func languageFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if output, err := exec.Command("bash", filepath.Join("testdata", "fixture.sh"), dir).CombinedOutput(); err != nil {
		t.Fatalf("build the fixture repository: %v\n%s", err, output)
	}
	return dir
}

// languageResult is what one launch of the executable answered.
type languageResult struct {
	code           int
	stdout, stderr string
}

// render writes the result in the golden format testdata/record.sh writes.
func (r languageResult) render() string {
	return "exit " + string(rune('0'+r.code)) + "\n-- stdout --\n" + r.stdout + "-- stderr --\n" + r.stderr
}

// launchLanguage runs `language <args>` in dir, as putnami launches the
// command: the working directory is the workspace root and the launch ends
// with --putnamiContext. extraEnv adds environment entries.
func launchLanguage(t *testing.T, executable, dir, stdin string, args []string, extraEnv ...string) languageResult {
	t.Helper()
	command := exec.Command(executable, append(append([]string{"language"}, args...), "--putnamiContext", filepath.Join(dir, "absent-context.json"))...)
	command.Dir = dir
	command.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null"}, extraEnv...)
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := languageResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		result.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("launch the executable: %v", err)
	}
	return result
}

// TestChangeLanguageMatchesTheCoreCommand runs the built executable on the
// fixture repository for every mode and compares its exit code and both
// output streams with testdata/parity/*.golden. The goldens come from the
// core command: testdata/record.sh recorded them by running
// `putnami contributor language` on the same fixture. The test never reads
// that command's files, so a change to the core cannot change what this
// test compares against. Two documented differences apply to a golden before
// the comparison: the core's trailing coreFailureLine on an exit 2 is
// removed, and coreName becomes changeName.
func TestChangeLanguageMatchesTheCoreCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, languageRequirement, "change-language-reports-what-contributor-language-reports")
	executable := languageExecutable(t)
	fixture := languageFixture(t)
	cases, err := os.ReadFile(filepath.Join("testdata", "parity", "cases.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var covered []string
	for _, line := range strings.Split(string(cases), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "|", 3)
		if len(fields) != 3 {
			t.Fatalf("case line %q has no name|input|arguments", line)
		}
		name, input, arguments := fields[0], fields[1], strings.Fields(fields[2])
		covered = append(covered, arguments[0])
		t.Run(name, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", "parity", name+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			if input != "" {
				input += "\n"
			}
			got := launchLanguage(t, executable, fixture, input, arguments)
			want := string(golden)
			if got.code == 2 {
				want = strings.TrimSuffix(want, coreFailureLine)
			}
			want = strings.ReplaceAll(want, coreName, changeName)
			if rendered := got.render(); rendered != want {
				t.Errorf("the executable differs from the core command's golden\n--- got\n%s\n--- want\n%s", rendered, want)
			}
		})
	}
	// The tasks mode needs a provider the core's golden cannot record, so
	// its argument errors are the cases for it; the others cover each mode.
	for _, mode := range []string{"text", "files", "commits", "tasks"} {
		if !slices.Contains(covered, mode) {
			t.Errorf("no parity case runs the %s mode", mode)
		}
	}
}

// TestLanguageWithoutAModeIsAUsageError pins that the executable refuses a
// bare `language`.
func TestLanguageWithoutAModeIsAUsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"language", "--putnamiContext", "context.json"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "text, files, commits or tasks") || stdout.Len() != 0 {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

// TestTheContextFlagLeavesBeforeParsing pins that the launch's trailing
// --putnamiContext pair never reaches the language arguments.
func TestTheContextFlagLeavesBeforeParsing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ args, want []string }{
		"none":            {[]string{"language", "files"}, []string{"language", "files"}},
		"trailing":        {[]string{"language", "files", "--putnamiContext", "c.json"}, []string{"language", "files"}},
		"after the mode":  {[]string{"language", "--putnamiContext", "c.json", "files"}, []string{"language", "files"}},
		"a lone flag":     {[]string{"language", "--putnamiContext"}, []string{"language", "--putnamiContext"}},
		"only the last":   {[]string{"language", "text", "--putnamiContext", "a", "--putnamiContext", "b"}, []string{"language", "text", "--putnamiContext", "a"}},
		"the empty input": {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			original := slices.Clone(tc.args)
			if got := withoutContext(tc.args); !slices.Equal(got, tc.want) {
				t.Errorf("withoutContext(%v) = %v, want %v", tc.args, got, tc.want)
			}
			if !slices.Equal(tc.args, original) {
				t.Errorf("withoutContext changed its argument: %v, was %v", tc.args, original)
			}
		})
	}
}

// fakeHost serves the host RPC channel on a Unix socket: it answers
// collaboration.call with answer and counts the connections it accepts.
type fakeHost struct {
	socket, token string

	mu          sync.Mutex
	connections int
	requests    []wire.Request
}

func newFakeHost(t *testing.T, answer func(wire.CollaborationCallParams) any) *fakeHost {
	t.Helper()
	// A Unix socket path is limited to about 100 bytes: the temporary
	// directory of a test is longer on macOS.
	dir, err := os.MkdirTemp("/tmp", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	host := &fakeHost{socket: filepath.Join(dir, "host.sock"), token: "launch-token"}
	listener, err := net.Listen("unix", host.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			host.mu.Lock()
			host.connections++
			host.mu.Unlock()
			go host.serve(conn, answer)
		}
	}()
	return host
}

func (h *fakeHost) serve(conn net.Conn, answer func(wire.CollaborationCallParams) any) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		frame, err := wire.ReadFrame(reader, wire.MaxFrameBytes)
		if err != nil {
			return
		}
		var request wire.Request
		if err := json.Unmarshal(frame, &request); err != nil {
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, request)
		h.mu.Unlock()
		response := wire.Response{JSONRPC: wire.JSONRPCVersion, ID: request.ID}
		var params wire.CollaborationCallParams
		if request.Token != h.token || json.Unmarshal(request.Params, &params) != nil {
			response.Error = &wire.Error{Code: wire.CodeUnauthorized, Message: "refused"}
		} else {
			response.Result, _ = json.Marshal(answer(params))
		}
		reply, err := wire.EncodeFrame(response)
		if err != nil {
			return
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

func (h *fakeHost) env() []string {
	return []string{wire.SocketEnv + "=" + h.socket, wire.TokenEnv + "=" + h.token}
}

func (h *fakeHost) seen() (int, []wire.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connections, slices.Clone(h.requests)
}

// TestTasksReadsTheProviderThroughTheHostChannel pins that the tasks mode
// reaches the tasks provider only through the channel putnami names in the
// environment: it asks tasks find over that channel, pages with the cursor,
// and reports a French task with the core's wording.
func TestTasksReadsTheProviderThroughTheHostChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, languageRequirement, "tasks-reach-the-provider-through-the-host-channel")
	executable := languageExecutable(t)
	fixture := languageFixture(t)
	host := newFakeHost(t, func(params wire.CollaborationCallParams) any {
		var input collab.TaskFindInput
		_ = json.Unmarshal(params.Input, &input)
		page := collab.TaskListResult{Items: []collab.Task{{Ref: collab.Ref{Source: "local:x", ID: "one"}, Title: "English title"}}, Page: collab.Page{Next: "next"}}
		if input.Page != nil && input.Page.Cursor == "next" {
			page = collab.TaskListResult{Items: []collab.Task{{Ref: collab.Ref{Source: "local:x", ID: "two"}, Title: "Nous corrigeons le code"}}}
		}
		result, _ := json.Marshal(page)
		return collab.Envelope{Contract: params.Contract, Operation: params.Operation, Outcome: collab.OutcomeOK, Result: result}
	})
	got := launchLanguage(t, executable, fixture, "", []string{"tasks", "--limit", "10"}, host.env()...)
	want := languageResult{
		code:   1,
		stdout: "task two: non-English signals: nous\ntask two:1:Nous corrigeons le code\n",
		stderr: "change language: 1 of 2 scanned unit(s) are not in English\n",
	}
	if got != want {
		t.Errorf("tasks over the channel answered\n%s\nwant\n%s", got.render(), want.render())
	}
	connections, requests := host.seen()
	if connections != 1 || len(requests) != 2 {
		t.Fatalf("%d connections and %d requests, want 1 and 2", connections, len(requests))
	}
	for _, request := range requests {
		var params wire.CollaborationCallParams
		if request.Method != wire.MethodCollaborationCall || json.Unmarshal(request.Params, &params) != nil ||
			params.Contract != collab.ContractTasks || params.Operation != collab.OperationFind {
			t.Errorf("request %s %s, want %s tasks find", request.Method, request.Params, wire.MethodCollaborationCall)
		}
	}
}

// TestOtherModesNeverDialTheHost pins that text, files and commits run with a
// channel in the environment and open no connection to it.
func TestOtherModesNeverDialTheHost(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, languageRequirement, "other-modes-never-dial-the-host")
	executable := languageExecutable(t)
	fixture := languageFixture(t)
	host := newFakeHost(t, func(wire.CollaborationCallParams) any { return nil })
	for _, args := range [][]string{{"text", "README.md"}, {"files", "--", "README.md"}, {"commits", "HEAD~1"}} {
		if got := launchLanguage(t, executable, fixture, "", args, host.env()...); got.code != 0 {
			t.Errorf("%v exit %d, stderr %q", args, got.code, got.stderr)
		}
	}
	if connections, _ := host.seen(); connections != 0 {
		t.Errorf("%d connections, want none", connections)
	}
}

// TestTasksWithoutAChannelNeedsAnInteractiveLaunch pins the answer of a run
// that putnami did not launch interactively: exit 2, the remedy, no output.
func TestTasksWithoutAChannelNeedsAnInteractiveLaunch(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, languageRequirement, "tasks-without-a-channel-exit-2")
	executable := languageExecutable(t)
	fixture := languageFixture(t)
	got := launchLanguage(t, executable, fixture, "", []string{"tasks"})
	if got.code != 2 || got.stdout != "" {
		t.Errorf("exit %d, stdout %q, want 2 and nothing", got.code, got.stdout)
	}
	for _, want := range []string{"change language: ", "needs an interactive launch by putnami", "putnami change language tasks", wire.SocketEnv} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr %q does not say %q", got.stderr, want)
		}
	}
}
