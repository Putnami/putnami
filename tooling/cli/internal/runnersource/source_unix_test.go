//go:build !windows

package runnersource

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSourceRejectsFIFOWithoutOpeningIt(t *testing.T) {
	repo, store := sourceFixture(t)
	if err := syscall.Mkfifo(filepath.Join(repo, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := store.Capture(ctx, repo, nil); err == nil {
		t.Fatal("accepted a FIFO as source")
	}
}

func TestSourceMaterializationPreservesModeUnderRestrictiveUmask(t *testing.T) {
	repo, store := sourceFixture(t)
	writeSource(t, repo, "exec", []byte("#!/bin/sh\nexit 0\n"), 0o755)
	snapshot := captureSource(t, store, repo)
	parent := t.TempDir()
	previous := syscall.Umask(0o111)
	defer syscall.Umask(previous)
	dir, err := store.Materialize(parent, snapshot.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "exec"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode lost to umask: %v (%v)", info, err)
	}
}
