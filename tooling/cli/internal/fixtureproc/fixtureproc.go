// Package fixtureproc places programs that stand in for the ones the code under
// test starts, so a fixture needs no POSIX shell and runs on every platform.
// It is test-only: nothing here may be imported by a production (non-_test.go)
// file, which keeps "testing" out of the shipped CLI binary's dependency graph.
//
// Write places a small helper program (./program) at a path, with a
// description of what the program does beside it. The helper is built once per
// test process, by the first Write or Prepare, without the race detector the
// test binary may carry: a race-instrumented binary spends about a second
// starting, on every run.
package fixtureproc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/scratch"
)

// Program is what a program placed by Write does, in this order: it appends
// the Run it was started with to Record, starts the copy HoldOutput asks for,
// reads ReadLines lines of standard input, writes Stdout, then Stderr, waits for WaitFor, sleeps for Sleep and exits
// with Exit. A run whose arguments On names writes and exits as that Outcome
// says instead. A relative path is relative to the program's working
// directory.
type Program struct {
	// Record names a file that gains one JSON line per run, which Runs reads.
	// A test uses it to observe that the program ran, how often, and with
	// which arguments, working directory and environment.
	Record string `json:"record,omitempty"`
	// PathEnv names environment variables whose value is a path, such as a
	// file the caller removes once the program exits: Run.EnvKinds records
	// what each named while the program ran.
	PathEnv []string `json:"pathEnv,omitempty"`
	// HoldOutput names a file. When set, the program starts a copy of itself
	// that inherits its standard output and error and keeps them open after
	// the program exits, until that file exists. Release creates the file and
	// waits for the copy to report that it goes.
	HoldOutput string `json:"holdOutput,omitempty"`
	// ReadLines is how many lines the program reads from standard input
	// before it writes, so a test can make it exit only after it received a
	// request. An input that ends early ends the reading.
	ReadLines int    `json:"readLines,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	// WaitFor names files the program waits to exist, so a test can hold it
	// until the caller reached a point. The program gives up after
	// waitForBound and exits with status 124.
	WaitFor []string      `json:"waitFor,omitempty"`
	Sleep   time.Duration `json:"sleep,omitempty"`
	Exit    int           `json:"exit,omitempty"`
	// On maps the arguments of a run, joined by single spaces, to what that
	// run writes and how it exits in place of Stdout, Stderr and Exit, so one
	// program can fail one subcommand and answer the others.
	On map[string]Outcome `json:"on,omitempty"`
}

// Outcome is what a run Program.On names writes and how it exits.
type Outcome struct {
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Exit   int    `json:"exit,omitempty"`
}

// Run is what one run of a program was started with.
type Run struct {
	// Args are the arguments after the program path.
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
	// ArgKinds is what each argument named while the program ran: "file",
	// "dir", or "" for neither.
	ArgKinds []string `json:"argKinds"`
	// EnvKinds is the same for each variable Program.PathEnv names.
	EnvKinds map[string]string `json:"envKinds,omitempty"`
}

// LookupEnv returns the value of the environment variable name in the run's
// environment, and whether it was set.
func (r Run) LookupEnv(name string) (string, bool) {
	for _, entry := range r.Env {
		if key, value, ok := strings.Cut(entry, "="); ok && sameEnvName(key, name) {
			return value, true
		}
	}
	return "", false
}

// Windows compares environment variable names without regard to case.
func sameEnvName(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// descriptionSuffix names the file beside a program that says what it does.
const descriptionSuffix = ".fixture.json"

// waitForBound is a hang detector, not a latency budget: a program still
// waiting after it failed to see the file it was held for.
const waitForBound = time.Minute

// holdOutputEnv names, in the environment of the copy a program starts for
// Program.HoldOutput, the file that releases the copy.
const holdOutputEnv = "PUTNAMI_FIXTUREPROC_HOLD_OUTPUT"

// The copy a program starts for Program.HoldOutput writes two files beside
// the release file: its process ID when it starts, and one line with its exit
// status and the reason right before it exits. Release waits for the second
// and names both when the copy fails. Neither side ever removes or renames a
// file the other may hold open: Windows refuses to delete a file another
// handle holds open without delete sharing, and os.WriteFile opens files that
// way, so the copy that used to remove the release file lost that race to
// Release's own write and exited with no trace.
const (
	holderStartedSuffix = ".holder-pid"
	holderExitSuffix    = ".holder-exit"
)

// warmEnv, set in a program's environment, makes it exit 0 before it does
// anything else (Warm).
const warmEnv = "PUTNAMI_FIXTUREPROC_WARM"

// Write places the helper program at path, with ".exe" appended on Windows,
// as a program that does p, warms it (Warm), and returns the program's path.
func Write(t testing.TB, path string, p Program) string {
	t.Helper()
	path = executablePath(path)
	description, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("fixtureproc: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("fixtureproc: %v", err)
	}
	if err := os.WriteFile(path+descriptionSuffix, description, 0o644); err != nil {
		t.Fatalf("fixtureproc: %v", err)
	}
	Prepare(t)
	place(t, helper.path, path)
	Warm(t, path)
	return path
}

// Warm runs the program at path once with args, and fails the test unless it
// exits 0. A host that checks a new executable file on its first launch does
// so here, after the start returns: macOS takes about 0.3 s per new file, and
// seconds under load, where the checks queue. A later run that a deadline
// bounds then measures the program alone.
//
// The run has warmEnv set, so a program Write placed exits 0 before it records
// a run, writes output or waits, whatever args are. Any other program, such as
// a copy Binary placed or a script, must answer args and do nothing else; the
// TestMain of a copy answers them without running the tests. GORACE has a
// race-enabled copy exit without the race runtime's 1 s exit sleep.
func Warm(t testing.TB, path string, args ...string) {
	t.Helper()
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), warmEnv+"=1", "GORACE=atexit_sleep_ms=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixtureproc: warm %s: %v\n%s", path, err, out)
	}
}

// Prepare builds the helper program Write places, unless a call already built
// it. The build runs go from PATH under the environment of the first call, so
// a test that takes go off PATH or moves the user home calls Prepare first.
func Prepare(t testing.TB) {
	t.Helper()
	helper.once.Do(buildHelper)
	if helper.err != nil {
		t.Fatalf("fixtureproc: %v", helper.err)
	}
}

// helper is the program Write places, built by the first call.
var helper struct {
	once sync.Once
	// dir holds the build. Keeping it reachable keeps its owner lock held.
	dir  *scratch.Dir
	path string
	err  error
}

// helperPackage is built by import path from the test's working directory,
// which is a package directory of this module.
const helperPackage = "go.putnami.dev/tooling/cli/internal/fixtureproc/program"

func buildHelper() {
	// scratch.New, not os.MkdirTemp: a test process that dies before Remove
	// leaves the build to the next run's sweep.
	dir, err := scratch.New("putnami-fixtureproc-")
	if err != nil {
		helper.err = err
		return
	}
	helper.dir = dir
	helper.path = executablePath(filepath.Join(dir.Path(), "program"))
	cmd := exec.Command("go", "build", "-o", helper.path, helperPackage)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		helper.err = fmt.Errorf("build %s: %w\n%s", helperPackage, err, out)
	}
}

// Remove removes the helper program Write built, if any. Call it in TestMain
// after m.Run.
func Remove() {
	if helper.dir != nil {
		_ = helper.dir.Remove()
	}
}

// Binary places the running test binary at path, with ".exe" appended on
// Windows, and returns its path. It runs as the test binary under another
// name, which a TestMain can tell by os.Args[0]. A test warms the copy (Warm)
// before a run that a deadline bounds.
func Binary(t testing.TB, path string) string {
	t.Helper()
	path = executablePath(path)
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("fixtureproc: locate the test binary: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("fixtureproc: %v", err)
	}
	place(t, executable, path)
	return path
}

// place puts a copy of the executable source at target. A hard link would be
// cheaper, but neither platform keeps one honest. macOS SIGKILLs some of the
// programs parallel tests start from links to one binary while they place more
// links to it; the mcp tests lost 4 runs in 12 that way and none with copies.
// Windows refuses to delete any name of a running image, so a link to this
// test binary would outlive the test's cleanup there.
func place(t testing.TB, source, target string) {
	t.Helper()
	if err := copyExecutable(source, target); err != nil {
		t.Fatalf("fixtureproc: place %s at %s: %v", source, target, err)
	}
}

func executablePath(path string) string {
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(path), ".exe") {
		return path + ".exe"
	}
	return path
}

func copyExecutable(source, target string) error {
	// Hold off this process's forks while target is open for writing: a child
	// forked meanwhile inherits the descriptor until it execs, and Linux refuses
	// to start a program any process holds open for writing (ETXTBSY).
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Runs returns the runs recorded in record, oldest first, and none when the
// file does not exist.
func Runs(t testing.TB, record string) []Run {
	t.Helper()
	data, err := os.ReadFile(record)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("fixtureproc: %v", err)
	}
	var runs []Run
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var run Run
		if err := json.Unmarshal([]byte(line), &run); err != nil {
			t.Fatalf("fixtureproc: read %s: %v", record, err)
		}
		runs = append(runs, run)
	}
	return runs
}

// Holder returns the process ID of the copy a program started for
// Program.HoldOutput, once the copy reported it. It fails the test when no copy
// reported within a minute.
func Holder(t testing.TB, release string) int {
	t.Helper()
	for deadline := time.Now().Add(waitForBound); ; time.Sleep(10 * time.Millisecond) {
		// The copy writes the whole line in one write, so a line without its
		// newline is still being written.
		data, err := os.ReadFile(release + holderStartedSuffix)
		if err == nil && strings.HasSuffix(string(data), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("fixtureproc: copy pid file = %q: %v", data, err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixtureproc: the copy never wrote %s", release+holderStartedSuffix)
		}
	}
}

// Release lets go the copy a program started for Program.HoldOutput: it
// creates the file the copy waits for, and returns once the copy reported that
// it saw the file, which the copy does right before it exits. It fails the
// test with the copy's exit status when the copy failed, and with what the
// copy left behind when it never reported.
func Release(t testing.TB, path string) {
	t.Helper()
	if err := release(path, waitForBound); err != nil {
		t.Fatalf("fixtureproc: %v", err)
	}
}

// release is Release, bounded by bound and returning its failure.
func release(path string, bound time.Duration) error {
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return err
	}
	for deadline := time.Now().Add(bound); ; time.Sleep(10 * time.Millisecond) {
		// The copy writes the whole line in one write, so a line without its
		// newline is still being written.
		if status, err := os.ReadFile(path + holderExitSuffix); err == nil && strings.HasSuffix(string(status), "\n") {
			if line := strings.TrimSuffix(string(status), "\n"); line != holderExitReleased {
				return fmt.Errorf("the copy holding the output for %s failed: %s", path, line)
			}
			return nil
		}
		if time.Now().After(deadline) {
			pid, err := os.ReadFile(path + holderStartedSuffix)
			if err != nil {
				return fmt.Errorf("no copy holding the output took %s after %s: no copy started (%w)", path, bound, err)
			}
			return fmt.Errorf("no copy holding the output took %s after %s: the copy (pid %s) started and reported no exit status, so it was killed or could not write %s",
				path, bound, strings.TrimSpace(string(pid)), path+holderExitSuffix)
		}
	}
}

// Main is the helper program: it does what the description beside its own
// path says, and exits.
func Main() {
	if os.Getenv(warmEnv) != "" {
		os.Exit(0)
	}
	if release := os.Getenv(holdOutputEnv); release != "" {
		os.Exit(holdOutput(release))
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixtureproc: locate the program: %v\n", err)
		os.Exit(125)
	}
	description, err := os.ReadFile(executable + descriptionSuffix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixtureproc: read the description: %v\n", err)
		os.Exit(125)
	}
	var p Program
	if err := json.Unmarshal(description, &p); err != nil {
		fmt.Fprintf(os.Stderr, "fixtureproc: read %s: %v\n", executable+descriptionSuffix, err)
		os.Exit(125)
	}
	if p.Record != "" {
		if err := record(p.Record, p.PathEnv); err != nil {
			fmt.Fprintf(os.Stderr, "fixtureproc: record the run: %v\n", err)
			os.Exit(125)
		}
	}
	if p.HoldOutput != "" {
		if err := startOutputHolder(executable, p.HoldOutput); err != nil {
			fmt.Fprintf(os.Stderr, "fixtureproc: hold the output: %v\n", err)
			os.Exit(125)
		}
	}
	input := bufio.NewReader(os.Stdin)
	for range p.ReadLines {
		if _, err := input.ReadString('\n'); err != nil {
			break
		}
	}
	if outcome, ok := p.On[strings.Join(os.Args[1:], " ")]; ok {
		p.Stdout, p.Stderr, p.Exit = outcome.Stdout, outcome.Stderr, outcome.Exit
	}
	_, _ = os.Stdout.WriteString(p.Stdout)
	_, _ = os.Stderr.WriteString(p.Stderr)
	deadline := time.Now().Add(waitForBound)
	for _, wait := range p.WaitFor {
		for ; ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(wait); err == nil {
				break
			}
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "fixtureproc: %s never appeared\n", wait)
				os.Exit(124)
			}
		}
	}
	time.Sleep(p.Sleep)
	os.Exit(p.Exit)
}

// startOutputHolder starts the copy of the program that holds its standard
// output and error open until release exists. The program does not wait for
// it.
func startOutputHolder(executable, release string) error {
	cmd := exec.Command(executable)
	cmd.Env = append(os.Environ(), holdOutputEnv+"="+release)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// holderExitReleased is the exit line of a copy that saw its release file.
const holderExitReleased = "exit 0"

// holdOutput is the copy: it waits for release, reports why it exits beside
// release and returns the exit status. It gives up after waitForBound with
// status 124.
func holdOutput(release string) int {
	if err := os.WriteFile(release+holderStartedSuffix, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return 125
	}
	code, line := 0, holderExitReleased
	for deadline := time.Now().Add(waitForBound); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(release); err == nil {
			break
		}
		if time.Now().After(deadline) {
			code, line = 124, fmt.Sprintf("exit 124: %s never appeared within %s", release, waitForBound)
			break
		}
	}
	if err := os.WriteFile(release+holderExitSuffix, []byte(line+"\n"), 0o644); err != nil {
		return 125
	}
	return code
}

func record(path string, pathEnv []string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	run := Run{Args: os.Args[1:], Dir: dir, Env: os.Environ()}
	for _, arg := range run.Args {
		run.ArgKinds = append(run.ArgKinds, kind(arg))
	}
	for _, name := range pathEnv {
		if run.EnvKinds == nil {
			run.EnvKinds = map[string]string{}
		}
		run.EnvKinds[name] = kind(os.Getenv(name))
	}
	line, err := json.Marshal(run)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// kind is what path names: "file", "dir", or "" for neither.
func kind(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return ""
	case info.IsDir():
		return "dir"
	case info.Mode().IsRegular():
		return "file"
	}
	return ""
}
