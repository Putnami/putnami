package fixtureproc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	code := m.Run()
	Remove()
	os.Exit(code)
}

// Prepare builds the helper under the environment it runs in, so a Write after
// the test took go off PATH still places a program that runs.
func TestPrepareLetsAWriteRunWithoutGoOnPath(t *testing.T) {
	Prepare(t)
	t.Setenv("PATH", t.TempDir())
	if path, err := exec.LookPath("go"); err == nil {
		t.Fatalf("PATH still offers go at %s; this test guards nothing", path)
	}
	path := Write(t, filepath.Join(t.TempDir(), "tool"), Program{Stdout: "placed\n"})
	out, err := exec.Command(path).Output()
	if err != nil || string(out) != "placed\n" {
		t.Fatalf("run = %q, %v; want the placed program's output", out, err)
	}
}

// A placed program writes its streams, records its run and exits with its
// status, and Runs reads back what it was started with.
func TestWriteRunsTheProgramItDescribes(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "runs.jsonl")
	path := Write(t, filepath.Join(dir, "tool"), Program{Record: record, PathEnv: []string{"FIXTUREPROC_DIR"}, Stdout: "out\n", Stderr: "err\n", Exit: 3})
	if runtime.GOOS == "windows" && filepath.Ext(path) != ".exe" {
		t.Fatalf("path = %s, want the .exe suffix Windows runs", path)
	}

	for range 2 {
		var stdout, stderr strings.Builder
		cmd := exec.Command(path, "first", path)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "FIXTUREPROC_PROBE=seen", "FIXTUREPROC_DIR="+dir)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		var exit *exec.ExitError
		if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 3 {
			t.Fatalf("run = %v, want exit status 3", err)
		}
		if stdout.String() != "out\n" || stderr.String() != "err\n" {
			t.Fatalf("streams = %q / %q, want out / err", stdout.String(), stderr.String())
		}
	}

	runs := Runs(t, record)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	run := runs[0]
	if len(run.Args) != 2 || run.Args[0] != "first" || run.Args[1] != path {
		t.Errorf("args = %q, want first and the program's path", run.Args)
	}
	if strings.Join(run.ArgKinds, ",") != ",file" {
		t.Errorf("argument kinds = %q, want nothing and a file", run.ArgKinds)
	}
	if run.EnvKinds["FIXTUREPROC_DIR"] != "dir" {
		t.Errorf("FIXTUREPROC_DIR kind = %q, want dir", run.EnvKinds["FIXTUREPROC_DIR"])
	}
	if got, err := filepath.EvalSymlinks(run.Dir); err != nil || got != mustEvalSymlinks(t, dir) {
		t.Errorf("dir = %s, want %s", run.Dir, dir)
	}
	if value, ok := run.LookupEnv("FIXTUREPROC_PROBE"); !ok || value != "seen" {
		t.Errorf("FIXTUREPROC_PROBE = %q (set %v), want seen", value, ok)
	}
	if _, ok := run.LookupEnv("FIXTUREPROC_UNSET"); ok {
		t.Error("an unset variable was reported set")
	}
}

// A run whose arguments On names writes and exits as its Outcome says; every
// other run does what the program says.
func TestOnAnswersTheRunsItNames(t *testing.T) {
	path := Write(t, filepath.Join(t.TempDir(), "tool"), Program{
		Stdout: "default\n",
		On:     map[string]Outcome{"mod tidy": {Stdout: "tidy\n", Stderr: "unresolved\n", Exit: 1}},
	})
	for _, run := range []struct {
		args           []string
		stdout, stderr string
		exit           int
	}{
		{[]string{"mod", "tidy"}, "tidy\n", "unresolved\n", 1},
		{[]string{"mod", "edit"}, "default\n", "", 0},
		{[]string{"mod", "tidy", "-v"}, "default\n", "", 0},
	} {
		var stdout, stderr strings.Builder
		cmd := exec.Command(path, run.args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		exit := 0
		var exitErr *exec.ExitError
		if err := cmd.Run(); errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("%q: %v", run.args, err)
		}
		if stdout.String() != run.stdout || stderr.String() != run.stderr || exit != run.exit {
			t.Errorf("%q = %q / %q / exit %d, want %q / %q / exit %d",
				run.args, stdout.String(), stderr.String(), exit, run.stdout, run.stderr, run.exit)
		}
	}
}

