package machineproc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// serveOnce serves req with run as a worker serves a client: it plays the
// client until the run's exit, and returns the exit code and a channel closed
// once serve returned, when the worker reads its next request.
func serveOnce(t *testing.T, req request, run RunFunc) (int, <-chan struct{}) {
	t.Helper()
	pair, err := socketpair()
	if err != nil {
		t.Fatal(err)
	}
	workerEnd, clientEnd := os.NewFile(uintptr(pair[0]), "worker-client"), os.NewFile(uintptr(pair[1]), "client")
	fc, err := net.FileConn(clientEnd)
	_ = clientEnd.Close()
	if err != nil {
		t.Fatal(err)
	}
	uc, err := unixConn(fc)
	if err != nil {
		t.Fatal(err)
	}
	client := &conn{c: uc}
	defer client.c.Close() //nolint:errcheck // the run is over
	// The exit comes before anything the worker does once the run returned.
	_ = client.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	files := []*os.File{workerEnd}
	for fd := 2; fd >= 0; fd-- {
		dup, err := unix.Dup(fd)
		if err != nil {
			t.Fatal(err)
		}
		files = append([]*os.File{os.NewFile(uintptr(dup), "std")}, files...)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(run, req, files, func() {})
	}()
	var ev event
	if _, err := client.receive(&ev); err != nil || !ev.Ready {
		t.Fatalf("the worker sent %+v, %v before the run, want Ready", ev, err)
	}
	if err := client.send(clientSignal{Signal: "go"}); err != nil {
		t.Fatal(err)
	}
	for ev.Exit == nil {
		ev = event{}
		if _, err := client.receive(&ev); err != nil || ev.Refused != "" || ev.Lost {
			t.Fatalf("the worker sent %+v, %v during the run, want its exit", ev, err)
		}
	}
	return *ev.Exit, done
}

// TestASampleThatDiffersOnceTheRunReturnedWithdrawsWhatTheWorkerHolds pins
// the sampled check of the input record in a worker (tooling/cli ADR 0113 §1,
// CLI ADR 0116 §6a): the run uses a sampled answer, the client gets its exit
// before the answer is collected again, and the worker takes no next request
// before the check ended. A check that finds no difference leaves the run
// held; one that differs empties the record on disk, withdraws every run the
// worker holds, so the next request is not answered from memory, and the
// worker's decision, which `putnami machine status` shows, names why.
func TestASampleThatDiffersOnceTheRunReturnedWithdrawsWhatTheWorkerHolds(t *testing.T) {
	runtimeDir, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if _, err := Dir(os.Getenv, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	w, root := watchedTree(t, 0)
	write(t, filepath.Join(root, "putnami.workspace.json"), "{}")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	// The record answers k under the stat of a file of the worktree.
	recordPath := fileindex.RecordPath(root)
	clock := time.Now().Add(time.Hour).UnixNano()
	seed := fileindex.OpenRecordAt(recordPath, "test", clock)
	witnessed := filepath.Join(root, "c", "g.txt")
	st, _ := fileindex.LstatAny(witnessed)
	if !seed.Put("digests", "k", []byte("v"), []fileindex.RecordPart{{Path: witnessed, Stat: st}}) {
		t.Skip("this platform or filesystem keeps no input record")
	}
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	previousFingerprint, previousEnv := ownFingerprint, ownRuntimeEnv
	ownFingerprint, ownRuntimeEnv = processFingerprint(), runtimeEnv(os.Environ())
	fileindex.DeferSamples(true)
	memory.Lock()
	previousWatcher := memory.watcher
	memory.watcher, memory.watched[root] = w, true
	memory.Unlock()
	t.Cleanup(func() {
		fileindex.DeferSamples(false)
		fileindex.TakeDeferredDiscard(recordPath)
		ownFingerprint, ownRuntimeEnv = previousFingerprint, previousEnv
		memory.Lock()
		memory.watcher, memory.withdrawn = previousWatcher, ""
		delete(memory.watched, root)
		delete(memory.held, root)
		delete(memory.gitDirs, root)
		delete(memory.gitConfig, root)
		memory.Unlock()
	})
	req := request{Dir: root, Args: []string{"build"}, Env: os.Environ(), Fingerprint: ownFingerprint, Umask: unix.Umask(0o022)}
	unix.Umask(req.Umask)

	// run answers k from the record, defers its sampled check, and records
	// a session whose every task was served from the cache, which the worker
	// may hold.
	var failed string
	sessions := 0
	run := func(check func() string) RunFunc {
		return func(context.Context, []string) int {
			r := fileindex.OpenRecordAt(recordPath, "test", clock)
			_, parts, ok := r.Answer("digests", "k", r.Hold)
			switch {
			case !ok:
				failed = "the record did not answer"
			case !r.Defer(parts, check):
				failed = "the worker's record did not defer the sampled check"
			}
			sessions++
			session := filepath.Join(root, ".putnami", "sessions", "s"+string(rune('0'+sessions)))
			if err := os.MkdirAll(session, 0o755); err != nil {
				failed = err.Error()
			} else if err := os.WriteFile(filepath.Join(session, "session.json"), []byte(`{"tasks":[]}`), 0o644); err != nil {
				failed = err.Error()
			}
			return 0
		}
	}

	code, done := serveOnce(t, req, run(func() string { return "" }))
	<-done
	if code != 0 || failed != "" {
		t.Fatalf("exit %d, %s", code, failed)
	}
	memory.Lock()
	held := memory.held[root]
	memory.Unlock()
	if d := ReadLastMemory(); held == nil || d == nil || d.Kept != "kept" || d.Sampled != 1 || d.Withdrawn != "" {
		t.Fatalf("held %v, decided %+v: want the run held after one sampled answer that holds", held != nil, d)
	}

	reason := "the input record answered planted for p src/**/*.go, which a collection gives as right"
	release := make(chan struct{})
	checkedIn := ""
	code, done = serveOnce(t, req, run(func() string {
		<-release
		checkedIn, _ = os.Getwd()
		return reason
	}))
	select {
	case <-done:
		t.Fatal("the worker could take its next request before the sampled check ended")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return once the sampled check ended")
	}
	if code != 0 || failed != "" {
		t.Fatalf("exit %d, %s", code, failed)
	}
	if resolved, _ := filepath.EvalSymlinks(checkedIn); resolved != resolvedRoot {
		t.Errorf("the check collected in %q, want the run's working directory %s", checkedIn, root)
	}
	memory.Lock()
	held = memory.held[root]
	memory.Unlock()
	if held != nil {
		t.Error("a sampled answer that differed left the run held")
	}
	if d := ReadLastMemory(); d == nil || d.Withdrawn != reason || !strings.HasPrefix(d.Kept, "not kept") {
		t.Errorf("decided %+v, want the difference named as why the worker withdrew what it held", d)
	}
	r := fileindex.OpenRecordAt(recordPath, "test", clock)
	if _, _, ok := r.Answer("digests", "k", r.Hold); ok {
		t.Error("the record on disk still answers after a sampled answer differed")
	}
	if _, d := beforeRun(req); d.Answerable || !strings.Contains(d.Reason, reason) {
		t.Errorf("the next request decided %+v, want it not answerable, and why", d)
	}
	memory.Lock()
	memory.run = runMemory{}
	memory.Unlock()
}
