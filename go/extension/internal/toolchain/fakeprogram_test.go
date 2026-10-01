package toolchain

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A fake program is a copy of this test binary with a spec file beside it,
// named <program>+fakeSpecSuffix. TestMain reads the spec and behaves as it
// says instead of running the tests, so the fake runs on every host a test
// runs on, Windows included, where a shell script does not.
const fakeSpecSuffix = ".fake.json"

// fakeParentEnv marks the processes this test binary starts. A child that
// finds no spec and runs no test is a fake that lost its spec: it fails
// instead of running the whole suite again.
const fakeParentEnv = "PUTNAMI_TOOLCHAIN_FAKE_PARENT"

// fakeProgram is what a fake program does when it runs.
type fakeProgram struct {
	// RequireEnv maps a variable to the only value the program accepts. Any
	// other value prints "<name> must be <value>" and exits 40.
	RequireEnv map[string]string `json:"requireEnv,omitempty"`
	// Answers are tried in order; the first that matches the arguments runs.
	Answers []fakeAnswer `json:"answers,omitempty"`
	// Otherwise runs when no answer matches.
	Otherwise fakeAnswer `json:"otherwise"`
}

// fakeAnswer is one reply of a fake program.
type fakeAnswer struct {
	// Args start the arguments the answer matches, or are all of them when
	// Exact is set.
	Args   []string `json:"args,omitempty"`
	Exact  bool     `json:"exact,omitempty"`
	Stdout string   `json:"stdout,omitempty"`
	Stderr string   `json:"stderr,omitempty"`
	Exit   int      `json:"exit,omitempty"`
}

func (a fakeAnswer) matches(args []string) bool {
	if a.Exact {
		return slices.Equal(args, a.Args)
	}
	return len(args) >= len(a.Args) && slices.Equal(args[:len(a.Args)], a.Args)
}

func TestMain(m *testing.M) {
	if exit, ok := runFakeProgram(os.Args[1:], os.Stdout, os.Stderr); ok {
		os.Exit(exit)
	}
	if err := os.Setenv(fakeParentEnv, "1"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// runFakeProgram behaves as the fake program this binary is, and reports false
// when it is the test binary itself.
func runFakeProgram(args []string, stdout, stderr io.Writer) (int, bool) {
	self, err := os.Executable()
	if err != nil {
		return 0, false
	}
	data, err := os.ReadFile(self + fakeSpecSuffix)
	if err != nil {
		if os.Getenv(fakeParentEnv) != "" && !slices.ContainsFunc(args, func(arg string) bool {
			return strings.HasPrefix(arg, "-test.")
		}) {
			fmt.Fprintf(stderr, "fake program %s has no spec: %v\n", self, err)
			return 2, true
		}
		return 0, false
	}
	var program fakeProgram
	if err := json.Unmarshal(data, &program); err != nil {
		fmt.Fprintf(stderr, "fake program %s: %v\n", self, err)
		return 2, true
	}
	for name, want := range program.RequireEnv {
		if os.Getenv(name) != want {
			fmt.Fprintf(stderr, "%s must be %s\n", name, want)
			return 40, true
		}
	}
	answer := program.Otherwise
	for _, candidate := range program.Answers {
		if candidate.matches(args) {
			answer = candidate
			break
		}
	}
	_, _ = io.WriteString(stdout, answer.Stdout)
	_, _ = io.WriteString(stderr, answer.Stderr)
	return answer.Exit, true
}

// writeFakeProgram places a fake program at path and returns path. Unix gets a
// hard link to the test binary where the file system allows one; Windows
// always gets a copy, because a hard link to the running test binary cannot be
// deleted while it runs and the test's TempDir cleanup would fail. A symbolic
// link is never used: Linux reports the link's target as the executable, so the
// program would not find its spec.
func writeFakeProgram(t *testing.T, path string, program fakeProgram) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" || os.Link(self, path) != nil {
		data, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+fakeSpecSuffix, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// printing is a fake program that prints stdout for any arguments.
func printing(stdout string) fakeProgram {
	return fakeProgram{Otherwise: fakeAnswer{Stdout: stdout}}
}

// A fake program answers from its spec, and a fake that lost its spec fails
// instead of running this suite again.
func TestFakeProgramAnswersFromItsSpec(t *testing.T) {
	dir := t.TempDir()
	program := writeFakeProgram(t, filepath.Join(dir, goBinaryName()), fakeProgram{
		RequireEnv: map[string]string{"GOTOOLCHAIN": "local"},
		Answers: []fakeAnswer{
			{Args: []string{"env", "GOVERSION"}, Stdout: "go1.26.1\n"},
			{Args: []string{"version", "--short"}, Exact: true, Stdout: "short\n"},
		},
		Otherwise: fakeAnswer{Stderr: "unexpected arguments\n", Exit: 41},
	})
	run := func(env []string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(program, args...)
		cmd.Env = append(os.Environ(), env...)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	local := []string{"GOTOOLCHAIN=local"}
	for _, tc := range []struct {
		env  []string
		args []string
		out  string
		exit int
	}{
		{local, []string{"env", "GOVERSION"}, "go1.26.1\n", 0},
		{local, []string{"version", "--short"}, "short\n", 0},
		{local, []string{"version", "--short", "extra"}, "unexpected arguments\n", 41},
		{local, []string{"build"}, "unexpected arguments\n", 41},
		{[]string{"GOTOOLCHAIN=auto"}, []string{"env", "GOVERSION"}, "GOTOOLCHAIN must be local\n", 40},
	} {
		if out, exit := run(tc.env, tc.args...); out != tc.out || exit != tc.exit {
			t.Errorf("%v %q = (%q, %d), want (%q, %d)", tc.env, tc.args, out, exit, tc.out, tc.exit)
		}
	}

	if err := os.Remove(program + fakeSpecSuffix); err != nil {
		t.Fatal(err)
	}
	if out, exit := run(local, "env", "GOVERSION"); exit != 2 || !strings.Contains(out, "has no spec") {
		t.Errorf("a fake without a spec = (%q, %d), want exit 2 naming the missing spec", out, exit)
	}
}
