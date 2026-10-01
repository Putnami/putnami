//go:build windows

package versioncmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/launch"
)

// fakeCLI is a CLI binary that answers --version with reports: a copy of this
// test binary, whose TestMain answers through fakeCLIReportsEnv. The variable
// stays set for the rest of the test.
func fakeCLI(t *testing.T, reports string) []byte {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeCLIReportsEnv, reports)
	return content
}

// assertInstalledBinaryMode checks the mode installBinary gives a binary on
// Windows: 0666. Windows has no execute bit, and chmod 0755 leaves the
// read-only attribute clear.
func assertInstalledBinaryMode(t *testing.T, info os.FileInfo) {
	t.Helper()
	if perm := info.Mode().Perm(); perm != 0o666 {
		t.Errorf("mode = %v, want 0666", perm)
	}
}

// On Windows `version use` makes putnami.exe a copy of the named version and
// leaves the version in place.
func TestVersionUse_CopiesTheVersionToPutnamiExe(t *testing.T) {
	dir := t.TempDir()
	binary := []byte("putnami-go-dev binary")
	if err := os.WriteFile(filepath.Join(dir, "putnami-go-dev.exe"), binary, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := VersionUse(context.Background(), dir, "go-dev", false); err != nil {
		t.Fatalf("VersionUse: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "putnami.exe")); err != nil || string(got) != string(binary) {
		t.Fatalf("putnami.exe = %q (err %v), want a copy of putnami-go-dev.exe", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "putnami-go-dev.exe")); err != nil || string(got) != string(binary) {
		t.Fatalf("putnami-go-dev.exe = %q (err %v), want it left in place", got, err)
	}
	if got := activeCLIName(dir); got != "putnami-go-dev.exe" {
		t.Errorf("active CLI = %q, want putnami-go-dev.exe", got)
	}
}

// Switching versions on Windows replaces the copy and leaves no moved-aside
// file once nothing runs the previous one.
func TestVersionUse_SwitchesAnExistingCopy(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"putnami-go-1.0.exe", "putnami-go-2.0.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := VersionUse(context.Background(), dir, "go-1.0", false); err != nil {
		t.Fatalf("first VersionUse: %v", err)
	}
	if got := activeCLIName(dir); got != "putnami-go-1.0.exe" {
		t.Errorf("after first use, active CLI = %q, want putnami-go-1.0.exe", got)
	}

	if err := VersionUse(context.Background(), dir, "go-2.0", false); err != nil {
		t.Fatalf("second VersionUse: %v", err)
	}
	if got := activeCLIName(dir); got != "putnami-go-2.0.exe" {
		t.Errorf("after second use, active CLI = %q, want putnami-go-2.0.exe", got)
	}
	if asides := asideFiles(t, dir); len(asides) != 0 {
		t.Errorf("moved-aside files = %v, want none", asides)
	}
	assertNoStagingLeftovers(t, dir)
}

func TestGetBinaryVersionDisablesWorkspaceRelaunchOnWindows(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "putnami.exe")
	if err := os.WriteFile(binaryPath, fakeCLI(t, "putnami 9.9.9"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(launch.LaunchedEnv, "abc123")

	if got := getBinaryVersion(context.Background(), binaryPath); got != "putnami 9.9.9" {
		t.Fatalf("getBinaryVersion = %q, want direct binary version", got)
	}
}
