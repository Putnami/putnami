//go:build !windows

package serve

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
)

const toolchainFeature = "typescript/typescript-project-toolchain"

// fakeNode writes a node program that appends its PORT and arguments to log,
// one line per start, then stays up until it is stopped.
func fakeNode(t *testing.T, log string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node")
	script := "#!/bin/sh\necho \"start PORT=$PORT $*\" >> " + log + "\nexec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readStarts(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// edit replaces a source of the project with new content in one step: it
// writes the content beside the source and renames it over the source, so a
// watch reads either the previous file or the new one, never a truncated one.
func edit(t *testing.T, project, content string) {
	t.Helper()
	source := filepath.Join(project, "src", "main.ts")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	next := source + ".next"
	if err := os.WriteFile(next, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, source); err != nil {
		t.Fatal(err)
	}
}

type watchFixture struct {
	project string
	log     string
	serve   NodeServe
	bundles atomic.Int32
	failing atomic.Bool
	sigCh   chan os.Signal
	done    chan bool
	once    sync.Once
	stopped bool
	// events holds the message of every log and diagnostic the watch emitted.
	eventsMu sync.Mutex
	events   []string
	// returned is set once the watch returned.
	returned atomic.Bool
}

// reportOnFailure logs, when the test failed, what the watch left behind: the
// start log as written, the bundle count, whether the watch returned, and the
// events it emitted, among them a node that did not start.
func (f *watchFixture) reportOnFailure(t *testing.T) {
	if !t.Failed() {
		return
	}
	starts, err := os.ReadFile(f.log)
	if err != nil {
		starts = []byte(err.Error())
	}
	f.eventsMu.Lock()
	events := slices.Clone(f.events)
	f.eventsMu.Unlock()
	t.Logf("start log: %q; bundles: %d; watch returned: %t; watch events: %q",
		starts, f.bundles.Load(), f.returned.Load(), events)
}

// build is the bundle the fixture's bundler returns: built from src/main.ts.
func (f *watchFixture) build() NodeBuild {
	return NodeBuild{Entry: "/bundle/src/main.js", Inputs: []string{filepath.Join(f.project, "src", "main.ts")}}
}

// stop sends the watch a termination signal once and returns what it
// reported.
func (f *watchFixture) stop() bool {
	f.once.Do(func() {
		f.sigCh <- syscall.SIGTERM
		f.stopped = <-f.done
	})
	return f.stopped
}

func startWatch(t *testing.T) *watchFixture {
	t.Helper()
	previous := watchInterval
	watchInterval = 20 * time.Millisecond
	t.Cleanup(func() { watchInterval = previous })

	f := &watchFixture{project: t.TempDir(), sigCh: make(chan os.Signal, 1), done: make(chan bool, 1)}
	f.log = filepath.Join(t.TempDir(), "starts.log")
	edit(t, f.project, "v1")
	f.serve = NodeServe{
		NodeBin:     fakeNode(t, f.log),
		ProjectPath: f.project,
		Port:        4321,
		Params:      Params{Watch: true},
		Bundle: func() (NodeBuild, []string, error) {
			f.bundles.Add(1)
			if f.failing.Load() {
				return NodeBuild{}, []string{"src/main.ts: unexpected token"}, nil
			}
			return f.build(), nil, nil
		},
	}
	env := NodeEnv(os.Environ(), f.project, f.serve.Port)
	emit := jsonl.New()
	emit.Observe(func(message string) {
		f.eventsMu.Lock()
		f.events = append(f.events, message)
		f.eventsMu.Unlock()
	})
	go func() {
		stopped := watchNode(emit, f.serve, f.build(), env, f.sigCh)
		f.returned.Store(true)
		f.done <- stopped
	}()
	t.Cleanup(func() {
		stopped := make(chan struct{})
		go func() {
			f.stop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(2 * terminateGrace):
			t.Error("the watch did not stop")
		}
	})
	t.Cleanup(func() { f.reportOnFailure(t) })
	waitFor(t, "the first start", func() bool { return len(readStarts(t, f.log)) == 1 })
	return f
}

func TestNodeServeAnswersOnThePortAndRestartsAfterAnEdit(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-serve", "node-serve-answers-on-the-port-and-restarts-after-an-edit")
	f := startWatch(t)
	starts := readStarts(t, f.log)
	if want := "start PORT=4321 --enable-source-maps /bundle/src/main.js"; starts[0] != want {
		t.Fatalf("first start = %q, want %q", starts[0], want)
	}
	edit(t, f.project, "v2, a longer source")
	waitFor(t, "the restart", func() bool { return len(readStarts(t, f.log)) == 2 })
	if got := f.bundles.Load(); got != 1 {
		t.Errorf("bundles after one edit = %d, want 1: the start bundles before watchNode", got)
	}
	if !f.stop() {
		t.Error("a stopped watch did not report the stop")
	}
}

func TestAChildHoldingNodeOutputDoesNotHoldARestart(t *testing.T) {
	previous := nodeOutputDelay
	nodeOutputDelay = 100 * time.Millisecond
	t.Cleanup(func() { nodeOutputDelay = previous })
	f := startWatch(t)
	// The next node leaves a background process holding its stdout for
	// longer than the restart may take.
	script := "#!/bin/sh\necho \"start PORT=$PORT $*\" >> " + f.log + "\n(sleep 20 &)\nexec sleep 30\n"
	if err := os.WriteFile(f.serve.NodeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	edit(t, f.project, "v2, a longer source")
	waitFor(t, "the first restart", func() bool { return len(readStarts(t, f.log)) == 2 })
	restart := time.Now()
	edit(t, f.project, "v3, a still longer source")
	waitFor(t, "the restart of a node whose child holds its output", func() bool { return len(readStarts(t, f.log)) == 3 })
	if elapsed := time.Since(restart); elapsed > terminateGrace {
		t.Errorf("the restart took %s, past the %s grace: it waited for the child", elapsed, terminateGrace)
	}
	if !f.stop() {
		t.Error("the watch did not stop")
	}
}

func TestALineLongerThanTheForwarderReadsDoesNotHoldAStop(t *testing.T) {
	f := startWatch(t)
	script := "#!/bin/sh\necho \"start PORT=$PORT $*\" >> " + f.log + "\nhead -c 70000 /dev/zero | tr '\\0' x\necho\necho after\nexec sleep 30\n"
	if err := os.WriteFile(f.serve.NodeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	edit(t, f.project, "v2, a longer source")
	waitFor(t, "the node that writes a long line", func() bool { return len(readStarts(t, f.log)) == 2 })
	time.Sleep(100 * time.Millisecond)
	stopped := make(chan bool, 1)
	go func() { stopped <- f.stop() }()
	select {
	case ok := <-stopped:
		if !ok {
			t.Error("the watch did not report the stop")
		}
	case <-time.After(2 * terminateGrace):
		t.Fatal("a stop after a long line did not return")
	}
}

func TestNodeOutputDelayStaysBelowTheGrace(t *testing.T) {
	if nodeOutputDelay >= terminateGrace {
		t.Fatalf("nodeOutputDelay %s is not below terminateGrace %s", nodeOutputDelay, terminateGrace)
	}
}

func TestAFileTheWorkloadWritesDoesNotRestartNode(t *testing.T) {
	f := startWatch(t)
	for _, name := range []string{"dev.db", "logs/app.log", "src/uploads.json"} {
		path := filepath.Join(f.project, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("written at run time"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if got := f.bundles.Load(); got != 0 {
		t.Fatalf("files outside the bundle's inputs bundled %d times", got)
	}
	if starts := readStarts(t, f.log); len(starts) != 1 {
		t.Fatalf("files outside the bundle's inputs restarted node: %v", starts)
	}
}

func TestAFailedRebuildKeepsTheRunningProcess(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-serve", "a-failed-rebuild-keeps-the-running-process")
	f := startWatch(t)
	f.failing.Store(true)
	edit(t, f.project, "v2 does not parse")
	waitFor(t, "the failed bundle", func() bool { return f.bundles.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	if starts := readStarts(t, f.log); len(starts) != 1 {
		t.Fatalf("a failed bundle restarted node: %v", starts)
	}
	f.failing.Store(false)
	edit(t, f.project, "v3 parses again")
	waitFor(t, "the restart after the fix", func() bool { return len(readStarts(t, f.log)) == 2 })
}

func TestLookNodeWithoutNodeFailsNamingNode(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-run", "serve-and-run-without-node-fail-naming-node")
	t.Setenv("PATH", t.TempDir())
	_, err := LookNode()
	if err == nil {
		t.Fatal("LookNode succeeded with no node on PATH")
	}
	if errs.CodeOf(err) != errs.CodeNodeNotFound || !strings.Contains(err.Error(), "node is not on PATH") {
		t.Fatalf("LookNode error = %q (%s)", err, errs.CodeOf(err))
	}
}

func TestNodeServeArgs(t *testing.T) {
	if got := strings.Join(nodeServeArgs("/b/main.js", Params{InspectBrk: true}, false), " "); got != "--enable-source-maps --inspect-brk /b/main.js" {
		t.Errorf("dev args = %q", got)
	}
	if got := strings.Join(nodeServeArgs("/b/main.js", Params{Inspect: true}, true), " "); got != "--enable-source-maps /b/main.js" {
		t.Errorf("production args = %q", got)
	}
}

func TestWatchSnapshotReadsTheInputsOutsideNodeModules(t *testing.T) {
	root := t.TempDir()
	edit(t, root, "v1")
	source := filepath.Join(root, "src", "main.ts")
	dependency := filepath.Join(root, "node_modules", "x", "index.js")
	if err := os.MkdirAll(filepath.Dir(dependency), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dependency, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	inputs := []string{source, dependency}
	before := WatchSnapshot(inputs)
	if err := os.WriteFile(dependency, []byte("a longer x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if WatchSnapshot(inputs) != before {
		t.Error("an input under node_modules moved the snapshot")
	}
	edit(t, root, "v2, longer")
	if WatchSnapshot(inputs) == before {
		t.Error("an edited input did not move the snapshot")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if WatchSnapshot(inputs) == WatchSnapshot([]string{dependency}) {
		t.Error("a removed input reads as no input")
	}
}

// TestARealNodeAnswersOnThePortAfterEachEdit serves a module with the node on
// PATH and reads its answer before and after an edit.
func TestARealNodeAnswersOnThePortAfterEachEdit(t *testing.T) {
	spectest.Proves(t, toolchainFeature, "node-serve", "node-serve-answers-on-the-port-and-restarts-after-an-edit")
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	previous := watchInterval
	watchInterval = 20 * time.Millisecond
	t.Cleanup(func() { watchInterval = previous })

	project := t.TempDir()
	entry := filepath.Join(project, "server.mjs")
	write := func(version string) {
		source := "import http from \"node:http\";\n" +
			"http.createServer((_q, r) => r.end(\"" + version + "\")).listen(Number(process.env.PORT));\n"
		if err := os.WriteFile(entry+".next", []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(entry+".next", entry); err != nil {
			t.Fatal(err)
		}
	}
	write("v1")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	built := NodeBuild{Entry: entry, Inputs: []string{entry}}
	s := NodeServe{NodeBin: nodeBin, ProjectPath: project, Port: port, Params: Params{Watch: true},
		Bundle: func() (NodeBuild, []string, error) { return built, nil, nil }}
	sigCh := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	go func() { done <- watchNode(jsonl.New(), s, built, NodeEnv(os.Environ(), project, port), sigCh) }()
	defer func() {
		sigCh <- syscall.SIGTERM
		<-done
	}()

	answer := func() string {
		response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return ""
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body)
	}
	waitFor(t, "node to answer v1", func() bool { return answer() == "v1" })
	write("v2 after an edit")
	waitFor(t, "node to answer after the edit", func() bool { return answer() == "v2 after an edit" })
}
