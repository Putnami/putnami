package machineproc_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/machineproc"
)

// terminal is a pseudo-terminal that a test puts in the place of a person's
// terminal: the command runs with its slave as stdin, stdout and stderr, and
// the test reads and types through its master.
type terminal struct {
	master, slave *os.File
	mu            sync.Mutex
	out           bytes.Buffer
	done          chan struct{}
}

func openTerminal(t *testing.T, rows, cols uint16) *terminal {
	t.Helper()
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetUint32(mfd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	sfd, err := unix.Open("/dev/pts/"+strconv.Itoa(int(n)), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	term := &terminal{master: os.NewFile(uintptr(mfd), "master"), slave: os.NewFile(uintptr(sfd), "slave"), done: make(chan struct{})}
	term.resize(t, rows, cols)
	go func() {
		defer close(term.done)
		buf := make([]byte, 4096)
		for {
			n, err := term.master.Read(buf)
			term.mu.Lock()
			term.out.Write(buf[:n])
			term.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = term.slave.Close()
		_ = term.master.Close()
	})
	return term
}

func (term *terminal) resize(t *testing.T, rows, cols uint16) {
	t.Helper()
	if err := unix.IoctlSetWinsize(int(term.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatal(err)
	}
}

func (term *terminal) output() string {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.out.String()
}

// run runs args at the terminal, as a person's command would, and returns
// its exit code when it ends.
func (term *terminal) run(t *testing.T, root string, args ...string) <-chan int {
	t.Helper()
	t.Chdir(root)
	code := make(chan int, 1)
	stdin, stdout, stderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = term.slave, term.slave, term.slave
	go func() {
		// The streams are given back before the code is sent: a test that
		// received the code, and the next test, read them after this write.
		result := -1
		defer func() {
			os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr
			code <- result
		}()
		app, err := cli.NewApp()
		if err != nil {
			return
		}
		result = app.Run(context.Background(), args)
	}()
	return code
}

// slowTerminalFixture is the fixture whose tasks wait, then write the size of
// their controlling terminal to their report: a job reads the run's terminal
// as it would read a person's.
func slowTerminalFixture(t *testing.T, wait string) string {
	t.Helper()
	root := clitest.WritingCountsFixture(t, 1)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n.gen\n")
	manifestPath := filepath.Join(root, "extension", "putnami.extension.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	slow := strings.ReplaceAll(string(manifest), `printf '%s\\n' \"$0\" \u003e \"$1\"`,
		`sleep `+wait+`; stty size \u003c /dev/tty \u003e \"$1\"`)
	if slow == string(manifest) {
		t.Fatalf("the fixture's task command was not found in %s", manifest)
	}
	clitest.WriteFile(t, manifestPath, slow)
	clitest.RunGit(t, root, "add", "-A")
	clitest.RunGit(t, root, "commit", "-m", "slow tasks")
	return root
}

func busyWorker(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := machineproc.Query("status"); err == nil {
			for _, w := range r.Status.Workers {
				if w.Busy {
					return w.PID
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no worker took the run in time")
	return 0
}

// TestACommandAtATerminalRunsInAWorkerWithTheTerminalItsJobsSee runs a command
// at a pseudo-terminal through a worker: the run renders for a terminal, its
// jobs read a controlling terminal of the person's size, and a resize during
// the run reaches them (CLI ADR 0116 §4).
func TestACommandAtATerminalRunsInAWorkerWithTheTerminalItsJobsSee(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	root := slowTerminalFixture(t, "2")
	startSupervisor(t)
	t.Setenv(machineproc.Env, "on")
	t.Setenv("TERM", "xterm-256color")
	term := openTerminal(t, 40, 120)

	code := term.run(t, root, "lint,test,build", "--all")
	busyWorker(t)
	// A resize while the jobs wait: the jobs read the new size.
	term.resize(t, 50, 100)
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-code:
		if got != 0 {
			t.Fatalf("exit %d at the terminal\n%s", got, term.output())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("the run did not end\n%s", term.output())
	}
	out := term.output()
	if strings.Contains(out, "running it again") {
		t.Fatalf("the run did not go through the worker:\n%s", out)
	}
	if last := machineproc.ReadLastLocal(); last != nil {
		t.Fatalf("the run ran in its own process: %+v", last)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("the run did not render for a terminal:\n%q", out)
	}
	reports, err := filepath.Glob(filepath.Join(root, ".putnami", "out", "apps", "p000", "*", "report.txt"))
	if err != nil || len(reports) != 3 {
		t.Fatalf("reports %v, %v; want 3", reports, err)
	}
	for _, report := range reports {
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(data)); got != "50 100" {
			t.Errorf("%s: a job read the terminal size %q, want the resized 50 100", report, got)
		}
	}
}

// TestCtrlCAtATerminalStopsARunInAWorker types Ctrl-C during a run that a
// worker serves: the run stops with the code a run in its own process stops
// with, and does not run again.
func TestCtrlCAtATerminalStopsARunInAWorker(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	root := slowTerminalFixture(t, "20")
	startSupervisor(t)
	t.Setenv(machineproc.Env, "on")
	term := openTerminal(t, 40, 120)

	started := time.Now()
	code := term.run(t, root, "lint,test,build", "--all")
	busyWorker(t)
	time.Sleep(time.Second)
	if _, err := term.master.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-code:
		if got != cli.ExitSignalReceived {
			t.Errorf("exit %d after Ctrl-C, want %d\n%s", got, cli.ExitSignalReceived, term.output())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the run did not stop after Ctrl-C\n%s", term.output())
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("the run took %s, want it stopped before its 20 s tasks ended", elapsed)
	}
	if out := term.output(); strings.Contains(out, "running it again") {
		t.Errorf("a stopped run ran again:\n%s", out)
	}
}
