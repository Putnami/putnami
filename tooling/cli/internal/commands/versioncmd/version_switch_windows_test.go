//go:build windows

package versioncmd

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const blockingHelperEnv = "PUTNAMI_VERSIONCMD_BLOCKING_HELPER"

// TestBlockingHelper is not a test: TestActivateCLI_ReplacesRunningExecutable
// runs a copy of this test binary with it, so the copy keeps running until its
// stdin closes.
func TestBlockingHelper(t *testing.T) {
	if os.Getenv(blockingHelperEnv) != "1" {
		t.Skip("helper process for TestActivateCLI_ReplacesRunningExecutable")
	}
	os.Stdout.WriteString("ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestActivateCLI_ReplacesRunningExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	link := filepath.Join(binDir, "putnami.exe")
	staging, err := stageBinary(self, link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, link); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(link, "-test.run=^TestBlockingHelper$")
	cmd.Env = append(os.Environ(), blockingHelperEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper did not start: %q, %v", line, err)
	}
	running := lstatNow(t, link)

	// The premise: Windows refuses to delete the running image.
	if err := os.Remove(link); err == nil {
		t.Fatal("removed a running executable; the rename-aside switch premise does not hold")
	}

	newBinary := []byte("new-binary")
	if err := os.WriteFile(filepath.Join(binDir, "putnami-go-2.0.0.exe"), newBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := activateCLI("putnami-go-2.0.0.exe", link); err != nil {
		t.Fatalf("activateCLI while putnami.exe runs: %v", err)
	}
	if got, _ := os.ReadFile(link); string(got) != string(newBinary) {
		t.Errorf("putnami.exe = %d bytes, want the new binary", len(got))
	}
	if got := activeCLIName(binDir); got != "putnami-go-2.0.0.exe" {
		t.Errorf("active CLI = %q, want putnami-go-2.0.0.exe", got)
	}
	asides := asideFiles(t, binDir)
	if len(asides) != 1 {
		t.Fatalf("moved-aside files = %v, want exactly the running executable", asides)
	}
	if info, err := os.Lstat(filepath.Join(binDir, asides[0])); err != nil || !os.SameFile(info, running) {
		t.Fatalf("aside %s is not the running executable (err %v)", asides[0], err)
	}

	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
	exited = true

	// The next install in the directory reclaims the exited executable.
	if err := installBinary(filepath.Join(binDir, "putnami-go-2.0.0.exe"), filepath.Join(binDir, "putnami-go-3.0.0.exe")); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	if asides := asideFiles(t, binDir); len(asides) != 0 {
		t.Errorf("moved-aside files = %v, want them reclaimed", asides)
	}
}
