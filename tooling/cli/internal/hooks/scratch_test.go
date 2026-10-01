//go:build unix

package hooks

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/tooling/cli/internal/store"
)

func TestDetachedCacheGCProtectsWorkspaceScratchUntilChildExit(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "finish")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	control, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup releases the child even when an assertion fails.
	t.Cleanup(func() {
		_, _ = control.Write([]byte("finish\n"))
		_ = control.Close()
	})
	ext := &extension.ExtensionDescription{
		Name: "scratch-collector",
		Path: root,
		Jobs: map[string]*extension.JobDefinition{
			"cache-gc": {Command: "/bin/sh", Args: []string{"-c", "touch ready; read finish < finish"}},
		},
	}
	if !StartDetachedCacheGC(&workspace.Workspace{Root: root}, ext) {
		t.Fatal("collector did not start")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("collector did not announce readiness")
		}
		time.Sleep(10 * time.Millisecond)
	}
	scratch := store.ResolveScratchRoot(root)
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(scratch, "in-use")
	if err := os.WriteFile(file, []byte("active"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, ".putnami", "cache-generation"), old, old); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireScratch(root)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("detached collector lost scratch: %v", err)
	}
	if _, err := control.Write([]byte("finish\n")); err != nil {
		t.Fatal(err)
	}
	for {
		lease, err := store.AcquireScratch(root)
		if err != nil {
			t.Fatal(err)
		}
		_ = lease.Close()
		if _, err := os.Stat(file); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("collector retained its lease after exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
