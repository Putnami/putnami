package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The exec provider is tested against a REAL child process, not a stub.
//
// Everything it exists to get right is a process-boundary property — stdin
// framing, one document on stdout, a provider that logs where its answer
// belongs, a provider that never returns — and none of those are observable
// through an in-process fake. The child is this test binary re-executed with a
// mode env var, which is the standard Go helper-process pattern and needs no
// shell, no fixture binary, and no build step.

const probeHelperEnv = "PUTNAMI_PROBE_HELPER_MODE"

// TestProbeHelperProcess is the child. It is a real test so `go test` builds
// it, and it exits before doing anything when the mode variable is unset.
func TestProbeHelperProcess(t *testing.T) {
	mode := os.Getenv(probeHelperEnv)
	if mode == "" {
		t.Skip("helper process: not invoked as a child")
	}
	switch mode {
	case "answer":
		data, err := wsproto.DecodeProbeDocument(os.Stdin)
		if err != nil {
			os.Stderr.WriteString(err.Error()) //nolint:errcheck // child process diagnostics
			os.Exit(2)
		}
		var parsed wsproto.ProbeRequest
		if err := json.Unmarshal(data, &parsed); err != nil {
			os.Exit(2)
		}
		result := wsproto.ProbeResult{
			Version:   wsproto.ProbeProtocolVersion,
			Extension: parsed.Extension,
			Projects:  []wsproto.ProbeProject{{Path: "web", SourceName: "@acme/web"}},
		}
		encoded, _ := json.Marshal(result)
		os.Stdout.Write(append(encoded, '\n')) //nolint:errcheck // child process output
	case "fail":
		os.Stderr.WriteString("bun: command not found") //nolint:errcheck // child process diagnostics
		os.Exit(3)
	case "chatty":
		os.Stdout.WriteString(`{"version":1,"extension":"x"}` + "\ninfo: finished\n") //nolint:errcheck // child process output
	case "hang":
		time.Sleep(30 * time.Second)
	case "garbage":
		os.Stdout.WriteString("not json at all\n") //nolint:errcheck // child process output
	}
	os.Exit(0)
}

func helperProvider(t *testing.T, mode string) *ExecProbeProvider {
	t.Helper()
	return &ExecProbeProvider{
		Extension:  "@putnami/typescript",
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestProbeHelperProcess"},
		Env:        append(os.Environ(), probeHelperEnv+"="+mode),
		Dir:        t.TempDir(),
		Timeout:    30 * time.Second,
	}
}

func TestExecProbeProvider_RoundTrip(t *testing.T) {
	provider := helperProvider(t, "answer")
	result, err := provider.Probe(wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension, Paths: []string{"web"},
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Extension != provider.Extension || len(result.Projects) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Projects[0].SourceName != "@acme/web" {
		t.Errorf("sourceName = %q", result.Projects[0].SourceName)
	}
}

func TestExecProbeProvider_NonZeroExitCarriesProviderStderr(t *testing.T) {
	provider := helperProvider(t, "fail")
	_, err := provider.Probe(wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension})

	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v (%T), want a typed failure", err, err)
	}
	if failure.Kind != wsproto.ProbeFailureTransport {
		t.Errorf("kind = %q, want %q", failure.Kind, wsproto.ProbeFailureTransport)
	}
	// The provider's own stderr is usually the actual cause; dropping it leaves
	// the user with an exit status and nothing to act on.
	if !strings.Contains(failure.Message, "bun: command not found") {
		t.Errorf("message = %q, want it to carry the provider's stderr", failure.Message)
	}
}

func TestExecProbeProvider_RejectsAProviderThatLogsToStdout(t *testing.T) {
	provider := helperProvider(t, "chatty")
	_, err := provider.Probe(wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension})
	if err == nil {
		t.Fatal("a provider that logs after its result must be rejected, not silently truncated")
	}
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureTransport {
		t.Fatalf("error = %v, want a transport failure", err)
	}
}

func TestExecProbeProvider_RejectsGarbage(t *testing.T) {
	provider := helperProvider(t, "garbage")
	_, err := provider.Probe(wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension})
	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want a typed failure", err)
	}
}

func TestExecProbeProvider_TimeoutIsReportedAsATimeout(t *testing.T) {
	provider := helperProvider(t, "hang")
	provider.Timeout = 150 * time.Millisecond

	start := time.Now()
	_, err := provider.Probe(wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension})
	elapsed := time.Since(start)

	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want a typed failure", err)
	}
	// A killed process reports a generic signal error; reporting THAT would
	// send the reader looking for a crash that never happened.
	if failure.Kind != wsproto.ProbeFailureTimeout {
		t.Fatalf("kind = %q (%v), want %q", failure.Kind, failure.Message, wsproto.ProbeFailureTimeout)
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout took %v; the deadline did not bound the run", elapsed)
	}
}

func TestExecProbeProvider_MissingExecutableIsUnavailable(t *testing.T) {
	provider := &ExecProbeProvider{
		Extension:  "@putnami/typescript",
		Executable: filepath.Join(t.TempDir(), "does-not-exist"),
	}
	_, err := provider.Probe(wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension})

	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want a typed failure", err)
	}
	if failure.Kind != wsproto.ProbeFailureUnavailable {
		t.Errorf("kind = %q, want %q", failure.Kind, wsproto.ProbeFailureUnavailable)
	}
}

// Lazy resolution is what keeps a valid snapshot from paying for a compile:
// the hook must run on the first probe and never before it.
func TestExecProbeProvider_ResolveRunsOnceOnFirstProbe(t *testing.T) {
	calls := 0
	provider := helperProvider(t, "answer")
	executable := provider.Executable
	provider.Executable = ""
	provider.Resolve = func() (string, error) {
		calls++
		return executable, nil
	}
	if calls != 0 {
		t.Fatal("Resolve ran before any probe")
	}

	for range 2 {
		if _, err := provider.Probe(wsproto.ProbeRequest{
			Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension,
		}); err != nil {
			t.Fatalf("Probe: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("Resolve ran %d times, want exactly 1", calls)
	}
}

func TestExecProbeProvider_ResolveFailureIsUnavailable(t *testing.T) {
	provider := &ExecProbeProvider{
		Extension: "@putnami/typescript",
		Resolve:   func() (string, error) { return "", errors.New("prepare failed") },
	}
	_, err := provider.Probe(wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension})

	var failure *wsproto.ProbeFailure
	if !errors.As(err, &failure) || failure.Kind != wsproto.ProbeFailureUnavailable {
		t.Fatalf("error = %v, want an unavailable failure", err)
	}
	if !strings.Contains(failure.Message, "prepare failed") {
		t.Errorf("message = %q, want the preparation cause preserved", failure.Message)
	}
}

// The provider's working directory must be the workspace root: every path in
// the exchange is repo-relative, so a provider resolving them elsewhere answers
// about a different tree.
func TestExecProbeProvider_RunsInTheGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	provider := helperProvider(t, "answer")
	provider.Dir = dir

	cmd := exec.Command(provider.Executable, provider.Args...) //nolint:gosec // test helper
	cmd.Dir = provider.Dir
	if cmd.Dir != dir {
		t.Fatalf("Dir = %q, want %q", cmd.Dir, dir)
	}
	if _, err := provider.Probe(wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension,
	}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}
