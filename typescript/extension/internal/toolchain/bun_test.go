package toolchain

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveBun_Found(t *testing.T) {
	// Skip if bun is not installed on this machine.
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not found on PATH, skipping")
	}

	got, err := ResolveBun()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty path for bun")
	}
}

func writeBunExe(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "bin", "bun.exe")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func noEnv(string) string { return "" }

func TestInstalledBun_WindowsDefaultHome(t *testing.T) {
	home := t.TempDir()
	want := writeBunExe(t, filepath.Join(home, ".bun"))

	got := installedBun("windows", noEnv, func() (string, error) { return home, nil })
	if got != want {
		t.Errorf("installedBun() = %q, want %q", got, want)
	}
}

func TestInstalledBun_WindowsBunInstallWins(t *testing.T) {
	home := t.TempDir()
	writeBunExe(t, filepath.Join(home, ".bun"))
	custom := t.TempDir()
	want := writeBunExe(t, custom)
	getenv := func(key string) string {
		if key == "BUN_INSTALL" {
			return custom
		}
		return ""
	}

	got := installedBun("windows", getenv, func() (string, error) { return home, nil })
	if got != want {
		t.Errorf("installedBun() = %q, want %q", got, want)
	}
}

func TestInstalledBun_WindowsNothingInstalled(t *testing.T) {
	home := t.TempDir()
	if got := installedBun("windows", noEnv, func() (string, error) { return home, nil }); got != "" {
		t.Errorf("installedBun() = %q, want empty", got)
	}
	noHome := func() (string, error) { return "", errors.New("no home") }
	if got := installedBun("windows", noEnv, noHome); got != "" {
		t.Errorf("installedBun() without a home = %q, want empty", got)
	}
}

func TestInstalledBun_OnlyWindows(t *testing.T) {
	home := t.TempDir()
	writeBunExe(t, filepath.Join(home, ".bun"))
	for _, goos := range []string{"linux", "darwin"} {
		if got := installedBun(goos, noEnv, func() (string, error) { return home, nil }); got != "" {
			t.Errorf("installedBun(%q) = %q, want empty: PATH is the only lookup there", goos, got)
		}
	}
}
