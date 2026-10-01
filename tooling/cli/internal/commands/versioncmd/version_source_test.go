package versioncmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

func TestVersionInstallFromSource_NoWorkspace(t *testing.T) {
	err := VersionInstallFromSource(context.Background(), "", t.TempDir(), false)
	if !errors.Is(err, cmderr.ErrUsage) {
		t.Errorf("err = %v, want ErrUsage", err)
	}
}

func TestVersionInstallFromSource_SourceMissing(t *testing.T) {
	// A workspace with no putnami CLI source tree.
	err := VersionInstallFromSource(context.Background(), t.TempDir(), t.TempDir(), false)
	if !errors.Is(err, cmderr.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestVersionInstallFromSource_DryRunDoesNotBuild(t *testing.T) {
	wsRoot := t.TempDir()
	entry := filepath.Join(wsRoot, cliSourceModuleRel, "cmd", "putnami")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(wsRoot, ".putnami", "bin")

	out, err := sharedtest.CaptureStdout(t, func() error {
		return VersionInstallFromSource(context.Background(), wsRoot, binDir, true)
	})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(out, "Would build") {
		t.Errorf("dry-run output missing plan; got:\n%s", out)
	}
	// Dry-run must not create the bin directory or any binary.
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Errorf("dry-run created bin dir %s (err=%v)", binDir, err)
	}
}

func TestSourceBuildVersion_NonGitWorkspace(t *testing.T) {
	got := sourceBuildVersion(context.Background(), t.TempDir())
	if got != "0.0.0-source-local" {
		t.Errorf("version = %q, want 0.0.0-source-local", got)
	}
}

func TestSourceBuildVersion_GitWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	wsRoot := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", wsRoot}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	got := sourceBuildVersion(context.Background(), wsRoot)
	if !strings.HasPrefix(got, "0.0.0-source-") {
		t.Fatalf("version = %q, want 0.0.0-source-<sha> prefix", got)
	}
	if strings.HasSuffix(got, "-dirty") {
		t.Errorf("clean tree marked dirty: %q", got)
	}

	// An untracked file makes the tree dirty.
	if err := os.WriteFile(filepath.Join(wsRoot, "scratch"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirty := sourceBuildVersion(context.Background(), wsRoot); !strings.HasSuffix(dirty, "-dirty") {
		t.Errorf("dirty tree not marked: %q", dirty)
	}
}

// TestBuildStampedCLI_StampsVersion compiles a throwaway module and asserts the
// -ldflags version stamp lands in the built binary's output. This exercises the
// real build+stamp path without depending on the full CLI module.
func TestBuildStampedCLI_StampsVersion(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	// Simulate Putnami's workspace-aware test runner. The throwaway module below
	// is standalone, so buildStampedCLI must drop this unrelated inherited
	// GOWORK instead of letting cmd/go reject a module outside its use list.
	unrelatedWorkspace := t.TempDir()
	writeFile(t, filepath.Join(unrelatedWorkspace, "go.work"), "go 1.21\n\nuse ./member\n")
	writeFile(t, filepath.Join(unrelatedWorkspace, "member", "go.mod"), "module example.test/unrelated\n\ngo 1.21\n")
	t.Setenv("GOWORK", filepath.Join(unrelatedWorkspace, "go.work"))

	moduleDir := t.TempDir()
	writeFile(t, filepath.Join(moduleDir, "go.mod"), "module example.test/stamp\n\ngo 1.21\n")
	writeFile(t, filepath.Join(moduleDir, "cmd", "putnami", "main.go"),
		"package main\n\nimport \"fmt\"\n\nvar Version = \"dev\"\n\nfunc main() { fmt.Print(Version) }\n")

	const want = "0.0.0-source-test"
	built, cleanup, err := buildStampedCLI(context.Background(), moduleDir, "main.Version", want)
	if err != nil {
		t.Fatalf("buildStampedCLI: %v", err)
	}
	defer cleanup()

	out, err := exec.Command(built).Output()
	if err != nil {
		t.Fatalf("run built binary: %v", err)
	}
	if string(out) != want {
		t.Errorf("stamped version = %q, want %q", out, want)
	}
}

// TestSourceBuildPatternCarriesTheExecutableSuffix pins the temp name a source
// build is written to: Windows runs only a file that ends in .exe, and every
// other platform keeps the suffix-free name.
func TestSourceBuildPatternCarriesTheExecutableSuffix(t *testing.T) {
	for goos, want := range map[string]string{
		"windows": "putnami-source-build-*.exe",
		"linux":   "putnami-source-build-*",
		"darwin":  "putnami-source-build-*",
	} {
		if got := sourceBuildPattern(goos); got != want {
			t.Errorf("sourceBuildPattern(%q) = %q, want %q", goos, got, want)
		}
	}
	file, err := os.CreateTemp(t.TempDir(), sourceBuildPattern("windows"))
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if !strings.HasSuffix(file.Name(), ".exe") {
		t.Errorf("os.CreateTemp(%q) named %q, want the .exe suffix kept after the random part", sourceBuildPattern("windows"), file.Name())
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
