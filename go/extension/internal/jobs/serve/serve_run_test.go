package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- computeWatchSnapshot: additional edge cases ---

func TestComputeWatchSnapshot_IgnoresVendorDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	snap1 := computeWatchSnapshot(dir)

	vendorDir := filepath.Join(dir, "vendor")
	os.MkdirAll(vendorDir, 0o755)
	os.WriteFile(filepath.Join(vendorDir, "dep.go"), []byte("package dep"), 0o644)

	snap2 := computeWatchSnapshot(dir)
	if snap1 != snap2 {
		t.Error("computeWatchSnapshot should ignore vendor directory")
	}
}

func TestComputeWatchSnapshot_TracksGoSum(t *testing.T) {
	dir := t.TempDir()
	goSumPath := filepath.Join(dir, "go.sum")
	os.WriteFile(goSumPath, []byte("module foo v1.0.0 h1:hash"), 0o644)

	snap1 := computeWatchSnapshot(dir)

	// Modify go.sum mtime
	time.Sleep(10 * time.Millisecond)
	os.Chtimes(goSumPath, time.Now().Add(time.Second), time.Now().Add(time.Second))
	snap2 := computeWatchSnapshot(dir)

	if snap1 == snap2 {
		t.Error("computeWatchSnapshot should detect go.sum changes")
	}
}

func TestComputeWatchSnapshot_TracksGoWork(t *testing.T) {
	dir := t.TempDir()
	goWorkPath := filepath.Join(dir, "go.work")
	os.WriteFile(goWorkPath, []byte("go 1.21\nuse ."), 0o644)

	snap1 := computeWatchSnapshot(dir)

	time.Sleep(10 * time.Millisecond)
	os.Chtimes(goWorkPath, time.Now().Add(time.Second), time.Now().Add(time.Second))
	snap2 := computeWatchSnapshot(dir)

	if snap1 == snap2 {
		t.Error("computeWatchSnapshot should detect go.work changes")
	}
}

func TestComputeWatchSnapshot_NestedGoFiles(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "pkg", "sub")
	os.MkdirAll(subDir, 0o755)
	os.WriteFile(filepath.Join(subDir, "handler.go"), []byte("package sub"), 0o644)

	emptyDir := t.TempDir()
	snapWithFiles := computeWatchSnapshot(dir)
	snapEmpty := computeWatchSnapshot(emptyDir)

	if snapWithFiles == snapEmpty {
		t.Error("nested .go files should be included in snapshot")
	}
}

// --- killProcessOnPort ---

func TestKillProcessOnPort_NoProcessOnPort(t *testing.T) {
	// Use a high port number unlikely to be in use
	killed := killProcessOnPort("59999")
	// On systems with lsof, returns false when no process found
	// On systems without lsof/fuser, also returns false
	_ = killed
}

func TestKillPortUnavailable(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		if err := killPortUnavailable(goos); err != nil {
			t.Errorf("killPortUnavailable(%q) = %v, want nil", goos, err)
		}
	}
	err := killPortUnavailable("windows")
	if err == nil || !strings.Contains(err.Error(), "not available on Windows") {
		t.Errorf("killPortUnavailable(windows) = %v, want a not-available error", err)
	}
}

func TestKillProcessOnPortRejectsInvalidPorts(t *testing.T) {
	for _, port := range []string{"", "0", "65536", "8080; echo unsafe", "-1"} {
		t.Run(port, func(t *testing.T) {
			if killProcessOnPort(port) {
				t.Errorf("killProcessOnPort(%q) accepted an invalid TCP port", port)
			}
		})
	}
}
