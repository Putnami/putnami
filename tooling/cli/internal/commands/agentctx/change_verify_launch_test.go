package agentctx

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	gitexec "go.putnami.dev/tooling/cli/internal/git"
)

// The build skill runs `putnami change verify`, which the installed
// @putnami/contributor serves through an interactive launch. For one invocation
// in one workspace, the command prints the same stdout and exits with the same
// code as the contributor executable run directly in the workspace root with
// the arguments putnami hands it and no host channel. The cases pin what only
// the launch decides: an inline `--flag=value` reaches the executable, a
// global output flag does not and leaves the document as it is, the executable
// starts in the workspace root whatever directory the user runs putnami from,
// and its exit code is the command's.
func TestChangeVerifyThroughPutnamiRunsTheContributorExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness launcher requires bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("change verify requires git: %v", err)
	}
	harness := collaborationHarness(t)
	root := t.TempDir()
	config, err := json.Marshal(struct {
		Name       string   `json:"name"`
		Extensions []string `json:"extensions"`
	}{"change-verify-fixture", []string{filepath.ToSlash(filepath.Join(harness, "contributor"))}})
	if err != nil {
		t.Fatal(err)
	}
	writePathFile(t, root, "putnami.workspace.json", string(config))
	writePathFile(t, root, ".gitignore", ".putnami/\n")
	writePathFile(t, root, "README.md", "# Verify fixture\n")
	writePathFile(t, root, "docs/guide.md", "# Guide\n")
	env := append(os.Environ(), "PATH="+harness+string(os.PathListSeparator)+os.Getenv("PATH"))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitexec.Executable(), args...)
		cmd.Dir = root
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Verify Test")
	git("config", "user.email", "verify-test@example.com")
	git("config", "commit.gpgSign", "false")
	git("add", ".")
	git("commit", "-m", "docs: add the verify fixture")

	run := func(cmd *exec.Cmd) (string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return stdout.String(), 0
		case errors.As(err, &exit):
			return stdout.String(), exit.ExitCode()
		}
		t.Fatalf("%v did not run: %v: %s", cmd.Args, err, stderr.String())
		return "", 0
	}
	putnami := func(dir string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(filepath.Join(harness, "putnami"), args...)
		cmd.Dir = filepath.Join(root, filepath.FromSlash(dir))
		cmd.Env = env
		return run(cmd)
	}
	// The executable run directly has no host channel: neither variable
	// reaches it.
	direct := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(filepath.Join(harness, programName("putnami-contributor")), append([]string{"verify"}, args...)...)
		cmd.Dir = root
		for _, entry := range env {
			if !strings.HasPrefix(entry, wire.SocketEnv+"=") && !strings.HasPrefix(entry, wire.TokenEnv+"=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		return run(cmd)
	}
	// received is what the executable gets when it differs from what the user
	// types: putnami splits an inline `--flag=value` into the flag and its value,
	// keeps a global output flag for itself, and the executable reads only the
	// split form.
	for _, tc := range []struct {
		dir      string
		args     []string
		received []string
		wantExit int
	}{
		{".", []string{"--ref", "README.md"}, nil, 0},
		{".", []string{"--ref=README.md"}, []string{"--ref", "README.md"}, 0},
		{"docs", []string{"--ref", "docs/guide.md"}, nil, 0},
		{".", []string{"--ref", "absent.md"}, nil, 1},
		{".", []string{"--snapshot", "--base=main"}, []string{"--snapshot", "--base", "main"}, 0},
		{".", []string{"--ref", "README.md", "--output=json"}, []string{"--ref", "README.md"}, 0},
		{".", []string{"--output", "jsonl", "--ref", "README.md"}, []string{"--ref", "README.md"}, 0},
		{".", []string{"--json", "--ref", "absent.md"}, []string{"--ref", "absent.md"}, 1},
	} {
		name := tc.dir + " " + strings.Join(tc.args, " ")
		received := tc.received
		if received == nil {
			received = tc.args
		}
		want, wantExit := direct(received...)
		got, gotExit := putnami(tc.dir, append([]string{"change", "verify"}, tc.args...)...)
		if wantExit != tc.wantExit || gotExit != wantExit || got != want {
			t.Errorf("%s: change verify exited %d with\n%s\nthe executable exited %d with\n%s\nwant both to exit %d with the same output",
				name, gotExit, got, wantExit, want, tc.wantExit)
		}
	}
}
