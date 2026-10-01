//go:build !windows

package versioncmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/launch"
)

// fakeCLI is a CLI binary that answers --version with reports.
func fakeCLI(t *testing.T, reports string) []byte {
	t.Helper()
	return []byte("#!/bin/sh\necho '" + reports + "'\n")
}

// assertInstalledBinaryMode checks the mode installBinary gives a binary: 0755.
func assertInstalledBinaryMode(t *testing.T, info os.FileInfo) {
	t.Helper()
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("mode = %v, want 0755", perm)
	}
}

func TestVersionUse_Success(t *testing.T) {
	dir := t.TempDir()

	// Create a fake binary
	binaryName := "putnami-go-dev"
	binaryPath := filepath.Join(dir, binaryName)
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\necho v0.0.1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := VersionUse(context.Background(), dir, "go-dev", false); err != nil {
		t.Fatalf("VersionUse: %v", err)
	}

	// Verify symlink was created
	linkPath := filepath.Join(dir, "putnami")
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != binaryName {
		t.Errorf("symlink target = %q, want %q", target, binaryName)
	}
}

func TestGetBinaryVersionDisablesWorkspaceRelaunch(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "putnami")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nif [ \"$PUTNAMI_NO_RELAUNCH\" = \"1\" ] && [ -z \"$PUTNAMI_LAUNCHED\" ]; then\n  echo 'putnami 9.9.9'\nelse\n  echo 'putnami 1.2.3 (launched from workspace pin)'\nfi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(launch.LaunchedEnv, "abc123")

	got := getBinaryVersion(context.Background(), binaryPath)
	if got != "putnami 9.9.9" {
		t.Fatalf("getBinaryVersion = %q, want direct binary version", got)
	}
}

func TestVersionUse_UpdatesExistingSymlink(t *testing.T) {
	dir := t.TempDir()

	// Create two binaries
	for _, name := range []string{"putnami-go-1.0", "putnami-go-2.0"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// First set to 1.0
	if err := VersionUse(context.Background(), dir, "go-1.0", false); err != nil {
		t.Fatalf("first VersionUse: %v", err)
	}
	target, _ := os.Readlink(filepath.Join(dir, "putnami"))
	if target != "putnami-go-1.0" {
		t.Errorf("after first use, symlink = %q", target)
	}

	// Then switch to 2.0
	if err := VersionUse(context.Background(), dir, "go-2.0", false); err != nil {
		t.Fatalf("second VersionUse: %v", err)
	}
	target, _ = os.Readlink(filepath.Join(dir, "putnami"))
	if target != "putnami-go-2.0" {
		t.Errorf("after second use, symlink = %q", target)
	}
}

func TestReplaceSymlink_CreatesAndSwaps(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"putnami-go-1.0.0", "putnami-go-2.0.0"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "putnami")

	if err := replaceSymlink("putnami-go-1.0.0", link); err != nil {
		t.Fatalf("create: %v", err)
	}
	if target, _ := os.Readlink(link); target != "putnami-go-1.0.0" {
		t.Errorf("target = %q, want putnami-go-1.0.0", target)
	}

	if err := replaceSymlink("putnami-go-2.0.0", link); err != nil {
		t.Fatalf("swap over existing link: %v", err)
	}
	if target, _ := os.Readlink(link); target != "putnami-go-2.0.0" {
		t.Errorf("target = %q, want putnami-go-2.0.0", target)
	}
	assertNoStagingLeftovers(t, dir)
}
