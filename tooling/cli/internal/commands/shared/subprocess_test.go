package shared

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// childRoleEnv makes this test binary stand in for the child a test runs, so
// those tests need no POSIX tool: "echo" prints its arguments and "true" exits
// 0.
const childRoleEnv = "PUTNAMI_SHARED_TEST_CHILD"

func TestMain(m *testing.M) {
	switch os.Getenv(childRoleEnv) {
	case "echo":
		fmt.Println(strings.Join(os.Args[1:], " "))
		os.Exit(0)
	case "true":
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// child returns this test binary and an environment that runs it as role.
func child(role string) (string, []string) {
	return os.Args[0], append(os.Environ(), childRoleEnv+"="+role)
}

func TestRunGroupCombined_ReturnsOutput(t *testing.T) {
	name, env := child("echo")
	out, err := RunGroupCombined(context.Background(), "", env, name, "hello-group")
	if err != nil {
		t.Fatalf("RunGroupCombined: %v", err)
	}
	if !strings.Contains(string(out), "hello-group") {
		t.Errorf("output = %q, want hello-group", out)
	}
}

// The output combines both streams in the order the child wrote them, as
// cmd.CombinedOutput does, and is returned with a failed exit too.
func TestRunGroupCombined_CombinesStderrInOrder(t *testing.T) {
	out, err := RunGroupCombined(context.Background(), "", os.Environ(), "sh", "-c", "echo first; echo second >&2; echo third; exit 3")
	if err == nil {
		t.Fatal("RunGroupCombined of a failing command returned no error")
	}
	if got, want := string(out), "first\nsecond\nthird\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestRunGroupStreaming_Succeeds(t *testing.T) {
	// The "true" child exits 0; the helper should return nil and not block.
	name, env := child("true")
	if err := RunGroupStreaming(context.Background(), "", env, name); err != nil {
		t.Fatalf("RunGroupStreaming(true): %v", err)
	}
}

func TestRunGroupCombined_CancellationAbortsChild(t *testing.T) {
	// A child that ignores nothing: SIGTERM (sent by cmd.Cancel on ctx cancel)
	// terminates `sleep`, so the call returns well before the sleep elapses.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := RunGroupCombined(ctx, "", os.Environ(), "sleep", "30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from canceled sleep, got nil")
	}
	// Must abort promptly on cancellation, not run the full 30s sleep nor wait
	// out the kill-delay grace (SIGTERM already stops sleep).
	if elapsed > 10*time.Second {
		t.Errorf("cancellation took %s, want prompt abort", elapsed)
	}
}
