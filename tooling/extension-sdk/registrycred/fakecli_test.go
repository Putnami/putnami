package registrycred

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// A loaded host can take longer than the production bound to start a fake
	// CLI, and the call would then read as a timeout no test asked for. A test
	// of the bound lowers it itself.
	materializeTimeout = 2 * time.Minute
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
	CwdFile string `json:"cwdFile,omitempty"`
	// OrphanSleep starts a copy of the test binary that sleeps this long
	// holding the fake's stdout and stderr, as a credential helper or a
	// browser the cloud CLI starts and leaves behind would. OrphanPidFile
	// receives its pid, so the test can kill it.
	OrphanSleep   time.Duration `json:"orphanSleep,omitempty"`
	OrphanPidFile string        `json:"orphanPidFile,omitempty"`
	Sleep         time.Duration `json:"sleep,omitempty"`
	Stdout        string        `json:"stdout,omitempty"`
	Stderr        string        `json:"stderr,omitempty"`
	Exit          int           `json:"exit,omitempty"`
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
	if fake.OrphanSleep > 0 {
		if err := startOrphan(fake); err != nil {
			fmt.Fprintf(os.Stderr, "fake CLI: %v\n", err)
			return 125
		}
	}
	time.Sleep(fake.Sleep)
	fmt.Fprint(os.Stdout, fake.Stdout)
	fmt.Fprint(os.Stderr, fake.Stderr)
	return fake.Exit
}

// startOrphan starts the sleeping grandchild fake.OrphanSleep asks for, with
// the fake's own stdout and stderr, and records its pid in fake.OrphanPidFile.
// The grandchild runs the test binary as a fake that only sleeps: the fake
// CLI variable is replaced, not appended, since the first value wins.
func startOrphan(fake fakeCLI) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	spec, err := json.Marshal(fakeCLI{Sleep: fake.OrphanSleep})
	if err != nil {
		return err
	}
	orphan := exec.Command(executable)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, fakeCLIEnv+"=") {
			orphan.Env = append(orphan.Env, entry)
		}
	}
	orphan.Env = append(orphan.Env, fakeCLIEnv+"="+string(spec))
	orphan.Stdout, orphan.Stderr = os.Stdout, os.Stderr
	if err := orphan.Start(); err != nil {
		return err
	}
	if fake.OrphanPidFile == "" {
		return nil
	}
	return os.WriteFile(fake.OrphanPidFile, []byte(strconv.Itoa(orphan.Process.Pid)), 0o600)
}

// stuckHelperCLI is a fake CLI that starts a grandchild holding its pipes and
// then sleeps past every bound a test sets, as a cloud CLI stuck behind a
// credential helper would. The grandchild is killed when the test ends.
func stuckHelperCLI(t *testing.T) fakeCLI {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "orphan-pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})
	return fakeCLI{OrphanSleep: time.Minute, OrphanPidFile: pidFile, Sleep: time.Minute}
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
