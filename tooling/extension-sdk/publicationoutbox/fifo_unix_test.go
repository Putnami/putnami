//go:build unix

package publicationoutbox

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// fifoReadDeadline bounds one read of an outbox file. A read that waits on a
// FIFO for a writer never returns, so any bound proves it does not wait.
const fifoReadDeadline = 5 * time.Second

// returnsPromptly runs read and reports whether it returned within
// fifoReadDeadline. A read that does not return keeps its goroutine, which
// touches nothing of the test.
func returnsPromptly(read func() error) (bool, error) {
	done := make(chan error, 1)
	go func() { done <- read() }()
	select {
	case err := <-done:
		return true, err
	case <-time.After(fifoReadDeadline):
		return false, nil
	}
}

// A FIFO in place of a member file is refused without waiting for a writer,
// whether it is there when the reader checks the path or swapped in between
// the check and the open.
func TestOutboxReaderRefusesAFIFOWithoutBlocking(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "publication-outbox", "every-path-stays-inside-the-outbox")
	root, member := writeNPMOutbox(t)
	outbox, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, filepath.FromSlash(member.NPM.Tarball.Path))
	regular := target + ".regular"
	fifo := target + ".fifo"
	if err := os.Rename(target, regular); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(target, 0o600); err != nil {
		t.Fatal(err)
	}
	returned, err := returnsPromptly(func() error {
		_, err := outbox.ReadFile(member.NPM.Tarball)
		return err
	})
	if !returned {
		t.Fatal("reading a FIFO member file waited for a writer")
	}
	if err == nil {
		t.Fatal("a FIFO member file was read")
	}
	returned, err = returnsPromptly(func() error { return outbox.VerifyFile(member.NPM.Tarball) })
	if !returned {
		t.Fatal("verifying a FIFO member file waited for a writer")
	}
	if err == nil {
		t.Fatal("a FIFO member file was verified")
	}

	// Swap the member file between the regular file and a FIFO while the
	// reader reads it. Every read returns, and every read that succeeds
	// returns the regular file's bytes.
	if err := os.Rename(target, fifo); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(regular, target); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		for !stop.Load() {
			// target holds the regular file: move it aside and put the FIFO
			// in its place, then restore it.
			if os.Rename(target, regular) != nil || os.Rename(fifo, target) != nil {
				return
			}
			if os.Rename(target, fifo) != nil || os.Rename(regular, target) != nil {
				return
			}
		}
	}()
	defer func() {
		stop.Store(true)
		<-swapped
	}()
	deadline := time.Now().Add(2 * time.Second)
	reads, successes := 0, 0
	for time.Now().Before(deadline) {
		var data []byte
		returned, err := returnsPromptly(func() error {
			var err error
			data, err = outbox.ReadFile(member.NPM.Tarball)
			return err
		})
		if !returned {
			stop.Store(true)
			releaseFIFO(target, fifo)
			t.Fatalf("read %d waited on a FIFO swapped in for the member file", reads+1)
		}
		reads++
		if err == nil {
			successes++
			if string(data) != "tarball bytes" {
				t.Fatalf("read %d returned %q", reads, data)
			}
		}
	}
	if reads == 0 {
		t.Fatal("no read ran")
	}
	t.Logf("%d reads, %d of the regular file", reads, successes)
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
