package exec

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// helperArg makes the test binary stand in for a small Unix program, so the
// suite runs where echo, false, pwd, cat and sleep are not programs, as on
// Windows: `<test binary> helperArg <program> <args>` behaves as that program.
const helperArg = "-putnami-exec-test-helper"

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == helperArg {
		os.Exit(runHelper(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

// runHelper runs program with args and returns its exit code.
func runHelper(program string, args []string) int {
	switch program {
	case "echo":
		fmt.Println(strings.Join(args, " "))
		return 0
	case "false":
		return 1
	case "pwd":
		wd, err := os.Getwd()
		if err != nil {
			return 2
		}
		fmt.Println(wd)
		return 0
	case "cat":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 2
		}
		return 0
	case "sleep":
		d, err := time.ParseDuration(args[0])
		if err != nil {
			return 2
		}
		time.Sleep(d)
		return 0
	}
	return 2
}

// helper returns the name and arguments that make Run start the test binary
// as program.
func helper(t *testing.T, program string, args ...string) (string, []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe, append([]string{helperArg, program}, args...)
}

func TestRun_Success(t *testing.T) {
	name, args := helper(t, "echo", "hello")
	result, err := Run(name, args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Success {
		t.Error("Success = false, want true")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello\n")
	}
}

func TestRun_Failure(t *testing.T) {
	name, args := helper(t, "false")
	result, err := Run(name, args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Success {
		t.Error("Success = true, want false")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
}

func TestRun_Dir(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "subprocess-control", "the-working-directory-is-honored")
	dir := t.TempDir()
	name, args := helper(t, "pwd")
	result, err := Run(name, args, Dir(dir))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The child may name the directory through a symbolic link that dir
	// crosses, as /var and /private/var on macOS.
	got, err := os.Stat(strings.TrimSuffix(result.Stdout, "\n"))
	if err != nil {
		t.Fatalf("Stdout = %q: %v", result.Stdout, err)
	}
	want, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, want) {
		t.Errorf("Stdout = %q, want %s", result.Stdout, dir)
	}
}

func TestRun_Env(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "subprocess-control", "an-explicit-environment-is-honored")
	result, err := Run("sh", []string{"-c", "echo $TEST_VAR"}, Env(map[string]string{"TEST_VAR": "hello"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello\n")
	}
}

// UnsetEnv is the only way to spawn a child that does NOT see a variable the
// extension process inherited. Env alone cannot express it.
func TestRun_UnsetEnv(t *testing.T) {
	t.Setenv("PUTNAMI_EXEC_TEST_A", "inherited")

	result, err := Run("sh", []string{"-c", "echo [$PUTNAMI_EXEC_TEST_A]"}, UnsetEnv("PUTNAMI_EXEC_TEST_A"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Stdout != "[]\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "[]\n")
	}
}

func TestRun_UnsetEnv_PreservesOtherVariables(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "subprocess-control", "an-unset-list-preserves-every-other-variable")
	t.Setenv("PUTNAMI_EXEC_TEST_A", "dropped")
	t.Setenv("PUTNAMI_EXEC_TEST_B", "kept")

	result, err := Run("sh", []string{"-c", "echo [$PUTNAMI_EXEC_TEST_A][$PUTNAMI_EXEC_TEST_B]"},
		UnsetEnv("PUTNAMI_EXEC_TEST_A"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Stdout != "[][kept]\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "[][kept]\n")
	}
}

// An explicit Env value wins over UnsetEnv: the filter applies to the inherited
// environment, the additions come after it.
func TestRun_UnsetEnv_ExplicitEnvWins(t *testing.T) {
	t.Setenv("PUTNAMI_EXEC_TEST_A", "inherited")

	result, err := Run("sh", []string{"-c", "echo [$PUTNAMI_EXEC_TEST_A]"},
		UnsetEnv("PUTNAMI_EXEC_TEST_A"),
		Env(map[string]string{"PUTNAMI_EXEC_TEST_A": "explicit"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Stdout != "[explicit]\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "[explicit]\n")
	}
}

func TestRun_UnsetEnv_AbsentVariableIsNoop(t *testing.T) {
	t.Setenv("PUTNAMI_EXEC_TEST_B", "kept")

	result, err := Run("sh", []string{"-c", "echo [$PUTNAMI_EXEC_TEST_B]"},
		UnsetEnv("PUTNAMI_EXEC_TEST_NEVER_SET"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Stdout != "[kept]\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "[kept]\n")
	}
}

func TestFilterEnv(t *testing.T) {
	env := []string{"A=1", "B=2", "MALFORMED", "C=3"}

	t.Run("returns the input untouched when nothing is unset", func(t *testing.T) {
		got := filterEnv(env, nil)
		if len(got) != len(env) {
			t.Fatalf("filterEnv() = %v, want %v", got, env)
		}
	})

	t.Run("drops named keys and keeps non-assignments", func(t *testing.T) {
		got := filterEnv(env, []string{"B", "MISSING"})
		want := []string{"A=1", "MALFORMED", "C=3"}
		if len(got) != len(want) {
			t.Fatalf("filterEnv() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("filterEnv() = %v, want %v", got, want)
			}
		}
	})
}

func TestRun_Stdin(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "subprocess-control", "stdin-is-forwarded")
	name, args := helper(t, "cat")
	result, err := Run(name, args, Stdin("hello input"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Stdout != "hello input" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello input")
	}
}

func TestRun_Timeout(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "subprocess-control", "a-timeout-is-honored")
	name, args := helper(t, "sleep", "10s")
	result, err := Run(name, args, Timeout(50*time.Millisecond))
	if err != nil {
		// Context cancellation may return an error
		return
	}
	if result.Success {
		t.Error("Run with timeout should not succeed")
	}
}

func TestRun_Context(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "subprocess-control", "an-external-cancellation-is-honored")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	name, args := helper(t, "sleep", "10s")
	_, err := Run(name, args, WithContext(ctx))
	if err == nil {
		t.Error("Run with canceled context should return error")
	}
}

func TestRun_NotFound(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "subprocess-control", "a-missing-binary-is-an-error-not-a-failed-run")
	_, err := Run("nonexistent-binary-xyz", nil)
	if err == nil {
		t.Error("Run with nonexistent binary should return error")
	}
}

func TestRun_Stderr(t *testing.T) {
	result, err := Run("sh", []string{"-c", "echo err >&2"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Stderr != "err\n" {
		t.Errorf("Stderr = %q, want %q", result.Stderr, "err\n")
	}
}
