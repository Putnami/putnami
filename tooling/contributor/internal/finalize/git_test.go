package finalize

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

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
	if err := g.runWrite(context.Background(), "status"); err == nil || !strings.Contains(err.Error(), "unsupported repository-writing git operation") {
		t.Fatalf("write boundary accepted status: %v", err)
	}
}

func TestNativeGitPushThatHangsTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hanging hook is a POSIX executable")
	}
	// Not parallel: it shortens the package write timeout.
	previous := gitWriteTimeout
	gitWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { gitWriteTimeout = previous })
	root, origin, hooks := t.TempDir(), t.TempDir(), t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	run(origin, "init", "-q", "--bare")
	run(root, "init", "-q", "-b", "fix/hang")
	run(root, "config", "user.name", "Test")
	run(root, "config", "user.email", "test@example.invalid")
	run(root, "config", "commit.gpgsign", "false")
	run(root, "config", "core.hooksPath", hooks)
	run(root, "remote", "add", "origin", origin)
	run(root, "commit", "-q", "--allow-empty", "-m", "fix: base")
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nsleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := (NativeGit{Root: root}).Push(context.Background(), "fix/hang")
	if err == nil || !strings.Contains(err.Error(), "timed out after 200ms") {
		t.Fatalf("hanging push: %v", err)
	}
	if elapsed := time.Since(started); elapsed > gitWriteWaitDelay+10*time.Second {
		t.Fatalf("hanging push returned after %s", elapsed)
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

// rebaseFixture is a checkout that pushed fix/rebase to a bare origin, then
// rebased it onto a base that moved: its head no longer contains the commit
// it pushed. The code under test drops GIT_CONFIG_* from its environment, so
// the fixture pins in the repositories' own configuration what a global
// configuration could change: hooks and push signing.
func rebaseFixture(t *testing.T) (root, origin, pushed string, run func(string, ...string) string) {
	t.Helper()
	root, origin = t.TempDir(), t.TempDir()
	hooks := t.TempDir()
	run = func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run(origin, "init", "-q", "--bare", "-b", "main")
	run(origin, "config", "core.hooksPath", hooks)
	run(root, "init", "-q", "-b", "main")
	run(root, "config", "core.hooksPath", hooks)
	run(root, "config", "push.gpgSign", "false")
	run(root, "config", "commit.gpgSign", "false")
	run(root, "config", "user.name", "Test")
	run(root, "config", "user.email", "test@example.invalid")
	run(root, "remote", "add", "origin", origin)
	run(root, "commit", "-q", "--allow-empty", "-m", "chore: base")
	run(root, "push", "-q", "origin", "main")
	run(root, "switch", "-q", "-c", "fix/rebase")
	run(root, "commit", "-q", "--allow-empty", "-m", "fix: first slice")
	if err := (NativeGit{Root: root}).Push(context.Background(), "fix/rebase"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	pushed = run(root, "rev-parse", "HEAD")
	// main moves, and the branch rebases onto it.
	run(root, "switch", "-q", "main")
	run(root, "commit", "-q", "--allow-empty", "-m", "chore: main moved")
	run(root, "push", "-q", "origin", "main")
	run(root, "switch", "-q", "fix/rebase")
	run(root, "rebase", "-q", "--keep-empty", "main")
	if head := run(root, "rev-parse", "HEAD"); head == pushed {
		t.Fatal("the rebase kept the pushed head")
	}
	return root, origin, pushed, run
}

// pushFromElsewhere pushes a commit on top of origin's fix/rebase from
// another clone, and returns origin's new head.
func pushFromElsewhere(t *testing.T, origin string, run func(string, ...string) string) string {
	t.Helper()
	other := t.TempDir()
	run(other, "clone", "-q", "-b", "fix/rebase", origin, ".")
	run(other, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-q", "--allow-empty", "-m", "fix: another push")
	run(other, "push", "-q", "origin", "fix/rebase")
	return run(other, "rev-parse", "HEAD")
}

func TestNativeGitPushPublishesARebasedBranch(t *testing.T) {
	t.Parallel()
	root, origin, _, run := rebaseFixture(t)
	if err := (NativeGit{Root: root}).Push(context.Background(), "fix/rebase"); err != nil {
		t.Fatalf("push of the rebased branch: %v", err)
	}
	if got, want := run(origin, "rev-parse", "refs/heads/fix/rebase"), run(root, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin head = %s, want the rebased head %s", got, want)
	}
	// The next rebase leases on the head this push wrote.
	run(root, "commit", "-q", "--amend", "--allow-empty", "-m", "fix: first slice, amended")
	if err := (NativeGit{Root: root}).Push(context.Background(), "fix/rebase"); err != nil {
		t.Fatalf("push of the amended branch: %v", err)
	}
	if got, want := run(origin, "rev-parse", "refs/heads/fix/rebase"), run(root, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin head = %s, want the amended head %s", got, want)
	}
}

func TestNativeGitPushRefusesToReplaceAnotherPush(t *testing.T) {
	t.Parallel()
	for _, fetched := range []bool{false, true} {
		root, origin, pushed, run := rebaseFixture(t)
		theirs := pushFromElsewhere(t, origin, run)
		if fetched {
			// A fetch moves the remote-tracking ref without a push from here.
			run(root, "fetch", "-q", "origin")
		}
		err := (NativeGit{Root: root}).Push(context.Background(), "fix/rebase")
		refused := (*PushRefusedError)(nil)
		if !errors.As(err, &refused) || refused.Pushed != pushed {
			t.Fatalf("fetched=%v: the push did not refuse another push: %v", fetched, err)
		}
		if got := run(origin, "rev-parse", "refs/heads/fix/rebase"); got != theirs {
			t.Fatalf("fetched=%v: origin head = %s, want the other push %s", fetched, got, theirs)
		}
	}
}

func TestNativeGitPushPublishesAfterIntegratingAnotherPush(t *testing.T) {
	t.Parallel()
	root, origin, _, run := rebaseFixture(t)
	pushFromElsewhere(t, origin, run)
	run(root, "pull", "-q", "--rebase", "origin", "fix/rebase")
	run(root, "commit", "-q", "--allow-empty", "-m", "fix: second slice")
	if err := (NativeGit{Root: root}).Push(context.Background(), "fix/rebase"); err != nil {
		t.Fatalf("push after integrating another push: %v", err)
	}
	if got, want := run(origin, "rev-parse", "refs/heads/fix/rebase"), run(root, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin head = %s, want %s", got, want)
	}
}

func TestNativeGitPushWithoutARecordedPushOnlyFastForwards(t *testing.T) {
	t.Parallel()
	root, origin, _, run := rebaseFixture(t)
	// Expire the reflog: this checkout keeps no record of its push.
	run(root, "reflog", "expire", "--expire=all", "refs/remotes/origin/fix/rebase")
	before := run(origin, "rev-parse", "refs/heads/fix/rebase")
	err := (NativeGit{Root: root}).Push(context.Background(), "fix/rebase")
	if refused := (*PushRefusedError)(nil); !errors.As(err, &refused) || refused.Pushed != "" || !strings.Contains(err.Error(), "records no push") {
		t.Fatalf("push without a recorded push: %v", err)
	}
	if got := run(origin, "rev-parse", "refs/heads/fix/rebase"); got != before {
		t.Fatalf("origin head = %s, want it unchanged at %s", got, before)
	}
}
