// Package clitest holds the test-only helpers the end-to-end packages under
// internal/cli/e2e share: driving one real CLI invocation (cli.NewApp →
// App.Run) inside a fixture workspace while both process streams are
// captured, the placement fixture those invocations run against, and the
// runner conformance harness (runnerfixture.go, portable.go) that the three
// portable-execution packages re-exec their test binaries through.
//
// Those tests swap os.Stdout/os.Stderr, change the working directory and set
// the environment, all process-wide, so Go runs them one after another within
// their test binary. Each e2e package is its own binary, which is what keeps
// their 5-11 s sessions from holding internal/cli's 300 other tests serial.
// A test that needs any of the three lives in an e2e package, never
// in internal/cli; internal/cli's serial-test ratchet counts what is left.
//
// Nothing here may be imported by a production (non-_test.go) file, for the
// reason internal/commands/sharedtest gives: it keeps "testing" out of the
// shipped binary's dependency graph. It imports internal/cli, so package cli's
// own tests cannot import it back (that would be an import cycle); they keep
// their unexported copies of the small helpers instead.
package clitest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// Main is the TestMain body of an e2e package. It drops a hosted run's
// invocation broker before any test runs: the broker wins over every
// authored route while answering 401 to a test archive, so a test that
// exercises it sets it with t.Setenv. internal/cli's own TestMain does the
// same for the package the e2e tests were carved out of.
func Main(m *testing.M) int {
	_ = os.Unsetenv(extension.PrivatePutRegistryURLEnv)
	return m.Run()
}

// RequireShell skips the test where /bin/sh, which every fixture task is
// written for, is unavailable.
func RequireShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
}

// WriteFile writes content at path, creating the parent directories.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// CaptureStdoutStderr captures both standard streams produced by fn, stdout
// first. It swaps the real *os.File descriptors, so output a spawned engine or
// provider child inherits is captured too, not only writes routed through an
// io.Writer the code under test was handed.
func CaptureStdoutStderr(t *testing.T, fn func()) string {
	t.Helper()
	stdout, stderr := CaptureStreams(t, fn)
	return stdout + stderr
}

// CaptureStreams is CaptureStdoutStderr with the two streams kept apart, for a
// test that pins what reaches each one.
func CaptureStreams(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		_ = wOut.Close()
		_ = rOut.Close()
		t.Fatalf("os.Pipe stderr: %v", err)
	}
	os.Stdout = wOut
	os.Stderr = wErr
	defer func() {
		os.Stdout = origOut
		os.Stderr = origErr
		_ = wOut.Close()
		_ = wErr.Close()
		_ = rOut.Close()
		_ = rErr.Close()
	}()

	outDone := sharedtest.DrainCapturedStream(rOut)
	errDone := sharedtest.DrainCapturedStream(rErr)
	fn()

	os.Stdout = origOut
	os.Stderr = origErr
	if err := wOut.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := wErr.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	out := <-outDone
	if out.Err != nil {
		t.Fatalf("read stdout: %v", out.Err)
	}
	errBuf := <-errDone
	if errBuf.Err != nil {
		t.Fatalf("read stderr: %v", errBuf.Err)
	}
	return string(out.Data), string(errBuf.Data)
}

// RunGateArgs drives one arbitrary invocation of the real CLI in the fixture
// workspace, so a scenario can narrow the selection the way a CI run does. It
// returns the exit code and both captured streams.
func RunGateArgs(t *testing.T, wsRoot string, args ...string) (int, string) {
	t.Helper()
	return RunGateArgsContext(t, context.Background(), wsRoot, args...)
}

// RunGateArgsContext is RunGateArgs with a caller-owned context, so a test can
// interrupt the run the way the shipped binary's signal handler does.
func RunGateArgsContext(t *testing.T, ctx context.Context, wsRoot string, args ...string) (int, string) {
	t.Helper()
	var code int
	output := CaptureStdoutStderr(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code = app.Run(ctx, args)
	})
	return code, output
}

// InitGitRepo turns dir into a repository with one commit on main, so the
// CLI can bind a tree and capture a source from it.
func InitGitRepo(t *testing.T, dir string) {
	t.Helper()
	RunGit(t, dir, "init")
	RunGit(t, dir, "config", "user.email", "test@test.com")
	RunGit(t, dir, "config", "user.name", "Test")
	RunGit(t, dir, "config", "commit.gpgsign", "false")
	RunGit(t, dir, "add", "-A")
	RunGit(t, dir, "commit", "-m", "initial")
	RunGit(t, dir, "branch", "-M", "main")
}

// RunGit runs git in dir with a fixed identity and fails the test on error.
func RunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// GitOutput runs git in dir and returns its stdout.
func GitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// WhereFixture writes the placement fixture: a workspace whose one project
// runs a shell gate task under every gate command, recording each execution
// in app/calls. With fail the task exits 7; with provider the extension also
// declares the reserved runner provider command, as an installed provider
// that cannot execute anything.
func WhereFixture(t *testing.T, fail, provider bool) string {
	t.Helper()
	root := t.TempDir()
	WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"placement-fixture","includes":["extension","app"]}`)
	WriteFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/gate"]}`)
	WriteFile(t, filepath.Join(root, "app", "gate.project"), "")
	WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/gate"}`)
	WriteFile(t, filepath.Join(root, "extension", "gate.sh"), `echo ran >> "$PUTNAMI_PROJECT_ROOT/calls"
if [ -f "$PUTNAMI_PROJECT_ROOT/fail" ]; then
  echo "intentional placement fixture failure" >&2
  exit 7
fi
`)
	commands := map[string]extensionproto.CommandDefinition{}
	for _, command := range []string{"lint", "test", "build", "validate", "validate-workspace", "format"} {
		commands[command] = extensionproto.CommandDefinition{ActivationFiles: []string{"gate.project"}, Run: []extensionproto.PipelineStep{{ID: "check", Task: "check"}}}
	}
	if provider {
		commands["runner-provider"] = extensionproto.CommandDefinition{Description: "Unimplemented execution capability", Run: []extensionproto.PipelineStep{{ID: "provider", Task: "check"}}}
	}
	disabled := false
	manifest := extensionproto.Manifest{
		Name: "@fixture/gate", Version: "1.0.0", CLIContract: protocolcli.CurrentContract, Commands: commands,
		// The task declares resources so a portable request carries reads and
		// writes and a submit envelope reaches its deepest admitted level.
		Tasks: map[string]extensionproto.TaskDefinition{"check": {
			Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/gate.sh"}, Cache: &extensionproto.TaskCachePolicy{Enabled: &disabled}, TimeoutMs: 60000,
			Reads:  []extensionproto.ResourceRef{{ID: extensionproto.ResourceIDSources}},
			Writes: []extensionproto.ResourceRef{{ID: "calls", Scope: extensionproto.ResourceScopeWorkspace}},
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), string(data))
	if fail {
		WriteFile(t, filepath.Join(root, "app", "fail"), "")
	}
	return root
}
