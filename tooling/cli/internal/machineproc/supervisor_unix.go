//go:build linux || darwin

package machineproc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// Idle bounds (CLI ADR 0116 §2).
const (
	workerIdle     = time.Hour
	supervisorIdle = 3 * time.Hour
	readyTimeout   = 10 * time.Second
	idleCheck      = time.Minute
)

// supervisorEnv is the environment of a supervisor and the workers it starts:
// what locates the user's directories and tools, nothing of the command that
// started it. A worker runs each command with that command's environment.
func supervisorEnv(environ []string) []string {
	keep := map[string]bool{
		"HOME": true, "PATH": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"TMPDIR": true, "XDG_RUNTIME_DIR": true, "PUTNAMI_HOME": true, "LANG": true,
	}
	var env []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if keep[name] || strings.HasPrefix(name, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

type worker struct {
	exeInfo    os.FileInfo
	key        string
	build      string
	executable string
	cmd        *exec.Cmd
	ctrl       *conn
	busy       bool
	runs       int
	lastUsed   time.Time
	heldBytes  uint64
	gone       chan struct{}
}

type supervisor struct {
	dir      string
	exe      string
	exeInfo  os.FileInfo
	started  time.Time
	listener *net.UnixListener

	mu        sync.Mutex
	workers   map[string]*worker
	lastUsed  time.Time
	stopping  bool
	stopped   chan struct{}
	closeOnce sync.Once
}

// Supervise runs the supervisor until it stops: idle, asked to, or replaced.
// It returns at once when another supervisor holds the lock.
func Supervise() int {
	dir, err := Dir(os.Getenv, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "machine:", err)
		return 1
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "machine:", err)
		return 1
	}
	defer lock.Close() //nolint:errcheck // released by the kernel on exit
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return 0 // another supervisor lives
	}
	sock := filepath.Join(dir, socketName)
	if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "machine:", err)
		return 1
	}
	old := unix.Umask(0o177)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	unix.Umask(old)
	if err != nil {
		fmt.Fprintln(os.Stderr, "machine:", err)
		return 1
	}
	listener.SetUnlinkOnClose(true)
	exe, _ := os.Executable()
	exeInfo, _ := os.Stat(exe)
	s := &supervisor{exeInfo: exeInfo,
		dir: dir, exe: exe, started: time.Now(), listener: listener,
		workers: map[string]*worker{}, lastUsed: time.Now(), stopped: make(chan struct{}),
	}
	s.describe()
	fmt.Fprintf(os.Stderr, "machine: supervisor %d listening, protocol %d\n", os.Getpid(), ProtocolVersion)
	go s.watchIdle()
	go s.accept()
	<-s.stopped
	s.shutdown()
	return 0
}

func (s *supervisor) accept() {
	for {
		c, err := s.listener.AcceptUnix()
		if err != nil {
			s.stop("listener closed")
			return
		}
		go s.handle(c)
	}
}

func (s *supervisor) handle(uc *net.UnixConn) {
	if uid, err := peerUID(uc); err != nil || uid != os.Getuid() {
		_ = uc.Close()
		return
	}
	c := &conn{c: uc}
	var req request
	files, err := c.receive(&req)
	if err != nil {
		_ = uc.Close()
		return
	}
	s.touch()
	switch req.Op {
	case "status":
		closeAll(files)
		_ = c.send(Reply{Protocol: ProtocolVersion, OK: true, Status: s.status()})
		_ = uc.Close()
	case "stop":
		closeAll(files)
		_ = c.send(Reply{Protocol: ProtocolVersion, OK: true})
		_ = uc.Close()
		s.stop("asked to stop")
	case "run":
		s.route(c, req, files)
	default:
		closeAll(files)
		_ = c.send(Reply{Protocol: ProtocolVersion, Code: "machine.unknown_op", Message: req.Op})
		_ = uc.Close()
	}
}

