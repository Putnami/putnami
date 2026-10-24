package bootstrap_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// frameTimeout bounds each wait on the wrapper.
const frameTimeout = 60 * time.Second

// mcpSession is one `./putnamiw mcp` process driven over its stdio.
type mcpSession struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	frames chan string
	stderr *lockedBuffer
}

// lockedBuffer is a stderr sink that the test reads while the process writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
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

// startMCP starts `./putnamiw mcp` in repo and reads its stdout frame by frame.
func startMCP(t *testing.T, repo string, env []string) *mcpSession {
	t.Helper()
	cmd := exec.Command("bash", "./putnamiw", "mcp")
	cmd.Dir = repo
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &mcpSession{t: t, cmd: cmd, stdin: stdin, frames: make(chan string, 64), stderr: stderr}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			s.frames <- scanner.Text()
		}
		close(s.frames)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return s
}

func (s *mcpSession) send(frame string) {
	s.t.Helper()
	if _, err := io.WriteString(s.stdin, frame+"\n"); err != nil {
		s.t.Fatalf("write %s: %v", frame, err)
	}
}

// mcpFrame is the part of a stdout frame these tests read. The sentinel's
// replies carry From and Method in their result.
type mcpFrame struct {
	ID     any    `json:"id"`
	Method string `json:"method"`
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools     struct{ ListChanged bool } `json:"tools"`
			Resources struct{ ListChanged bool } `json:"resources"`
		} `json:"capabilities"`
		ServerInfo struct{ Version string } `json:"serverInfo"`
		Tools      []json.RawMessage        `json:"tools"`
		From       string                   `json:"from"`
		Method     string                   `json:"method"`
	} `json:"result"`
	Error struct{ Code int } `json:"error"`
}

// next returns the next stdout frame, decoded.
func (s *mcpSession) next() mcpFrame {
	s.t.Helper()
	select {
	case frame, ok := <-s.frames:
		if !ok {
			s.t.Fatalf("stdout closed before the next frame; stderr:\n%s", s.stderr.String())
		}
		if strings.Contains(frame, "putnamiw-prepare") {
			s.t.Fatalf("the reply to the replayed initialize reached the client: %s", frame)
		}
		var msg mcpFrame
		if err := json.Unmarshal([]byte(frame), &msg); err != nil {
			s.t.Fatalf("stdout frame is not JSON: %q: %v", frame, err)
		}
		return msg
	case <-time.After(frameTimeout):
		s.t.Fatalf("no frame within %s; stderr:\n%s", frameTimeout, s.stderr.String())
		return mcpFrame{}
	}
}

// silent fails when stdout carries a frame within d.
func (s *mcpSession) silent(d time.Duration) {
	s.t.Helper()
	select {
	case frame, ok := <-s.frames:
		if ok {
			s.t.Fatalf("unexpected frame while the CLI builds: %s", frame)
		}
		s.t.Fatalf("stdout closed while the CLI builds; stderr:\n%s", s.stderr.String())
	case <-time.After(d):
	}
}

// wait closes stdin and returns the exit code once stdout closes.
func (s *mcpSession) wait(closeStdin bool) int {
	s.t.Helper()
	if closeStdin {
		_ = s.stdin.Close()
	}
	done := make(chan error, 1)
	go func() {
		for range s.frames {
		}
		done <- s.cmd.Wait()
	}()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			s.t.Fatal(err)
		}
		return 0
	case <-time.After(frameTimeout):
		s.t.Fatalf("./putnamiw mcp did not exit within %s; stderr:\n%s", frameTimeout, s.stderr.String())
		return -1
	}
}

func mcpEnv(t *testing.T, env []string) (out []string, gate, log, tmp string) {
	t.Helper()
	root := t.TempDir()
	gate = filepath.Join(root, "build-gate")
	log = filepath.Join(root, "mcp.log")
	tmp = filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	return append(env, "GO_BUILD_GATE="+gate, "SENTINEL_MCP_LOG="+log, "TMPDIR="+tmp), gate, log, tmp
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Errorf("%s still holds %v after the session", dir, entries)
	}
}

