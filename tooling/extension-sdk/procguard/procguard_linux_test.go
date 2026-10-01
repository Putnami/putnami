//go:build linux

package procguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The test runs DenyInspection in a child of the test binary, so the test
// process itself stays dumpable. The child starts a grandchild that reports
// its own flag and whether it can read the child's /proc/<pid>/environ.
const (
	helperRoleEnv   = "PROCGUARD_HELPER_ROLE"
	helperTargetEnv = "PROCGUARD_HELPER_TARGET"
	helperPrefix    = "procguard: "
)

func dumpable(t *testing.T) int {
	t.Helper()
	value, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		t.Fatalf("PR_GET_DUMPABLE: %v", err)
	}
	return value
}

func helperCommand(role string, env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcguardHelper$")
	cmd.Env = append(os.Environ(), helperRoleEnv+"="+role)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// TestProcguardHelper is not a test: it is one role of the child processes
// the test below starts, and it returns at once unless a test started it as
// one.
func TestProcguardHelper(t *testing.T) {
	switch os.Getenv(helperRoleEnv) {
	case "deny":
		before := dumpable(t)
		err := DenyInspection()
		after := dumpable(t)
		probe := helperCommand("probe", helperTargetEnv+"="+strconv.Itoa(os.Getpid()))
		out, probeErr := probe.Output()
		fmt.Printf("%sbefore=%d after=%d err=%v probe-err=%v\n", helperPrefix, before, after, err, probeErr)
		_, _ = os.Stdout.Write(out)
	case "probe":
		_, err := os.ReadFile("/proc/" + os.Getenv(helperTargetEnv) + "/environ")
		environ := "readable"
		switch {
		case errors.Is(err, fs.ErrPermission):
			environ = "denied"
		case err != nil:
			environ = strings.ReplaceAll(err.Error(), " ", "_")
		}
		fmt.Printf("%schild=%d environ=%s\n", helperPrefix, dumpable(t), environ)
	}
}

func TestDenyInspectionMarksOnlyTheCallingProcess(t *testing.T) {
	t.Parallel()
	out, err := helperCommand("deny").Output()
	if err != nil {
		t.Fatalf("run the helper: %v\n%s", err, out)
	}
	report := map[string]string{}
	for line := range strings.Lines(string(out)) {
		fields, ok := strings.CutPrefix(strings.TrimSpace(line), helperPrefix)
		if !ok {
			continue
		}
		for field := range strings.FieldsSeq(fields) {
			key, value, _ := strings.Cut(field, "=")
			report[key] = value
		}
	}
	want := map[string]string{"before": "1", "after": "0", "err": "<nil>", "probe-err": "<nil>", "child": "1"}
	for key, value := range want {
		if report[key] != value {
			t.Errorf("%s = %q, want %q\n%s", key, report[key], value, out)
		}
	}
	// A process with CAP_SYS_PTRACE, such as root on most hosts, reads the
	// environment of a non-dumpable process; nobody else does.
	if os.Geteuid() != 0 && report["environ"] != "denied" {
		t.Errorf("a process of the same user read /proc/<pid>/environ of a non-dumpable process: %q\n%s", report["environ"], out)
	}
	if got := dumpable(t); got != 1 {
		t.Errorf("the test process is not dumpable (PR_GET_DUMPABLE = %d)", got)
	}
}
