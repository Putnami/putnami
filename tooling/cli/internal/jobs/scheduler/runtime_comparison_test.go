package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func writeRuntimeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two prepared runtimes are the same only with the same files, bytes and
// executable bits.
func TestSameRuntimeContents(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeExecutable(t, filepath.Join(a, "compiled", "putnami-x"), "binary")
	writeExecutable(t, filepath.Join(b, "compiled", "putnami-x"), "binary")
	if same, err := sameRuntimeContents(a, b); err != nil || !same {
		t.Fatalf("identical runtimes: same=%v err=%v", same, err)
	}

	writeExecutable(t, filepath.Join(b, "compiled", "putnami-x"), "binary!")
	if same, _ := sameRuntimeContents(a, b); same {
		t.Error("runtimes with different bytes compare the same")
	}

	writeRuntimeFile(t, b, "compiled/putnami-x", "binary")
	if same, _ := sameRuntimeContents(a, b); same {
		t.Error("runtimes with different executable bits compare the same")
	}

	writeExecutable(t, filepath.Join(b, "compiled", "putnami-x"), "binary")
	writeRuntimeFile(t, b, "config/extra.json", "{}")
	if same, _ := sameRuntimeContents(a, b); same {
		t.Error("runtimes with different files compare the same")
	}

	if _, err := sameRuntimeContents(a, filepath.Join(b, "missing")); err == nil {
		t.Error("a missing runtime compares without an error")
	}
}

// The base checkout holds the commit's files, not the working tree's, and is
// removed afterwards.
func TestCheckoutBase(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := gittest.Command(root, args...)
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "commit.gpgsign", "false")
	writeRuntimeFile(t, root, "ext/main.go", "package main // base\n")
	writeExecutable(t, filepath.Join(root, "ext", "bin", "prepare"), "#!/bin/sh\n")
	git("add", "-A")
	git("commit", "-m", "base")
	writeRuntimeFile(t, root, "ext/main.go", "package main // head\n")

	dir, cleanup, err := checkoutBase(root, "HEAD")
	if err != nil {
		t.Fatalf("checkoutBase: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ext", "main.go"))
	if err != nil || string(got) != "package main // base\n" {
		t.Fatalf("base checkout holds %q (%v), want the committed file", got, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "ext", "bin", "prepare")); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("the committed executable is not executable in the base checkout: %v %v", info, err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the base checkout %s survives its cleanup: %v", dir, err)
	}

	if _, _, err := checkoutBase(root, "no-such-ref"); err == nil {
		t.Error("an unknown base checks out without an error")
	}
}

// Only candidates that are extensions preparing from source get an answer,
// and a diff with no base keeps every edge.
func TestCompareExtensionRuntimes_AnswersOnlyForSourcePreparedExtensions(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir(), Projects: []*workspace.Project{{ID: "/app", Name: "app", Path: "app"}}}
	if got := CompareExtensionRuntimes(context.Background(), ws, nil, []string{"/app"}, "abc"); got != nil {
		t.Errorf("a plain project got an answer: %+v", got)
	}
	if got := CompareExtensionRuntimes(context.Background(), nil, nil, []string{"/app"}, "abc"); got != nil {
		t.Errorf("no workspace got an answer: %+v", got)
	}
}