// On a cold machine `./putnamiw mcp` answers initialize while the CLI builds,
// queues the frames the CLI must answer, and hands the session to the built
// server once the build lands. A warm start then reaches the built server
// directly.
func TestPutnamiwMCP_ColdStartAnswersWhileTheCLIBuilds(t *testing.T) {
	repo, _, baseEnv := gitSourceRepo(t)
	env, gate, log, tmp := mcpEnv(t, baseEnv)

	s := startMCP(t, repo, env)
	initialize := `{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{"experimental":{"id":9}},"clientInfo":{"name":"pro{be","version":"1.0","icons":[{"src":"x]"}]}},"id":1}`
	s.send(initialize)
	reply := s.next()
	if reply.ID != float64(1) {
		t.Fatalf("initialize reply id = %v, want 1: %+v", reply.ID, reply)
	}
	if got := reply.Result.ServerInfo.Version; got != "preparing" {
		t.Errorf("serverInfo.version = %q, want preparing", got)
	}
	if got := reply.Result.ProtocolVersion; got != "2025-03-26" {
		t.Errorf("protocolVersion = %q, want the client's 2025-03-26", got)
	}
	if !reply.Result.Capabilities.Tools.ListChanged || !reply.Result.Capabilities.Resources.ListChanged {
		t.Errorf("capabilities = %+v, want tools and resources announced as changing", reply.Result.Capabilities)
	}

	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","method":"tools/list","id":2}`)
	if reply = s.next(); reply.ID != float64(2) || reply.Result.Tools == nil || len(reply.Result.Tools) != 0 {
		t.Fatalf("tools/list while building = %+v, want no tools for id 2", reply)
	}
	s.send(`{"jsonrpc":"2.0","method":"ping","params":{"note":"a}b[\"c"},"id":"ping-6"}`)
	if reply = s.next(); reply.ID != "ping-6" {
		t.Fatalf("ping while building = %+v, want the answer for ping-6", reply)
	}
	s.send(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"x","arguments":{"id":7,"method":"y"}},"id":"call-3"}`)
	s.send(`{"jsonrpc":"2.0","method":"prompts/list","id":4}`)
	if reply = s.next(); reply.ID != float64(4) || reply.Error.Code != -32601 {
		t.Fatalf("prompts/list while building = %+v, want method not found for id 4", reply)
	}
	s.silent(300 * time.Millisecond)
	for deadline := time.Now().Add(frameTimeout); goBuildCount(t, env) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("go build did not start within %s; stderr:\n%s", frameTimeout, s.stderr.String())
		}
	}

	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.next().Method; got != "notifications/tools/list_changed" {
		t.Fatalf("first frame after the build = %v, want notifications/tools/list_changed", got)
	}
	if got := s.next().Method; got != "notifications/resources/list_changed" {
		t.Fatalf("second frame after the build = %v, want notifications/resources/list_changed", got)
	}
	reply = s.next()
	if reply.ID != "call-3" || reply.Result.From != "built" || reply.Result.Method != "tools/call" {
		t.Fatalf("queued tools/call reply = %+v, want the built server's answer for call-3", reply)
	}
	s.send(`{"jsonrpc":"2.0","method":"tools/list","id":5}`)
	if reply = s.next(); reply.ID != float64(5) || reply.Result.From != "built" {
		t.Fatalf("tools/list after the build = %+v, want the built server's answer for id 5", reply)
	}
	if code := s.wait(true); code != 0 {
		t.Fatalf("exit code %d after the client closed stdin; stderr:\n%s", code, s.stderr.String())
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	received := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		strings.Replace(initialize, `"id":1}`, `"id":"putnamiw-prepare"}`, 1),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"x","arguments":{"id":7,"method":"y"}},"id":"call-3"}`,
		`{"jsonrpc":"2.0","method":"tools/list","id":5}`,
	}
	if strings.Join(received, "\n") != strings.Join(want, "\n") {
		t.Errorf("the built server read:\n%s\nwant:\n%s", strings.Join(received, "\n"), strings.Join(want, "\n"))
	}
	assertEmptyDir(t, tmp)

	warm := startMCP(t, repo, env)
	warm.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if reply = warm.next(); reply.Result.From != "built" {
		t.Fatalf("warm initialize = %+v, want the built server's answer", reply)
	}
	if code := warm.wait(true); code != 0 {
		t.Fatalf("warm exit code %d; stderr:\n%s", code, warm.stderr.String())
	}
	if got := goBuildCount(t, env); got != 1 {
		t.Errorf("go build count = %d, want 1: the warm start must not build", got)
	}
	spectest.Proves(t, "cli/engine-provenance", "cold-mcp-start",
		"a-cold-mcp-start-answers-initialize-and-hands-the-session-to-the-built-server")
}

