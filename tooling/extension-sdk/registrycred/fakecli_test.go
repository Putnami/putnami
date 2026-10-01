package registrycred

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	registryproto "go.putnami.dev/protocol/registry"
)

// fakeCLIEnv makes the test binary a fake putnami CLI: TestMain runs the
// fakeCLI the variable holds, as JSON, instead of the tests. A test sets it
// and points the seam at the test binary, so the fake runs wherever the tests
// run, with no shell.
const fakeCLIEnv = "PUTNAMI_REGISTRYCRED_FAKE_CLI"

func TestMain(m *testing.M) {
	if spec := os.Getenv(fakeCLIEnv); spec != "" {
		os.Exit(runFakeCLI(spec, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeCLI is what the fake CLI does when it runs, in field order.
type fakeCLI struct {
	// RequireEnv makes it exit 9 unless each variable holds its value.
	RequireEnv map[string]string `json:"requireEnv,omitempty"`
	// RefuseEnv makes it exit 9 when a variable holds its value.
	RefuseEnv map[string]string `json:"refuseEnv,omitempty"`
	// WordsFile receives the arguments joined by spaces, as sh's "$*".
	WordsFile string `json:"wordsFile,omitempty"`
	// LinesFile receives each argument followed by a newline.
	LinesFile string `json:"linesFile,omitempty"`
	// CountFile gains one "x" line.
	CountFile string `json:"countFile,omitempty"`
	// CwdFile receives the working directory with symbolic links resolved,
	// as pwd -P prints it.
	CwdFile string        `json:"cwdFile,omitempty"`
	Sleep   time.Duration `json:"sleep,omitempty"`
	Stdout  string        `json:"stdout,omitempty"`
	Stderr  string        `json:"stderr,omitempty"`
	Exit    int           `json:"exit,omitempty"`
}

func runFakeCLI(spec string, args []string) int {
	var fake fakeCLI
	if err := json.Unmarshal([]byte(spec), &fake); err != nil {
		fmt.Fprintf(os.Stderr, "fake CLI: %v\n", err)
		return 125
	}
	for name, want := range fake.RequireEnv {
		if got := os.Getenv(name); got != want {
			fmt.Fprintf(os.Stderr, "fake CLI: %s is %q, want %q\n", name, got, want)
			return 9
		}
	}
	for name, refused := range fake.RefuseEnv {
		if os.Getenv(name) == refused {
			fmt.Fprintf(os.Stderr, "fake CLI: %s is %q\n", name, refused)
			return 9
		}
	}
	var lines strings.Builder
	for _, arg := range args {
		lines.WriteString(arg + "\n")
	}
	writes := []struct{ path, content string }{
		{fake.WordsFile, strings.Join(args, " ")},
		{fake.LinesFile, lines.String()},
	}
	if fake.CwdFile != "" {
		wd, err := os.Getwd()
		if err == nil {
			wd, err = filepath.EvalSymlinks(wd)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake CLI: %v\n", err)
			return 125
		}
		writes = append(writes, struct{ path, content string }{fake.CwdFile, wd + "\n"})
	}
	for _, write := range writes {
		if write.path == "" {
			continue
		}
		if err := os.WriteFile(write.path, []byte(write.content), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "fake CLI: %v\n", err)
			return 125
		}
	}
	if fake.CountFile != "" {
		counter, err := os.OpenFile(fake.CountFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			_, err = counter.WriteString("x\n")
			if closeErr := counter.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake CLI: %v\n", err)
			return 125
		}
	}
	time.Sleep(fake.Sleep)
	fmt.Fprint(os.Stdout, fake.Stdout)
	fmt.Fprint(os.Stderr, fake.Stderr)
	return fake.Exit
}

// fakeCLIExecutable makes the test binary behave as fake when a child of this
// test runs it, and returns its absolute path.
func fakeCLIExecutable(t *testing.T, fake fakeCLI) string {
	t.Helper()
	spec, err := json.Marshal(fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeCLIEnv, string(spec))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

// fakeCloudCLI points cloudCLIName at the fake CLI, so the seam resolves
// through a controlled fake instead of a real `putnami` on PATH.
func fakeCloudCLI(t *testing.T, fake fakeCLI) {
	t.Helper()
	t.Setenv(registryproto.CLIExecutableEnv, "")
	executable := fakeCLIExecutable(t, fake)
	prev := cloudCLIName
	t.Cleanup(func() { cloudCLIName = prev })
	cloudCLIName = executable
}