// A warm run, the one Write makes included, does nothing the program
// describes: it records no run and exits 0 at once, whatever the description
// and the arguments say.
func TestWarmRunsTheProgramWithoutDoingWhatItDescribes(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "runs.jsonl")
	path := Write(t, filepath.Join(dir, "tool"), Program{Record: record, Stdout: "out\n", WaitFor: []string{filepath.Join(dir, "never")}, Exit: 3})
	Warm(t, path, "mod", "tidy")
	if runs := Runs(t, record); len(runs) != 0 {
		t.Fatalf("runs = %d after a warm run, want none", len(runs))
	}
}

// A held program waits for its file before it exits.
func TestWaitForHoldsTheProgram(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	path := Write(t, filepath.Join(dir, "held"), Program{WaitFor: []string{release}, Exit: 7})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("the program exited before its file existed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := <-done; !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("run = %v, want exit status 7", err)
	}
}

// A program that reads lines writes once it read them, and an input that ends
// early ends the reading instead of holding the program.
func TestReadLinesReadsTheInputBeforeTheProgramWrites(t *testing.T) {
	dir := t.TempDir()
	path := Write(t, filepath.Join(dir, "reader"), Program{ReadLines: 2, Stdout: "out\n"})
	for _, input := range []string{"first\nsecond\n", "only\n", ""} {
		cmd := exec.Command(path)
		cmd.Stdin = strings.NewReader(input)
		stdout, err := cmd.Output()
		if err != nil || string(stdout) != "out\n" {
			t.Fatalf("input %q: stdout %q, err %v; want the program to read and write out", input, stdout, err)
		}
	}
}

// A program that holds its output exits while a copy keeps its standard output
// open, so a caller that bounds the wait for the output sees it outlive the
// program; Release lets the copy go.
func TestHoldOutputKeepsTheOutputOpenAfterTheProgramExits(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	path := Write(t, filepath.Join(dir, "holder"), Program{HoldOutput: release, Stdout: "out\n"})
	var stdout strings.Builder
	cmd := exec.Command(path)
	cmd.Stdout = &stdout
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		t.Fatalf("run = %v, want the program to exit 0", err)
	}
	Release(t, release)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("run = %v, want %v: the output closed with the program", err, exec.ErrWaitDelay)
	}
	if stdout.String() != "out\n" {
		t.Fatalf("stdout = %q, want the program's own output", stdout.String())
	}
}

// Release lets the copy go while another handle holds the release file open.
// Windows refuses to delete a file that os.Create or os.WriteFile holds open,
// so a copy that removed the release file failed there, with no trace, and
// Release waited a minute for it.
func TestReleaseLetsTheCopyGoWhileTheReleaseFileIsOpen(t *testing.T) {
	dir := t.TempDir()
	releasePath := filepath.Join(dir, "release")
	path := Write(t, filepath.Join(dir, "holder"), Program{HoldOutput: releasePath})
	if err := exec.Command(path).Run(); err != nil {
		t.Fatalf("run = %v, want the program to exit 0", err)
	}
	held, err := os.Create(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	Release(t, releasePath)
}

// A copy that dies before it reports leaves its process ID behind, and
// Release names it instead of a bare timeout.
func TestReleaseNamesACopyThatDiedWithoutReporting(t *testing.T) {
	dir := t.TempDir()
	releasePath := filepath.Join(dir, "release")
	path := Write(t, filepath.Join(dir, "holder"), Program{HoldOutput: releasePath})
	if err := exec.Command(path).Run(); err != nil {
		t.Fatalf("run = %v, want the program to exit 0", err)
	}
	pid := Holder(t, releasePath)
	copyProcess, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyProcess.Kill(); err != nil {
		t.Fatalf("kill the copy (pid %d): %v", pid, err)
	}
	// On Windows this waits for the copy to go, so the test directory can be
	// removed; elsewhere the copy is no child of this process, and Wait fails.
	_, _ = copyProcess.Wait()
	// The copy is dead, so the bound only sets how soon release gives up.
	err = release(releasePath, 100*time.Millisecond)
	if want := fmt.Sprintf("the copy (pid %d) started and reported no exit status", pid); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("release = %v, want an error naming %q", err, want)
	}
}

