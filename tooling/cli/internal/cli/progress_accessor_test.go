package cli

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns
// everything written to stderr during the call.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()

	_ = w.Close()
	os.Stderr = orig
	out := <-done
	_ = r.Close()
	return out
}

// TestCommandEnvProgressGating verifies that CommandEnv.Progress returns a
// reporter gated on the command's output mode: inert (no stderr writes) for the
// machine renderers "jsonl" and "cloud-logging", and live for human/auto "".
// This exercises the real accessor, which targets os.Stderr, so the test
// captures os.Stderr to observe inertness vs. liveness.
func TestCommandEnvProgressGating(t *testing.T) {
	cases := []struct {
		format string
		inert  bool
	}{
		{"", false},
		{"jsonl", true},
		{"cloud-logging", true},
	}
	for _, tc := range cases {
		t.Run("format="+tc.format, func(t *testing.T) {
			env := &CommandEnv{OutputFormat: tc.format}
			out := captureStderr(t, func() {
				r := env.Progress()
				if r == nil {
					t.Error("Progress() returned nil reporter")
					return
				}
				r.Phase("provisioning")
				r.Step("upload", "running")
			})
			if tc.inert && out != "" {
				t.Fatalf("format %q: expected inert reporter, got stderr output %q", tc.format, out)
			}
			if !tc.inert && out == "" {
				t.Fatalf("format %q: expected live reporter to write to stderr, got nothing", tc.format)
			}
		})
	}
}
