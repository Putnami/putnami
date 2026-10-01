package refstore

import (
	"context"
	"strings"
	"testing"
)

func TestPorcelainStatusFindsTheRefLine(t *testing.T) {
	ref := "refs/heads/putnami-memory"
	out := []byte("To /tmp/remote.git\n" +
		"!\t0123abc:refs/heads/putnami-memory\t[rejected] (stale info)\n" +
		"Done\n")
	flag, status, found := porcelainStatus(out, ref)
	if !found || flag != '!' || status != "[rejected] (stale info)" {
		t.Fatalf("%q %q %v", flag, status, found)
	}
	if _, _, found := porcelainStatus([]byte("To /tmp/remote.git\n*\t0123abc:refs/heads/other\t[new branch]\n"), ref); found {
		t.Fatal("another ref's line was taken")
	}
	if _, _, found := porcelainStatus(nil, ref); found {
		t.Fatal("an empty answer has a status")
	}
}

func TestFailureMessagesCarryNoCredential(t *testing.T) {
	stderr := []byte("\nfatal: unable to access 'https://user:secret@example.com/acme/memory.git/': Could not resolve host\nmore\n")
	message := describe([]string{"-c", "a=b", "--git-dir=/x", "push", "origin"}, Result{stderr: stderr, code: 128})
	if strings.Contains(message, "secret") || strings.Contains(message, "user:") || !strings.Contains(message, "git push exited 128") ||
		!strings.Contains(message, "https://example.com/acme/memory.git") || strings.Contains(message, "more") {
		t.Fatalf("message %q", message)
	}
	long := summary([]byte(strings.Repeat("x", 400)))
	if len(long) > 310 || !strings.HasSuffix(long, "…") {
		t.Fatalf("an unbounded summary: %d bytes", len(long))
	}
	if verb([]string{"-C", "/x", "-c", "k=v"}) != "command" {
		t.Fatal("options named a verb")
	}
}

func TestTheEnvironmentNamesNoOtherRepository(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere/.git")
	t.Setenv("GIT_INDEX_FILE", "/elsewhere/index")
	t.Setenv("LANG", "fr_FR.UTF-8")
	env := environment([]string{"EXTRA=1"})
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, absent := range []string{"\nGIT_DIR=", "\nGIT_INDEX_FILE=", "\nLANG="} {
		if strings.Contains(joined, absent) {
			t.Errorf("the environment keeps %s", strings.TrimSpace(absent))
		}
	}
	for _, present := range []string{"\nLC_ALL=C\n", "\nGIT_TERMINAL_PROMPT=0\n", "\nEXTRA=1\n"} {
		if !strings.Contains(joined, present) {
			t.Errorf("the environment lacks %s", strings.TrimSpace(present))
		}
	}
}

func TestACanceledCommandIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := execGit(ctx, environment(nil), nil, "--version"); err == nil {
		t.Fatal("a canceled command succeeded")
	}
}