// A copy that failed says why, and Release fails with that line at once.
func TestReleaseReportsTheExitStatusOfACopyThatFailed(t *testing.T) {
	releasePath := filepath.Join(t.TempDir(), "release")
	line := "exit 124: " + releasePath + " never appeared within 1m0s\n"
	if err := os.WriteFile(releasePath+holderExitSuffix, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	err := release(releasePath, waitForBound)
	if want := "failed: exit 124: "; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("release = %v, want an error containing %q", err, want)
	}
}

// A placed binary runs as the test binary.
func TestBinaryRunsAsTheTestBinary(t *testing.T) {
	path := Binary(t, filepath.Join(t.TempDir(), "plain"))
	out, err := exec.Command(path, "-test.run=^TestRunsOfAProgramThatNeverRanIsEmpty$", "-test.v").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestRunsOfAProgramThatNeverRanIsEmpty") {
		t.Fatalf("placed binary = %v:\n%s", err, out)
	}
}

// A warm run of a placed binary passes it the arguments its TestMain answers,
// and fails the test when it exits non-zero: a warm that did not run cannot
// pass for one that did.
func TestWarmRunsACopyWithItsArguments(t *testing.T) {
	path := Binary(t, filepath.Join(t.TempDir(), "plain"))
	Warm(t, path, "-test.run=^$")
	failed := &fatalRecorder{TB: t}
	Warm(failed, path, "-test.no-such-flag")
	if !strings.Contains(failed.fatal, "no-such-flag") {
		t.Fatalf("warm with an argument the copy refuses = %q, want a failure that names it", failed.fatal)
	}
}

// A placed script is executable, replaces a file at its path, and its warm run
// passes it args: the script records them. A script that fails its warm run
// fails the test.
func TestScriptPlacesAnExecutableAndWarmsItWithItsArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a #! script runs on unix only")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	path := Script(t, filepath.Join(dir, "bin", "runtime"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+record+"'\n", "__putnami", "runtime-info")
	if got, err := os.ReadFile(record); err != nil || string(got) != "__putnami runtime-info\n" {
		t.Fatalf("the warm run recorded %q, %v; want its arguments", got, err)
	}
	if out, err := exec.Command(path).CombinedOutput(); err != nil {
		t.Fatalf("placed script = %v:\n%s", err, out)
	}
	// A second script at the path replaces the first.
	Script(t, path, "#!/bin/sh\nprintf 'second\\n' >> '"+record+"'\n")
	if got, err := os.ReadFile(record); err != nil || !strings.HasSuffix(string(got), "second\n") {
		t.Fatalf("the replacing script recorded %q, %v; want its own line", got, err)
	}
	failed := &fatalRecorder{TB: t}
	Script(failed, filepath.Join(dir, "failing"), "#!/bin/sh\nexit 3\n")
	if !strings.Contains(failed.fatal, "exit status 3") {
		t.Fatalf("warm of a script that exits 3 = %q, want a failure that names the status", failed.fatal)
	}
}

// The GORACE options a warm run and QuietRaceExit set end with
// atexit_sleep_ms=0 and keep every other option a developer set.
func TestQuietRaceOptionsKeepsTheOtherOptions(t *testing.T) {
	for value, want := range map[string]string{
		"":                                     "atexit_sleep_ms=0",
		"halt_on_error=1 log_path=/tmp/race":   "halt_on_error=1 log_path=/tmp/race atexit_sleep_ms=0",
		"atexit_sleep_ms=0":                    "atexit_sleep_ms=0",
		"atexit_sleep_ms=500":                  "atexit_sleep_ms=500 atexit_sleep_ms=0",
		"atexit_sleep_ms=0 log_path=/tmp/race": "atexit_sleep_ms=0 log_path=/tmp/race",
	} {
		if got := quietRaceOptions(value); got != want {
			t.Errorf("quietRaceOptions(%q) = %q, want %q", value, got, want)
		}
	}
	t.Setenv("GORACE", "log_path=/tmp/race")
	QuietRaceExit()
	if got, want := os.Getenv("GORACE"), "log_path=/tmp/race atexit_sleep_ms=0"; got != want {
		t.Errorf("GORACE after QuietRaceExit = %q, want %q", got, want)
	}
}

// fatalRecorder keeps the message of a Fatalf instead of ending the test.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
}

func TestRunsOfAProgramThatNeverRanIsEmpty(t *testing.T) {
	if runs := Runs(t, filepath.Join(t.TempDir(), "absent.jsonl")); len(runs) != 0 {
		// A count, never the records: each one carries the run's whole environment.
		t.Fatalf("runs = %d, want none", len(runs))
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
