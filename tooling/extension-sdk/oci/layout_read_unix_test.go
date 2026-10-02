//go:build unix

package oci

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"go.putnami.dev/protocol/features/spectest"
)

// layoutReadDeadline bounds one read of a layout. A read that waits on a FIFO
// for a writer never returns, so any bound proves it does not wait.
const layoutReadDeadline = 5 * time.Second

// returnsPromptly runs read and reports whether it returned within
// layoutReadDeadline. A read that does not return keeps its goroutine, which
// touches nothing of the test.
func returnsPromptly(read func() error) (bool, error) {
	done := make(chan error, 1)
	go func() { done <- read() }()
	select {
	case err := <-done:
		return true, err
	case <-time.After(layoutReadDeadline):
		return false, nil
	}
}

// releaseFIFO opens for writing whichever of the paths is a FIFO, so a reader
// that waits on it returns.
func releaseFIFO(paths ...string) {
	for _, path := range paths {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeNamedPipe != 0 {
			if file, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = file.Close()
			}
		}
	}
}

// replaceWithFIFO moves the regular file at name to name+".regular" and puts
// a FIFO in its place.
func replaceWithFIFO(t *testing.T, name string) {
	t.Helper()
	if err := os.Rename(name, name+".regular"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A FIFO in place of index.json, the manifest or a layer is refused without
// waiting for a writer.
func TestLayoutImageRefusesAFIFOWithoutBlocking(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "every-path-stays-inside-the-outbox")
	// Each case puts the FIFO in place and returns the read that meets it.
	for name, setup := range map[string]func(t *testing.T, dir string, digest v1.Hash) func() error{
		"index.json": func(t *testing.T, dir string, digest v1.Hash) func() error {
			replaceWithFIFO(t, filepath.Join(dir, "index.json"))
			return func() error {
				_, err := layoutImage(dir, digest)
				return err
			}
		},
		"the manifest": func(t *testing.T, dir string, digest v1.Hash) func() error {
			img, err := layoutImage(dir, digest)
			if err != nil {
				t.Fatal(err)
			}
			replaceWithFIFO(t, layoutBlobPath(dir, digest))
			return func() error {
				_, err := img.RawManifest()
				return err
			}
		},
		"a layer": func(t *testing.T, dir string, digest v1.Hash) func() error {
			img, err := layoutImage(dir, digest)
			if err != nil {
				t.Fatal(err)
			}
			layers, err := img.Layers()
			if err != nil || len(layers) == 0 {
				t.Fatalf("layers = %d, %v", len(layers), err)
			}
			layerDigest, err := layers[0].Digest()
			if err != nil {
				t.Fatal(err)
			}
			replaceWithFIFO(t, layoutBlobPath(dir, layerDigest))
			return func() error {
				reader, err := layers[0].Compressed()
				if err == nil {
					_ = reader.Close()
				}
				return err
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, digest := writeRandomLayout(t)
			hash, err := v1.NewHash(digest)
			if err != nil {
				t.Fatal(err)
			}
			returned, err := returnsPromptly(setup(t, dir, hash))
			if !returned {
				t.Fatalf("reading %s waited on a FIFO for a writer", name)
			}
			if err == nil {
				t.Fatalf("%s was read from a FIFO", name)
			}
		})
	}
}

// While index.json is swapped between the regular file and a FIFO, every
// push returns: one that finds the FIFO fails, none waits on it.
func TestPushLayoutNeverBlocksOnAFIFOSwappedIn(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "every-path-stays-inside-the-outbox")
	dir, _ := writeRandomLayout(t)
	index := filepath.Join(dir, "index.json")
	regular := index + ".regular"
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		for !stop.Load() {
			if os.Rename(index, regular) != nil || os.Rename(fifo, index) != nil ||
				os.Rename(index, fifo) != nil || os.Rename(regular, index) != nil {
				return
			}
		}
	}()
	defer func() {
		stop.Store(true)
		<-swapped
	}()
	// The layout does not list this digest, so a push that reads index.json
	// fails before any request: the registry is never reached.
	target := LayoutTarget{Repository: "127.0.0.1:1/team/app", Digest: "sha256:" + strings.Repeat("a", 64)}
	pushes := 0
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); pushes++ {
		returned, err := returnsPromptly(func() error {
			_, err := PushLayout(context.Background(), dir, target, "")
			return err
		})
		if !returned {
			stop.Store(true)
			releaseFIFO(index, fifo)
			t.Fatalf("push %d waited on a FIFO swapped in for index.json", pushes+1)
		}
		if err == nil {
			t.Fatalf("push %d of a digest the layout does not list succeeded", pushes+1)
		}
	}
	if pushes == 0 {
		t.Fatal("no push ran")
	}
}