// route hands a run to the worker of its build, or refuses it.
func (s *supervisor) route(c *conn, req request, files []*os.File) {
	refuse := func(code string) {
		closeAll(files)
		_ = c.send(event{Refused: code})
		_ = c.c.Close()
	}
	if req.Protocol > ProtocolVersion {
		refuse(CodeOutdated)
		s.replaceWhenIdle()
		return
	}
	if len(files) != 3 {
		refuse("machine.descriptors")
		return
	}
	w, code := s.claim(req)
	if w == nil {
		refuse(code)
		return
	}
	connFile, err := c.c.File()
	if err != nil {
		s.release(w)
		refuse("machine.handoff")
		return
	}
	err = w.ctrl.send(req, append(files, connFile)...)
	closeAll(files)
	_ = connFile.Close()
	_ = c.c.Close()
	if err != nil {
		s.release(w)
	}
}

// claim returns the idle worker of req's build, starting it when none runs,
// and marks it busy; or nil and the refusal's code.
func (s *supervisor) claim(req request) (*worker, string) {
	// A worker serves one executable file: two files of one build can stand
	// for different engines (a source workspace pins the file, not the build).
	key := req.Build + "\x00" + req.Executable
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil, CodeStopping
	}
	if w := s.workers[key]; w != nil {
		if w.busy {
			s.mu.Unlock()
			return nil, CodeBusy
		}
		w.busy = true
		s.mu.Unlock()
		return w, ""
	}
	s.mu.Unlock()
	if !trustedExecutable(req.Executable) || fileindex.BuildIdentity(req.Executable) != req.Build {
		return nil, CodeIdentity
	}
	w, err := s.startWorker(key, req.Build, req.Executable)
	if err != nil {
		fmt.Fprintln(os.Stderr, "machine: worker:", err)
		return nil, "machine.worker_start"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if other := s.workers[key]; other != nil {
		// Another request started one meanwhile: keep that one.
		_ = w.ctrl.c.Close()
		if other.busy {
			return nil, CodeBusy
		}
		other.busy = true
		return other, ""
	}
	w.busy = true
	s.workers[key] = w
	return w, ""
}

// trustedExecutable reports whether path is a regular file that this user or
// root owns and no other user may write (CLI ADR 0116 §11).
func trustedExecutable(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && (int(st.Uid) == os.Getuid() || st.Uid == 0)
}

func (s *supervisor) startWorker(key, build, exe string) (*worker, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	mine := os.NewFile(uintptr(pair[0]), "machine-ctrl")
	theirs := os.NewFile(uintptr(pair[1]), "machine-ctrl-worker")
	defer theirs.Close() //nolint:errcheck // the worker holds its own copy
	fc, err := net.FileConn(mine)
	_ = mine.Close()
	if err != nil {
		return nil, err
	}
	uc, err := unixConn(fc)
	if err != nil {
		return nil, err
	}
	ctrl := &conn{c: uc}
	cmd := exec.Command(exe, "__machine", "work") //nolint:gosec // checked by trustedExecutable and its identity
	cmd.Dir = s.dir
	cmd.Env = supervisorEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.ExtraFiles = []*os.File{theirs}
	if err := cmd.Start(); err != nil {
		_ = ctrl.c.Close()
		return nil, err
	}
	exeInfo, _ := os.Stat(exe)
	w := &worker{exeInfo: exeInfo, key: key, build: build, executable: exe, cmd: cmd, ctrl: ctrl, lastUsed: time.Now(), gone: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		var msg control
		_, err := ctrl.receive(&msg)
		if err == nil && msg.Op != "ready" {
			err = fmt.Errorf("worker sent %q before ready", msg.Op)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, err
		}
	case <-time.After(readyTimeout):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.New("worker not ready in time")
	}
	fmt.Fprintf(os.Stderr, "machine: worker %d started for build %s\n", cmd.Process.Pid, short(build))
	go s.follow(w)
	return w, nil
}

// follow reads a worker's control messages until it ends.
func (s *supervisor) follow(w *worker) {
	for {
		var msg control
		files, err := w.ctrl.receive(&msg)
		closeAll(files)
		if err != nil {
			break
		}
		if msg.Op == "done" {
			s.mu.Lock()
			w.busy = false
			w.runs++
			w.lastUsed = time.Now()
			w.heldBytes = msg.HeldBytes
			s.mu.Unlock()
			s.touch()
			s.maybeStopWhenIdle()
		}
	}
	_ = w.ctrl.c.Close()
	_ = w.cmd.Wait()
	s.mu.Lock()
	if s.workers[w.key] == w {
		delete(s.workers, w.key)
	}
	s.mu.Unlock()
	close(w.gone)
	fmt.Fprintf(os.Stderr, "machine: worker %d ended\n", w.cmd.Process.Pid)
	s.describe()
	s.maybeStopWhenIdle()
}

