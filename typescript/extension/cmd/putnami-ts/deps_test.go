package main

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
)

// withMockWsExec swaps wsExecRunFunc for the duration of a test.
func withMockWsExec(t *testing.T, fn func(string, []string, ...exec.Option) (*exec.Result, error)) {
	t.Helper()
	orig := wsExecRunFunc
	t.Cleanup(func() { wsExecRunFunc = orig })
	wsExecRunFunc = fn
}

// ---- runBunWithTimeout ----

func TestRunBunWithTimeout_PassesResultThrough(t *testing.T) {
	withMockWsExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		if name != "bun" {
			t.Errorf("expected bun, got %q", name)
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: "ok"}, nil
	})

	result, err := runBunWithTimeout("bun install", "bun", []string{"install"}, t.TempDir(), bunNetworkTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("expected successful result, got %+v", result)
	}
}

func TestRunBunWithTimeout_SuppliesContextOption(t *testing.T) {
	var gotOpts int
	withMockWsExec(t, func(_ string, _ []string, opts ...exec.Option) (*exec.Result, error) {
		gotOpts = len(opts)
		return &exec.Result{Success: true}, nil
	})

	if _, err := runBunWithTimeout("bun install", "bun", []string{"install"}, t.TempDir(), bunNetworkTimeout); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Dir + WithContext = 2 options.
	if gotOpts != 2 {
		t.Errorf("expected 2 exec options (Dir, WithContext), got %d", gotOpts)
	}
}

func TestRunBunWithTimeout_PropagatesExecError(t *testing.T) {
	sentinel := context.Canceled // any non-nil, non-deadline error
	withMockWsExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return nil, sentinel
	})

	_, err := runBunWithTimeout("bun update", "bun", []string{"update"}, t.TempDir(), bunNetworkTimeout)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	// A non-timeout failure must surface as-is, not as a "timed out" message.
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("non-timeout error should not be reported as timeout, got %v", err)
	}
}

func TestRunBunWithTimeout_ReportsTimeout(t *testing.T) {
	// A zero timeout makes the context deadline deterministic without relying
	// on wall-clock scheduling under parallel test load.
	withMockWsExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true}, nil
	})

	_, err := runBunWithTimeout("bun install", "bun", []string{"install"}, t.TempDir(), 0)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "bun install timed out after") {
		t.Errorf("expected 'bun install timed out after' message, got %v", err)
	}
}
