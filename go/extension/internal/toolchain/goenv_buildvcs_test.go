package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const worktreeVCSRequirement = "worktree-vcs-stamp"

// writeCheckout lays out a module at root/checkout. gitFile makes the
// checkout's `.git` a file, as `git worktree add` writes it; otherwise it is a
// directory. strayGitDir adds a `.git` directory that is no repository above
// the checkout, the shape a tool's leftover gives a parent folder.
func writeCheckout(t *testing.T, gitFile, strayGitDir bool) string {
	t.Helper()
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if strayGitDir {
		if err := os.MkdirAll(filepath.Join(root, ".git", "leftover"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitEntry := filepath.Join(checkout, ".git")
	if gitFile {
		if err := os.WriteFile(gitEntry, []byte("gitdir: "+filepath.Join(root, "main", ".git", "worktrees", "checkout")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else if err := os.MkdirAll(gitEntry, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":  "module example.test/checkout\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(checkout, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return checkout
}

// A worktree below a `.git` directory that is no repository: Go took that
// directory for the checkout's repository, ran `git status` in it and failed
// every build with "error obtaining VCS status: exit status 128".
func TestWorkspaceBuildEnv_WorktreeBuildsBelowAStrayGitDirectory(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", worktreeVCSRequirement,
		"a-worktree-builds-below-a-git-directory-that-is-no-repository")
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go binary unavailable: %v", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable, so go stamps nothing: %v", err)
	}
	checkout := writeCheckout(t, true, true)

	env := WorkspaceBuildEnv(removeEnv(os.Environ(), "GOFLAGS"), checkout, goBinary)
	cmd := exec.Command(goBinary, "build", "-o", filepath.Join(t.TempDir(), "checkout"), ".")
	cmd.Dir = checkout
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build in a worktree below a stray .git directory: %v\n%s", err, output)
	}
}

func TestWorkspaceBuildEnv_BuildvcsFollowsTheCheckoutGitEntry(t *testing.T) {
	for _, check := range []string{"a-worktree-turns-the-vcs-stamp-off", "an-explicit-buildvcs-wins", "a-clone-keeps-go-default-stamping"} {
		spectest.Proves(t, "go/go-project-toolchain", worktreeVCSRequirement, check)
	}
	for _, tc := range []struct {
		name        string
		gitFile     bool
		goflags     string
		wantGoflags string
	}{
		{name: "worktree", gitFile: true, wantGoflags: "-buildvcs=false"},
		{name: "worktree keeps other flags", gitFile: true, goflags: " -mod=mod ", wantGoflags: "-mod=mod -buildvcs=false"},
		{name: "worktree keeps an explicit buildvcs", gitFile: true, goflags: "-buildvcs=true", wantGoflags: "-buildvcs=true"},
		{name: "worktree keeps a double-dash buildvcs", gitFile: true, goflags: "--buildvcs=auto", wantGoflags: "--buildvcs=auto"},
		{name: "clone keeps Go's default", gitFile: false, wantGoflags: ""},
		{name: "clone keeps its flags", gitFile: false, goflags: "-mod=mod", wantGoflags: "-mod=mod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkout := writeCheckout(t, tc.gitFile, false)
			module := filepath.Join(checkout, "svc")
			if err := os.MkdirAll(module, 0o755); err != nil {
				t.Fatal(err)
			}
			// GOENV=off: no environment file seeds GOFLAGS.
			in := []string{"PATH=/usr/bin", "GOENV=off"}
			if tc.goflags != "" {
				in = append(in, "GOFLAGS="+tc.goflags)
			}
			out := WorkspaceBuildEnv(in, module, "")
			if got := envValue(out, "GOFLAGS"); got != tc.wantGoflags {
				t.Errorf("GOFLAGS = %q, want %q", got, tc.wantGoflags)
			}
		})
	}
}

func TestCheckoutGitIsFile_NoRepository(t *testing.T) {
	if checkoutGitIsFile(t.TempDir()) {
		t.Error("a directory with no .git above it reported a .git file")
	}
}

// go test stamps no test binary, and a test's own go commands inherit its
// environment: a worktree must not turn their VCS stamp off.
func TestWorkspaceTestEnv_WorktreeKeepsGoflags(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", worktreeVCSRequirement,
		"test-processes-keep-their-vcs-stamp")
	checkout := writeCheckout(t, true, false)
	out := WorkspaceTestEnv([]string{"PATH=/usr/bin", "GOFLAGS=-mod=mod"}, checkout, "")
	if got := envValue(out, "GOFLAGS"); got != "-mod=mod" {
		t.Errorf("GOFLAGS = %q, want the inherited %q", got, "-mod=mod")
	}
}

// A GOFLAGS variable replaces the GOFLAGS of Go's environment file whole: the
// worktree rule starts from the file's flags, so they still apply.
func TestWorkspaceBuildEnv_WorktreeKeepsTheGoEnvFileFlags(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", worktreeVCSRequirement,
		"a-worktree-keeps-the-go-env-file-flags")
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go binary unavailable: %v", err)
	}
	checkout := writeCheckout(t, true, false)
	for _, tc := range []struct {
		name, file, want string
	}{
		{name: "file flags", file: "GOFLAGS=-tags=fromfile\n", want: "-tags=fromfile -buildvcs=false"},
		{name: "file buildvcs", file: "GOFLAGS=-buildvcs=true\n", want: "-buildvcs=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			goenv := filepath.Join(t.TempDir(), "env")
			if err := os.WriteFile(goenv, []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}
			base := append(removeEnv(removeEnv(os.Environ(), "GOFLAGS"), "GOENV"), "GOENV="+goenv)
			// An empty GOFLAGS reads the file too, as Go reads it.
			for _, in := range [][]string{base, append(base, "GOFLAGS=")} {
				out := WorkspaceBuildEnv(in, checkout, goBinary)
				if got := envValue(out, "GOFLAGS"); got != tc.want {
					t.Errorf("GOFLAGS = %q, want %q", got, tc.want)
				}
			}
		})
	}
}
