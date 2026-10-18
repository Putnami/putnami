package finalize

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
)

func TestNativeGitReadBoundaryRejectsWritingVerbs(t *testing.T) {
	t.Parallel()
	g := NativeGit{Root: t.TempDir()}
	for _, verb := range []string{"commit", "push", "config"} {
		if _, err := g.run(verb); err == nil || !strings.Contains(err.Error(), "unsupported read-only git operation") {
			t.Fatalf("read boundary accepted %q: %v", verb, err)
		}
	}
	if err := g.runWrite("status"); err == nil || !strings.Contains(err.Error(), "unsupported repository-writing git operation") {
		t.Fatalf("write boundary accepted status: %v", err)
	}
}

func TestNativeGitCommitMessagesNeverRunsConfiguredVerifier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the verifier sentinel is a POSIX executable")
	}
	t.Parallel()
	root := t.TempDir()
	command := func(input []byte, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Stdin = bytes.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return output
	}
	command(nil, "init", "-q")
	command(nil, "config", "user.name", "Test")
	command(nil, "config", "user.email", "test@example.invalid")
	command(nil, "config", "commit.gpgsign", "false")
	command(nil, "commit", "-q", "--allow-empty", "-m", "base")
	base := strings.TrimSpace(string(command(nil, "rev-parse", "HEAD")))
	command(nil, "commit", "-q", "--allow-empty", "-m", "feat: English title", "-m", "Body kept exactly")
	raw := command(nil, "cat-file", "commit", "HEAD")
	parts := bytes.SplitN(raw, []byte("\n\n"), 2)
	if len(parts) != 2 {
		t.Fatal("commit object has no body boundary")
	}
	signed := append(bytes.Clone(parts[0]), []byte("\ngpgsig -----BEGIN PGP SIGNATURE-----\n fake-signature\n -----END PGP SIGNATURE-----\n\n")...)
	signed = append(signed, parts[1]...)
	sha := strings.TrimSpace(string(command(signed, "hash-object", "-t", "commit", "-w", "--stdin")))
	command(nil, "update-ref", "HEAD", sha)

	verifier := filepath.Join(root, "signature-verifier")
	marker := verifier + ".invoked"
	if err := os.WriteFile(verifier, []byte("#!/bin/sh\nprintf invoked > \"$0.invoked\"\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	command(nil, "config", "log.showSignature", "true")
	command(nil, "config", "gpg.format", "openpgp")
	command(nil, "config", "gpg.program", verifier)
	// The configured verifier really is reachable for this signed object.
	probe := exec.Command("git", "-C", root, "log", "-1", "--format=%B", sha)
	_, _ = probe.CombinedOutput()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fixture did not invoke configured verifier: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	messages, err := (NativeGit{Root: root}).CommitMessages(base)
	if err != nil || len(messages) != 1 || messages[0].Text != "feat: English title\n\nBody kept exactly" {
		t.Fatalf("commit body changed: messages=%+v err=%v", messages, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("read invoked the configured verifier: %v", err)
	}
}

// TestGitEnvironmentDropsTheHostChannel pins that no git command the
// finalizer starts receives the host channel, and that the repository
// selectors leave too.
func TestGitEnvironmentDropsTheHostChannel(t *testing.T) {
	t.Parallel()
	environ := []string{"PATH=/bin", wire.SocketEnv + "=/tmp/host.sock", wire.TokenEnv + "=launch-token", "GIT_DIR=/elsewhere", "GIT_SSH_COMMAND=ssh"}
	if got, want := gitEnvironment(environ), []string{"PATH=/bin", "GIT_SSH_COMMAND=ssh"}; !slices.Equal(got, want) {
		t.Errorf("gitEnvironment = %q, want %q", got, want)
	}
}
