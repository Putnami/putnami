package githooks

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/pkgmeta"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		msg     string
		wantSub string // substring of the error; "" means valid
	}{
		{"feat(auth): add login", ""},
		{"fix: bug", ""},
		{"feat!: breaking change", ""},
		{"refactor(core): tidy", ""},
		{"", "empty commit message"},
		{"no conventional prefix", "does not match"},
		{"wat(x): subject", "unknown commit type"},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			got := Validate(c.msg)
			if c.wantSub == "" {
				if got != "" {
					t.Errorf("Validate(%q) = %q, want valid", c.msg, got)
				}
				return
			}
			if !strings.Contains(got, c.wantSub) {
				t.Errorf("Validate(%q) = %q, want substring %q", c.msg, got, c.wantSub)
			}
		})
	}
}

func TestRunCommitMsg(t *testing.T) {
	write := func(msg string) string {
		p := filepath.Join(t.TempDir(), "MSG")
		if err := os.WriteFile(p, []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if code := runCommitMsg([]string{write("feat(x): ok")}, io.Discard); code != 0 {
		t.Errorf("valid message: code %d, want 0", code)
	}
	if code := runCommitMsg([]string{write("not conventional")}, io.Discard); code != 1 {
		t.Errorf("invalid message: code %d, want 1", code)
	}
	if code := runCommitMsg([]string{write("Merge branch 'x'")}, io.Discard); code != 0 {
		t.Errorf("merge commit should be skipped: code %d, want 0", code)
	}
	if code := runCommitMsg(nil, io.Discard); code != 1 {
		t.Errorf("missing arg: code %d, want 1", code)
	}
}

func TestInstall_WritesHooks(t *testing.T) {
	t.Setenv("CI", "") // Install is a no-op when CI=true

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Chdir(repo)

	Install("/path/to/putnami")

	for _, name := range []string{"pre-commit", "pre-push", "commit-msg"} {
		info, err := os.Stat(filepath.Join(repo, ".git", "hooks", name))
		if err != nil {
			t.Fatalf("hook %s not written: %v", name, err)
		}
		// Windows has no execute permission bit; Git for Windows runs a hook
		// whose first line is a #! line.
		if runtime.GOOS != "windows" && info.Mode()&0o100 == 0 {
			t.Errorf("hook %s is not executable: %v", name, info.Mode())
		}
	}

	data, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", "commit-msg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/path/to/putnami") {
		t.Errorf("commit-msg hook should re-invoke the self binary, got:\n%s", data)
	}
}

// The commit-msg hook is durable: it outlives the command that wrote it. Naming
// the running binary bakes in a from-source store blob — GC-reapable, and removed
// outright by `putnami cache clean --all` — so every later `git commit` would fail
// with "no such file". The workspace's engine link is a stable name kept pointed
// at a current engine, so the hook must prefer it.
func TestInstall_CommitMsgHookPrefersTheWorkspaceEngineLink(t *testing.T) {
	t.Setenv("CI", "")

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	binDir := filepath.Join(repo, ".putnami", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The engine link is putnami.exe on Windows, the name the wrapper and the
	// installer give it there.
	link := filepath.Join(binDir, pkgmeta.ExecutableName(runtime.GOOS, "putnami"))
	if err := os.WriteFile(link, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	reapable := "/home/dev/.putnami/artifacts/cli-source/726e724c/putnami"
	Install(reapable)

	data, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", "commit-msg"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), reapable) {
		t.Errorf("the hook baked in a reapable store blob:\n%s", data)
	}
	if !strings.Contains(string(data), link) {
		t.Errorf("the hook should re-invoke the workspace engine link %q, got:\n%s", link, data)
	}
}

// With no engine link — a consumer workspace that has never run the wrapper, or a
// globally installed CLI — the running binary is still the only answer available.
func TestInstall_CommitMsgHookFallsBackToTheRunningBinary(t *testing.T) {
	t.Setenv("CI", "")

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Chdir(repo)

	Install("/usr/local/bin/putnami")

	data, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", "commit-msg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/usr/local/bin/putnami") {
		t.Errorf("the hook should fall back to the running binary, got:\n%s", data)
	}
}

// On Windows the engine link is .putnami/bin/putnami.exe; a suffix-less file
// there is not the engine, so the running binary stays the answer.
func TestHookTarget_WindowsEngineLinkIsPutnamiExe(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, ".putnami", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(t.TempDir(), "putnami.exe")
	if err := os.WriteFile(filepath.Join(binDir, "putnami"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := hookTarget(root, self, "windows"); got != self {
		t.Fatalf("hookTarget without putnami.exe = %q, want the running binary %q", got, self)
	}
	exe := filepath.Join(binDir, "putnami.exe")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := hookTarget(root, self, "windows"); got != exe {
		t.Fatalf("hookTarget = %q, want the engine link %q", got, exe)
	}
	if got := hookTarget(root, self, "linux"); got != filepath.Join(binDir, "putnami") {
		t.Fatalf("hookTarget on linux = %q, want the suffix-less link", got)
	}
}
