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
)

// The build skill runs `putnami change verify`, which the installed
// @putnami/contributor serves through an interactive launch. While the core
// still serves `putnami tree verify`, both print the same stdout and exit with
// the same code for one invocation in one workspace. The cases pin what only
// the launch decides: an inline `--flag=value` reaches the executable, the
// executable starts in the workspace root whatever directory the user runs
// putnami from, and its exit code is the command's.
func TestChangeVerifyThroughPutnamiMatchesTreeVerify(t *testing.T) {
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
		cmd := exec.Command("git", args...)
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

	putnami := func(dir string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(filepath.Join(harness, "putnami"), args...)
		cmd.Dir = filepath.Join(root, filepath.FromSlash(dir))
		cmd.Env = env
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
		t.Fatalf("putnami %v did not run: %v: %s", args, err, stderr.String())
		return "", 0
	}
	for _, tc := range []struct {
		dir      string
		args     []string
		wantExit int
	}{
		{".", []string{"--ref", "README.md"}, 0},
		{".", []string{"--ref=README.md"}, 0},
		{"docs", []string{"--ref", "docs/guide.md"}, 0},
		{".", []string{"--ref", "absent.md"}, 1},
		{".", []string{"--snapshot", "--base=main"}, 0},
	} {
		name := tc.dir + " " + strings.Join(tc.args, " ")
		core, coreExit := putnami(tc.dir, append([]string{"tree", "verify"}, tc.args...)...)
		moved, movedExit := putnami(tc.dir, append([]string{"change", "verify"}, tc.args...)...)
		if coreExit != tc.wantExit || movedExit != coreExit || moved != core {
			t.Errorf("%s: change verify exited %d with\n%s\ntree verify exited %d with\n%s\nwant both to exit %d with the same output",
				name, movedExit, moved, coreExit, core, tc.wantExit)
		}
	}
}