// The wrapper answers initialize with a version it speaks: a supported version
// echoes, and any other version, or none, gets 2025-11-25. It answers
// server/discover, which Claude Code sends first, with method not found.
func TestPutnamiwMCP_InitializeAnswersWithAVersionTheRelaySpeaks(t *testing.T) {
	cases := []struct{ name, params, want string }{
		{"2024-11-05", `{"protocolVersion":"2024-11-05"}`, "2024-11-05"},
		{"2025-03-26", `{"protocolVersion":"2025-03-26"}`, "2025-03-26"},
		{"2025-06-18", `{"protocolVersion":"2025-06-18"}`, "2025-06-18"},
		{"2025-11-25", `{"protocolVersion":"2025-11-25"}`, "2025-11-25"},
		{"a newer revision", `{"protocolVersion":"2026-07-28"}`, "2025-11-25"},
		{"an unknown version", `{"protocolVersion":"latest"}`, "2025-11-25"},
		{"no version", `{}`, "2025-11-25"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, baseEnv := gitSourceRepo(t)
			env, gate, _, tmp := mcpEnv(t, baseEnv)
			if err := os.WriteFile(envValue(env, "GO_BUILD_FAIL"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			s := startMCP(t, repo, env)
			s.send(`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`)
			if reply := s.next(); reply.ID != float64(1) || reply.Error.Code != -32601 {
				t.Fatalf("server/discover = %+v, want method not found for id 1", reply)
			}
			s.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":` + tc.params + `}`)
			reply := s.next()
			if reply.ID != float64(2) || reply.Result.ServerInfo.Version != "preparing" {
				t.Fatalf("initialize reply = %+v, want the preparing server's answer for id 2", reply)
			}
			if got := reply.Result.ProtocolVersion; got != tc.want {
				t.Errorf("protocolVersion = %q, want %q", got, tc.want)
			}
			if err := os.WriteFile(gate, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			s.wait(false)
			assertEmptyDir(t, tmp)
			spectest.Proves(t, "cli/context-mcp-discovery", "mcp-protocol-version",
				"the-relay-answers-initialize-with-a-version-it-speaks")
			spectest.Proves(t, "cli/context-mcp-discovery", "mcp-protocol-version",
				"server-discover-is-method-not-found")
		})
	}
}

// A build that fails on the cold path ends the session: stdout closes with the
// client's stdin still open, and the build error reaches stderr.
func TestPutnamiwMCP_FailedBuildEndsTheSession(t *testing.T) {
	repo, _, baseEnv := gitSourceRepo(t)
	env, gate, _, tmp := mcpEnv(t, baseEnv)
	if err := os.WriteFile(envValue(env, "GO_BUILD_FAIL"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s := startMCP(t, repo, env)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if reply := s.next(); reply.Result.ServerInfo.Version != "preparing" {
		t.Fatalf("initialize reply = %+v, want the preparing server", reply)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(false); code == 0 {
		t.Fatal("a failed build exited 0")
	}
	if !strings.Contains(s.stderr.String(), "Failed to build putnami CLI") {
		t.Errorf("stderr does not name the failed build:\n%s", s.stderr.String())
	}
	assertEmptyDir(t, tmp)
	spectest.Proves(t, "cli/engine-provenance", "cold-mcp-start",
		"a-failed-cold-mcp-build-ends-the-session-with-its-error")
}

// Once the CLI runs, it is the process the client started: a signal the client
// sends to that process ends it with the client's stdin still open.
func TestPutnamiwMCP_TheBuiltServerReceivesTheClientsSignals(t *testing.T) {
	repo, _, baseEnv := gitSourceRepo(t)
	env, gate, _, tmp := mcpEnv(t, baseEnv)

	s := startMCP(t, repo, env)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if reply := s.next(); reply.Result.ServerInfo.Version != "preparing" {
		t.Fatalf("initialize reply = %+v, want the preparing server", reply)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.next().Method; got != "notifications/tools/list_changed" {
		t.Fatalf("first frame after the build = %v, want notifications/tools/list_changed", got)
	}
	s.next()
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if reply := s.next(); reply.Result.From != "built" {
		t.Fatalf("tools/list after the build = %+v, want the built server's answer", reply)
	}

	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan *os.ProcessState, 1)
	go func() {
		state, _ := s.cmd.Process.Wait()
		exited <- state
	}()
	select {
	case state := <-exited:
		status, ok := state.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Errorf("process state %v, want the built server ended by SIGTERM", state)
		}
	case <-time.After(frameTimeout):
		t.Fatalf("./putnamiw mcp did not exit within %s of SIGTERM; stderr:\n%s", frameTimeout, s.stderr.String())
	}
	assertEmptyDir(t, tmp)
	spectest.Proves(t, "cli/engine-provenance", "cold-mcp-start",
		"the-built-server-receives-the-signals-the-client-sends")
}

// A built server that exits on its own closes the client's stdout, with the
// client's stdin still open.
func TestPutnamiwMCP_AnExitedServerEndsTheSession(t *testing.T) {
	repo, _, baseEnv := gitSourceRepo(t)
	env, gate, _, tmp := mcpEnv(t, baseEnv)
	if err := os.WriteFile(envValue(env, "SENTINEL_FAIL"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s := startMCP(t, repo, env)
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if reply := s.next(); reply.Result.ServerInfo.Version != "preparing" {
		t.Fatalf("initialize reply = %+v, want the preparing server", reply)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(false); code != 7 {
		t.Fatalf("exit code %d, want the built server's 7; stderr:\n%s", code, s.stderr.String())
	}
	assertEmptyDir(t, tmp)
	spectest.Proves(t, "cli/engine-provenance", "cold-mcp-start",
		"an-exited-server-ends-the-session")
}