// release marks w idle after a hand-off that failed.
func (s *supervisor) release(w *worker) {
	s.mu.Lock()
	w.busy = false
	s.mu.Unlock()
}

func (s *supervisor) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
	s.describe()
}

// replaceWhenIdle stops the supervisor once no run is active, so that a
// newer client's next run starts a supervisor of its own build.
func (s *supervisor) replaceWhenIdle() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.maybeStopWhenIdle()
}

func (s *supervisor) maybeStopWhenIdle() {
	s.mu.Lock()
	stopping := s.stopping
	busy := false
	for _, w := range s.workers {
		busy = busy || w.busy
	}
	s.mu.Unlock()
	if stopping && !busy {
		s.stop("replaced or asked to stop")
	}
}

// stop begins the supervisor's end: new runs are refused, and it exits once
// no run is active.
func (s *supervisor) stop(reason string) {
	s.mu.Lock()
	s.stopping = true
	busy := false
	for _, w := range s.workers {
		busy = busy || w.busy
	}
	s.mu.Unlock()
	if busy {
		return
	}
	s.closeOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "machine: supervisor stopping:", reason)
		close(s.stopped)
	})
}

func (s *supervisor) watchIdle() {
	ticker := time.NewTicker(idleCheck)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		var idle []*worker
		for _, w := range s.workers {
			if !w.busy && (now.Sub(w.lastUsed) > workerIdle || !sameFile(w.executable, w.exeInfo)) {
				idle = append(idle, w)
			}
		}
		replaced := !sameFile(s.exe, s.exeInfo)
		empty := len(s.workers) == len(idle) && now.Sub(s.lastUsed) > supervisorIdle
		s.mu.Unlock()
		for _, w := range idle {
			_ = w.ctrl.c.Close() // the worker exits when its channel closes
		}
		if empty {
			s.stop("idle")
			return
		}
		if replaced {
			// A rebuilt or removed CLI cannot start this code again.
			s.stop("its executable changed")
			return
		}
	}
}

func (s *supervisor) shutdown() {
	_ = s.listener.Close()
	s.mu.Lock()
	workers := make([]*worker, 0, len(s.workers))
	for _, w := range s.workers {
		workers = append(workers, w)
	}
	s.mu.Unlock()
	for _, w := range workers {
		_ = w.ctrl.c.Close()
		select {
		case <-w.gone:
		case <-time.After(5 * time.Second):
			_ = w.cmd.Process.Kill()
		}
	}
	_ = os.Remove(filepath.Join(s.dir, descriptionName))
}

func (s *supervisor) status() *Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &Status{
		Protocol: ProtocolVersion, PID: os.Getpid(),
		Started: s.started.UTC().Format(time.RFC3339), Executable: s.exe,
		Workers: []WorkerStatus{},
	}
	for _, w := range s.workers {
		st.Workers = append(st.Workers, WorkerStatus{
			Build: w.build, PID: w.cmd.Process.Pid, Executable: w.executable, Busy: w.busy,
			Runs: w.runs, LastUsed: w.lastUsed.UTC().Format(time.RFC3339), HeldBytes: w.heldBytes,
		})
	}
	sort.Slice(st.Workers, func(i, j int) bool { return st.Workers[i].Build < st.Workers[j].Build })
	return st
}

// describe writes supervisor.json, the description `putnami machine status`
// falls back to.
func (s *supervisor) describe() {
	data, err := json.MarshalIndent(s.status(), "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(s.dir, descriptionName)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, append(data, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

func short(build string) string {
	if len(build) > 12 {
		return build[:12]
	}
	return build
}

// sameFile reports whether path still names the file info described.
func sameFile(path string, info os.FileInfo) bool {
	if info == nil {
		return false
	}
	now, err := os.Stat(path)
	return err == nil && os.SameFile(now, info) && now.ModTime().Equal(info.ModTime())
}
